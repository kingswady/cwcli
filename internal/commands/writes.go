package commands

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
)

// The commands that change something: cw backup, cw restart, cw runs retry,
// cw maintenance, cw apps update, cw apps update-modules (updates.go).
// Each asks first (or takes --yes), sends one request with one Idempotency-Key
// — reused on cw's own retries, so the platform acts once whatever happens on
// the way — and with --wait follows the run it started.

const (
	// exitStillRunning: --wait gave up, the run goes on.
	exitStillRunning = 4
	pollInterval     = 3 * time.Second
	defaultTimeout   = 30 * time.Minute
	// writeAttempts is how often cw sends one write before it gives up: a rate
	// limit or a lost connection is worth waiting out, a refusal is not.
	writeAttempts = 4
)

var (
	errCancelled = errors.New("cancelled — nothing was changed")
	// errRunFailed and errStillRunning end --wait; the message is printed before.
	errRunFailed      = errors.New("the run failed")
	errStillRunning   = errors.New("the run is still going")
	errStoppedWaiting = errors.New("stopped waiting")
)

// writeFlags are the flags every write command takes.
type writeFlags struct {
	yes       bool
	wait      bool
	timeout   time.Duration
	asJSON    bool
	namespace string
	color     string
}

func (a *App) writeFlagSet(name string) (*flag.FlagSet, *writeFlags) {
	fs := a.newFlags(name)
	opts := &writeFlags{}
	fs.BoolVar(&opts.yes, "yes", false, "do it without asking (needed from a script)")
	fs.BoolVar(&opts.wait, "wait", false, "follow the run until it ends: exit 0 done, 1 failed, 4 still running")
	fs.DurationVar(&opts.timeout, "timeout", defaultTimeout, "how long --wait follows the run")
	fs.BoolVar(&opts.asJSON, "json", false, "print the platform's answer as JSON")
	fs.StringVar(&opts.namespace, "namespace", "", "look the app up in this namespace only")
	fs.StringVar(&opts.color, "color", "auto", "auto (in a terminal, unless NO_COLOR is set), always or never")
	return fs, opts
}

// subject is what a change is about, for the question cw asks first.
type subject struct {
	name       string // the app ("shop")
	project    string
	env        string // environment_type
	production bool
}

func (s subject) where() string {
	parts := []string{}
	for _, part := range []string{s.project, s.env} {
		if part != "" && part != "-" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " / ") + ")"
}

func subjectOf(r record, nameKey string) subject {
	env := output.Text(r["environment_type"])
	return subject{
		name:       output.Text(r[nameKey]),
		project:    output.Text(r["project"]),
		env:        env,
		production: env == "production",
	}
}

// confirm asks before a change. A production app is confirmed by typing its
// name; anything else by y. Without a terminal there is nobody to ask: a
// script says --yes. note, when given, is a sentence more on what it does.
func (a *App) confirm(opts *writeFlags, action, note string, s subject) error {
	if opts.yes {
		return nil
	}
	if !a.stdinIsTerminal() {
		return usagef("%s%s changes it: pass --yes to do it without asking", action, s.where())
	}
	if note != "" {
		note = " " + note
	}
	reader := bufio.NewReader(a.stdin)
	if s.production {
		fmt.Fprintf(a.stderr, "%s%s — a production app.%s Type %s to go on: ", action, s.where(), note, s.name)
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(line) != s.name {
			return errCancelled
		}
		return nil
	}
	fmt.Fprintf(a.stderr, "%s%s?%s [y/N] ", action, s.where(), note)
	line, _ := reader.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errCancelled
}

// newKey is one Idempotency-Key: random, one per command.
func newKey() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return "cw-" + hex.EncodeToString(raw)
}

