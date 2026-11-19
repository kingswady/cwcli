package commands

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
)

// cw ssh signs in to a server with a certificate the platform signs for one of
// the person's own registered keys: as odoo by default (the login appshell,
// forced into the dashboard's server shell), as root with -u root. It lasts the
// server's setting, is kept per server and account beside the server's pinned
// host key in cw's own folder, and is asked for again once it runs out. The
// platform's side, in deployment_workflow: docs/specs/ssh-certificates.md and
// docs/specs/ssh-odoo-default-root-owners.md.

// certificateMargin is how long a kept certificate must still be valid to be used again.
const certificateMargin = time.Minute

// keyVariable names the private key cw ssh signs in with when --key is not
// given: keyVariable_<server id> for one server, else keyVariable for every
// server. cw never guesses a key: someone with several is signed in with the one they named.
const keyVariable = "CW_SERVER_SSH"

// The accounts -u asks for: odoo, the default, opens the server's shell only;
// root takes an operator or an Admin of the namespace owning the server's cloud provider.
const (
	odooAccount = "odoo"
	rootAccount = "root"
)

// accessHints say what a certificate for each account takes. The platform
// answers an unregistered key, no terminal access and root asked by someone
// neither staff nor owner with the same 403, so cw names what could be missing.
var accessHints = map[string]string{
	odooAccount: "the odoo shell takes terminal access to the server; your key must also be in My Settings → SSH Keys",
	rootAccount: "root on a server is for operators and the Admins of the namespace that owns its cloud provider account; " +
		"your key must also be in My Settings → SSH Keys",
}

// sshCertificate is the platform's answer, and what cw keeps of it.
type sshCertificate struct {
	Certificate string      `json:"certificate"`
	Serial      json.Number `json:"serial"`
	ValidBefore string      `json:"valid_before"`
	Host        string      `json:"host"`
	Port        json.Number `json:"port"`
	// User is the login ssh signs in to (appshell, root); Account is what it
	// opens: odoo or root from cw ssh, app from cw apps shell.
	User       string   `json:"user"`
	Account    string   `json:"account"`
	KnownHosts []string `json:"known_hosts"`
	// Mode is the app shell's (shell or odoo); App and Server name the app and its server.
	Mode   string    `json:"mode,omitempty"`
	App    string    `json:"app,omitempty"`
	Server recordRef `json:"server,omitempty"`
	// PublicKey is the key it was signed for, so another --key asks again.
	PublicKey string `json:"public_key,omitempty"`
}

// sshServer is what cw ssh signs in to: the server's id, the server as typed, and the account.
type sshServer struct{ id, typed, account string }

// kept names the files cw keeps for this server and account.
func (s sshServer) kept() string { return s.id + "-" + s.account }

// keptPath is a file cw keeps for one certificate, kept naming it ("41-odoo",
// "app-12-shell"): suffix "-cert.pub", "-known_hosts" or ".json".
func keptPath(dir, kept, suffix string) string {
	return filepath.Join(dir, kept+suffix)
}

// certificateRequest is one certificate cw asks the platform for: cw ssh's for
// a server and account, cw apps shell's for an app and mode.
type certificateRequest struct {
	command string         // "cw ssh", as a refusal names it
	kept    string         // the name of the files cw keeps it in
	path    string         // where it is asked for
	body    map[string]any // what is asked besides the public key
	hint    string         // the line a 403 access_denied adds: what the certificate takes
	// signed refuses an answer that is not what was asked (an older platform
	// signs something else) and says what it opens: "for odoo on web-1".
	signed func(sshCertificate) (string, error)
}

