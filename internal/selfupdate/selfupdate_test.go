package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseOrder(t *testing.T) {
	cases := map[[2]string]bool{
		{"v0.3.0", "v0.2.9"}: true, {"v0.10.0", "v0.9.0"}: true, {"v1.0.0", "v0.99.99"}: true,
		{"v0.2.0", "v0.2.0"}: false, {"v0.2.0", "v0.3.0"}: false,
		// A pre-release is below its release, above the one before.
		{"v0.3.0", "v0.3.0-rc1"}: true, {"v0.3.0-rc1", "v0.3.0"}: false, {"v0.3.0-rc1", "v0.2.9"}: true,
		// The patch of 0.3.1-rc.2 is 1, not unreadable.
		{"v0.3.1-rc.2", "v0.3.0"}: true, {"v0.3.1-rc.2", "v0.3.1"}: false,
		// Identifiers: numbers by value, below words; more identifiers above fewer.
		{"v0.3.0-rc.10", "v0.3.0-rc.9"}: true, {"v0.3.0-beta", "v0.3.0-alpha"}: true,
		{"v0.3.0-rc.1", "v0.3.0-1"}: true, {"v0.3.0-alpha.1", "v0.3.0-alpha"}: true,
		// Build metadata does not order.
		{"v0.3.0+build.5", "v0.3.0"}: false, {"v0.3.0", "v0.3.0+build.5"}: false,
	}
	for pair, want := range cases {
		if got := Newer(pair[0], pair[1]); got != want {
			t.Errorf("Newer(%s, %s) = %v", pair[0], pair[1], got)
		}
	}
}

func TestTagHasOneLeadingV(t *testing.T) {
	for in, want := range map[string]string{"0.3.0": "v0.3.0", "v0.3.0": "v0.3.0"} {
		if got := Tag(in); got != want {
			t.Errorf("Tag(%q) = %q", in, got)
		}
	}
}

func TestPlainHTTPAcrossANetworkIsRefused(t *testing.T) {
	for _, raw := range []string{"http://example.com/cw.tar.gz", "ftp://example.com/cw.tar.gz"} {
		if _, err := download(raw); err == nil || !strings.Contains(err.Error(), "releases come over https") {
			t.Errorf("download(%s): %v", raw, err)
		}
	}
	if _, err := Fetch("http://example.com/releases", "v1.0.0"); err == nil {
		t.Error("Fetch over plain http must be refused")
	}
	if _, err := LatestTag("http://example.com/releases"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("LatestTag: %v", err)
	}
	for _, ok := range []string{"https://github.com/x", "http://127.0.0.1:8080/x", "http://localhost/x"} {
		if err := fetchableURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func TestALatestRedirectThatIsNoReleaseTagIsRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/tag/v9.9.9;curl evil")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	if tag, err := LatestTag(server.URL); err == nil {
		t.Errorf("tag %q accepted", tag)
	}
	if _, err := Fetch(server.URL, "../../v1.0.0"); err == nil {
		t.Error("Fetch takes release tags only")
	}
}

func TestAFailedSwapPutsThePreviousBinaryBack(t *testing.T) {
	dir := t.TempDir()
	target, temp := filepath.Join(dir, "cw.exe"), filepath.Join(dir, ".cw-update-1")
	os.WriteFile(target, []byte("old binary"), 0o755)
	os.WriteFile(temp, []byte("new binary"), 0o755)
	refused := errors.New("the file is in use")
	rename := func(from, to string) error {
		if from == temp {
			return refused
		}
		return os.Rename(from, to)
	}
	if err := swapIn(temp, target, true, rename); !errors.Is(err, refused) {
		t.Fatalf("got %v", err)
	}
	if body, _ := os.ReadFile(target); string(body) != "old binary" {
		t.Errorf("cw.exe is %q, want the previous binary back", body)
	}
	if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
		t.Errorf("cw.exe.old is left: %v", err)
	}
}

func TestASwapMovesThePreviousBinaryAside(t *testing.T) {
	dir := t.TempDir()
	target, temp := filepath.Join(dir, "cw.exe"), filepath.Join(dir, ".cw-update-1")
	os.WriteFile(target, []byte("old binary"), 0o755)
	os.WriteFile(target+".old", []byte("older binary"), 0o755)
	os.WriteFile(temp, []byte("new binary"), 0o755)
	if err := swapIn(temp, target, true, os.Rename); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(target); string(body) != "new binary" {
		t.Errorf("cw.exe is %q", body)
	}
	if body, _ := os.ReadFile(target + ".old"); string(body) != "old binary" {
		t.Errorf("cw.exe.old is %q", body)
	}
}

func TestALatestReleaseIsFoundAfterTheRepositoryMoved(t *testing.T) {
	// A renamed repository answers 301 to its new name, which answers 302 to the tag.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old/releases/latest":
			http.Redirect(w, r, "/new/releases/latest", http.StatusMovedPermanently)
		case "/new/releases/latest":
			http.Redirect(w, r, "/new/releases/tag/v1.2.3", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if tag, err := LatestTag(server.URL + "/old/releases"); err != nil || tag != "v1.2.3" {
		t.Fatalf("tag %q, err %v", tag, err)
	}
}

func TestAMoveIsFollowedOnlyToReleasesOverHTTPSAndNotForever(t *testing.T) {
	var hops int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plain/releases/latest":
			http.Redirect(w, r, "http://example.com/x/releases/latest", http.StatusMovedPermanently)
		case "/elsewhere/releases/latest":
			http.Redirect(w, r, "/login", http.StatusFound)
		default: // /loop/<n>/releases/latest moves on and on
			hops++
			http.Redirect(w, r, fmt.Sprintf("/loop/%d/releases/latest", hops), http.StatusMovedPermanently)
		}
	}))
	t.Cleanup(server.Close)
	for _, base := range []string{"/plain/releases", "/elsewhere/releases", "/loop/0/releases"} {
		if tag, err := LatestTag(server.URL + base); err == nil {
			t.Errorf("%s: tag %q accepted", base, tag)
		}
	}
	if hops > maxMoves+1 {
		t.Errorf("followed %d moves", hops)
	}
}

func TestOnlyASignatureByAReleaseKeyCounts(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	t.Cleanup(TrustOnlyForTest(base64.StdEncoding.EncodeToString(public)))
	sums := []byte("abc  cw_linux_amd64.tar.gz\n")
	if !signedByRelease(sums, ed25519.Sign(private, sums)) {
		t.Error("the release key's signature is refused")
	}
	for name, signature := range map[string][]byte{
		"another key": ed25519.Sign(other, sums),
		"other text":  ed25519.Sign(private, []byte("abc  cw_linux_arm64.tar.gz\n")),
		"short":       []byte("not a signature"),
		"none":        nil,
	} {
		if signedByRelease(sums, signature) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestTheBuiltInReleaseKeyIsAKey(t *testing.T) {
	for _, encoded := range releaseKeys {
		if key, err := base64.StdEncoding.DecodeString(encoded); err != nil || len(key) != ed25519.PublicKeySize {
			t.Errorf("release key %q: %d bytes, %v", encoded, len(key), err)
		}
	}
}