// postWrite sends one change, with one key on every attempt: a rate limit is
// waited out, a lost connection asked again — the platform answers a repeat of
// the key with its first answer and acts once.
func (a *App) postWrite(c *platform.Client, path string, body map[string]any) (json.RawMessage, int, error) {
	key := newKey()
	for attempt := 1; ; attempt++ {
		raw, status, err := c.Post(context.Background(), path, body, key)
		if err == nil || attempt == writeAttempts {
			return raw, status, err
		}
		var refused *platform.APIError
		var network *platform.NetworkError
		switch {
		case errors.As(err, &refused) && refused.Status == http.StatusTooManyRequests:
			pause := max(refused.RetryAfter, time.Second)
			fmt.Fprintf(a.stderr, "cw: %s — trying again in %s\n", output.Clean(refused.Message), pause)
			a.wait(context.Background(), pause)
		case errors.As(err, &network):
			fmt.Fprintf(a.stderr, "cw: %s — trying again\n", output.Clean(err.Error()))
			a.wait(context.Background(), 2*time.Second)
		default:
			return raw, status, err
		}
	}
}

// retryReasons say why the platform refused a retry, in a sentence.
var retryReasons = map[string]string{
	"newer_run":      "a newer run of the same record exists — retry that one",
	"queue_order":    "an earlier run in its queue has to go first",
	"queue_gated":    "its queue waits on another queue",
	"workflow_state": "it has not failed",
	"retry_refused":  "the platform refuses to run it again — see cw runs show",
	"nothing_failed": "nothing in it failed",
}

// writeError is a refusal said for the command that met it.
func writeError(c *platform.Client, command string, err error) error {
	if offered := notOffered(c, command, err); offered != err {
		return offered
	}
	var refused *platform.APIError
	if !errors.As(err, &refused) {
		return err
	}
	if refused.Status == http.StatusMethodNotAllowed {
		// A platform from before writes has no POST route at all.
		return fmt.Errorf("%s does not offer %s yet — it needs a newer platform version", c.Base, command)
	}
	hint := map[string]string{
		"writes_disabled":        "changes through the API are not open to this account on this platform yet",
		"in_progress":            "wait until it ends, then run this again",
		"retry_coming":           "an automatic retry is due in a few minutes — add --now to retry now anyway",
		"locked":                 "that is the platform's safety lock, not something wrong with the app",
		"request_in_progress":    "the same request is still being handled — check cw runs before asking again",
		"idempotency_key_reused": "run the command again",
		"access_denied":          "your access level does not allow this change in this environment",
		"upgrade_standby":        "a version upgrade holds the app — finish or cancel it in the dashboard first",
	}[refused.Code]
	if refused.Code == "not_retryable" {
		hint = retryReasons[refused.Reason]
	}
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w — %s", err, hint)
}

// writeAnswer is what the platform answers a change. BackupRunID is the backup
// taken first, when one was: RunID's run waits for it in the same queue.
type writeAnswer struct {
	State       string      `json:"state"`
	RunID       json.Number `json:"run_id"`
	BackupID    json.Number `json:"backup_id"`
	BackupRunID json.Number `json:"backup_run_id"`
}

func (a *App) resolveApp(c *platform.Client, ref string, opts *writeFlags, narrow map[string]*string, out output.Table) (string, record, error) {
	apps := resourceNamed("apps")
	id, err := a.resolveID(c, apps, ref, scope{
		namespace: opts.namespace, filters: given(narrow), offered: narrowing(apps),
	}, out)
	if err != nil {
		return "", nil, err
	}
	raw, err := c.Get("/apps/"+id, nil)
	if err != nil {
		return "", nil, err
	}
	var app record
	if err := platform.Decode(raw, &app); err != nil {
		return "", nil, err
	}
	return id, app, nil
}

// writeCommand is the shape the three commands share once they know what to
// change: ask, send, report, and maybe follow.
type writeCommand struct {
	name     string // "cw backup", for an older platform's refusal
	action   string // "Back up shop"
	note     string // a sentence more in the question, or ""
	path     string
	body     map[string]any
	subject  subject
	opts     *writeFlags
	started  func(writeAnswer) string // the line once it started
	extraRun func(writeAnswer) string // already_running and the like; "" when none
	// appUntouched: it changes nothing in the app (a backup), so a production
	// app is asked y/N too — typing its name is for changes that can break it.
	appUntouched bool
	// finished, when set, reports what the run made once --wait saw it done.
	finished func(writeAnswer)
}

