// Package selfupdate replaces the running cw with a release: found through the
// releases/latest redirect, checked against checksums.txt, swapped in atomically.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kingswady/cwcli/internal/platform"
)

const (
	DefaultReleases = "https://github.com/kingswady/cwcli/releases"
	maxDownload     = 64 << 20 // an archive is a few MB; anything this large is wrong
)

// downloadClient follows the redirects GitHub serves release files through,
// but only to https.
var downloadClient = &http.Client{
	Timeout: 5 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if err := fetchable(req.URL); err != nil {
			return fmt.Errorf("refusing to follow a redirect: %w", err)
		}
		if len(via) > 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

// fetchable is where a release may come from: https, or plain http to this
// machine (a test's server) — never plain http across a network, where anyone
// on the way could swap the archive and its checksums together.
func fetchable(target *url.URL) error {
	if target.Scheme == "https" || (target.Scheme == "http" && platform.IsLoopback(target.Hostname())) {
		return nil
	}
	return fmt.Errorf("releases come over https, not %s", target.Redacted())
}

func fetchableURL(raw string) error {
	target, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return fetchable(target)
}

// releaseTag is a tag cw installs: vMAJOR.MINOR.PATCH, a pre-release or build after it.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]+)?$`)

// LatestTag reads the tag from the releases/latest redirect — no API call, so
// no rate limit.
func LatestTag(releases string) (string, error) {
	return LatestTagWith(platform.NewHTTPClient(), releases)
}

func LatestTagWith(httpClient *http.Client, releases string) (string, error) {
	latest := strings.TrimRight(releases, "/") + "/latest"
	if err := fetchableURL(latest); err != nil {
		return "", err
	}
	resp, err := httpClient.Get(latest)
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", releases, err)
	}
	resp.Body.Close()
	tag := path.Base(resp.Header.Get("Location"))
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || !releaseTag.MatchString(tag) {
		return "", fmt.Errorf("could not find the latest release at %s (HTTP %d)", releases, resp.StatusCode)
	}
	return tag, nil
}

func download(url string) ([]byte, error) {
	if err := fetchableURL(url); err != nil {
		return nil, fmt.Errorf("download refused: %w", err)
	}
	resp, err := downloadClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: %s (HTTP %d)", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err == nil && len(body) > maxDownload {
		err = fmt.Errorf("%s is larger than any cw release", url)
	}
	return body, err
}

// Fetch downloads this platform's archive of tag, checks it against
// the release's checksums.txt and returns the cw binary inside.
func Fetch(releases, tag string) ([]byte, error) {
	if !releaseTag.MatchString(tag) {
		return nil, fmt.Errorf("%q is not a release tag (vMAJOR.MINOR.PATCH)", tag)
	}
	name := fmt.Sprintf("cw_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name = fmt.Sprintf("cw_%s_%s.zip", runtime.GOOS, runtime.GOARCH)
	}
	base := releases + "/download/" + tag + "/"
	archive, err := download(base + name)
	if err != nil {
		return nil, err
	}
	sums, err := download(base + "checksums.txt")
	if err != nil {
		return nil, err
	}
	expected := checksumFor(sums, name)
	if expected == "" {
		return nil, fmt.Errorf("%s is not in %s's checksums.txt", name, tag)
	}
	actual := sha256.Sum256(archive)
	if hex.EncodeToString(actual[:]) != expected {
		return nil, fmt.Errorf("checksum mismatch for %s — not updating", name)
	}
	if strings.HasSuffix(name, ".zip") {
		return fromZip(archive)
	}
	return fromTarGz(archive)
}

func checksumFor(sums []byte, name string) string {
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[1] == name {
			return fields[0]
		}
	}
	return ""
}

func fromTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("the archive holds no cw binary")
		}
		if err != nil {
			return nil, err
		}
		if header.Name == "cw" && header.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(reader, maxDownload))
		}
	}
}

func fromZip(archive []byte) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, file := range reader.File {
		if file.Name == "cw.exe" {
			body, err := file.Open()
			if err != nil {
				return nil, err
			}
			defer body.Close()
			return io.ReadAll(io.LimitReader(body, maxDownload))
		}
	}
	return nil, fmt.Errorf("the archive holds no cw.exe")
}

// ReplaceExecutable swaps target for binary atomically: written beside it,
// then renamed over it, so a failure never leaves half a program. Windows
// cannot overwrite a running .exe, so the old one is moved aside first.
func ReplaceExecutable(target string, binary []byte) error {
	dir := filepath.Dir(target)
	temp, err := os.CreateTemp(dir, ".cw-update-*")
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("cannot write to %s — rerun with sudo: sudo cw update", dir)
		}
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(binary); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp.Name(), 0o755); err != nil {
		return err
	}
	return swapIn(temp.Name(), target, runtime.GOOS == "windows", os.Rename)
}

// swapIn renames temp over target. moveAside first moves target to
// target.old — Windows cannot overwrite a running .exe — and moves it back
// when temp cannot take its place, so cw is never left missing.
func swapIn(temp, target string, moveAside bool, rename func(from, to string) error) error {
	if !moveAside {
		return rename(temp, target)
	}
	old := target + ".old"
	_ = os.Remove(old)
	if err := rename(target, old); err != nil {
		return err
	}
	if err := rename(temp, target); err != nil {
		if back := rename(old, target); back != nil {
			return fmt.Errorf("%w — and putting the previous cw back failed (%v): it is at %s", err, back, old)
		}
		return err
	}
	return nil
}

// Tag is a version as a release tag, with one leading v: 0.3.0 and v0.3.0 are v0.3.0.
func Tag(version string) string { return "v" + strings.TrimPrefix(version, "v") }

// Newer reports whether release tag a is above b, by semantic versioning:
// MAJOR.MINOR.PATCH (each by its leading digits), then a pre-release
// (v1.0.0-rc.2) below its release; build metadata (+…) does not count.
func Newer(a, b string) bool { return parseVersion(a).compare(parseVersion(b)) > 0 }

type version struct {
	core [3]int
	pre  []string // the pre-release's dot-separated identifiers; none for a release
}

func parseVersion(tag string) version {
	s, _, _ := strings.Cut(strings.TrimPrefix(tag, "v"), "+")
	core, pre, isPre := strings.Cut(s, "-")
	var v version
	for i, part := range strings.SplitN(core, ".", 3) {
		v.core[i] = leadingNumber(part)
	}
	if isPre {
		v.pre = strings.Split(pre, ".")
	}
	return v
}

func leadingNumber(s string) int {
	end := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(s)
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

func (v version) compare(w version) int {
	for i := range v.core {
		if c := cmp.Compare(v.core[i], w.core[i]); c != 0 {
			return c
		}
	}
	if len(v.pre) == 0 || len(w.pre) == 0 {
		return cmp.Compare(len(w.pre), len(v.pre)) // a release is above its pre-releases
	}
	for i := 0; i < len(v.pre) && i < len(w.pre); i++ {
		if c := compareIdentifier(v.pre[i], w.pre[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(v.pre), len(w.pre))
}

// compareIdentifier orders pre-release identifiers: numbers by value, below words, words by text.
func compareIdentifier(a, b string) int {
	an, aErr := strconv.Atoi(a)
	bn, bErr := strconv.Atoi(b)
	switch {
	case aErr == nil && bErr == nil:
		return cmp.Compare(an, bn)
	case aErr == nil:
		return -1
	case bErr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// ExecutablePath is this program's own file, symlinks resolved.
func ExecutablePath() (string, error) {
	file, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(file)
}
