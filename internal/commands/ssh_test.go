package commands

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOj6hLbTx3Z2c5Y4lD0Wv6f2tY0VQ7xS3hJmZpZQx1aB me@laptop"

// certificateAnswer is the platform's answer for account: odoo signs in to appshell, root to root.
func certificateAnswer(account, validBefore string) map[string]any {
	login := map[string]string{"odoo": "appshell", "root": "root"}[account]
	return map[string]any{
		"certificate":  "ssh-ed25519-cert-v01@openssh.com AAAACERT " + login + "#cli-7",
		"serial":       7,
		"valid_before": validBefore,
		"host":         "192.0.2.9",
		"port":         2222,
		"user":         login,
		"account":      account,
		"known_hosts":  []string{"[192.0.2.9]:2222 ssh-ed25519 AAAAHOST"},
	}
}

// sshPlatform answers servers.ssh_certificate for server 41 as the platform
// does (the account asked for, odoo when none), keeping the user each request
// sent ("" for none) in asked, "|" and its reason when it sent one. GET /servers
// knows server 41 as web-1.
func sshPlatform(t *testing.T, validBefore string, asked *[]string) *httptest.Server {
	t.Helper()
	return sshPlatformAnswering(t, asked, func(user string) map[string]any {
		if user == "" {
			user = "odoo"
		}
		return certificateAnswer(user, validBefore)
	})
}

func sshPlatformAnswering(t *testing.T, asked *[]string, answer func(user string) map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/servers") {
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"items": []map[string]any{{"id": 41, "name": "web-1"}}, "total": 1,
			}})
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/servers/41/ssh-certificate") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["public_key"] != publicKey {
			t.Errorf("body %v %v", body, err)
		}
		user := body["user"]
		if reason, sent := body["reason"]; sent {
			*asked = append(*asked, user+"|"+reason)
		} else {
			*asked = append(*asked, user)
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("a change carries an Idempotency-Key")
		}
		json.NewEncoder(w).Encode(map[string]any{"data": answer(user)})
	}))
	t.Cleanup(server.Close)
	return server
}

