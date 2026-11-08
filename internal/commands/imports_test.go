package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// backupBytes is the file imported: 25 bytes, sent in pieces of 10.
const backupBytes = "0123456789abcdefghijKLMNO"

const uploadID = "5f0c2d7e9a1b4c3d8e6f7a8b9c0d1e2f"

var imported = ok(202, map[string]any{
	"state": "started", "run_id": 41977, "app_id": 7, "backup_id": 9482, "backup_run_id": 41976,
})

// downloader plays the DownloaderWady on the app's server: GET says where its
// copy of the file ends, PUT takes the piece that starts there, DELETE drops it.
type downloader struct {
	mu   sync.Mutex
	size int64
	held []byte
	// seen is every request in order: "GET", "PUT bytes 0-9/25", "DELETE".
	seen []string
	// authorized is the Authorization headers that reached it: the token never may.
	authorized []string
	puts       int
	// status, when set, answers every request (404: a DownloaderWady from before uploads).
	status int
	// odd, when set, may handle the n-th PUT (from 1) itself; true when it did.
	odd func(n int, w http.ResponseWriter, r *http.Request) bool
}

func (d *downloader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if auth := r.Header.Get("Authorization"); auth != "" {
		d.authorized = append(d.authorized, auth)
	}
	what := r.Method
	if r.Method == http.MethodPut {
		what += " " + r.Header.Get("Content-Range")
	}
	d.seen = append(d.seen, what)
	switch {
	case r.URL.Query().Get("token") != "signed":
		d.answer(w, http.StatusForbidden, map[string]any{"detail": "Invalid token"})
	case d.status != 0:
		d.answer(w, d.status, map[string]any{"detail": http.StatusText(d.status)})
	case r.Method == http.MethodGet:
		d.state(w, http.StatusOK)
	case r.Method == http.MethodDelete:
		d.held = nil
		d.answer(w, http.StatusOK, map[string]any{"canceled": true})
	case r.Method == http.MethodPut:
		d.puts++
		if d.odd == nil || !d.odd(d.puts, w, r) {
			d.receive(w, r)
		}
	}
}

func (d *downloader) receive(w http.ResponseWriter, r *http.Request) {
	var start, end, total int64
	if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil || total != d.size {
		d.answer(w, http.StatusRequestedRangeNotSatisfiable, map[string]any{"detail": "The range is not within the signed size"})
		return
	}
	if start != int64(len(d.held)) {
		d.state(w, http.StatusConflict)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || int64(len(body)) != end-start+1 {
		d.answer(w, http.StatusBadRequest, map[string]any{"detail": "The body is shorter than its range"})
		return
	}
	d.held = append(d.held, body...)
	d.state(w, http.StatusOK)
}

func (d *downloader) state(w http.ResponseWriter, status int) {
	d.answer(w, status, map[string]any{"offset": len(d.held), "size": d.size, "complete": int64(len(d.held)) == d.size})
}

func (d *downloader) answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func (d *downloader) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string{}, d.seen...)
}

// on is app on a server, as the platform names it.
func on(app map[string]any, server string) map[string]any {
	copied := map[string]any{"server": server}
	for key, value := range app {
		copied[key] = value
	}
	return copied
}

