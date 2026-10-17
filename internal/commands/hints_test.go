package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// expiringAPI answers /whoami with the given token expiry header.
func expiringAPI(t *testing.T, expires time.Time) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Token-Expires-At", expires.UTC().Format(time.RFC3339))
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"login": "a", "name": "A", "namespaces": []any{}}})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestATokenInItsLastWeekIsWarnedAbout(t *testing.T) {
	h := newHarness(t, expiringAPI(t, time.Now().Add(3*24*time.Hour+time.Hour)))
	if code := h.run("whoami"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "expires in 3 days") {
		t.Errorf("stderr %q", h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "Token expires ") {
		t.Errorf("whoami should show the expiry: %q", h.stdout)
	}
	far := newHarness(t, expiringAPI(t, time.Now().Add(60*24*time.Hour)))
	far.run("whoami")
	if strings.Contains(far.stderr.String(), "expires") {
		t.Errorf("no warning two months out: %q", far.stderr)
	}
}

func TestAnUpdateHintAtMostOnceADayInATerminal(t *testing.T) {
	checks := 0
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks++
		http.Redirect(w, r, "/tag/v9.9.9", http.StatusFound)
	}))
	t.Cleanup(releases.Close)
	api := fakeAPI(t, map[string]any{"/whoami": map[string]any{"login": "a", "name": "A"}})

	h := newHarness(t, api)
	h.app.version = "0.3.0"
	h.env["CW_DOWNLOAD_BASE"] = releases.URL
	h.app.stdoutIsTerminal = func() bool { return true }
	now := time.Now()
	h.app.clock = func() time.Time { return now }
	h.run("whoami")
	if !strings.Contains(h.stderr.String(), "cw v9.9.9 is available (you have v0.3.0) — run: cw update") {
		t.Fatalf("stderr %q", h.stderr)
	}
	h.stderr.Reset()
	h.run("whoami") // the same day: told already, and GitHub not asked again
	if checks != 1 || strings.Contains(h.stderr.String(), "available") {
		t.Errorf("checks %d, stderr %q", checks, h.stderr)
	}
	now = now.Add(25 * time.Hour)
	h.run("whoami")
	if checks != 2 || !strings.Contains(h.stderr.String(), "v9.9.9 is available") {
		t.Errorf("a day later: checks %d, stderr %q", checks, h.stderr)
	}

	for name, tweak := range map[string]func(*harness){
		"in CI":   func(h *harness) { h.env["CI"] = "true" },
		"opt-out": func(h *harness) { h.env["CW_NO_UPDATE_CHECK"] = "1" },
		"piped":   func(h *harness) { h.app.stdoutIsTerminal = func() bool { return false } },
	} {
		quiet := newHarness(t, api)
		quiet.app.version = "0.3.0"
		quiet.env["CW_DOWNLOAD_BASE"] = releases.URL
		quiet.app.stdoutIsTerminal = func() bool { return true }
		tweak(quiet)
		quiet.run("whoami")
		if strings.Contains(quiet.stderr.String(), "available") {
			t.Errorf("%s: %q", name, quiet.stderr)
		}
	}
}

// offerHarness is a cw 0.2.0 in a terminal, someone at the keyboard typing
// answer, with v0.3.0 out.
func offerHarness(t *testing.T, answer string) (*harness, string) {
	t.Helper()
	server, _ := fakeReleases(t, "v0.3.0", []byte("new binary"), false)
	h, target := updateHarness(t, server, "0.2.0")
	h.app.stdin = strings.NewReader(answer)
	h.app.stdoutIsTerminal = func() bool { return true }
	h.app.stdinIsTerminal = func() bool { return true }
	return h, target
}

func TestANewerReleaseIsOfferedAndInstalledOnYes(t *testing.T) {
	for _, answer := range []string{"\n", "y\n", "YES\n"} {
		h, target := offerHarness(t, answer)
		if code := h.run("help"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr)
		}
		if !strings.Contains(h.stderr.String(), "cw v0.3.0 is available (you have v0.2.0). Update now? [Y/n]") {
			t.Errorf("%q: stderr %q", answer, h.stderr)
		}
		if body, _ := os.ReadFile(target); string(body) != "new binary" || !strings.Contains(h.stdout.String(), "Updated cw v0.2.0 → v0.3.0") {
			t.Errorf("%q: binary %q, stdout %q", answer, body, h.stdout)
		}
	}
}

func TestANoKeepsThisCwAndIsNotAskedAgainToday(t *testing.T) {
	h, target := offerHarness(t, "n\n")
	h.run("help")
	if body, _ := os.ReadFile(target); string(body) != "old binary" || !strings.Contains(h.stderr.String(), "Not now") {
		t.Fatalf("binary %q, stderr %q", body, h.stderr)
	}
	h.stderr.Reset()
	h.app.stdin = strings.NewReader("y\n")
	h.run("help")
	if strings.Contains(h.stderr.String(), "Update now?") {
		t.Errorf("asked again the same day: %q", h.stderr)
	}
}

func TestAnUnsignedReleaseIsOfferedButNotInstalled(t *testing.T) {
	server, _ := fakeSignedReleases(t, "v0.3.0", []byte("unsigned binary"), false, "none")
	h, target := updateHarness(t, server, "0.2.0")
	h.app.stdin = strings.NewReader("y\n")
	h.app.stdoutIsTerminal = func() bool { return true }
	h.app.stdinIsTerminal = func() bool { return true }
	if code := h.run("help"); code != 0 {
		t.Errorf("the command itself succeeded; exit %d", code)
	}
	if body, _ := os.ReadFile(target); string(body) != "old binary" || !strings.Contains(h.stderr.String(), "is not signed") {
		t.Errorf("binary %q, stderr %q", body, h.stderr)
	}
}

func TestAHomebrewInstallIsToldToUseBrew(t *testing.T) {
	h, _ := offerHarness(t, "y\n")
	h.app.executable = func() (string, error) { return "/opt/homebrew/Caskroom/cw/0.2.0/cw", nil }
	h.run("help")
	if !strings.Contains(h.stderr.String(), "run: brew upgrade cw") || strings.Contains(h.stderr.String(), "Update now?") {
		t.Errorf("stderr %q", h.stderr)
	}
	if code := h.run("update"); code != 1 || !strings.Contains(h.stderr.String(), "installed with Homebrew") {
		t.Errorf("cw update: exit %d, %q", code, h.stderr)
	}
}

func TestAVersionStampedWithItsVIsNotDoubled(t *testing.T) {
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tag/v9.9.9", http.StatusFound)
	}))
	t.Cleanup(releases.Close)
	h := newHarness(t, fakeAPI(t, map[string]any{"/whoami": map[string]any{"login": "a", "name": "A"}}))
	h.app.version = "v0.3.0" // make build: git describe
	h.env["CW_DOWNLOAD_BASE"] = releases.URL
	h.app.stdoutIsTerminal = func() bool { return true }
	h.run("whoami")
	if !strings.Contains(h.stderr.String(), "(you have v0.3.0)") {
		t.Errorf("stderr %q", h.stderr)
	}
}
