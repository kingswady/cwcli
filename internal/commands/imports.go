package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
	"github.com/kingswady/cwcli/internal/upload"
)

// cw backups import: a backup file on this computer, restored into an app.
// The platform signs an upload (apps.upload); the file goes in pieces straight
// to the DownloaderWady on the app's server (internal/upload), each piece from
// where the server's copy ends; then apps.import makes it a backup of the app
// and restores it: its import run goes first, the restore after it in the same
// queue. The platform's side: docs/specs/backup-import.md in deployment_workflow.

const (
	// pieceAttempts is how often in a row cw asks the server before an upload
	// gives up: a piece cut off on the way is sent again from where its copy ends.
	pieceAttempts = 5
	firstPause    = 2 * time.Second
	longestPause  = 30 * time.Second
	// cancelWait bounds asking the server to drop an upload that stopped.
	cancelWait = 10 * time.Second
	// meterInterval is how often the progress line is redrawn in a terminal.
	meterInterval = 500 * time.Millisecond
)

// errUploadStopped ends an upload stopped by Ctrl-C; the message is printed before.
var errUploadStopped = errors.New("the upload stopped")

const importAbout = "Restores a backup file from this computer into the app, replacing its database — what the\n" +
	"dashboard's Import on an app's Backups tab does. The file goes in pieces straight to the app's\n" +
	"server; a dropped connection resumes where it stopped, and Ctrl-C stops the upload.\n\n" +
	"It takes an Odoo backup .zip (from Odoo's database manager or Odoo.sh, with or without the\n" +
	"filestore), a pg_dump custom dump (.dump) or plain SQL (.sql, .sql.gz): the platform reads which\n" +
	"from the file's content. Its import goes first and checks the file: when it is not an Odoo\n" +
	"backup, is damaged or comes from another Odoo version than the app's, the restore does not start\n" +
	"and the app is not changed. The file stays among the app's backups, marked imported (one from\n" +
	"another version too). Refused while something runs on the app. It takes two of the token's\n" +
	"functions: apps.upload and apps.import."

// signedUpload is the platform's answer to apps.upload: where the pieces go, signed.
type signedUpload struct {
	UploadID  recordRef `json:"upload_id"`
	URL       string    `json:"url"`
	Size      int64     `json:"size"`
	PieceSize int64     `json:"piece_size"`
	ExpiresAt string    `json:"expires_at"`
}

// importJob is one cw backups import, its command line read.
type importJob struct {
	file     *os.File
	filename string // the file's name, as sent
	name     string // …and as shown
	size     int64
	appID    string
	subject  subject
	server   string         // the app's server, where the file goes
	options  map[string]any // apps.import's body; upload_id is added once the file is in
}

func (a *App) importBackup(args []string) error {
	fs, opts := a.writeFlagSet("backups import")
	appRef := fs.String("app", "", "the app to restore it into (id or name)")
	neutralize := fs.Bool("neutralize", false,
		"neutralize it as a test copy: outgoing mail, scheduled actions and payment providers off")
	resetPassword := fs.Bool("reset-admin-password", false,
		"give its admin user a new password, shown on the restore in the dashboard")
	update := fs.Bool("update", false, "update the app's modules once the data is in")
	narrow := narrowFlags(fs, resourceNamed("apps"), "--app")
	fs.Usage = a.writeUsage(fs, "cw backups import <file> --app <app> [flags]", importAbout)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one file: cw backups import <file> --app <id|name>")
	}
	if *appRef == "" {
		return usagef("name the app to restore it into: cw backups import %s --app <id|name>", positional[0])
	}
	if err := jsonOrWait(opts); err != nil {
		return err
	}
	file, size, err := openBackup(positional[0])
	if err != nil {
		return err
	}
	defer file.Close()
	c, out, err := a.writeClient(opts)
	if err != nil {
		return err
	}
	id, app, err := a.resolveApp(c, *appRef, opts, narrow, out)
	if err != nil {
		return err
	}
	filename := filepath.Base(positional[0])
	server := output.Text(app["server"])
	if server == "-" {
		server = "the app's server"
	}
	return a.runImport(c, importJob{
		file: file, filename: filename, name: output.Clean(filename), size: size,
		appID: id, subject: subjectOf(app, "name"), server: server,
		options: map[string]any{"neutralize": *neutralize, "reset_admin_password": *resetPassword, "update": *update},
	}, opts, out)
}

