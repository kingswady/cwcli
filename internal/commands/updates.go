package commands

import (
	"flag"
	"fmt"
	"regexp"
	"strings"
)

// The changes that update an app or put it in maintenance: cw maintenance,
// cw apps update, cw apps update-modules. Like cw backup and cw restart they ask
// first, send one keyed request and, with --wait, follow what they started —
// the backup first when one goes ahead of the change.

// moduleName is what Odoo names a module; anything else is refused before it
// reaches the platform, which refuses it too.
var moduleName = regexp.MustCompile(`^[a-z0-9_]+$`)

// updateScopes are what an update's module update covers, in the question.
var updateScopes = map[string]string{
	"changed":    "the modules the pull changed",
	"configured": "its configured modules",
}

// backupFlags are --backup / --no-backup / --backup-format: a backup taken
// first, in the same queue. Neither flag leaves it to the platform, which
// backs a production app up and nothing else — the dashboard's own default.
type backupFlags struct {
	on, off bool
	format  string
}

func backupFlagSet(fs *flag.FlagSet) *backupFlags {
	b := &backupFlags{}
	fs.BoolVar(&b.on, "backup", false, "back the app up first (production does by default)")
	fs.BoolVar(&b.off, "no-backup", false, "do not back up first, also in production")
	fs.StringVar(&b.format, "backup-format", "zip", "the backup taken first: zip (database and files) or dump (database only)")
	return b
}

func (b *backupFlags) check() error {
	if b.on && b.off {
		return usagef("--backup and --no-backup cannot both be given")
	}
	if b.format != "zip" && b.format != "dump" {
		return usagef("--backup-format is zip or dump, not %q", b.format)
	}
	return nil
}

// add puts the backup choice in body: only when a flag made one.
func (b *backupFlags) add(body map[string]any) map[string]any {
	switch {
	case b.on:
		body["backup"] = true
	case b.off:
		body["backup"] = false
	}
	if b.format != "zip" {
		body["backup_format"] = b.format
	}
	return body
}

// first says whether a backup goes first, for the question: as asked, else
// the platform's default.
func (b *backupFlags) first(s subject) string {
	if b.on || (s.production && !b.off) {
		return ", after a backup"
	}
	return ""
}

// startedLine is the line once a change started, naming the backup that goes
// first when there is one.
func startedLine(what string) func(writeAnswer) string {
	return func(w writeAnswer) string {
		line := fmt.Sprintf("%s started: run %s.", what, w.RunID)
		if backupRun := w.BackupRunID.String(); backupRun != "" {
			line += fmt.Sprintf(" A backup goes first: run %s (backup %s).", backupRun, w.BackupID)
		}
		return line
	}
}

func (a *App) maintenance(args []string) error {
	fs, opts := a.writeFlagSet("maintenance")
	narrow := narrowFlags(fs, resourceNamed("apps"), "<app>")
	fs.Usage = a.writeUsage(fs, "cw maintenance <app> on|off [flags]",
		"Puts the app in maintenance — its users see the maintenance page — or takes it out again.\n"+
			"Refused while an update, a backup or a restore of it runs: it never cancels them.")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 || (positional[1] != "on" && positional[1] != "off") {
		fs.Usage()
		return usagef("name one app and say on or off: cw maintenance <id|name> on|off")
	}
	on := positional[1] == "on"
	c, out, err := a.writeClient(opts)
	if err != nil {
		return err
	}
	id, app, err := a.resolveApp(c, positional[0], opts, narrow, out)
	if err != nil {
		return err
	}
	s := subjectOf(app, "name")
	cmd := writeCommand{
		name: "cw maintenance", action: "Take " + s.name + " out of maintenance",
		path: "/apps/" + id + "/maintenance", body: map[string]any{"on": on}, subject: s, opts: opts,
		started: startedLine("Taking " + s.name + " out of maintenance"),
	}
	if on {
		cmd.action = "Put " + s.name + " in maintenance"
		cmd.note = "Its users see the maintenance page until you take it out."
		cmd.started = startedLine("Maintenance of " + s.name)
	}
	return a.runWrite(c, cmd, out)
}

// updateSelf is cw update: cw's own update, nothing else. An app named there
// (cw update shop) or an app update's flag says where that went: an app's
// update is cw apps update, so the one word never means both.
func (a *App) updateSelf(args []string) error {
	if namesSomething(args) || appUpdateFlag(args) != "" {
		return usagef("cw update updates cw itself; to update an app: cw apps update <app>")
	}
	return a.update(args)
}

// appUpdateFlag is the first flag in args that cw's own update does not take,
// else "": only --check, --version and --force are its own.
func appUpdateFlag(args []string) string {
	for _, word := range args {
		if !strings.HasPrefix(word, "-") || word == "--" {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(word, "-"), "=")
		switch name {
		case "check", "version", "force", "h", "help":
		default:
			return "--" + name
		}
	}
	return ""
}