// importAPI is the platform and the DownloaderWady on the app's server, on one
// test server, and the file to import. more replaces answers; nil removes one.
func importAPI(t *testing.T, more map[string][]answer) (*writeAPI, *downloader, func(bool, string) *harness, string) {
	t.Helper()
	api := &writeAPI{served: map[string]int{}}
	d := &downloader{size: int64(len(backupBytes))}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/upload/") {
			d.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	signed := ok(200, map[string]any{
		"upload_id": uploadID, "url": server.URL + "/upload/" + uploadID + "?token=signed&expires=1793000000&size=25",
		"size": len(backupBytes), "piece_size": 10, "expires_at": "2026-11-06T12:00:00Z",
	})
	answers := map[string][]answer{
		"GET /apps":            {appsList},
		"GET /apps/7":          {ok(200, on(stagingApp, "prod-1"))},
		"GET /apps/8":          {ok(200, on(productionApp, "prod-2"))},
		"POST /apps/7/uploads": {signed},
		"POST /apps/8/uploads": {signed},
		"POST /apps/7/import":  {imported},
		"POST /apps/8/import":  {imported},
	}
	for key, list := range more {
		if list == nil {
			delete(answers, key)
			continue
		}
		answers[key] = list
	}
	api.mu.Lock()
	api.answers = answers
	api.mu.Unlock()
	file := filepath.Join(t.TempDir(), "acme.zip")
	if err := os.WriteFile(file, []byte(backupBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	return api, d, func(terminal bool, typed string) *harness { return writeHarness(t, server, terminal, typed) }, file
}

func (api *writeAPI) postsTo(path string) []post {
	api.mu.Lock()
	defer api.mu.Unlock()
	var matching []post
	for _, p := range api.posts {
		if p.path == path {
			matching = append(matching, p)
		}
	}
	return matching
}

func TestAnImportSendsThePiecesThenRestores(t *testing.T) {
	api, d, harnessFor, file := importAPI(t, nil)
	h := harnessFor(true, "y\n")
	if code := h.run("backups", "import", file, "--app", "shop", "--neutralize"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	question := "Restore acme.zip (25 bytes) into shop (Shop / staging)? Its database is replaced with the one in the file. [y/N]"
	if !strings.Contains(h.stderr.String(), question) {
		t.Errorf("asked %q", h.stderr)
	}
	want := []string{"GET", "PUT bytes 0-9/25", "PUT bytes 10-19/25", "PUT bytes 20-24/25"}
	if got := d.requests(); !reflect.DeepEqual(got, want) {
		t.Errorf("the server saw %q, want %q", got, want)
	}
	if string(d.held) != backupBytes || len(d.authorized) != 0 {
		t.Errorf("held %q, Authorization sent %q", d.held, d.authorized)
	}
	uploads, imports := api.postsTo("/apps/7/uploads"), api.postsTo("/apps/7/import")
	if len(uploads) != 1 || !reflect.DeepEqual(uploads[0].body, map[string]any{"filename": "acme.zip", "size": float64(25)}) {
		t.Errorf("uploads %+v", uploads)
	}
	body := map[string]any{"upload_id": uploadID, "neutralize": true, "reset_admin_password": false, "update": false}
	if len(imports) != 1 || !reflect.DeepEqual(imports[0].body, body) {
		t.Errorf("imports %+v", imports)
	}
	if uploads[0].key == imports[0].key || !strings.HasPrefix(imports[0].key, "cw-") {
		t.Errorf("each request its own key: %q %q", uploads[0].key, imports[0].key)
	}
	if !strings.Contains(h.stdout.String(), "Restore of shop from acme.zip started: run 41977. The import goes first: run 41976 (backup 9482).") {
		t.Errorf("stdout %q", h.stdout)
	}
	for _, said := range []string{
		"Uploading acme.zip (25 bytes) to prod-1.\n", "Uploading acme.zip: 40%  10 bytes of 25 bytes\n",
		"Uploading acme.zip: 80%  20 bytes of 25 bytes\n", "Uploaded acme.zip (25 bytes) in 0s.\n",
		"Follow it: cw runs show 41977",
	} {
		if !strings.Contains(h.stderr.String(), said) {
			t.Errorf("missing %q in\n%s", said, h.stderr)
		}
	}
}

// A 409 says where the server's copy ends: a piece it holds already (its answer
// lost on the way) is not sent again, the next one starts there.
func TestA409SaysWhereTheNextPieceStarts(t *testing.T) {
	_, d, harnessFor, file := importAPI(t, nil)
	d.odd = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n != 1 {
			return false
		}
		body, _ := io.ReadAll(r.Body)
		d.held = append(d.held, body...)
		d.state(w, http.StatusConflict)
		return true
	}
	h := harnessFor(false, "")
	if code := h.run("backups", "import", file, "--app", "shop", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	want := []string{"GET", "PUT bytes 0-9/25", "PUT bytes 10-19/25", "PUT bytes 20-24/25"}
	if got := d.requests(); !reflect.DeepEqual(got, want) || string(d.held) != backupBytes {
		t.Errorf("the server saw %q and holds %q", got, d.held)
	}
}

func TestAPieceCutOffOnTheWayIsSentAgainFromWhereTheServerSays(t *testing.T) {
	_, d, harnessFor, file := importAPI(t, nil)
	d.odd = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n != 2 {
			return false
		}
		io.ReadFull(r.Body, make([]byte, 3))
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return false
		}
		conn.Close() // the connection drops in the middle of the piece
		return true
	}
	h := harnessFor(false, "")
	var waited []time.Duration
	h.app.wait = func(_ context.Context, pause time.Duration) bool { waited = append(waited, pause); return true }
	if code := h.run("backups", "import", file, "--app", "shop", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	want := []string{"GET", "PUT bytes 0-9/25", "PUT bytes 10-19/25", "GET", "PUT bytes 10-19/25", "PUT bytes 20-24/25"}
	if got := d.requests(); !reflect.DeepEqual(got, want) || string(d.held) != backupBytes {
		t.Errorf("the server saw %q and holds %q", got, d.held)
	}
	if !reflect.DeepEqual(waited, []time.Duration{2 * time.Second}) || !strings.Contains(h.stderr.String(), "— trying again in 2s (2 of 5)") {
		t.Errorf("waited %v, stderr %q", waited, h.stderr)
	}
	if strings.Contains(h.stderr.String(), "token=") {
		t.Errorf("the upload's signature was printed: %q", h.stderr)
	}
}

func TestAnOldDownloaderWadySaysToUpdateIt(t *testing.T) {
	api, d, harnessFor, file := importAPI(t, nil)
	d.status = http.StatusNotFound
	h := harnessFor(false, "")
	if code := h.run("backups", "import", file, "--app", "shop", "--yes"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	want := "the DownloaderWady on prod-1 must be updated before it takes uploads: set its Image Tag to 1.1.0 or later and Update it"
	if !strings.Contains(h.stderr.String(), want) {
		t.Errorf("stderr %q", h.stderr)
	}
	if got := d.requests(); !reflect.DeepEqual(got, []string{"GET", "DELETE"}) {
		t.Errorf("the server saw %q", got)
	}
	if len(api.postsTo("/apps/7/import")) != 0 {
		t.Error("restored a file that never got there")
	}
}

func TestTheServerSaysWhyItRefusesAnUpload(t *testing.T) {
	for status, says := range map[int]string{
		http.StatusServiceUnavailable:           "prod-1 does not take uploads yet: its DownloaderWady needs an Update",
		http.StatusInsufficientStorage:          "prod-1 has not enough free disk for this file (25 bytes)",
		http.StatusUnauthorized:                 "the upload expired",
		http.StatusForbidden:                    "refused the upload's signature",
		http.StatusRequestedRangeNotSatisfiable: "a bug in cw",
	} {
		_, d, harnessFor, file := importAPI(t, nil)
		d.status = status
		h := harnessFor(false, "")
		if code := h.run("backups", "import", file, "--app", "shop", "--yes"); code != 1 || !strings.Contains(h.stderr.String(), says) {
			t.Errorf("%d: exit %d: %s", status, code, h.stderr)
		}
	}
}

func TestAnImportIsAskedAsAChangeToTheDatabase(t *testing.T) {
	api, d, harnessFor, file := importAPI(t, nil)
	h := harnessFor(true, "n\n")
	if code := h.run("backups", "import", file, "--app", "shop"); code != 1 || !strings.Contains(h.stderr.String(), "nothing was changed") {
		t.Errorf("no: exit %d: %s", code, h.stderr)
	}
	h = harnessFor(true, "y\n")
	if code := h.run("backups", "import", file, "--app", "erp"); code != 1 {
		t.Errorf("y is not the name: exit %d: %s", code, h.stderr)
	}
	want := "Restore acme.zip (25 bytes) into erp (ERP / production) — a production app. " +
		"Its database is replaced with the one in the file. Type erp to go on:"
	if !strings.Contains(h.stderr.String(), want) {
		t.Errorf("asked %q", h.stderr)
	}
	h = harnessFor(false, "")
	if code := h.run("backups", "import", file, "--app", "shop"); code != 2 || !strings.Contains(h.stderr.String(), "pass --yes") {
		t.Errorf("no terminal: exit %d: %s", code, h.stderr)
	}
	if len(api.posts) != 0 || len(d.requests()) != 0 {
		t.Fatalf("sent something unasked: posts %+v, server %q", api.posts, d.requests())
	}
	h = harnessFor(true, "erp\n")
	if code := h.run("backups", "import", file, "--app", "erp"); code != 0 || len(api.postsTo("/apps/8/import")) != 1 {
		t.Errorf("its name: exit %d: %s", code, h.stderr)
	}
	h = harnessFor(false, "")
	if code := h.run("backups", "import", file, "--app", "shop", "--yes"); code != 0 || len(api.postsTo("/apps/7/import")) != 1 {
		t.Errorf("--yes: exit %d: %s", code, h.stderr)
	}
}

func TestImportWaitFollowsTheImportThenTheRestore(t *testing.T) {
	_, _, harnessFor, file := importAPI(t, map[string][]answer{
		"GET /runs/41976": {run("done", false, step("Import backup", "successful", ""))},
		"GET /runs/41977": {run("done", false, step("Restore database", "successful", ""))},
	})
	h := harnessFor(false, "")
	if code := h.run("backups", "import", file, "--app", "shop", "--yes", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	want := strings.Join([]string{
		"Restore of shop from acme.zip started: run 41977. The import goes first: run 41976 (backup 9482).",
		"Import first: run 41976",
		"  1. Import backup  successful",
		"Run 41976 done.",
		"Then the restore: run 41977",
		"  1. Restore database  successful",
		"Run 41977 done.",
		"The file is kept among shop's backups: cw backups show 9482",
	}, "\n")
	if !strings.Contains(h.stdout.String(), want) {
		t.Errorf("want\n%s\nin\n%s", want, h.stdout)
	}
}

func TestAFailedImportLeavesTheAppUnchanged(t *testing.T) {
	api, _, harnessFor, file := importAPI(t, map[string][]answer{
		"GET /runs/41976": {run("error", false, step("Import backup", "failed", "The file is not an Odoo backup: no dump.sql in the zip"))},
		"GET /runs/41977": {run("draft", false, step("Restore database", "draft", ""))},
	})
	h := harnessFor(false, "")
	if code := h.run("backups", "import", file, "--app", "shop", "--yes", "--wait"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stdout)
	}
	for _, want := range []string{
		"1. Import backup: The file is not an Odoo backup: no dump.sql in the zip",
		"The import failed, so the restore (run 41977) did not start: the app was not changed.",
	} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("missing %q in\n%s", want, h.stdout)
		}
	}
	if api.served["GET /runs/41977"] != 0 || strings.Contains(h.stdout.String(), "kept among") {
		t.Errorf("followed the restore the import held back:\n%s", h.stdout)
	}
}

func TestImportRefusesAMistakeBeforeAnything(t *testing.T) {
	api, d, harnessFor, file := importAPI(t, nil)
	folder := t.TempDir()
	empty := filepath.Join(folder, "empty.zip")
	os.WriteFile(empty, nil, 0o600)
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"backups", "import", folder, "--app", "shop"}, "is a folder"},
		{[]string{"backups", "import", filepath.Join(folder, "nope.zip"), "--app", "shop"}, "there is no file"},
		{[]string{"backups", "import", empty, "--app", "shop"}, "is empty"},
		{[]string{"backups", "import", file}, "name the app to restore it into"},
		{[]string{"backups", "import", "--app", "shop"}, "name one file"},
		{[]string{"backups", "import", file, "--app", "shop", "--json", "--wait"}, "--json prints the platform's answer"},
	} {
		h := harnessFor(false, "")
		if code := h.run(append(c.args, "--yes")...); code != 2 || !strings.Contains(h.stderr.String(), c.says) {
			t.Errorf("%v: exit %d: %s", c.args, code, h.stderr)
		}
	}
	if len(api.posts) != 0 || len(api.served) != 0 || len(d.requests()) != 0 {
		t.Errorf("a mistake reached the platform: %+v %v %q", api.posts, api.served, d.requests())
	}
}