func sshHarness(t *testing.T, server *httptest.Server) (*harness, *[]string) {
	t.Helper()
	h := newHarness(t, server)
	// Where cw keeps its files on macOS has a space in it.
	h.app.configDir = filepath.Join(t.TempDir(), "Application Support", "cw")
	home := t.TempDir()
	h.env["HOME"] = home
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte(publicKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.env["CW_SERVER_SSH"] = filepath.Join(home, ".ssh", "id_ed25519")
	var ran []string
	h.app.sshRunner = func(args []string) error {
		ran = args
		return nil
	}
	return h, &ran
}

func inAnHour() string { return time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05") }

// The files cw keeps under the name kept ("41-odoo"), 0600, with what they hold.
func checkKeptFiles(t *testing.T, dir, kept, login string) {
	t.Helper()
	for name, content := range map[string]string{
		kept + "-cert.pub":    "ssh-ed25519-cert-v01@openssh.com AAAACERT " + login + "#cli-7\n",
		kept + "-known_hosts": "[192.0.2.9]:2222 ssh-ed25519 AAAAHOST\n",
		kept + ".json":        "",
	} {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil || (content != "" && string(raw) != content) {
			t.Errorf("%s: %q %v", name, raw, err)
		}
		if info, _ := os.Stat(path); info == nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s is not 0600", name)
		}
	}
}

func sshCommandLine(h *harness, account, login string, command ...string) string {
	dir := filepath.Join(h.app.configDir, "ssh")
	return strings.Join(append([]string{
		"-i", filepath.Join(h.env["HOME"], ".ssh", "id_ed25519"),
		"-o", `CertificateFile="` + filepath.Join(dir, "41-"+account+"-cert.pub") + `"`,
		"-o", "IdentitiesOnly=yes",
		"-o", `UserKnownHostsFile="` + filepath.Join(dir, "41-"+account+"-known_hosts") + `"`,
		"-o", "StrictHostKeyChecking=yes",
		"-p", "2222", login + "@192.0.2.9",
	}, command...), " ")
}

func TestSSHOpensTheOdooShellByDefault(t *testing.T) {
	var asked []string
	h, ran := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	if code := h.run("servers", "ssh", "41"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if strings.Join(asked, ",") != "odoo" {
		t.Errorf("asked for %q: no -u asks for odoo", asked)
	}
	if got, want := strings.Join(*ran, " "), sshCommandLine(h, "odoo", "appshell"); got != want {
		t.Errorf("ssh\n got %s\nwant %s", got, want)
	}
	checkKeptFiles(t, filepath.Join(h.app.configDir, "ssh"), "41-odoo", "appshell")
	if !strings.Contains(h.stderr.String(), "cw: certificate 7 for odoo on 41, valid until ") {
		t.Errorf("stderr %q", h.stderr)
	}
	// Still valid for the hour: used again, nothing asked.
	if code := h.run("servers", "ssh", "41"); code != 0 || len(asked) != 1 {
		t.Errorf("exit %d, %d requests: a valid certificate is used again", code, len(asked))
	}
}

func TestSSHSignsInToRootOnRequest(t *testing.T) {
	var asked []string
	h, ran := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	if code := h.run("servers", "ssh", "web-1", "-u", "root", "--", "uptime", "-p"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if strings.Join(asked, ",") != "root" {
		t.Errorf("asked for %q, want root", asked)
	}
	if got, want := strings.Join(*ran, " "), sshCommandLine(h, "root", "root", "uptime", "-p"); got != want {
		t.Errorf("ssh\n got %s\nwant %s", got, want)
	}
	checkKeptFiles(t, filepath.Join(h.app.configDir, "ssh"), "41-root", "root")
	// The account is the answer's, the server as typed.
	if !strings.Contains(h.stderr.String(), "cw: certificate 7 for root on web-1, valid until ") {
		t.Errorf("stderr %q", h.stderr)
	}
}

// A root certificate's reason goes to the platform, which quotes it in the
// notice; with no reason none is sent, which a platform before reasons refuses.
func TestSSHSendsTheReasonForRoot(t *testing.T) {
	var asked []string
	h, _ := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	if code := h.run("servers", "ssh", "41", "-u", "root", "--reason", "  disk full on /var  "); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if strings.Join(asked, ",") != "root|disk full on /var" {
		t.Errorf("asked for %q, want the trimmed reason", asked)
	}
}

// A reason reaches the owners only with a new certificate: a kept one is not used.
func TestSSHAsksForANewCertificateWithAReason(t *testing.T) {
	var asked []string
	h, _ := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	for _, args := range [][]string{
		{"servers", "ssh", "41", "-u", "root"},
		{"servers", "ssh", "41", "-u", "root", "--reason", "restart postgres"},
		{"servers", "ssh", "41", "-u", "root"},
	} {
		if code := h.run(args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, h.stderr)
		}
	}
	if got := strings.Join(asked, ","); got != "root,root|restart postgres" {
		t.Errorf("asked %q: the reason asks again, the run after it uses the kept one", got)
	}
}

// sshExit is how ssh ended, as exec reports it.
type sshExit int

func (e sshExit) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e sshExit) ExitCode() int { return int(e) }

// A revoked certificate is refused until it runs out and ssh says only
// "Permission denied": when ssh fails with a kept one, cw lets it go.
func TestSSHLetsAKeptCertificateGoWhenSSHFails(t *testing.T) {
	var asked []string
	h, _ := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	exits := []error{nil, sshExit(255), nil}
	h.app.sshRunner = func([]string) error {
		err := exits[0]
		exits = exits[1:]
		return err
	}
	for i, want := range []int{0, 1, 0} {
		if code := h.run("servers", "ssh", "41"); code != want {
			t.Fatalf("run %d: exit %d, want %d: %s", i+1, code, want, h.stderr)
		}
	}
	if len(asked) != 2 {
		t.Errorf("%d requests, want 2: the third run asks for a new certificate", len(asked))
	}
	if !strings.Contains(h.stderr.String(), "ssh failed with certificate 7, kept from an earlier sign-in") {
		t.Errorf("stderr %q does not say why the next run asks again", h.stderr)
	}
}

// A certificate cw just got is not let go (ssh failed for another reason),
// nor a kept one when only the command failed.
func TestSSHKeepsTheCertificateWhenSSHDidNotFail(t *testing.T) {
	for name, exits := range map[string][]error{
		"a new certificate":  {sshExit(255), nil},
		"the command failed": {nil, sshExit(1), nil},
	} {
		t.Run(name, func(t *testing.T) {
			var asked []string
			h, _ := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
			h.app.sshRunner = func([]string) error {
				err := exits[0]
				exits = exits[1:]
				return err
			}
			for range exits {
				h.run("servers", "ssh", "41", "-u", "root", "--", "false")
			}
			if len(asked) != 1 || strings.Contains(h.stderr.String(), "let it go") {
				t.Errorf("%d requests, stderr %q: the kept certificate stays", len(asked), h.stderr)
			}
		})
	}
}

func TestSSHTakesAReasonOnlyForRoot(t *testing.T) {
	var asked []string
	h, _ := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	if code := h.run("servers", "ssh", "41", "--reason", "look around"); code != 2 {
		t.Errorf("exit %d, want a usage error", code)
	}
	if !strings.Contains(h.stderr.String(), "--reason goes with -u root") || len(asked) != 0 {
		t.Errorf("stderr %q, %d requests", h.stderr, len(asked))
	}
}

// A kept certificate opens only the account it was signed for: a kept odoo one
// is not used for -u root, nor one cw 0.17 kept (41-cert.pub, 41.json).
func TestSSHKeepsACertificatePerAccount(t *testing.T) {
	var asked []string
	h, ran := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	dir := filepath.Join(h.app.configDir, "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(map[string]any{"valid_before": inAnHour(), "user": "root", "host": "192.0.2.9", "public_key": publicKey})
	if err := os.WriteFile(filepath.Join(dir, "41.json"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		args  []string
		asked string
		login string
	}{
		{[]string{"servers", "ssh", "41"}, "odoo", "appshell"},
		{[]string{"servers", "ssh", "41", "-u", "root"}, "odoo,root", "root"},
		{[]string{"servers", "ssh", "41"}, "odoo,root", "appshell"},
		{[]string{"servers", "ssh", "41", "-u", "root"}, "odoo,root", "root"},
		{[]string{"servers", "ssh", "41", "-u", "odoo"}, "odoo,root", "appshell"},
	}
	for _, step := range steps {
		if code := h.run(step.args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", step.args, code, h.stderr)
		}
		if got := strings.Join(asked, ","); got != step.asked {
			t.Errorf("%v: asked %q, want %q", step.args, got, step.asked)
		}
		if target := (*ran)[len(*ran)-1]; target != step.login+"@192.0.2.9" {
			t.Errorf("%v: ssh to %s", step.args, target)
		}
	}
}

func TestSSHTakesOdooOrRoot(t *testing.T) {
	var asked []string
	h, _ := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	if code := h.run("servers", "ssh", "41", "-u", "admin"); code != 2 || len(asked) != 0 {
		t.Errorf("exit %d, %d requests", code, len(asked))
	}
	if want := `cw: -u is odoo or root, not "admin"`; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("want %q in %q", want, h.stderr)
	}
}

// A command needs -u root: cw says so before it asks the platform anything,
// even the server's name.
func TestSSHRefusesACommandWithoutRoot(t *testing.T) {
	for _, args := range [][]string{
		{"servers", "ssh", "web-1", "--", "df", "-h"},
		{"servers", "ssh", "web-1", "-u", "odoo", "--", "df", "-h"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			untouched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("asked %s %s", r.Method, r.URL.Path)
			}))
			t.Cleanup(untouched.Close)
			h, ran := sshHarness(t, untouched)
			if code := h.run(args...); code != 2 || *ran != nil {
				t.Errorf("exit %d, ssh %v", code, *ran)
			}
			want := "cw: a command runs as root: add -u root (the odoo shell opens a shell only)"
			if !strings.Contains(h.stderr.String(), want) {
				t.Errorf("want %q in %q", want, h.stderr)
			}
		})
	}
}

