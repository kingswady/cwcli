package commands

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOj6hLbTx3Z2c5Y4lD0Wv6f2tY0VQ7xS3hJmZpZQx1aB me@laptop"

// sshPlatform answers servers.ssh_certificate for server 41, counting the calls.
func sshPlatform(t *testing.T, validBefore string, calls *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/servers/41/ssh-certificate") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		*calls++
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["public_key"] != publicKey {
			t.Errorf("body %v %v", body, err)
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("a change carries an Idempotency-Key")
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"certificate":  "ssh-ed25519-cert-v01@openssh.com AAAACERT root#cli-7",
			"serial":       7,
			"valid_before": validBefore,
			"host":         "192.0.2.9",
			"port":         2222,
			"user":         "root",
			"known_hosts":  []string{"[192.0.2.9]:2222 ssh-ed25519 AAAAHOST"},
		}})
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

func TestSSHSignsInToRootWithACertificateForYourKey(t *testing.T) {
	calls := 0
	server := sshPlatform(t, time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05"), &calls)
	h, ran := sshHarness(t, server)
	if code := h.run("ssh", "41", "--", "uptime", "-p"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	dir := filepath.Join(h.app.configDir, "ssh")
	want := strings.Join([]string{
		"-i", filepath.Join(h.env["HOME"], ".ssh", "id_ed25519"),
		"-o", `CertificateFile="` + filepath.Join(dir, "41-cert.pub") + `"`,
		"-o", "IdentitiesOnly=yes",
		"-o", `UserKnownHostsFile="` + filepath.Join(dir, "41-known_hosts") + `"`,
		"-o", "StrictHostKeyChecking=yes",
		"-p", "2222", "root@192.0.2.9", "uptime", "-p",
	}, " ")
	if got := strings.Join(*ran, " "); got != want {
		t.Errorf("ssh\n got %s\nwant %s", got, want)
	}
	for name, content := range map[string]string{
		"41-cert.pub":    "ssh-ed25519-cert-v01@openssh.com AAAACERT root#cli-7\n",
		"41-known_hosts": "[192.0.2.9]:2222 ssh-ed25519 AAAAHOST\n",
	} {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != content {
			t.Errorf("%s: %q %v", name, raw, err)
		}
		if info, _ := os.Stat(path); info == nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s is not 0600", name)
		}
	}
	if !strings.Contains(h.stderr.String(), "certificate 7 for root@192.0.2.9") {
		t.Errorf("stderr %q", h.stderr)
	}
	// Still valid for the hour: used again, nothing asked.
	if code := h.run("ssh", "41"); code != 0 || calls != 1 {
		t.Errorf("exit %d, %d requests: a valid certificate is used again", code, calls)
	}
}

func TestSSHAsksAgainOnceTheCertificateRunsOut(t *testing.T) {
	calls := 0
	server := sshPlatform(t, time.Now().UTC().Add(30*time.Second).Format("2006-01-02 15:04:05"), &calls)
	h, _ := sshHarness(t, server)
	for range 2 {
		if code := h.run("ssh", "41"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr)
		}
	}
	if calls != 2 {
		t.Errorf("%d requests: a certificate within a minute of its end is asked for again", calls)
	}
}

// A private key without its .pub (copied over, from a password manager, in CI)
// has its public line read by ssh-keygen -y.
func TestSSHDerivesThePublicKeyWhenThePubIsMissing(t *testing.T) {
	calls := 0
	server := sshPlatform(t, time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05"), &calls)
	h, ran := sshHarness(t, server)
	key := filepath.Join(h.env["HOME"], ".ssh", "mykey")
	if err := os.WriteFile(key, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	var derived []string
	h.app.keygenRunner = func(path string) (string, error) {
		derived = append(derived, path)
		return publicKey, nil
	}
	if code := h.run("ssh", "41", "--key", key); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if len(derived) != 1 || derived[0] != key || calls != 1 || (*ran)[1] != key {
		t.Errorf("derived %v, %d certificates, ssh %v", derived, calls, *ran)
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
	calls := 0
	server := sshPlatform(t, time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05"), &calls)
	h, ran := sshHarness(t, server)
	work := filepath.Join(h.env["HOME"], ".ssh", "work")
	if err := os.WriteFile(work+".pub", []byte(publicKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.env["CW_SERVER_SSH_41"] = work
	if code := h.run("ssh", "41"); code != 0 {
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
			calls := 0
			h, _ := sshHarness(t, sshPlatform(t, "", &calls))
			key := filepath.Join(h.env["HOME"], ".ssh", "mykey")
			h.app.keygenRunner = func(string) (string, error) { return "", errors.New("incorrect passphrase") }
			args := []string{"ssh", "41"}
			if named == "--key" {
				args = append(args, "--key", key)
			} else {
				h.env[named] = key
			}
			if code := h.run(args...); code != 1 || calls != 0 {
				t.Fatalf("exit %d, %d requests", code, calls)
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
	calls := 0
	h, _ := sshHarness(t, sshPlatform(t, "", &calls))
	delete(h.env, "CW_SERVER_SSH")
	if err := os.WriteFile(filepath.Join(h.env["HOME"], ".ssh", "id_ed25519"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "no SSH key named for 41: set CW_SERVER_SSH_41 (this server) or CW_SERVER_SSH (every server)"
	if code := h.run("ssh", "41"); code != 1 || calls != 0 || !strings.Contains(h.stderr.String(), want) {
		t.Errorf("exit %d, %d requests: %s", code, calls, h.stderr)
	}
	h = newHarness(t, nil)
	if code := h.run("ssh"); code != 2 {
		t.Errorf("no server: exit %d", code)
	}
}