func (a *App) ssh(args []string) error {
	fs := a.newFlags("ssh")
	account := fs.String("u", odooAccount, "the account: odoo (the server's shell) or root")
	keyPath := fs.String("key", "", "your private key (default: $"+keyVariable+"_<server id>, else $"+keyVariable+")")
	namespace := fs.String("namespace", "", "look the server up in this namespace only")
	fs.Usage = func() {
		fmt.Fprint(a.stderr, "Usage: cw ssh <server> [-u odoo|root] [--key path] [--namespace code] [-- command]\n\n"+
			"Opens the server's shell as odoo from this machine (the dashboard's server shell), with a\n"+
			"certificate the platform signs for your own key; it takes terminal access to the server.\n"+
			"-u root signs in to root instead, which takes an operator or an Admin of the namespace that owns\n"+
			"the server's cloud provider. A command after -- needs -u root: the odoo shell opens a shell only.\n\n"+
			"The certificate lasts as long as the server's setting says (15 minutes by default); cw keeps it\n"+
			"per server and account and asks again once it runs out. Register the key in My Settings → SSH\n"+
			"Keys first; the token needs servers.ssh_certificate ticked; the server's sign-in log names you.\n"+
			"A key without its .pub has its public key read by ssh-keygen -y (an encrypted key asks for its\n"+
			"passphrase).\n\n"+
			"The key is --key, else the path in "+keyVariable+"_<server id> (the id cw servers lists, as in\n"+
			keyVariable+"_srv_7k2m9xq4pzta, or in capitals), else the path in "+keyVariable+".\n\nFlags:\n")
		fs.PrintDefaults()
	}
	own, command := splitCommand(args)
	positional, err := parseArgs(fs, own)
	if err != nil {
		return err
	}
	if *account != odooAccount && *account != rootAccount {
		return usagef("-u is odoo or root, not %q", *account)
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one server: cw ssh <id|name>")
	}
	if len(command) > 0 && *account != rootAccount {
		return usagef("a command runs as root: add -u root (the odoo shell opens a shell only)")
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	id, err := a.resolveID(c, resourceNamed("servers"), positional[0], scope{namespace: *namespace}, output.Table{})
	if err != nil {
		return err
	}
	server := sshServer{id: id, typed: positional[0], account: *account}
	key, public, err := a.sshKey(*keyPath, id)
	if err != nil {
		return err
	}
	dir, err := a.sshDir()
	if err != nil {
		return err
	}
	certificate, err := a.serverCertificate(c, server, public, dir)
	if err != nil {
		return err
	}
	return a.runSSH(sshArgs(key, dir, server.kept(), certificate, command))
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

// sshKey is the private key and its public line, for server id: the key
// keyPath names, its public line from the .pub beside it or from ssh-keygen.
func (a *App) sshKey(flagPath, id string) (key, public string, err error) {
	path, from := a.keyPath(flagPath, id)
	if path == "" {
		return "", "", fmt.Errorf("no SSH key named for %s: set %s_%s (this server) or %s (every server) "+
			"to your private key's path, or pass --key", id, keyVariable, id, keyVariable)
	}
	key = a.expandHome(path)
	if public, err = a.publicKey(key); err != nil {
		return "", "", fmt.Errorf("%s: %w", from, err)
	}
	return key, public, nil
}

// keyPath is the key's path and what named it: --key, else the server's own
// variable (its id as cw lists it, or in capitals), else the one for every server.
func (a *App) keyPath(flagPath, id string) (path, from string) {
	if flagPath != "" {
		return flagPath, "--key"
	}
	for _, name := range []string{keyVariable + "_" + id, keyVariable + "_" + strings.ToUpper(id), keyVariable} {
		if value := a.getenv(name); value != "" {
			return value, name
		}
	}
	return "", ""
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

// serverCertificate is cw ssh's certificate for the server and account.
func (a *App) serverCertificate(c *platform.Client, server sshServer, public, dir string) (sshCertificate, error) {
	return a.certificate(c, certificateRequest{
		command: "cw ssh",
		kept:    server.kept(),
		path:    "/servers/" + server.id + "/ssh-certificate",
		body:    map[string]any{"user": server.account},
		hint:    accessHints[server.account],
		signed: func(fresh sshCertificate) (string, error) {
			// A platform from before accounts takes no user and signs root.
			if signed := cmp.Or(fresh.Account, rootAccount); signed != server.account {
				return "", fmt.Errorf("%s signed a certificate for %s, not %s — it needs a newer platform version; cw did not sign in",
					c.Base, signed, server.account)
			}
			return "for " + server.account + " on " + server.typed, nil
		},
	}, public, dir)
}

// certificate is the kept certificate while it holds for this key, else a new
// one from the platform, kept once it is what was asked.
func (a *App) certificate(c *platform.Client, req certificateRequest, public, dir string) (sshCertificate, error) {
	metaPath := keptPath(dir, req.kept, ".json")
	var kept sshCertificate
	if raw, err := os.ReadFile(metaPath); err == nil && json.Unmarshal(raw, &kept) == nil {
		if sameKey(kept.PublicKey, public) && a.clock().Add(certificateMargin).Before(validUntil(kept.ValidBefore)) {
			return kept, nil
		}
	}
	body := map[string]any{"public_key": public}
	maps.Copy(body, req.body)
	raw, _, err := a.postWrite(c, req.path, body)
	if err != nil {
		return sshCertificate{}, certificateError(c, req, err)
	}
	var fresh sshCertificate
	if err := platform.Decode(raw, &fresh); err != nil {
		return sshCertificate{}, err
	}
	what, err := req.signed(fresh)
	if err != nil {
		return sshCertificate{}, err
	}
	fresh.PublicKey = public
	fmt.Fprintf(a.stderr, "cw: certificate %s %s, valid until %s UTC\n", fresh.Serial, what, output.Clean(fresh.ValidBefore))
	files := map[string]string{
		keptPath(dir, req.kept, "-cert.pub"):    fresh.Certificate + "\n",
		keptPath(dir, req.kept, "-known_hosts"): strings.Join(fresh.KnownHosts, "\n") + "\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return sshCertificate{}, err
		}
	}
	meta, err := json.Marshal(fresh)
	if err != nil {
		return sshCertificate{}, err
	}
	return fresh, os.WriteFile(metaPath, meta, 0o600)
}

// certificateError is a refused certificate, said as cw says a refused change,
// except that a 403 access_denied gets the request's hint on a line of its own
// (a 400 says its reason itself: Harden Server sets the server up).
func certificateError(c *platform.Client, req certificateRequest, err error) error {
	var refused *platform.APIError
	if errors.As(err, &refused) && refused.Status == http.StatusForbidden && refused.Code == "access_denied" {
		return fmt.Errorf("%w\n%s", err, req.hint)
	}
	return writeError(c, req.command, err)
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

// sshArgs is ssh's command line: the key and its certificate (kept names their
// files), the platform's pin of the host key.
func sshArgs(key, dir, kept string, certificate sshCertificate, command []string) []string {
	checking := "StrictHostKeyChecking=yes"
	if len(certificate.KnownHosts) == 0 {
		// The platform has no pin yet: the first sign-in pins it in cw's own file.
		checking = "StrictHostKeyChecking=accept-new"
	}
	args := []string{
		"-i", key,
		"-o", "CertificateFile=" + optionPath(keptPath(dir, kept, "-cert.pub")),
		"-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=" + optionPath(keptPath(dir, kept, "-known_hosts")),
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
