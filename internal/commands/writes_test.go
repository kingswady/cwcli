package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// answer is one reply of the write fake: an HTTP status and the whole JSON body.
type answer struct {
	status  int
	body    any
	headers map[string]string
}

func ok(status int, data any) answer {
	return answer{status: status, body: map[string]any{"data": data}}
}

func refuse(status int, code, message, reason string) answer {
	body := map[string]any{"code": code, "message": message}
	if reason != "" {
		body["reason"] = reason
	}
	return answer{status: status, body: map[string]any{"error": body}}
}

// post is one write the fake received.
type post struct {
	path string
	key  string
	body map[string]any
}

// writeAPI answers "METHOD /path" with a list of answers, one per request in
// turn (the last one repeats), and records every POST.
type writeAPI struct {
	mu      sync.Mutex
	answers map[string][]answer
	served  map[string]int
	posts   []post
}

func newWriteAPI(t *testing.T, answers map[string][]answer) (*httptest.Server, *writeAPI) {
	t.Helper()
	api := &writeAPI{answers: answers, served: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		if r.Method == http.MethodPost {
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &body)
			api.posts = append(api.posts, post{path: path, key: r.Header.Get("Idempotency-Key"), body: body})
		}
		key := r.Method + " " + path
		list, found := api.answers[key]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":{"code":"not_found","message":"No API function at %s"}}`, path)
			return
		}
		turn := min(api.served[key], len(list)-1)
		api.served[key]++
		reply := list[turn]
		for name, value := range reply.headers {
			w.Header().Set(name, value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		json.NewEncoder(w).Encode(reply.body)
	}))
	t.Cleanup(server.Close)
	return server, api
}

var (
	stagingApp    = map[string]any{"id": 7, "name": "shop", "project": "Shop", "environment_type": "staging"}
	productionApp = map[string]any{"id": 8, "name": "erp", "project": "ERP", "environment_type": "production"}
	appsList      = ok(200, map[string]any{"items": []any{stagingApp, productionApp}, "total": 2})
)

func run(state string, retrying bool, steps ...map[string]any) answer {
	return ok(200, map[string]any{"id": 41977, "state": state, "retrying": retrying, "steps": steps})
}

func step(name, state, reason string) map[string]any {
	return map[string]any{"name": name, "state": state, "reason": reason}
}

// writeHarness is cw in a terminal (or not), the answer typed in.
func writeHarness(t *testing.T, server *httptest.Server, terminal bool, typed string) *harness {
	t.Helper()
	h := newHarness(t, server)
	h.app.stdinIsTerminal = func() bool { return terminal }
	h.app.stdin = strings.NewReader(typed)
	return h
}

func TestARestartIsAskedConfirmedAndFollowed(t *testing.T) {
	server, api := newWriteAPI(t, map[string][]answer{
		"GET /apps":            {appsList},
		"GET /apps/7":          {ok(200, stagingApp)},
		"POST /apps/7/restart": {ok(202, map[string]any{"state": "started", "run_id": 41977, "app_id": 7})},
		"GET /runs/41977": {
			run("running", false, step("Restart app", "running", "")),
			run("done", false, step("Restart app", "successful", "")),
		},
	})
	h := writeHarness(t, server, true, "y\n")
	if code := h.run("restart", "shop", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "Restart shop (Shop / staging)? [y/N]") {
		t.Errorf("asked %q", h.stderr)
	}
	for _, want := range []string{"Restart of shop started: run 41977.", "1. Restart app  running", "1. Restart app  successful", "Run 41977 done."} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("missing %q in\n%s", want, h.stdout)
		}
	}
	if len(api.posts) != 1 || !strings.HasPrefix(api.posts[0].key, "cw-") {
		t.Errorf("posts %+v", api.posts)
	}
}

func TestNoAnswerIsNoChange(t *testing.T) {
	server, api := newWriteAPI(t, map[string][]answer{"GET /apps": {appsList}, "GET /apps/7": {ok(200, stagingApp)}})
	for _, typed := range []string{"\n", "n\n", ""} {
		h := writeHarness(t, server, true, typed)
		if code := h.run("restart", "shop"); code != 1 || !strings.Contains(h.stderr.String(), "nothing was changed") {
			t.Errorf("%q: exit %d: %s", typed, code, h.stderr)
		}
	}
	if len(api.posts) != 0 {
		t.Errorf("posted %+v", api.posts)
	}
}

