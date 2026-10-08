// Package config is what cw keeps on this machine: the platforms logged in to,
// the one in use, and one token per platform (keychain, or a 0600 file).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kingswady/cwcli/internal/platform"
)

const DefaultURL = "https://www.cloudwady.com"

type Config struct {
	URL string `json:"url"`
	// Platforms are the ones logged in to, for cw use.
	Platforms []string `json:"platforms,omitempty"`
}

func (cfg *Config) Remember(base string) {
	for _, known := range cfg.Platforms {
		if known == base {
			return
		}
	}
	cfg.Platforms = append(cfg.Platforms, base)
}

func (cfg *Config) Forget(base string) {
	kept := cfg.Platforms[:0]
	for _, known := range cfg.Platforms {
		if known != base {
			kept = append(kept, known)
		}
	}
	cfg.Platforms = kept
}

func path(dir string) string { return filepath.Join(dir, "config.json") }

// ErrNoConfigDir is a config path that is not absolute: the user's config
// directory could not be found, and cw never falls back to the current one.
var ErrNoConfigDir = errors.New("cw has no config directory")

// Load reads dir's config; a missing or unreadable one is empty, and so is
// one in no absolute directory (never the current directory's).
func Load(dir string) Config {
	var cfg Config
	if !filepath.IsAbs(dir) {
		return cfg
	}
	if raw, err := os.ReadFile(path(dir)); err == nil {
		_ = json.Unmarshal(raw, &cfg)
	}
	return cfg
}

func Save(dir string, cfg Config) error {
	return WritePrivateJSON(path(dir), cfg)
}

// Where the platform URL came from, so login can say why it picked it.
const (
	FromFlag    = "flag"
	FromEnv     = "env"
	FromSaved   = "saved"
	FromDefault = "default"
)

// Resolve is the platform to talk to: an explicit value, then CW_URL, then the
// one saved at login, then the default — and where it came from.
func Resolve(explicit string, getenv func(string) string, dir string) (base, source string, err error) {
	candidates := []struct{ value, source string }{
		{explicit, FromFlag},
		{getenv("CW_URL"), FromEnv},
		{Load(dir).URL, FromSaved},
	}
	for _, candidate := range candidates {
		if candidate.value != "" {
			base, err := platform.NormalizeURL(candidate.value)
			return base, candidate.source, err
		}
	}
	return DefaultURL, FromDefault, nil
}

// WritePrivateJSON writes value to path readable by this user only. It
// writes a temporary file beside path, 0600 and synced, and renames it over
// path: a crash or a full disk leaves the previous file whole, and the mode of
// a file that was there (a restored 0644 copy) never carries over. path's
// directory — cw's own — is made, or made again, 0700.
func WritePrivateJSON(path string, value any) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: refusing to write %s in the current directory", ErrNoConfigDir, path)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if err := writeSynced(temp, append(raw, '\n')); err != nil {
		os.Remove(temp.Name())
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		os.Remove(temp.Name())
		return err
	}
	syncDir(dir)
	return nil
}

// writeSynced writes data to f, 0600, on disk before f is closed.
func writeSynced(f *os.File, data []byte) error {
	err := f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// syncDir makes a rename in dir durable where the system can (not Windows).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
