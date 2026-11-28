package commands

import (
	"fmt"

	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
)

// cw apps ssh opens an app's shell from this machine, what the dashboard's app
// shell opens: bash in its container, or Odoo's shell with --shell. Like cw
// servers ssh it signs in with a certificate the platform signs for one of the
// person's own keys, to the login appshell on the app's server; the
// certificate forces that shell, so it runs no command of its own. The
// platform's side, in deployment_workflow: docs/specs/ssh-odoo-default-root-owners.md.

// appShellAccount is the account an app shell's certificate opens.
const appShellAccount = "app"

// appShellHint says what an app's shell takes: the platform answers an
// unregistered key and too low a level with the same 403.
const appShellHint = "an app's shell takes Developer Plus outside production, Maintainer or Admin on production; " +
	"your key must also be in My Settings → SSH Keys"

func (a *App) appSSH(args []string) error {
	fs := a.newFlags("apps ssh")
	odooShell := fs.Bool("shell", false, "open Odoo's shell (its Python shell on the app's database) instead of bash")
	keyPath := fs.String("key", "", "your private key (default: $"+keyVariable+"_<server id>, else $"+keyVariable+")")
	namespace := fs.String("namespace", "", "look the app up in this namespace only")
	apps := resourceNamed("apps")
	narrow := narrowFlags(fs, apps, "<app>")
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "Usage: cw apps ssh <app> [--shell] [--key path] [--namespace code]\n\n"+
			"Opens the app's shell from this machine, what the dashboard's app shell opens: bash in its\n"+
			"container, or with --shell Odoo's shell on its database. It takes Developer Plus outside\n"+
			"production, Maintainer or Admin on production. The shell is all it opens: a command after --\n"+
			"is refused.\n\n"+
			"The certificate the platform signs for your own key lasts as long as the setting of the app's\n"+
			"server says (15 minutes by default); cw keeps it per app and mode and asks again once it runs\n"+
			"out. Register the key in My Settings → SSH Keys first; the token needs apps.get and\n"+
			"apps.ssh_certificate ticked.\n\n"+
			"The key is --key, else the path in "+keyVariable+"_<server id> (the app's server, its id as cw\n"+
			"servers lists it), else the path in "+keyVariable+". Where several apps share the name,\n"+
			"%s narrow which one it means.\n\nFlags:\n", joinFlags(narrowing(apps), "and"))
		fs.PrintDefaults()
	}
	own, command := splitCommand(args)
	positional, err := parseArgs(fs, own)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one app: cw apps ssh <id|name>")
	}
	if len(command) > 0 {
		return usagef("an app shell takes no command")
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	id, app, err := a.resolveApp(c, positional[0], &writeFlags{namespace: *namespace}, narrow, output.Table{})
	if err != nil {
		return err
	}
	serverID, err := serverOf(c, app)
	if err != nil {
		return err
	}
	key, public, err := a.sshKey(*keyPath, serverID)
	if err != nil {
		return err
	}
	dir, err := a.sshDir()
	if err != nil {
		return err
	}
	mode, words := "shell", "the shell"
	if *odooShell {
		mode, words = "odoo", "the Odoo shell"
	}
	kept := "app-" + id + "-" + mode
	certificate, err := a.certificate(c, certificateRequest{
		command: "cw apps ssh",
		kept:    kept,
		path:    "/apps/" + id + "/ssh-certificate",
		body:    map[string]any{"mode": mode},
		hint:    appShellHint,
		signed: func(fresh sshCertificate) (string, error) {
			if fresh.Account != appShellAccount || fresh.Mode != mode {
				return "", fmt.Errorf("%s did not sign a certificate for %s of an app — it needs a newer platform version; cw did not sign in",
					c.Base, words)
			}
			return "for " + words + " of " + output.Text(app["name"]), nil
		},
	}, public, dir)
	if err != nil {
		return err
	}
	// -t: the forced shell needs a terminal; no command, the certificate names it.
	return a.signIn(append([]string{"-t"}, sshArgs(key, dir, kept, certificate, nil)...), dir, kept, certificate)
}

// serverOf is the id of the server app runs on: its key is CW_SERVER_SSH_<server id>.
// A platform from before app shells does not send it.
func serverOf(c *platform.Client, app record) (string, error) {
	value, sent := app["server_id"]
	if !sent {
		return "", fmt.Errorf("%s does not offer cw apps ssh yet — it needs a newer platform version", c.Base)
	}
	if id := output.Text(value); id != "-" {
		return id, nil
	}
	return "", fmt.Errorf("%s runs on no server: it has no shell to open", output.Text(app["name"]))
}