func TestWithoutATerminalAScriptMustSayYes(t *testing.T) {
	server, api := newWriteAPI(t, map[string][]answer{
		"GET /apps":            {appsList},
		"GET /apps/7":          {ok(200, stagingApp)},
		"POST /apps/7/restart": {ok(202, map[string]any{"state": "started", "run_id": 41977})},
	})
	h := writeHarness(t, server, false, "")
	if code := h.run("restart", "shop"); code != 2 || !strings.Contains(h.stderr.String(), "pass --yes") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if len(api.posts) != 0 {
		t.Fatalf("posted without --yes: %+v", api.posts)
	}
	h = writeHarness(t, server, false, "")
	if code := h.run("restart", "shop", "--yes"); code != 0 || len(api.posts) != 1 {
		t.Errorf("--yes: exit %d, posts %d: %s", code, len(api.posts), h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "cw runs show 41977") {
		t.Errorf("no follow hint: %q", h.stderr)
	}
}

func TestAProductionAppIsConfirmedByItsName(t *testing.T) {
	server, api := newWriteAPI(t, map[string][]answer{
		"GET /apps":           {appsList},
		"GET /apps/8":         {ok(200, productionApp)},
		"POST /apps/8/backup": {ok(202, map[string]any{"state": "started", "run_id": 50, "backup_id": 9482})},
	})
	h := writeHarness(t, server, true, "y\n")
	if code := h.run("backup", "erp"); code != 1 || len(api.posts) != 0 {
		t.Fatalf("y is not the name: exit %d, posts %d", code, len(api.posts))
	}
	if !strings.Contains(h.stderr.String(), "a production app. Type erp to go on") {
		t.Errorf("asked %q", h.stderr)
	}
	h = writeHarness(t, server, true, "erp\n")
	if code := h.run("backup", "erp", "--format", "dump"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if api.posts[0].body["format"] != "dump" || !strings.Contains(h.stdout.String(), "Backup of erp started: run 50 (backup 9482).") {
		t.Errorf("posts %+v, stdout %q", api.posts, h.stdout)
	}
}

func TestOneKeyAcrossTheRetriesOfOneRequest(t *testing.T) {
	limited := refuse(429, "rate_limited", "Too many requests; retry in 2 s", "")
	limited.headers = map[string]string{"Retry-After": "2"}
	server, api := newWriteAPI(t, map[string][]answer{
		"GET /apps":           {appsList},
		"GET /apps/7":         {ok(200, stagingApp)},
		"POST /apps/7/backup": {limited, limited, ok(202, map[string]any{"state": "started", "run_id": 51, "backup_id": 1})},
	})
	var waited []time.Duration
	h := writeHarness(t, server, false, "")
	h.app.wait = func(_ context.Context, d time.Duration) bool { waited = append(waited, d); return true }
	if code := h.run("backup", "shop", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if len(api.posts) != 3 || api.posts[0].key != api.posts[1].key || api.posts[1].key != api.posts[2].key {
		t.Fatalf("posts %+v", api.posts)
	}
	if len(waited) != 2 || waited[0] != 2*time.Second {
		t.Errorf("waited %v", waited)
	}
	again := writeHarness(t, server, false, "")
	again.run("backup", "shop", "--yes")
	if api.posts[3].key == api.posts[0].key {
		t.Error("a new command must send a new key")
	}
}

func TestARunningBackupIsReportedAndCanBeFollowed(t *testing.T) {
	server, _ := newWriteAPI(t, map[string][]answer{
		"GET /apps":           {appsList},
		"GET /apps/7":         {ok(200, stagingApp)},
		"POST /apps/7/backup": {ok(200, map[string]any{"state": "already_running", "run_id": 41950, "backup_id": 9480})},
		"GET /runs/41950":     {run("done", false, step("Backup", "successful", ""))},
	})
	h := writeHarness(t, server, false, "")
	if code := h.run("backup", "shop", "--yes", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "already running (backup 9480, run 41950): nothing new was started") ||
		!strings.Contains(h.stdout.String(), "Run 41950 done.") {
		t.Errorf("stdout %q", h.stdout)
	}
}

func TestWaitEndsOnFailureAndOnTimeoutButNotOnARetryComing(t *testing.T) {
	answers := map[string][]answer{
		"GET /apps":            {appsList},
		"GET /apps/7":          {ok(200, stagingApp)},
		"POST /apps/7/restart": {ok(202, map[string]any{"state": "started", "run_id": 41977})},
		"GET /runs/41977": {
			run("error", true, step("Restart app", "failed", "first try")), // retried: not the end
			run("error", false, step("Restart app", "failed", "docker: no such container")),
		},
	}
	server, _ := newWriteAPI(t, answers)
	h := writeHarness(t, server, false, "")
	if code := h.run("restart", "shop", "--yes", "--wait"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stdout)
	}
	if !strings.Contains(h.stdout.String(), "Restart app: docker: no such container") {
		t.Errorf("stdout %q", h.stdout)
	}

	answers["GET /runs/41977"] = []answer{run("running", false, step("Restart app", "running", ""))}
	slow, _ := newWriteAPI(t, answers)
	h = writeHarness(t, slow, false, "")
	start := time.Now()
	calls := 0
	h.app.clock = func() time.Time { calls++; return start.Add(time.Duration(calls) * time.Minute) }
	if code := h.run("restart", "shop", "--yes", "--wait", "--timeout", "5m"); code != exitStillRunning {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "still going after 5m0s") {
		t.Errorf("stderr %q", h.stderr)
	}
}

func TestRefusalsSayWhatToDo(t *testing.T) {
	cases := []struct {
		answer answer
		code   int
		says   string
	}{
		{refuse(403, "writes_disabled", "Changes through the API are off", ""), 1, "not open to this account"},
		{refuse(409, "in_progress", "Cannot restart shop while 'Update' is running.", ""), 1, "wait until it ends"},
		{refuse(409, "retry_coming", "An automatic retry is due", ""), 1, "add --now"},
		{refuse(409, "not_retryable", "Newer run exists", "newer_run"), 1, "retry that one"},
		{refuse(423, "locked", "Runs are on hold", ""), 1, "safety lock"},
		{refuse(403, "plan_required", "The plan does not include Automated Deployments", ""), 1, "does not include Automated Deployments"},
		{refuse(403, "function_not_allowed", "This token may not call apps.restart", ""), 1, "My Settings → API Tokens"},
		{refuse(409, "request_in_progress", "Still handling it", ""), 1, "check cw runs"},
		{refuse(422, "idempotency_key_reused", "Key used for another request", ""), 1, "run the command again"},
		{refuse(400, "invalid_parameter", "format must be one of zip, dump", ""), 2, "format must be one of"},
	}
	for _, c := range cases {
		server, _ := newWriteAPI(t, map[string][]answer{
			"GET /apps": {appsList}, "GET /apps/7": {ok(200, stagingApp)}, "POST /apps/7/restart": {c.answer},
		})
		h := writeHarness(t, server, false, "")
		if code := h.run("restart", "shop", "--yes"); code != c.code || !strings.Contains(h.stderr.String(), c.says) {
			t.Errorf("%s: exit %d: %s", c.answer.body, code, h.stderr)
		}
	}
}

func TestAnOlderPlatformSaysItDoesNotOfferChanges(t *testing.T) {
	// A platform from before writes knows no POST route: 404 here, 405 from its router.
	server, _ := newWriteAPI(t, map[string][]answer{"GET /apps": {appsList}, "GET /apps/7": {ok(200, stagingApp)}})
	h := writeHarness(t, server, false, "")
	if code := h.run("backup", "shop", "--yes"); code != 1 || !strings.Contains(h.stderr.String(), "does not offer cw backup yet") {
		t.Errorf("exit %d: %s", code, h.stderr)
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte("<html>Method Not Allowed</html>"))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": stagingApp})
	}))
	t.Cleanup(old.Close)
	h = writeHarness(t, old, false, "")
	if code := h.run("restart", "7", "--yes"); code != 1 || !strings.Contains(h.stderr.String(), "does not offer cw restart yet") {
		t.Errorf("405: exit %d: %s", code, h.stderr)
	}
}