func TestCtrlCStopsTheUploadAndDropsIt(t *testing.T) {
	api, d, harnessFor, file := importAPI(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d.odd = func(n int, _ http.ResponseWriter, _ *http.Request) bool {
		if n == 2 {
			cancel() // Ctrl-C while the second piece is on its way
		}
		return false
	}
	h := harnessFor(false, "")
	h.app.interrupt = func() (context.Context, func()) { return ctx, func() {} }
	if code := h.run("backups", "import", file, "--app", "shop", "--yes"); code != exitInterrupted {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "Stopped: the upload was cancelled, and nothing was changed.") {
		t.Errorf("stderr %q", h.stderr)
	}
	seen := d.requests()
	if seen[len(seen)-1] != "DELETE" || d.held != nil || len(api.postsTo("/apps/7/import")) != 0 {
		t.Errorf("the server saw %q and holds %q; imports %+v", seen, d.held, api.postsTo("/apps/7/import"))
	}
}

func TestAnImportTheRestoreRefusesDropsTheFile(t *testing.T) {
	_, d, harnessFor, file := importAPI(t, map[string][]answer{
		"POST /apps/7/import": {refuse(409, "in_progress", "Cannot restore shop while 'Update' is running.", "")},
	})
	h := harnessFor(false, "")
	if code := h.run("backups", "import", file, "--app", "shop", "--yes"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	for _, want := range []string{"The restore did not start; the uploaded file was removed from the server.", "wait until it ends"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("missing %q in %q", want, h.stderr)
		}
	}
	if seen := d.requests(); seen[len(seen)-1] != "DELETE" {
		t.Errorf("the server saw %q", seen)
	}
}

func TestAnImportPrintsTheAnswerAsJSON(t *testing.T) {
	_, _, harnessFor, file := importAPI(t, nil)
	h := harnessFor(false, "")
	if code := h.run("backups", "import", file, "--app", "shop", "--yes", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	var answer map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &answer); err != nil || answer["backup_run_id"] != float64(41976) {
		t.Errorf("stdout %q (%v)", h.stdout, err)
	}
}

func TestAnOlderPlatformDoesNotOfferImports(t *testing.T) {
	_, d, harnessFor, file := importAPI(t, map[string][]answer{"POST /apps/7/uploads": nil})
	h := harnessFor(false, "")
	if code := h.run("backups", "import", file, "--app", "shop", "--yes"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "does not offer cw backups import yet") || len(d.requests()) != 0 {
		t.Errorf("stderr %q, server %q", h.stderr, d.requests())
	}
}

// In a terminal the progress is one line, redrawn; the line at the end replaces it.
func TestTheProgressIsOneLineInATerminal(t *testing.T) {
	_, _, harnessFor, file := importAPI(t, nil)
	h := harnessFor(false, "")
	h.app.stderrIsTerminal = func() bool { return true }
	if code := h.run("backups", "import", file, "--app", "shop", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	said := h.stderr.String()
	if !strings.Contains(said, "\r\033[KUploading acme.zip: ") || !strings.Contains(said, "\r\033[KUploaded acme.zip (25 bytes) in 0s.\n") {
		t.Errorf("stderr %q", said)
	}
	if strings.Contains(said, "to prod-1.") {
		t.Errorf("a terminal gets the line, not the plain lines: %q", said)
	}
}

func TestImportCompletesAndIsInTheReference(t *testing.T) {
	h := newHarness(t, nil)
	for words, want := range map[string][]string{
		"backups |":                           {"import", "show"},
		"backups import acme.zip --ne|":       {"--neutralize"},
		"backups import acme.zip --res|":      {"--reset-admin-password"},
		"backups import acme.zip --env |":     {"production", "staging", "development"},
		"backups import acme.zip --app shop|": nil,
	} {
		typed := strings.Fields(strings.TrimSuffix(words, "|"))
		if strings.HasSuffix(words, " |") {
			typed = append(typed, "")
		}
		if got := h.app.candidates(typed); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, want %v", words, got, want)
		}
	}
	for _, command := range readReference(t).Commands {
		if command.Name != "backups import" {
			continue
		}
		flags := map[string]bool{}
		for _, f := range command.Flags {
			flags[f.Name] = true
		}
		if command.Group != "Change" || !strings.HasPrefix(command.Usage, "cw backups import <file> --app <app>") ||
			!flags["reset-admin-password"] || !flags["project"] || !strings.Contains(command.Description, "pg_dump") {
			t.Errorf("reference %+v", command)
		}
		return
	}
	t.Error("the reference has no backups import")
}