func (a *App) runWrite(c *platform.Client, cmd writeCommand, out output.Table) error {
	if cmd.opts.asJSON && cmd.opts.wait {
		return usagef("--json prints the platform's answer; follow the run with cw runs show <id> --json")
	}
	asked := cmd.subject
	if cmd.appUntouched {
		asked.production = false
	}
	if err := a.confirm(cmd.opts, cmd.action, cmd.note, asked); err != nil {
		return err
	}
	raw, _, err := a.postWrite(c, cmd.path, cmd.body)
	if err != nil {
		return writeError(c, cmd.name, err)
	}
	if cmd.opts.asJSON {
		return output.WriteJSON(a.stdout, raw)
	}
	var answer writeAnswer
	if err := platform.Decode(raw, &answer); err != nil {
		return err
	}
	line := cmd.started(answer)
	if cmd.extraRun != nil {
		if other := cmd.extraRun(answer); other != "" {
			line = other
		}
	}
	fmt.Fprintln(a.stdout, line)
	runID := answer.RunID.String()
	if runID == "" {
		return nil
	}
	if !cmd.opts.wait {
		fmt.Fprintf(a.stderr, "Follow it: cw runs show %s  (or add --wait)\n", runID)
		return nil
	}
	if err := a.waitAfterBackup(c, answer, cmd.opts.timeout, out); err != nil {
		return err
	}
	if cmd.finished != nil {
		cmd.finished(answer)
	}
	return nil
}

// waitAfterBackup follows the change's run, and first the backup's when one
// goes ahead of it in its queue: a failed backup leaves the change unstarted.
// Both share one --timeout.
func (a *App) waitAfterBackup(c *platform.Client, answer writeAnswer, timeout time.Duration, out output.Table) error {
	runID, backupRun := answer.RunID.String(), answer.BackupRunID.String()
	if backupRun == "" {
		return a.waitRun(c, runID, timeout, out)
	}
	start := a.clock()
	fmt.Fprintf(a.stdout, "Backup first: run %s\n", backupRun)
	if err := a.waitRun(c, backupRun, timeout, out); err != nil {
		if errors.Is(err, errRunFailed) {
			fmt.Fprintf(a.stdout, "The backup failed, so run %s did not start: the app was not changed.\n", runID)
		}
		return err
	}
	fmt.Fprintf(a.stdout, "Then run %s:\n", runID)
	return a.waitRun(c, runID, timeout-a.clock().Sub(start), out)
}

func (a *App) backup(args []string) error {
	fs, opts := a.writeFlagSet("backup")
	format := fs.String("format", "zip", "zip (database and files) or dump (database only)")
	narrow := narrowFlags(fs, resourceNamed("apps"), "<app>")
	fs.Usage = a.writeUsage(fs, "cw backup <app> [flags]",
		"Backs up the app now — what the dashboard's Backup does. A backup of the app that already\n"+
			"runs is reported, not doubled.")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one app: cw backup <id|name>")
	}
	if *format != "zip" && *format != "dump" {
		return usagef("--format is zip or dump, not %q", *format)
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
		name: "cw backup", action: "Back up " + s.name, path: "/apps/" + id + "/backup",
		body: map[string]any{"format": *format}, subject: s, opts: opts, appUntouched: true,
		finished: func(w writeAnswer) { a.printBackupResult(c, w.BackupID.String()) },
		started: func(w writeAnswer) string {
			return fmt.Sprintf("Backup of %s started: run %s (backup %s).", s.name, w.RunID, w.BackupID)
		},
		extraRun: func(w writeAnswer) string {
			if w.State != "already_running" {
				return ""
			}
			if w.RunID.String() == "" {
				return fmt.Sprintf("A backup of %s is already running (backup %s): nothing new was started.", s.name, w.BackupID)
			}
			return fmt.Sprintf("A backup of %s is already running (backup %s, run %s): nothing new was started.",
				s.name, w.BackupID, w.RunID)
		},
	}, out)
}