func TestARetryNamesTheRunAndCanForceNow(t *testing.T) {
	failedRun := ok(200, map[string]any{"id": 4812, "workflow": "Update Odoo", "record": "shop", "project": "Shop",
		"environment_type": "staging", "state": "error", "retrying": true})
	server, api := newWriteAPI(t, map[string][]answer{
		"GET /runs/4812":        {failedRun},
		"POST /runs/4812/retry": {ok(202, map[string]any{"state": "started", "run_id": 4812})},
	})
	h := writeHarness(t, server, true, "yes\n")
	if code := h.run("runs", "retry", "4812", "--now"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "Retry run 4812 (Update Odoo of shop) (Shop / staging)? [y/N]") {
		t.Errorf("asked %q", h.stderr)
	}
	if api.posts[0].body["now"] != true || !strings.Contains(h.stdout.String(), "Run 4812 started again.") {
		t.Errorf("posts %+v stdout %q", api.posts, h.stdout)
	}
	if code := h.run("runs", "retry", "shop"); code != 2 {
		t.Errorf("a name is not a run id: exit %d", code)
	}
}

func TestWriteCommandsComplete(t *testing.T) {
	h := newHarness(t, nil)
	for words, want := range map[string]string{
		"re":                    "restart",
		"ba":                    "backup",
		"runs r":                "retry",
		"runs retry 4812 --n":   "--now",
		"backup shop --format ": "dump",
		"restart shop --w":      "--wait",
	} {
		got := strings.Join(h.app.candidates(strings.Split(words, " ")), " ")
		if !strings.Contains(got, want) {
			t.Errorf("%q → %q, want %q", words, got, want)
		}
	}
}