// A platform from before accounts takes no user and signs root: -u root works,
// the odoo shell is refused rather than opening root.
func TestSSHAgainstAPlatformWithoutAccounts(t *testing.T) {
	var asked []string
	server := sshPlatformAnswering(t, &asked, func(string) map[string]any {
		answer := certificateAnswer("root", inAnHour())
		delete(answer, "account")
		return answer
	})
	h, ran := sshHarness(t, server)
	if code := h.run("servers", "ssh", "41"); code != 1 || *ran != nil {
		t.Fatalf("exit %d, ssh %v: %s", code, *ran, h.stderr)
	}
	if want := "signed a certificate for root, not odoo"; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("want %q in %q", want, h.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.app.configDir, "ssh", "41-odoo.json")); err == nil {
		t.Error("a refused certificate is not kept")
	}
	if code := h.run("servers", "ssh", "41", "-u", "root"); code != 0 || strings.Join(*ran, " ") != sshCommandLine(h, "root", "root") {
		t.Errorf("exit %d, ssh %v: %s", code, *ran, h.stderr)
	}
}

// sshRefusing refuses every certificate the way the platform does.
func sshRefusing(t *testing.T, status int, code, message string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
	}))
	t.Cleanup(server.Close)
	return server
}

// A 403 does not say which of the account's conditions failed: cw names them,
// for the account asked, on a line after the platform's words.
func TestSSHSaysWhatTheAccountTakesWhenRefused(t *testing.T) {
	cases := map[string]string{
		"odoo": "the odoo shell takes terminal access to the server; your key must also be in My Settings → SSH Keys",
		"root": "root on a server is for operators and the Admins of the namespace that owns its cloud provider account; " +
			"your key must also be in My Settings → SSH Keys",
	}
	for account, hint := range cases {
		t.Run(account, func(t *testing.T) {
			h, ran := sshHarness(t, sshRefusing(t, http.StatusForbidden, "access_denied", "Access denied"))
			if code := h.run("servers", "ssh", "41", "-u", account); code != 1 || *ran != nil {
				t.Fatalf("exit %d, ssh %v: %s", code, *ran, h.stderr)
			}
			if got, want := h.stderr.String(), "cw: Access denied\n"+hint+"\n"; got != want {
				t.Errorf("stderr\n got %q\nwant %q", got, want)
			}
		})
	}
}

