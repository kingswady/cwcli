package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	shellApp    = "app_7k2m9xq4pzta"
	shellServer = "srv_3h8c2w6yrdne"
)

// shellAppRecord is shop as apps.get answers it: on web-1, whose id names the key.
func shellAppRecord() record {
	return record{
		"id": shellApp, "name": "shop", "project": "Shop", "environment_type": "staging",
		"server": "web-1", "server_id": shellServer,
	}
}

// shellAnswer is apps.shell_certificate's answer for mode.
func shellAnswer(mode, validBefore string) map[string]any {
	answer := certificateAnswer("odoo", validBefore)
	answer["account"], answer["mode"], answer["app"], answer["server"] = "app", mode, "shop", shellServer
	return answer
}

// appShellPlatform finds shop by name, answers app for it, and hands each
// certificate request to answer, keeping the mode it asked in asked.
func appShellPlatform(t *testing.T, app record, asked *[]string, answer func(w http.ResponseWriter, mode string)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/apps"):
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": []record{app}, "total": 1}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/apps/"+shellApp):
			json.NewEncoder(w).Encode(map[string]any{"data": app})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/apps/"+shellApp+"/shell-certificate"):
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["public_key"] != publicKey || len(body) != 2 {
				t.Errorf("body %v %v: the public key and the mode, nothing else", body, err)
			}
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("a change carries an Idempotency-Key")
			}
			*asked = append(*asked, body["mode"])
			answer(w, body["mode"])
		default:
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func signingShells(validBefore string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, mode string) {
		json.NewEncoder(w).Encode(map[string]any{"data": shellAnswer(mode, validBefore)})
	}
}

func refusingShells(status int, code, message string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, _ string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
	}
}

// appShellCommandLine is ssh's line for mode: a terminal, the kept files, and no command after the login.
func appShellCommandLine(h *harness, mode string) string {
	kept := filepath.Join(h.app.configDir, "ssh", "app-"+shellApp+"-"+mode)
	return strings.Join([]string{
		"-t",
		"-i", filepath.Join(h.env["HOME"], ".ssh", "id_ed25519"),
		"-o", `CertificateFile="` + kept + `-cert.pub"`,
		"-o", "IdentitiesOnly=yes",
		"-o", `UserKnownHostsFile="` + kept + `-known_hosts"`,
		"-o", "StrictHostKeyChecking=yes",
		"-p", "2222", "appshell@192.0.2.9",
	}, " ")
}

