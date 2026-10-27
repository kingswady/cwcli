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

// In the default search only the first key there is is derived: an encrypted
// key asks for its passphrase once, not once per candidate.
func TestSSHDerivesOnlyTheFirstDefaultKey(t *testing.T) {
	calls := 0
	server := sshPlatform(t, time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05"), &calls)
	h, ran := sshHarness(t, server)
	ssh := filepath.Join(h.env["HOME"], ".ssh")
	os.Remove(filepath.Join(ssh, "id_ed25519.pub"))
	for _, name := range []string{"id_ecdsa", "id_rsa"} {
		if err := os.WriteFile(filepath.Join(ssh, name), []byte("PRIVATE"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var derived []string
	h.app.keygenRunner = func(path string) (string, error) {
		derived = append(derived, path)
		return publicKey, nil
	}
	if code := h.run("ssh", "41"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	first := filepath.Join(ssh, "id_ecdsa")
	if len(derived) != 1 || derived[0] != first || (*ran)[1] != first {
		t.Errorf("derived %v, ssh %v", derived, *ran)
	}
}

// Only when both ways fail does cw stop, and it names both.
func TestSSHNamesBothWaysWhenNoPublicKeyCanBeHad(t *testing.T) {
	h, _ := sshHarness(t, nil)
	key := filepath.Join(h.env["HOME"], ".ssh", "mykey")
	h.app.keygenRunner = func(string) (string, error) { return "", errors.New("incorrect passphrase") }
	if code := h.run("ssh", "41", "--key", key); code != 1 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"no mykey.pub", "ssh-keygen -y could not read the key (incorrect passphrase)"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("want %q in %q", want, h.stderr)
		}
	}
}

func TestSSHNeedsAKeyAndOneServer(t *testing.T) {
	h := newHarness(t, nil)
	h.env["HOME"] = t.TempDir()
	if code := h.run("ssh", "41"); code != 1 || !strings.Contains(h.stderr.String(), "no SSH key in ~/.ssh") {
		t.Errorf("exit %d: %s", code, h.stderr)
	}
	h = newHarness(t, nil)
	if code := h.run("ssh"); code != 2 {
		t.Errorf("no server: exit %d", code)
	}
}