// A 400 carries its reason: printed as it comes, no hint.
func TestSSHPrintsTheReasonOfABadRequest(t *testing.T) {
	reason := "web-1 does not take certificates yet: Harden Server sets it up."
	h, ran := sshHarness(t, sshRefusing(t, http.StatusBadRequest, "invalid_request", reason))
	if code := h.run("servers", "ssh", "41"); code != 2 || *ran != nil {
		t.Fatalf("exit %d, ssh %v: %s", code, *ran, h.stderr)
	}
	if got, want := h.stderr.String(), "cw: "+reason+"\n"; got != want {
		t.Errorf("stderr\n got %q\nwant %q", got, want)
	}
}

func TestSSHAsksAgainOnceTheCertificateRunsOut(t *testing.T) {
	var asked []string
	server := sshPlatform(t, time.Now().UTC().Add(30*time.Second).Format("2006-01-02 15:04:05"), &asked)
	h, _ := sshHarness(t, server)
	for range 2 {
		if code := h.run("servers", "ssh", "41"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr)
		}
	}
	if len(asked) != 2 {
		t.Errorf("%d requests: a certificate within a minute of its end is asked for again", len(asked))
	}
}

// A private key without its .pub (copied over, from a password manager, in CI)
// has its public line read by ssh-keygen -y.
func TestSSHDerivesThePublicKeyWhenThePubIsMissing(t *testing.T) {
	var asked []string
	h, ran := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	key := filepath.Join(h.env["HOME"], ".ssh", "mykey")
	if err := os.WriteFile(key, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	var derived []string
	h.app.keygenRunner = func(path string) (string, error) {
		derived = append(derived, path)
		return publicKey, nil
	}
	if code := h.run("servers", "ssh", "41", "--key", key); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if len(derived) != 1 || derived[0] != key || len(asked) != 1 || (*ran)[1] != key {
		t.Errorf("derived %v, %d certificates, ssh %v", derived, len(asked), *ran)
	}
	if !strings.Contains(h.stderr.String(), "no mykey.pub, so its public key came from ssh-keygen -y") {
		t.Errorf("stderr %q", h.stderr)
	}
}

// A server's own variable names its key before the one for every server; --key before both.
func TestSSHKeyIsNamedNeverGuessed(t *testing.T) {
	const id = "srv_7k2m9xq4pzta"
	cases := []struct {
		name, flag string
		env        map[string]string
		path, from string
	}{
		{"nothing named", "", nil, "", ""},
		{"every server", "", map[string]string{"CW_SERVER_SSH": "~/.ssh/all"}, "~/.ssh/all", "CW_SERVER_SSH"},
		{"this server", "", map[string]string{
			"CW_SERVER_SSH": "~/.ssh/all", "CW_SERVER_SSH_" + id: "~/.ssh/one",
		}, "~/.ssh/one", "CW_SERVER_SSH_" + id},
		{"this server in capitals", "", map[string]string{
			"CW_SERVER_SSH": "~/.ssh/all", "CW_SERVER_SSH_SRV_7K2M9XQ4PZTA": "~/.ssh/one",
		}, "~/.ssh/one", "CW_SERVER_SSH_SRV_7K2M9XQ4PZTA"},
		{"another server's", "", map[string]string{"CW_SERVER_SSH_srv_aaaaaaaaaaaa": "~/.ssh/other"}, "", ""},
		{"--key first", "~/.ssh/flag", map[string]string{
			"CW_SERVER_SSH": "~/.ssh/all", "CW_SERVER_SSH_" + id: "~/.ssh/one",
		}, "~/.ssh/flag", "--key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			for key, value := range tc.env {
				h.env[key] = value
			}
			if path, from := h.app.keyPath(tc.flag, id); path != tc.path || from != tc.from {
				t.Errorf("got %q from %q, want %q from %q", path, from, tc.path, tc.from)
			}
		})
	}
}

