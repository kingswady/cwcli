package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func archiveOf(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string][]byte{"README.md": []byte("readme"), "cw": binary} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// fakeReleases answers like github.com/kingswady/cwcli/releases for one tag.
func fakeReleases(t *testing.T, latest string, binary []byte, tamper bool) (*httptest.Server, *int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake serves tar.gz archives")
	}
	name := fmt.Sprintf("cw_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := archiveOf(t, binary)
	sum := sha256.Sum256(archive)
	if tamper {
		sum[0] ^= 0xff
	}
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/latest":
			http.Redirect(w, r, "/tag/"+latest, http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/"+name):
			downloads++
			w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &downloads
}

func updateHarness(t *testing.T, server *httptest.Server, running string) (*harness, string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "cw")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, nil)
	h.env["CW_DOWNLOAD_BASE"] = server.URL
	h.app.executable = func() (string, error) { return target, nil }
	h.app.version = running
	return h, target
}

func TestUpdateReplacesTheBinaryWithTheLatestRelease(t *testing.T) {
	server, _ := fakeReleases(t, "v0.3.0", []byte("new binary"), false)
	h, target := updateHarness(t, server, "0.2.0")
	if code := h.run("update"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	body, _ := os.ReadFile(target)
	info, _ := os.Stat(target)
	if string(body) != "new binary" || info.Mode().Perm() != 0o755 {
		t.Errorf("binary %q mode %v", body, info.Mode())
	}
	if !strings.Contains(h.stdout.String(), "Updated cw v0.2.0 → v0.3.0") {
		t.Errorf("stdout %q", h.stdout)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".cw-update-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left: %v", leftovers)
	}
}

func TestUpdateWhenAlreadyCurrentDownloadsNothing(t *testing.T) {
	server, downloads := fakeReleases(t, "v0.2.0", []byte("new binary"), false)
	h, target := updateHarness(t, server, "0.2.0")
	if code := h.run("update"); code != 0 || !strings.Contains(h.stdout.String(), "up to date (v0.2.0)") {
		t.Fatalf("exit %d: %s %s", code, h.stdout, h.stderr)
	}
	if body, _ := os.ReadFile(target); string(body) != "old binary" || *downloads != 0 {
		t.Errorf("binary %q, %d downloads", body, *downloads)
	}
}

func TestUpdateCheckOnlyReports(t *testing.T) {
	server, downloads := fakeReleases(t, "v0.3.0", []byte("new binary"), false)
	h, target := updateHarness(t, server, "0.2.0")
	if code := h.run("update", "--check"); code != 0 || !strings.Contains(h.stdout.String(), "v0.3.0 is available") {
		t.Fatalf("exit %d: %s", code, h.stdout)
	}
	if body, _ := os.ReadFile(target); string(body) != "old binary" || *downloads != 0 {
		t.Errorf("--check changed something: %q, %d downloads", body, *downloads)
	}
}

func TestATamperedArchiveIsNotInstalled(t *testing.T) {
	server, _ := fakeReleases(t, "v0.3.0", []byte("evil binary"), true)
	h, target := updateHarness(t, server, "0.2.0")
	if code := h.run("update"); code != 1 || !strings.Contains(h.stderr.String(), "checksum mismatch") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if body, _ := os.ReadFile(target); string(body) != "old binary" {
		t.Errorf("binary replaced: %q", body)
	}
}

func TestUpdateToAChosenVersion(t *testing.T) {
	server, _ := fakeReleases(t, "v9.9.9", []byte("pinned binary"), false)
	h, target := updateHarness(t, server, "0.3.0")
	if code := h.run("update", "--version", "v0.2.0", "--force"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if body, _ := os.ReadFile(target); string(body) != "pinned binary" {
		t.Errorf("binary %q", body)
	}
	bare, _ := updateHarness(t, server, "0.3.0")
	if code := bare.run("update", "--version", "0.2.0", "--force"); code != 0 || !strings.Contains(bare.stdout.String(), "→ v0.2.0") {
		t.Errorf("--version without its v: exit %d: %s %s", code, bare.stdout, bare.stderr)
	}
}

func TestADevelopmentBuildNeedsForce(t *testing.T) {
	server, _ := fakeReleases(t, "v0.3.0", []byte("new binary"), false)
	h, _ := updateHarness(t, server, "dev")
	if code := h.run("update"); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestAnUnwritableDirectorySuggestsSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	server, _ := fakeReleases(t, "v0.3.0", []byte("new binary"), false)
	h, target := updateHarness(t, server, "0.2.0")
	dir := filepath.Dir(target)
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if code := h.run("update"); code != 1 || !strings.Contains(h.stderr.String(), "sudo cw update") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}