// updateValueFlags are cw update's flags that take a value: the word after one
// is its value, not an app (TestUpdateValueFlagsAreAllKnown).
var updateValueFlags = map[string]bool{"version": true}

// namesSomething says whether args name anything besides flags and their values.
func namesSomething(args []string) bool {
	for i := 0; i < len(args); i++ {
		word := args[i]
		if word == "--" {
			return i+1 < len(args)
		}
		if !strings.HasPrefix(word, "-") {
			return true
		}
		if name := strings.TrimLeft(word, "-"); !strings.Contains(name, "=") && updateValueFlags[name] {
			i++ // its value
		}
	}
	return false
}

func (a *App) updateApp(args []string) error {
	fs, opts := a.writeFlagSet("apps update")
	quick := fs.Bool("quick", false, "pull the code and restart on the same image, without rebuilding")
	modules := fs.String("modules", "changed",
		"the module update that follows: changed (what the pull changed), configured (the app's own scope) or none (--quick only)")
	backup := backupFlagSet(fs)
	narrow := narrowFlags(fs, resourceNamed("apps"), "<app>")
	fs.Usage = a.writeUsage(fs, "cw apps update <app> [flags]",
		"Updates the app to its branches' latest code — what the dashboard's Update does: pulled,\n"+
			"rebuilt and recreated, then its modules updated. --quick pulls and restarts on the same image.\n"+
			"A production app is backed up first unless --no-backup. Refused while something runs on it.")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one app: cw apps update <id|name>")
	}
	if err := checkUpdateFlags(*quick, *modules, backup); err != nil {
		return err
	}
	c, out, err := a.writeClient(opts)
	if err != nil {
		return err
	}
	id, app, err := a.resolveApp(c, positional[0], opts, narrow, out)
	if err != nil {
		return err
	}
	s := subjectOf(app, "name")
	action, what := updateWords(s.name, *quick, *modules)
	return a.runWrite(c, writeCommand{
		name: "cw apps update", action: action + backup.first(s), path: "/apps/" + id + "/update",
		body:    backup.add(map[string]any{"quick": *quick, "modules": *modules}),
		subject: s, opts: opts, started: startedLine(what),
	}, out)
}

func checkUpdateFlags(quick bool, modules string, backup *backupFlags) error {
	if modules != "changed" && modules != "configured" && modules != "none" {
		return usagef("--modules is changed, configured or none, not %q", modules)
	}
	if modules == "none" && !quick {
		return usagef("a full update always updates modules as the app's automation does — --modules none needs --quick")
	}
	return backup.check()
}

// updateWords are the question and the "started" line for an update.
func updateWords(app string, quick bool, modules string) (action, what string) {
	if !quick {
		return fmt.Sprintf("Update %s: pull the latest code, rebuild and update %s", app, updateScopes[modules]),
			"Update of " + app
	}
	tail := "without a module update"
	if modules != "none" {
		tail = "updating " + updateScopes[modules]
	}
	return fmt.Sprintf("Quick-update %s: pull the latest code and restart, %s", app, tail), "Quick update of " + app
}

func (a *App) updateModules(args []string) error {
	fs, opts := a.writeFlagSet("apps update-modules")
	backup := backupFlagSet(fs)
	narrow := narrowFlags(fs, resourceNamed("apps"), "<app>")
	fs.Usage = a.writeUsage(fs, "cw apps update-modules <app> <module>... [flags]",
		"Updates the named modules (odoo -u) on the app's database, its code as it is — what the\n"+
			"dashboard's Update Modules does. A production app is backed up first unless --no-backup.\n"+
			"Refused while something runs on it.")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 {
		fs.Usage()
		return usagef("name one app and its modules: cw apps update-modules <id|name> sale stock")
	}
	names, err := moduleNames(positional[1:])
	if err != nil {
		return err
	}
	if err := backup.check(); err != nil {
		return err
	}
	c, out, err := a.writeClient(opts)
	if err != nil {
		return err
	}
	id, app, err := a.resolveApp(c, positional[0], opts, narrow, out)
	if err != nil {
		return err
	}
	s := subjectOf(app, "name")
	return a.runWrite(c, writeCommand{
		name:    "cw apps update-modules",
		action:  fmt.Sprintf("Update the modules %s of %s%s", strings.Join(names, ", "), s.name, backup.first(s)),
		path:    "/apps/" + id + "/update-modules",
		body:    backup.add(map[string]any{"modules": strings.Join(names, ",")}),
		subject: s, opts: opts, started: startedLine("Module update of " + s.name),
	}, out)
}

// moduleNames are the modules as typed — "sale stock" or "sale,stock" — once each.
func moduleNames(words []string) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	for _, word := range words {
		for _, name := range strings.Split(word, ",") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			if !moduleName.MatchString(name) {
				return nil, usagef("%q is not a module name: lowercase letters, digits and _ only", name)
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, usagef("name the modules to update: cw apps update-modules <app> sale stock")
	}
	return names, nil
}