func TestSSHSignsInWithTheServersOwnKey(t *testing.T) {
	var asked []string
	h, ran := sshHarness(t, sshPlatform(t, inAnHour(), &asked))
	work := filepath.Join(h.env["HOME"], ".ssh", "work")
	if err := os.WriteFile(work+".pub", []byte(publicKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.env["CW_SERVER_SSH_41"] = work
	if code := h.run("servers", "ssh", "41"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if (*ran)[1] != work {
		t.Errorf("ssh %v: the server's own key comes before CW_SERVER_SSH", *ran)
	}
}

// Only when both ways fail does cw stop, and it names both, and what named the key.
func TestSSHNamesBothWaysWhenNoPublicKeyCanBeHad(t *testing.T) {
	for _, named := range []string{"--key", "CW_SERVER_SSH_41"} {
		t.Run(named, func(t *testing.T) {
			var asked []string
			h, _ := sshHarness(t, sshPlatform(t, "", &asked))
			key := filepath.Join(h.env["HOME"], ".ssh", "mykey")
			h.app.keygenRunner = func(string) (string, error) { return "", errors.New("incorrect passphrase") }
			args := []string{"servers", "ssh", "41"}
			if named == "--key" {
				args = append(args, "--key", key)
			} else {
				h.env[named] = key
			}
			if code := h.run(args...); code != 1 || len(asked) != 0 {
				t.Fatalf("exit %d, %d requests", code, len(asked))
			}
			for _, want := range []string{named + ": no public key", "no mykey.pub", "ssh-keygen -y could not read the key (incorrect passphrase)"} {
				if !strings.Contains(h.stderr.String(), want) {
					t.Errorf("want %q in %q", want, h.stderr)
				}
			}
		})
	}
}

// A key in ~/.ssh is not taken unless a variable or --key names it.
func TestSSHNeedsAKeyAndOneServer(t *testing.T) {
	var asked []string
	h, _ := sshHarness(t, sshPlatform(t, "", &asked))
	delete(h.env, "CW_SERVER_SSH")
	if err := os.WriteFile(filepath.Join(h.env["HOME"], ".ssh", "id_ed25519"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "no SSH key named for 41: set CW_SERVER_SSH_41 (this server) or CW_SERVER_SSH (every server)"
	if code := h.run("servers", "ssh", "41"); code != 1 || len(asked) != 0 || !strings.Contains(h.stderr.String(), want) {
		t.Errorf("exit %d, %d requests: %s", code, len(asked), h.stderr)
	}
	h = newHarness(t, nil)
	if code := h.run("ssh"); code != 2 {
		t.Errorf("no server: exit %d", code)
	}
}

// cw ssh is gone (cw servers ssh since 0.18.0): it answers like any unknown command.
func TestOldSSHCommandIsUnknown(t *testing.T) {
	untouched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("asked %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(untouched.Close)
	h, ran := sshHarness(t, untouched)
	if code := h.run("ssh", "web-1"); code == 0 || *ran != nil {
		t.Errorf("exit %d, ssh %v", code, *ran)
	}
	if !strings.Contains(h.stderr.String(), `unknown command "ssh"`) {
		t.Errorf("stderr: %q", h.stderr.String())
	}
}
