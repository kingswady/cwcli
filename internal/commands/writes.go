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

// The commands that change something: cw backup, cw restart, cw runs retry.
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
// script says --yes.
func (a *App) confirm(opts *writeFlags, action string, s subject) error {
	if opts.yes {
		return nil
	}
	if !a.stdinIsTerminal() {
		return usagef("%s%s changes it: pass --yes to do it without asking", action, s.where())
	}
	reader := bufio.NewReader(a.stdin)
	if s.production {
		fmt.Fprintf(a.stderr, "%s%s — a production app. Type %s to go on: ", action, s.where(), s.name)
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(line) != s.name {
			return errCancelled
		}
		return nil
	}
	fmt.Fprintf(a.stderr, "%s%s? [y/N] ", action, s.where())
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
	}[refused.Code]
	if refused.Code == "not_retryable" {
		hint = retryReasons[refused.Reason]
	}
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w — %s", err, hint)
}

// writeAnswer is what the platform answers a change.
type writeAnswer struct {
	State    string      `json:"state"`
	RunID    json.Number `json:"run_id"`
	BackupID json.Number `json:"backup_id"`
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
	path     string
	body     map[string]any
	subject  subject
	opts     *writeFlags
	started  func(writeAnswer) string // the line once it started
	extraRun func(writeAnswer) string // already_running and the like; "" when none
}

func (a *App) runWrite(c *platform.Client, cmd writeCommand, out output.Table) error {
	if cmd.opts.asJSON && cmd.opts.wait {
		return usagef("--json prints the platform's answer; follow the run with cw runs show <id> --json")
	}
	if err := a.confirm(cmd.opts, cmd.action, cmd.subject); err != nil {
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
	return a.waitRun(c, runID, cmd.opts.timeout, out)
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
		body: map[string]any{"format": *format}, subject: s, opts: opts,
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
		fmt.Fprintf(a.stderr, "Usage: %s\n\n%s\n\nIt asks first: a production app by typing its name, anything else y/N.\n"+
			"From a script, --yes. The token needs the function ticked in My Settings → API Tokens.\n\nFlags:\n",
			usage, about)
		fs.PrintDefaults()
	}
}

// waitRun follows run id until it ends, printing each step as its state
// changes. A run whose failed step will be retried has not ended.
func (a *App) waitRun(c *platform.Client, id string, timeout time.Duration, out output.Table) error {
	ctx, stop := a.interrupt()
	defer stop()
	deadline := a.clock().Add(timeout)
	seen := map[int]string{}
	for {
		raw, err := c.GetContext(ctx, "/runs/"+id, nil)
		if ctx.Err() != nil {
			return a.stoppedWaiting(id)
		}
		if pause, done, err := a.pollFailure(err); done {
			return err
		} else if pause > 0 {
			if !a.wait(ctx, pause) {
				return a.stoppedWaiting(id)
			}
			continue
		}
		var run record
		if err := platform.Decode(raw, &run); err != nil {
			return err
		}
		a.printStepChanges(run, seen, out)
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

func (a *App) printStepChanges(run record, seen map[int]string, out output.Table) {
	for i, step := range records(run["steps"]) {
		state := output.Text(step["state"])
		if seen[i] == state {
			continue
		}
		seen[i] = state
		fmt.Fprintf(a.stdout, "  %d. %s  %s\n", i+1, output.Text(step["name"]), out.Cell("state", state))
	}
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
	return errRunFailed
}