// printBackupResult says what a finished backup made and where it is kept:
// "Backup 9482: 432.5 MB zip, checked — on the server and cw-sftp (uploading)."
// The backup is made whatever happens here; a failed read only costs the summary.
func (a *App) printBackupResult(c *platform.Client, id string) {
	if id == "" {
		return
	}
	var backup record
	raw, err := c.Get("/backups/"+id, nil)
	if err == nil {
		err = platform.Decode(raw, &backup)
	}
	if err != nil {
		fmt.Fprintf(a.stderr, "cw: %s — see it with cw backups show %s\n", output.Clean(err.Error()), id)
		return
	}
	made := output.Megabytes(backup["size_mb"]) + " " + output.Text(backup["format"])
	if verified, _ := backup["verified"].(bool); verified {
		made += ", checked"
	}
	fmt.Fprintf(a.stdout, "Backup %s: %s — %s.\n", id, made, keptOn(backup))
	fmt.Fprintf(a.stdout, "See it: cw backups show %s\n", id)
}

// keptOn is where a backup is: "on the server and cw-sftp (uploading)".
func keptOn(backup record) string {
	var places []string
	if on, _ := backup["on_server"].(bool); on {
		places = append(places, "the server")
	}
	for _, held := range records(backup["offsite"]) {
		name := output.Text(held["storage"])
		switch on, _ := held["on_storage"].(bool); {
		case on:
			places = append(places, name)
		case output.Text(held["state"]) == "error":
			places = append(places, name+" (upload failed)")
		default:
			places = append(places, name+" (uploading)")
		}
	}
	if len(places) == 0 {
		return "nowhere any more"
	}
	if len(places) == 1 {
		return "on " + places[0]
	}
	return "on " + strings.Join(places[:len(places)-1], ", ") + " and " + places[len(places)-1]
}

func (a *App) restart(args []string) error {
	fs, opts := a.writeFlagSet("restart")
	narrow := narrowFlags(fs, resourceNamed("apps"), "<app>")
	fs.Usage = a.writeUsage(fs, "cw restart <app> [flags]",
		"Restarts the app — its users are cut off for a moment. Refused while an update, a backup\n"+
			"or a restore of it runs: a restart never cancels them.")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one app: cw restart <id|name>")
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
		name: "cw restart", action: "Restart " + s.name, path: "/apps/" + id + "/restart",
		body: map[string]any{}, subject: s, opts: opts,
		started: func(w writeAnswer) string { return fmt.Sprintf("Restart of %s started: run %s.", s.name, w.RunID) },
	}, out)
}

// runsRetry is cw runs retry <id>: the failed run again, from its failed step.
func (a *App) runsRetry(args []string) error {
	fs, opts := a.writeFlagSet("runs retry")
	now := fs.Bool("now", false, "retry even though an automatic retry is due")
	fs.Usage = a.writeUsage(fs, "cw runs retry <id> [flags]",
		"Runs a failed run again from the step that failed — what the dashboard's Retry does.\n"+
			"Refused while an automatic retry is due (--now retries anyway), or when a newer run of\n"+
			"the same record exists.")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one run: cw runs retry <id>")
	}
	runID := positional[0]
	if !isDigits(runID) {
		return usagef("runs are named by id, not %q (cw runs lists them)", runID)
	}
	c, out, err := a.writeClient(opts)
	if err != nil {
		return err
	}
	raw, err := c.Get("/runs/"+runID, nil)
	if err != nil {
		return err
	}
	var run record
	if err := platform.Decode(raw, &run); err != nil {
		return err
	}
	s := subjectOf(run, "record")
	action := fmt.Sprintf("Retry run %s (%s of %s)", runID, output.Text(run["workflow"]), s.name)
	body := map[string]any{}
	if *now {
		body["now"] = true
	}
	return a.runWrite(c, writeCommand{
		name: "cw runs retry", action: action, path: "/runs/" + runID + "/retry",
		body: body, subject: s, opts: opts,
		started: func(w writeAnswer) string { return fmt.Sprintf("Run %s started again.", runID) },
	}, out)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (a *App) writeClient(opts *writeFlags) (*platform.Client, output.Table, error) {
	color, err := a.useColor(opts.color, opts.asJSON)
	if err != nil {
		return nil, output.Table{}, err
	}
	c, err := a.client()
	return c, output.Table{Color: color}, err
}