// openBackup opens the file to import: a plain file with something in it.
func openBackup(path string) (*os.File, int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, usagef("there is no file %s", path)
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	switch {
	case err != nil:
	case info.IsDir():
		err = usagef("%s is a folder: name the backup file in it", path)
	case !info.Mode().IsRegular():
		err = usagef("%s is not a file", path)
	case info.Size() == 0:
		err = usagef("%s is empty", path)
	}
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

// runImport asks, sends the file, then has the platform import and restore it.
// It replaces the app's database: a production app is confirmed by its name.
func (a *App) runImport(c *platform.Client, job importJob, opts *writeFlags, out output.Table) error {
	action := fmt.Sprintf("Restore %s (%s) into %s", job.name, output.Bytes(job.size), job.subject.name)
	if err := a.confirm(opts, action, "Its database is replaced with the one in the file.", job.subject); err != nil {
		return err
	}
	signed, err := a.signUpload(c, job)
	if err != nil {
		return err
	}
	target := &upload.Target{URL: signed.URL, HTTP: a.uploadHTTP()}
	if err := a.sendFile(target, job, signed.PieceSize); err != nil {
		return err
	}
	job.options["upload_id"] = signed.UploadID.String()
	cmd := writeCommand{
		name: "cw backups import", path: "/apps/" + job.appID + "/import", body: job.options,
		subject: job.subject, opts: opts, first: &importFirst,
		started: func(w writeAnswer) string {
			line := fmt.Sprintf("Restore of %s from %s started: run %s.", job.subject.name, job.name, w.RunID)
			if run := w.BackupRunID.String(); run != "" {
				line += fmt.Sprintf(" The import goes first: run %s (backup %s).", run, w.BackupID)
			}
			return line
		},
		finished: func(w writeAnswer) {
			if backup := w.BackupID.String(); backup != "" {
				fmt.Fprintf(a.stdout, "The file is kept among %s's backups: cw backups show %s\n", job.subject.name, backup)
			}
		},
	}
	raw, _, err := a.postWrite(c, cmd.path, cmd.body)
	if err != nil {
		a.importNotTaken(target, job.appID, err)
		return writeError(c, cmd.name, err)
	}
	return a.reportWrite(c, cmd, raw, out)
}

// signUpload asks the platform for an upload of the file to the app's server (apps.upload).
func (a *App) signUpload(c *platform.Client, job importJob) (signedUpload, error) {
	raw, _, err := a.postWrite(c, "/apps/"+job.appID+"/uploads", map[string]any{"filename": job.filename, "size": job.size})
	if err != nil {
		return signedUpload{}, writeError(c, "cw backups import", err)
	}
	var signed signedUpload
	if err := platform.Decode(raw, &signed); err != nil {
		return signed, err
	}
	if err := upload.CheckURL(signed.URL); err != nil {
		return signed, err
	}
	if signed.Size != job.size {
		return signed, fmt.Errorf("the platform signed an upload of %d bytes, and the file has %d", signed.Size, job.size)
	}
	if signed.PieceSize <= 0 {
		signed.PieceSize = upload.DefaultPiece
	}
	return signed, nil
}

// importNotTaken says what became of the uploaded file when apps.import did
// not answer yes. Refused: nothing started, and the server drops the file.
// Unanswered: the restore may have started, so the file stays.
func (a *App) importNotTaken(target *upload.Target, appID string, err error) {
	var refused *platform.APIError
	if errors.As(err, &refused) && refused.Status < http.StatusInternalServerError {
		a.dropUpload(target)
		fmt.Fprintln(a.stderr, "The restore did not start; the uploaded file was removed from the server.")
		return
	}
	fmt.Fprintf(a.stderr, "The file is uploaded, but whether its restore started is not known — see cw runs --app %s\n", appID)
}

func (a *App) uploadHTTP() *http.Client {
	if a.uploadClient != nil {
		return a.uploadClient
	}
	return upload.NewHTTPClient()
}

// sendFile puts the file on the app's server. When it does not get there
// (refused, cut off for good, Ctrl-C) the server drops what it holds: the app
// is untouched.
func (a *App) sendFile(target *upload.Target, job importJob, piece int64) error {
	ctx, stop := a.interrupt()
	defer stop()
	m := a.newMeter(job.name, job.size, job.server)
	err := a.transfer(ctx, target, job.file, job.size, piece, m)
	m.close(err == nil)
	if err == nil {
		return nil
	}
	a.dropUpload(target)
	if ctx.Err() != nil {
		fmt.Fprintln(a.stderr, "Stopped: the upload was cancelled, and nothing was changed.")
		return errUploadStopped
	}
	return uploadError(err, job.server, job.size)
}

// transfer sends what the server does not hold yet, piece by piece, each from
// where its copy ends. A piece cut off on the way (or a server briefly away)
// is sent again after a pause and a look at where the copy ends, at most
// pieceAttempts tries in a row; a 409 that says where it ends is followed at once.
func (a *App) transfer(ctx context.Context, target *upload.Target, file io.ReaderAt, size, piece int64, m *meter) error {
	state, err := target.Status(ctx)
	failures := 0
	for {
		held := heldAt(err)
		if err != nil {
			failures++
			if failures >= pieceAttempts || ctx.Err() != nil || (held == nil && !passing(err)) {
				return err
			}
		}
		switch {
		case held != nil:
			state = *held
		case err != nil:
			if !a.pauseUpload(ctx, m, err, failures) {
				return ctx.Err()
			}
			state, err = target.Status(ctx)
			continue
		}
		if state.Complete || state.Offset >= size {
			return nil
		}
		start := state.Offset
		m.at(start)
		var sent upload.State
		sent, err = target.Put(ctx, file, start, min(start+piece, size)-1, size, func(n int64) { m.sending(start + n) })
		if err == nil {
			state, failures = sent, 0
		}
	}
}

// heldAt is where the server's copy ends, from a 409 that says so; else nil.
func heldAt(err error) *upload.State {
	var refused *upload.Refused
	if errors.As(err, &refused) {
		return refused.State
	}
	return nil
}

// passing is a failure worth asking again: the network, another request of
// this upload still being written, or a gateway between cw and the server.
func passing(err error) bool {
	var network *upload.NetworkError
	var refused *upload.Refused
	switch {
	case errors.As(err, &network):
		return true
	case errors.As(err, &refused):
		switch refused.Status {
		case http.StatusConflict, http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

// pauseUpload says why cw asks again, and waits before it; false on Ctrl-C.
func (a *App) pauseUpload(ctx context.Context, m *meter, err error, failures int) bool {
	pause := min(firstPause<<(failures-1), longestPause)
	m.say(fmt.Sprintf("cw: %s — trying again in %s (%d of %d)", output.Clean(err.Error()), pause, failures+1, pieceAttempts))
	return a.wait(ctx, pause)
}

// uploadError is why an upload stopped, said for the server it went to.
func uploadError(err error, server string, size int64) error {
	var refused *upload.Refused
	if errors.As(err, &refused) {
		switch refused.Status {
		case http.StatusNotFound:
			return fmt.Errorf("the DownloaderWady on %s must be updated before it takes uploads: "+
				"set its Image Tag to 1.1.0 or later and Update it", server)
		case http.StatusServiceUnavailable:
			return fmt.Errorf("%s does not take uploads yet: its DownloaderWady needs an Update", server)
		case http.StatusInsufficientStorage:
			return fmt.Errorf("%s has not enough free disk for this file (%s): make room there, then run this again",
				server, output.Bytes(size))
		case http.StatusUnauthorized:
			return errors.New("the upload expired (it is signed for 24 hours): run this again to start a new one")
		case http.StatusForbidden:
			return errors.New("the server refused the upload's signature: run this again to start a new upload")
		case http.StatusRequestEntityTooLarge, http.StatusRequestedRangeNotSatisfiable:
			return fmt.Errorf("the server refused a piece (%v): that is a bug in cw, please report it", err)
		}
	}
	if passing(err) {
		return fmt.Errorf("the upload to %s stopped after %d tries: %w", server, pieceAttempts, err)
	}
	return fmt.Errorf("the server refused the upload (%w)", err)
}

// dropUpload asks the server to delete what it holds of an upload that will
// not be restored. Best effort: the server deletes what nobody finished within
// a day by itself.
func (a *App) dropUpload(target *upload.Target) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelWait)
	defer cancel()
	_ = target.Cancel(ctx)
}

// meter shows an upload's progress on stderr: in a terminal one line, redrawn
// as the bytes go out; elsewhere a line at each tenth of the file.
type meter struct {
	a      *App
	name   string
	size   int64
	live   bool
	opened time.Time // when the upload began, for the line at the end

	mu    sync.Mutex
	from  int64     // where the server's copy ended when cw began sending; -1 before
	began time.Time // when it began sending, for the speed
	tenth int64     // the last tenth a plain line said

	shown         atomic.Int64 // bytes in, and on their way
	stop, stopped chan struct{}
}

func (a *App) newMeter(name string, size int64, server string) *meter {
	m := &meter{a: a, name: name, size: size, live: a.stderrIsTerminal(), opened: a.clock(), from: -1}
	if !m.live {
		fmt.Fprintf(a.stderr, "Uploading %s (%s) to %s.\n", name, output.Bytes(size), server)
		return m
	}
	m.stop, m.stopped = make(chan struct{}), make(chan struct{})
	go m.redraw()
	return m
}

// at is where the server's copy ends, as a piece leaves from there.
func (m *meter) at(offset int64) {
	m.shown.Store(offset)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.from < 0 {
		m.from, m.began = offset, m.a.clock()
	}
	if tenth := offset * 10 / m.size; !m.live && tenth > m.tenth {
		m.tenth = tenth
		fmt.Fprintln(m.a.stderr, m.line(offset))
	}
}

// sending is how far a piece on its way has got (another goroutine says it).
func (m *meter) sending(offset int64) { m.shown.Store(offset) }

// say prints a line of its own; in a terminal the progress line comes back after it.
func (m *meter) say(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.live {
		fmt.Fprint(m.a.stderr, "\r\033[K")
	}
	fmt.Fprintln(m.a.stderr, text)
}

func (m *meter) redraw() {
	defer close(m.stopped)
	ticker := time.NewTicker(meterInterval)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		fmt.Fprint(m.a.stderr, "\r\033[K"+m.line(m.shown.Load()))
		m.mu.Unlock()
		select {
		case <-m.stop:
			return
		case <-ticker.C:
		}
	}
}

// close ends the progress: a line saying the file is in, or, in a terminal,
// the progress line left where it stopped.
func (m *meter) close(done bool) {
	if m.live {
		close(m.stop)
		<-m.stopped
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case done && m.live:
		fmt.Fprint(m.a.stderr, "\r\033[K")
		fallthrough
	case done:
		fmt.Fprintf(m.a.stderr, "Uploaded %s (%s) in %s.\n", m.name, output.Bytes(m.size), output.Elapsed(m.a.clock().Sub(m.opened)))
	case m.live:
		fmt.Fprintln(m.a.stderr)
	}
}

// line is the progress: "Uploading acme.zip: 42%  1.8 GB of 4.2 GB  12.3 MB/s  3m 20s left".
// The caller holds m.mu.
func (m *meter) line(at int64) string {
	name := m.name
	if runes := []rune(name); len(runes) > 32 {
		name = string(runes[:31]) + "…"
	}
	parts := []string{fmt.Sprintf("Uploading %s: %d%%", name, at*100/m.size), output.Bytes(at) + " of " + output.Bytes(m.size)}
	if rate := m.rate(at); rate > 0 {
		left := time.Duration(float64(m.size-at) / rate * float64(time.Second))
		parts = append(parts, output.Bytes(int64(rate))+"/s", output.Elapsed(left)+" left")
	}
	return strings.Join(parts, "  ")
}

// rate is the bytes a second since cw began sending; 0 in the first second.
func (m *meter) rate(at int64) float64 {
	if m.from < 0 {
		return 0
	}
	elapsed := m.a.clock().Sub(m.began).Seconds()
	if elapsed < 1 || at <= m.from {
		return 0
	}
	return float64(at-m.from) / elapsed
}
