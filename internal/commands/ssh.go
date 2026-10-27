package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
)

// cw ssh signs in to root on a server with a certificate the platform signs
// for one of the person's own registered keys: valid an hour, kept beside the
// server's pinned host key in cw's own folder, asked for again once it runs out.
// The platform's side: docs/specs/ssh-certificates.md in deployment_workflow.

// certificateMargin is how long a kept certificate must still be valid to be used again.
const certificateMargin = time.Minute

// defaultKeys are tried in this order when --key is not given.
var defaultKeys = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// sshCertificate is the platform's answer, and what cw keeps of it.
type sshCertificate struct {
	Certificate string      `json:"certificate"`
	Serial      json.Number `json:"serial"`
	ValidBefore string      `json:"valid_before"`
	Host        string      `json:"host"`
	Port        json.Number `json:"port"`
	User        string      `json:"user"`
	KnownHosts  []string    `json:"known_hosts"`
	// PublicKey is the key it was signed for, so another --key asks again.
	PublicKey string `json:"public_key,omitempty"`
}

func (a *App) ssh(args []string) error {
	fs := a.newFlags("ssh")
	keyPath := fs.String("key", "", "your private key (default: the first of ~/.ssh/id_ed25519, id_ecdsa, id_rsa)")
	namespace := fs.String("namespace", "", "look the server up in this namespace only")
	fs.Usage = func() {
		fmt.Fprint(a.stderr, "Usage: cw ssh <server> [flags] [-- command]\n\n"+
			"Signs in to root on the server from this machine, with a certificate the platform signs for\n"+
			"your own key: valid an hour, asked for again once it runs out. Register the key in My Settings\n"+
			"→ SSH Keys first. It takes a Maintainer or Admin with terminal access to the server, and a\n"+
			"token with servers.ssh_certificate ticked; the server's sign-in log names you. A key without its\n"+
			".pub has its public key read by ssh-keygen -y (an encrypted key asks for its passphrase).\n\nFlags:\n")
		fs.PrintDefaults()
	}
	own, command := splitCommand(args)
	positional, err := parseArgs(fs, own)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one server: cw ssh <id|name>")
	}
	key, public, err := a.sshKey(*keyPath)
	if err != nil {
		return err
	}
	dir, err := a.sshDir()
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	id, err := a.resolveID(c, resourceNamed("servers"), positional[0], scope{namespace: *namespace}, output.Table{})
	if err != nil {
		return err
	}
	certificate, err := a.rootCertificate(c, id, public, dir)
	if err != nil {
		return err
	}
	return a.runSSH(sshArgs(key, dir, id, certificate, command))
}

// splitCommand parts cw's own arguments from the command after "--", which is ssh's.
func splitCommand(args []string) (own, command []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// sshKey is the private key and its public line: --key, else the first default
// key with a .pub beside it. A key without its .pub (copied over, restored from
// a password manager, provisioned in CI) has its public line derived by
// ssh-keygen; in the default search only the first key there is, so an
// encrypted one asks for its passphrase once.
func (a *App) sshKey(path string) (key, public string, err error) {
	if path != "" {
		key = a.expandHome(path)
		public, err = a.publicKey(key)
		return key, public, err
	}
	home := a.getenv("HOME")
	if home == "" {
		return "", "", errors.New("no HOME to look for ~/.ssh in: name the key with --key")
	}
	first := ""
	for _, name := range defaultKeys {
		candidate := filepath.Join(home, ".ssh", name)
		if line, readErr := os.ReadFile(candidate + ".pub"); readErr == nil {
			return candidate, strings.TrimSpace(string(line)), nil
		}
		if _, statErr := os.Stat(candidate); first == "" && statErr == nil {
			first = candidate
		}
	}
	if first == "" {
		return "", "", errors.New("no SSH key in ~/.ssh (id_ed25519, id_ecdsa or id_rsa): name one with --key")
	}
	public, err = a.publicKey(first)
	return first, public, err
}

// publicKey is key's public line: its .pub, else what ssh-keygen -y reads from
// the private key. cw never parses a private key itself.
func (a *App) publicKey(key string) (string, error) {
	line, readErr := os.ReadFile(key + ".pub")
	if readErr == nil {
		return strings.TrimSpace(string(line)), nil
	}
	derived, deriveErr := a.deriveKey(key)
	if deriveErr != nil {
		return "", fmt.Errorf("no public key for %s: no %s.pub (%v), and ssh-keygen -y could not read the key (%v)",
			key, filepath.Base(key), readErr, deriveErr)
	}
	fmt.Fprintf(a.stderr, "cw: no %s.pub, so its public key came from ssh-keygen -y\n", filepath.Base(key))
	return derived, nil
}

// deriveKey runs ssh-keygen -y on key in this terminal, so an encrypted key asks
// for its passphrase; tests replace it.
func (a *App) deriveKey(key string) (string, error) {
	if a.keygenRunner != nil {
		return a.keygenRunner(key)
	}
	cmd := exec.Command("ssh-keygen", "-y", "-f", key)
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if len(strings.Fields(line)) < 2 {
		return "", errors.New("ssh-keygen printed no public key")
	}
	return line, nil
}

func (a *App) expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(a.getenv("HOME"), rest)
	}
	return path
}