func (a *App) writeUsage(fs *flag.FlagSet, usage, about string) func() {
	return func() {
		fmt.Fprintf(a.stderr, "Usage: %s\n\n%s\n\nIt asks first: a change to a production app by typing its name, anything else y/N.\n"+
			"From a script, --yes. The token needs the function ticked in My Settings → API Tokens.\n\nFlags:\n",
			usage, about)
		fs.PrintDefaults()
	}
}

// waitRun follows run id until it ends, printing each step as its state
// changes, then what the watcher on the server reports for a backup or restore
// (the run stays running until it does). A run whose failed step will be
// retried has not ended.
func (a *App) waitRun(c *platform.Client, id string, timeout time.Duration, out output.Table) error {
	ctx, stop := a.interrupt()
	defer stop()
	deadline := a.clock().Add(timeout)
	seen := map[int]string{}
	watched := &watchLine{}
	for {
		raw, err := c.GetContext(ctx, "/runs/"+id, nil)
		if ctx.Err() != nil {
			return a.stoppedWaiting(id)
		}
		if pause, done, err := a.pollFailure(err); done {
			return err
		} else if pause > 0 {
			watched.open = false // the failure was said after it
			if !a.wait(ctx, pause) {
				return a.stoppedWaiting(id)
			}
			continue
		}
		var run record
		if err := platform.Decode(raw, &run); err != nil {
			return err
		}
		if a.printStepChanges(run, seen, out) {
			watched.open = false
		}
		a.printWatcherChange(run, watched, out)
		if result, ended := runEnded(run); ended {
			return a.reportEnd(id, run, result, out)
		}
		if !a.clock().Before(deadline) {
			fmt.Fprintf(a.stderr, "\nRun %s is still going after %s — follow it: cw runs show %s\n", id, timeout, id)
			return errStillRunning
		}
		if !a.wait(ctx, pollInterval) {
			return a.stoppedWaiting(id)
		}
	}
}

// pollFailure is what a failed poll means: a pause before asking again, or the end.
func (a *App) pollFailure(err error) (pause time.Duration, done bool, out error) {
	if err == nil {
		return 0, false, nil
	}
	var refused *platform.APIError
	switch {
	case errors.As(err, &refused) && refused.Status == http.StatusTooManyRequests:
		return max(refused.RetryAfter, pollInterval), false, nil
	case errors.As(err, &refused):
		return 0, true, err
	}
	fmt.Fprintln(a.stderr, "cw:", output.Clean(err.Error()), "— still following")
	return pollInterval, false, nil
}

func (a *App) stoppedWaiting(id string) error {
	fmt.Fprintf(a.stderr, "\nStopped waiting — run %s goes on: cw runs show %s\n", id, id)
	return errStoppedWaiting
}

// printStepChanges prints each step whose state changed; it says whether it printed.
func (a *App) printStepChanges(run record, seen map[int]string, out output.Table) bool {
	steps := records(run["steps"])
	printed := false
	for i, step := range steps {
		state := output.Text(step["state"])
		if seen[i] == state {
			continue
		}
		seen[i] = state
		printed = true
		shown := stepState(run, i, len(steps), state)
		fmt.Fprintf(a.stdout, "  %d. %s  %s\n", i+1, output.Text(step["name"]), out.Cell("state", shown))
	}
	return printed
}