func TestAppShellOpensTheShellByDefault(t *testing.T) {
	var asked []string
	h, ran := sshHarness(t, appShellPlatform(t, shellAppRecord(), &asked, signingShells(inAnHour())))
	if code := h.run("apps", "shell", "shop"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if strings.Join(asked, ",") != "shell" {
		t.Errorf("asked for %q: no --odoo asks for the shell", asked)
	}
	if got, want := strings.Join(*ran, " "), appShellCommandLine(h, "shell"); got != want {
		t.Errorf("ssh\n got %s\nwant %s", got, want)
	}
	checkKeptFiles(t, filepath.Join(h.app.configDir, "ssh"), "app-"+shellApp+"-shell", "appshell")
	if !strings.Contains(h.stderr.String(), "cw: certificate 7 for the shell of shop, valid until ") {
		t.Errorf("stderr %q", h.stderr)
	}
	// Still valid for the hour: used again, nothing asked.
	if code := h.run("apps", "shell", "shop"); code != 0 || len(asked) != 1 {
		t.Errorf("exit %d, %d requests: a valid certificate is used again", code, len(asked))
	}
}

// --odoo asks for the odoo shell; each mode keeps its own certificate.
func TestAppShellKeepsACertificatePerMode(t *testing.T) {
	var asked []string
	h, ran := sshHarness(t, appShellPlatform(t, shellAppRecord(), &asked, signingShells(inAnHour())))
	steps := []struct {
		args  []string
		asked string
		mode  string
	}{
		{[]string{"apps", "shell", "shop", "--odoo"}, "odoo", "odoo"},
		{[]string{"apps", "shell", "shop"}, "odoo,shell", "shell"},
		{[]string{"apps", "shell", "--odoo", shellApp}, "odoo,shell", "odoo"},
		{[]string{"apps", "shell", shellApp}, "odoo,shell", "shell"},
	}
	for _, step := range steps {
		if code := h.run(step.args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", step.args, code, h.stderr)
		}
		if got := strings.Join(asked, ","); got != step.asked {
			t.Errorf("%v: asked %q, want %q", step.args, got, step.asked)
		}
		if got, want := strings.Join(*ran, " "), appShellCommandLine(h, step.mode); got != want {
			t.Errorf("%v: ssh\n got %s\nwant %s", step.args, got, want)
		}
	}
	checkKeptFiles(t, filepath.Join(h.app.configDir, "ssh"), "app-"+shellApp+"-odoo", "appshell")
	if !strings.Contains(h.stderr.String(), "cw: certificate 7 for the odoo shell of shop, valid until ") {
		t.Errorf("stderr %q", h.stderr)
	}
}

// The key is the one named for the app's server, which apps.get says.
func TestAppShellSignsInWithItsServersKey(t *testing.T) {
	var asked []string
	h, ran := sshHarness(t, appShellPlatform(t, shellAppRecord(), &asked, signingShells(inAnHour())))
	work := filepath.Join(h.env["HOME"], ".ssh", "work")
	if err := os.WriteFile(work+".pub", []byte(publicKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.env["CW_SERVER_SSH_"+shellServer] = work
	if code := h.run("apps", "shell", "shop"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if (*ran)[2] != work {
		t.Errorf("ssh %v: the key of the app's server comes before CW_SERVER_SSH", *ran)
	}
	delete(h.env, "CW_SERVER_SSH_"+shellServer)
	delete(h.env, "CW_SERVER_SSH")
	want := "no SSH key named for " + shellServer + ": set CW_SERVER_SSH_" + shellServer + " (this server)"
	if code := h.run("apps", "shell", "shop", "--odoo"); code != 1 || len(asked) != 1 || !strings.Contains(h.stderr.String(), want) {
		t.Errorf("exit %d, %d requests: %s", code, len(asked), h.stderr)
	}
}

// The certificate forces the shell: a command is refused before anything is asked.
func TestAppShellRefusesACommand(t *testing.T) {
	untouched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("asked %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(untouched.Close)
	h, ran := sshHarness(t, untouched)
	if code := h.run("apps", "shell", "shop", "--odoo", "--", "ls"); code != 2 || *ran != nil {
		t.Errorf("exit %d, ssh %v", code, *ran)
	}
	if want := "cw: an app shell takes no command\n"; h.stderr.String() != want {
		t.Errorf("stderr %q, want %q", h.stderr, want)
	}
}

// A platform without app shells says so through each of its answers, and cw
// says it needs a newer version: nothing is kept, ssh never runs.
func TestAppShellNeedsAPlatformWithAppShells(t *testing.T) {
	withoutServerID := shellAppRecord()
	delete(withoutServerID, "server_id")
	newer := "does not offer cw apps shell yet — it needs a newer platform version"
	cases := []struct {
		name   string
		app    record
		answer func(http.ResponseWriter, string)
		asked  int
		want   string
	}{
		{"apps.get without the server's id", withoutServerID, nil, 0, newer},
		{"no such function", shellAppRecord(),
			refusingShells(http.StatusNotFound, "not_found", "No API function at /apps/"+shellApp+"/shell-certificate"), 1, newer},
		{"a certificate that is no app shell", shellAppRecord(), func(w http.ResponseWriter, _ string) {
			json.NewEncoder(w).Encode(map[string]any{"data": certificateAnswer("odoo", inAnHour())})
		}, 1, "did not sign a certificate for the shell of an app — it needs a newer platform version; cw did not sign in"},
		{"another mode", shellAppRecord(), func(w http.ResponseWriter, _ string) {
			json.NewEncoder(w).Encode(map[string]any{"data": shellAnswer("odoo", inAnHour())})
		}, 1, "did not sign a certificate for the shell of an app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			h, ran := sshHarness(t, appShellPlatform(t, tc.app, &asked, tc.answer))
			if code := h.run("apps", "shell", "shop"); code != 1 || *ran != nil || len(asked) != tc.asked {
				t.Fatalf("exit %d, ssh %v, %d requests: %s", code, *ran, len(asked), h.stderr)
			}
			if !strings.Contains(h.stderr.String(), tc.want) {
				t.Errorf("want %q in %q", tc.want, h.stderr)
			}
			if _, err := os.Stat(filepath.Join(h.app.configDir, "ssh", "app-"+shellApp+"-shell.json")); err == nil {
				t.Error("a refused certificate is not kept")
			}
		})
	}
}

func TestAppShellOnNoServer(t *testing.T) {
	app := shellAppRecord()
	app["server_id"] = nil
	var asked []string
	h, ran := sshHarness(t, appShellPlatform(t, app, &asked, nil))
	if code := h.run("apps", "shell", "shop"); code != 1 || *ran != nil || len(asked) != 0 {
		t.Fatalf("exit %d, ssh %v, %d requests", code, *ran, len(asked))
	}
	if want := "cw: shop runs on no server: it has no shell to open\n"; h.stderr.String() != want {
		t.Errorf("stderr %q, want %q", h.stderr, want)
	}
}

// A 403 does not say whether the level or the key failed: cw names both on a
// line after the platform's words. A 400 carries its reason, printed as it comes.
func TestAppShellSaysWhatItTakesWhenRefused(t *testing.T) {
	reason := "web-1's app shell is not ready: Harden Server sets it up."
	cases := []struct {
		name   string
		answer func(http.ResponseWriter, string)
		code   int
		want   string
	}{
		{"403", refusingShells(http.StatusForbidden, "access_denied", "Access denied"), 1,
			"cw: Access denied\nan app's shell takes Developer Plus outside production, Maintainer or Admin on production; " +
				"your key must also be in My Settings → SSH Keys\n"},
		{"400", refusingShells(http.StatusBadRequest, "invalid_request", reason), 2, "cw: " + reason + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			h, ran := sshHarness(t, appShellPlatform(t, shellAppRecord(), &asked, tc.answer))
			if code := h.run("apps", "shell", "shop"); code != tc.code || *ran != nil {
				t.Fatalf("exit %d, ssh %v: %s", code, *ran, h.stderr)
			}
			if got := h.stderr.String(); got != tc.want {
				t.Errorf("stderr\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