func TestCtrlCStopsWaitingNotTheRun(t *testing.T) {
	server, _ := newWriteAPI(t, map[string][]answer{
		"GET /apps":            {appsList},
		"GET /apps/7":          {ok(200, stagingApp)},
		"POST /apps/7/restart": {ok(202, map[string]any{"state": "started", "run_id": 41977})},
		"GET /runs/41977":      {run("running", false, step("Restart app", "running", ""))},
	})
	h := writeHarness(t, server, false, "")
	h.app.wait = func(context.Context, time.Duration) bool { return false } // Ctrl-C during the pause
	if code := h.run("restart", "shop", "--yes", "--wait"); code != exitInterrupted {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "Stopped waiting — run 41977 goes on: cw runs show 41977") {
		t.Errorf("stderr %q", h.stderr)
	}
}

// watched is a backup run whose steps launched the work and whose watcher reports.
func watched(state, watcher, progress, reason string) answer {
	reply := run(state, false, step("Backup", "successful", ""))
	data := reply.body.(map[string]any)["data"].(map[string]any)
	data["id"] = 41976
	data["watcher"] = map[string]any{"state": watcher, "progress": progress, "reason": reason}
	if progress == "" {
		data["watcher"].(map[string]any)["progress"] = nil
	}
	if reason == "" {
		data["watcher"].(map[string]any)["reason"] = nil
	}
	return reply
}

func TestWaitFollowsABackupThroughItsWatcher(t *testing.T) {
	answers := map[string][]answer{
		"GET /apps":           {appsList},
		"GET /apps/7":         {ok(200, stagingApp)},
		"POST /apps/7/backup": {ok(202, map[string]any{"state": "started", "run_id": 41976, "backup_id": 9482})},
		"GET /runs/41976": {
			watched("running", "waiting", "", ""),
			watched("running", "waiting", "Dumping the database", ""),
			watched("running", "waiting", "Dumping the database", ""), // unchanged: not printed again
			watched("running", "waiting", "Packing the archive", ""),
			watched("done", "completed", "", ""),
		},
	}
	server, _ := newWriteAPI(t, answers)
	h := writeHarness(t, server, false, "")
	h.app.wait = func(context.Context, time.Duration) bool { return true }
	if code := h.run("backup", "shop", "--yes", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	want := strings.Join([]string{
		"  1. Backup  successful",
		"  Watcher: waiting for the work on the server to finish",
		"  Watcher: Dumping the database",
		"  Watcher: Packing the archive",
		"  Watcher: finished",
		"Run 41976 done.",
	}, "\n")
	if !strings.Contains(h.stdout.String(), want) {
		t.Errorf("want\n%s\nin\n%s", want, h.stdout)
	}

	// Every step launched fine; the work on the server failed: the watcher says why.
	answers["GET /runs/41976"] = []answer{watched("error", "failed", "", "Success marker not found or invalid")}
	failing, _ := newWriteAPI(t, answers)
	h = writeHarness(t, failing, false, "")
	if code := h.run("backup", "shop", "--yes", "--wait"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stdout)
	}
	if got := strings.Count(h.stdout.String(), "Watcher: failed: Success marker not found or invalid"); got != 2 {
		t.Errorf("want the failure as it happened and in the end report, got %d in\n%s", got, h.stdout)
	}
}

func TestARunShowsItsWatcher(t *testing.T) {
	server, _ := newWriteAPI(t, map[string][]answer{
		"GET /runs/41976": {watched("running", "waiting", "Copying the filestore", "")},
	})
	h := newHarness(t, server)
	if code := h.run("runs", "show", "41976"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "\nWatcher: Copying the filestore\n") {
		t.Errorf("stdout\n%s", h.stdout)
	}
}