// stepState is a step's state as cw says it. The last step of a run whose
// watcher still waits only launched the work on the server: it has started,
// and the watcher says how the work went.
func stepState(run record, i, count int, state string) string {
	if state == "successful" && i == count-1 && watcherWaits(run) {
		return "started"
	}
	return state
}

func watcherWaits(run record) bool {
	watcher, _ := run["watcher"].(record)
	return watcher != nil && output.Text(watcher["state"]) == "waiting"
}

// watchLine is the watcher's line as last printed, and since when it says so.
type watchLine struct {
	text  string
	since time.Time
	// open: nothing was printed after it, so a terminal redraws it in place.
	open bool
}

// printWatcherChange prints what the watcher reports when it changes, once the
// steps have launched the work it watches. In a terminal a line that still
// waits is redrawn with how long it has said so, so a long dump never looks stuck.
func (a *App) printWatcherChange(run record, last *watchLine, out output.Table) {
	line := watcherLine(run, out)
	if line == "" || !stepsFinished(run) {
		return
	}
	live := watcherWaits(run) && a.stdoutIsTerminal()
	if line != last.text {
		*last = watchLine{text: line, since: a.clock(), open: live}
		fmt.Fprintln(a.stdout, "  "+line)
		return
	}
	if live && last.open {
		took := output.Elapsed(a.clock().Sub(last.since))
		fmt.Fprintf(a.stdout, "\033[1A\r\033[K  %s %s\n", line, out.Label("· "+took))
	}
}

func stepsFinished(run record) bool {
	steps := records(run["steps"])
	for _, step := range steps {
		if output.Text(step["state"]) != "successful" {
			return false
		}
	}
	return len(steps) > 0
}

// watcherLine is the watcher in a line: "Watcher: Dumping the database", or ""
// for a run without one.
func watcherLine(run record, out output.Table) string {
	watcher, _ := run["watcher"].(record)
	if watcher == nil {
		return ""
	}
	state := output.Text(watcher["state"])
	if progress := output.Text(watcher["progress"]); progress != "-" && state == "waiting" {
		return out.Label("Watcher:") + " " + progress
	}
	words := map[string]string{
		"waiting": "waiting for the work on the server to finish", "completed": "finished",
		"failed": "failed", "timeout": "gave up waiting", "stopped": "stopped before it reported",
	}
	said, ok := words[state]
	if !ok {
		said = state
	}
	if reason := output.Text(watcher["reason"]); reason != "-" {
		said += ": " + output.FirstLine(reason)
	}
	if out.Color {
		// In the colour of the run state it amounts to.
		tone := map[string]string{"waiting": "running", "completed": "done", "stopped": "failed"}[state]
		if tone == "" {
			tone = state
		}
		said = output.StyleAs("state", tone, said)
	}
	return out.Label("Watcher:") + " " + said
}

// runEnded says whether run has ended, and how: "done" or "failed".
func runEnded(run record) (string, bool) {
	state := output.Text(run["state"])
	if retrying, _ := run["retrying"].(bool); retrying {
		return "", false
	}
	switch state {
	case "done", "successful":
		return "done", true
	case "error", "failed", "canceled", "cancel", "timeout", "interrupted":
		return "failed", true
	}
	return "", false
}

func (a *App) reportEnd(id string, run record, result string, out output.Table) error {
	if result == "done" {
		fmt.Fprintf(a.stdout, "Run %s done.\n", id)
		return nil
	}
	fmt.Fprintf(a.stdout, "Run %s %s.\n", id, out.Cell("state", output.Text(run["state"])))
	for i, step := range records(run["steps"]) {
		if reason := output.Text(step["reason"]); reason != "-" {
			fmt.Fprintf(a.stdout, "  %d. %s: %s\n", i+1, output.Text(step["name"]), output.FirstLine(reason))
		}
	}
	// Every step launched fine and the work on the server failed: the watcher says why.
	if line := watcherLine(run, out); line != "" && stepsFinished(run) {
		fmt.Fprintln(a.stdout, "  "+line)
	}
	return errRunFailed
}