// sshDir is where cw keeps certificates and host keys: its own folder, 0700.
func (a *App) sshDir() (string, error) {
	if a.configErr != nil {
		return "", fmt.Errorf("nowhere to keep the certificate: %w", a.configErr)
	}
	dir := filepath.Join(a.configDir, "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o700)
}

// rootCertificate is the kept certificate while it holds for this key, else a new one from the platform.
func (a *App) rootCertificate(c *platform.Client, id, public, dir string) (sshCertificate, error) {
	metaPath := filepath.Join(dir, id+".json")
	var kept sshCertificate
	if raw, err := os.ReadFile(metaPath); err == nil && json.Unmarshal(raw, &kept) == nil {
		if sameKey(kept.PublicKey, public) && a.clock().Add(certificateMargin).Before(validUntil(kept.ValidBefore)) {
			return kept, nil
		}
	}
	raw, _, err := a.postWrite(c, "/servers/"+id+"/ssh-certificate", map[string]any{"public_key": public})
	if err != nil {
		return sshCertificate{}, writeError(c, "cw ssh", err)
	}
	var fresh sshCertificate
	if err := platform.Decode(raw, &fresh); err != nil {
		return sshCertificate{}, err
	}
	fresh.PublicKey = public
	fmt.Fprintf(a.stderr, "cw: certificate %s for %s@%s, valid until %s UTC\n", fresh.Serial, fresh.User, fresh.Host, fresh.ValidBefore)
	files := map[string]string{
		id + "-cert.pub":    fresh.Certificate + "\n",
		id + "-known_hosts": strings.Join(fresh.KnownHosts, "\n") + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return sshCertificate{}, err
		}
	}
	meta, err := json.Marshal(fresh)
	if err != nil {
		return sshCertificate{}, err
	}
	return fresh, os.WriteFile(metaPath, meta, 0o600)
}

func sameKey(a, b string) bool {
	left, right := strings.Fields(a), strings.Fields(b)
	return len(left) >= 2 && len(right) >= 2 && left[0] == right[0] && left[1] == right[1]
}

// validUntil reads the platform's UTC time ("2006-01-02 15:04:05"); zero when unreadable.
func validUntil(value string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if at, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return at
		}
	}
	return time.Time{}
}

// sshArgs is ssh's command line: the key and its certificate, the platform's pin of the host key.
func sshArgs(key, dir, id string, certificate sshCertificate, command []string) []string {
	checking := "StrictHostKeyChecking=yes"
	if len(certificate.KnownHosts) == 0 {
		// The platform has no pin yet: the first sign-in pins it in cw's own file.
		checking = "StrictHostKeyChecking=accept-new"
	}
	args := []string{
		"-i", key,
		"-o", "CertificateFile=" + optionPath(filepath.Join(dir, id+"-cert.pub")),
		"-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=" + optionPath(filepath.Join(dir, id+"-known_hosts")),
		"-o", checking,
	}
	if port := certificate.Port.String(); port != "" && port != "22" {
		args = append(args, "-p", port)
	}
	args = append(args, certificate.User+"@"+certificate.Host)
	return append(args, command...)
}

// optionPath quotes a path for an -o value: ssh ends the value at a space, and
// cw's folder on macOS is under "Application Support".
func optionPath(path string) string {
	return `"` + path + `"`
}

// runSSH runs ssh in this terminal; tests replace it.
func (a *App) runSSH(args []string) error {
	if a.sshRunner != nil {
		return a.sshRunner(args)
	}
	cmd := exec.Command("ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
