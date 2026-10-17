package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kingswady/cwcli/internal/output"
)

// fullApp is apps.get on a platform that lists every URL, repository and an open upgrade.
var fullApp = map[string]any{
	"id": 1849, "name": "amodoo-staging", "namespace": "syscoon-gmbh", "project": "odoo-test",
	"environment": "Production", "environment_type": "production", "version": "18.0", "edition": "enterprise",
	"state": "deploy", "url": "https://amodoo-staging.example.eu", "server": "odoo-test-production",
	"backup_health": "none",
	"urls": []map[string]any{
		{"url": "https://amodoo-staging.example.eu", "primary": true, "state": "deploy", "ssl_status": "valid",
			"ssl_expires_at": "2099-11-26T22:27:57Z", "public_check": "passed", "public_check_code": "https_odoo_version"},
		{"url": "https://shop.amodoo.de", "primary": false, "state": "deploy", "ssl_status": "expiring_soon",
			"ssl_expires_at": "2099-10-20T08:00:00Z", "public_check": "failed", "public_check_code": "http_502"},
		{"url": "https://www.amodoo.de", "primary": false, "state": "draft", "ssl_status": "unknown",
			"ssl_expires_at": nil, "public_check": nil, "public_check_code": nil},
	},
	"repositories": []map[string]any{
		{"repository": "odoo EP", "branch": "staging", "main": true,
			"deployed_sha": "972dba2bee0123456789abcdef0123456789abcd", "head_sha": "972dba2bee0123456789abcdef0123456789abcd",
			"deployed_message": "Merge pull request #12 from acme/fix", "up_to_date": true, "deleted_upstream": false},
		{"repository": "everphone", "branch": "18.0-amodoo", "main": false,
			"deployed_sha": "4b4259817b0123456789abcdef0123456789abcd", "head_sha": "aaaaaaaabbbb0123456789abcdef0123456789ab",
			"deployed_message": nil, "up_to_date": false, "deleted_upstream": false},
	},
	"upgrade": map[string]any{"id": 12, "from_version": "18.0", "to_version": "19.0", "purpose": "test",
		"state": "both_kept", "source_app_id": 1849, "source_app": "amodoo-staging", "new_app_id": 2194, "new_app": "amodoo-19"},
}

func TestAppShowListsEveryURLAndRepository(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{"/apps/1849": fullApp}))
	if code := h.run("apps", "show", "1849"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{
		"URLs:", "https://shop.amodoo.de", "https://www.amodoo.de", "failed (http_502)", "in ", "expiring_soon",
		"Repositories:", "odoo EP", "staging", "972dba2b", "aaaaaaaa", "up to date", "behind",
		"Merge pull request #12", "Upgrade:", "18.0 → 19.0, both kept (test run) — new app amodoo-19 (2194)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nURL:") {
		t.Errorf("the primary URL is in the URLs table, not a row of its own:\n%s", out)
	}
	if strings.Contains(out, "passed (") {
		t.Errorf("a passed check needs no reason:\n%s", out)
	}
}

func TestAnOlderPlatformsAppShowsItsOneURL(t *testing.T) {
	old := map[string]any{"id": 7, "name": "shop", "url": "https://shop.example", "state": "deploy"}
	h := newHarness(t, fakeAPI(t, map[string]any{"/apps/7": old}))
	if code := h.run("apps", "show", "7"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "URL:") || !strings.Contains(out, "https://shop.example") {
		t.Errorf("stdout:\n%s", out)
	}
	for _, absent := range []string{"URLs:", "Repositories:", "Upgrade:"} {
		if strings.Contains(out, absent) {
			t.Errorf("%s without the data:\n%s", absent, out)
		}
	}
}

func TestTheNewAppOfAnUpgradeNamesWhereItCameFrom(t *testing.T) {
	app := map[string]any{"id": 2194, "name": "amodoo-19", "urls": []any{}, "upgrade": map[string]any{
		"from_version": "18.0", "to_version": "19.0", "purpose": "production", "state": "review",
		"source_app_id": 1849, "source_app": "amodoo-staging", "new_app_id": 2194, "new_app": "amodoo-19"}}
	h := newHarness(t, fakeAPI(t, map[string]any{"/apps/2194": app}))
	if code := h.run("apps", "show", "2194"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "18.0 → 19.0, review — upgraded from amodoo-staging (1849)") {
		t.Errorf("stdout:\n%s", h.stdout)
	}
}

var offsiteBackup = map[string]any{
	"id": 9482, "name": "amodoo_20261014", "app": "amodoo-staging", "state": "deploy", "storage": "local",
	"on_server": false, "verified": true,
	"offsite": []map[string]any{
		{"storage": "s3-staging", "type": "s3", "on_storage": true, "state": "done", "uploaded_at": "2026-10-14T03:10:00Z", "error": nil},
		{"storage": "sftp-box", "type": "sftp", "on_storage": false, "state": "error", "uploaded_at": nil,
			"error": "Host key verification failed\nmore"},
	},
}

func TestBackupShowListsItsOffsiteCopies(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{"/backups/9482": offsiteBackup}))
	if code := h.run("backups", "show", "9482"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{"On server:", "Verified:", "Off-site copies:", "s3-staging", "sftp-box", "Host key verification failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "more") {
		t.Errorf("an error is one line in a table:\n%s", out)
	}
}

func TestBackupsCountTheirOffsiteCopies(t *testing.T) {
	plain := map[string]any{"id": 1, "app": "lab", "state": "deploy", "offsite": []any{}}
	h := newHarness(t, fakeAPI(t, map[string]any{"/backups": map[string]any{
		"items": []map[string]any{offsiteBackup, plain}, "total": 2}}))
	if code := h.run("backups"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "OFFSITE") || !strings.Contains(out, "1/2") {
		t.Errorf("stdout:\n%s", out)
	}
	// An older platform sends no offsite: the column is noise there.
	old := newHarness(t, fakeAPI(t, map[string]any{"/backups": map[string]any{
		"items": []map[string]any{{"id": 1, "app": "lab"}}, "total": 1}}))
	old.run("backups")
	if strings.Contains(old.stdout.String(), "OFFSITE") {
		t.Errorf("stdout:\n%s", old.stdout)
	}
	// Nor do its backups show the rows they have no value for.
	oldShow := newHarness(t, fakeAPI(t, map[string]any{"/backups/1": map[string]any{"id": 1, "app": "lab"}}))
	oldShow.run("backups", "show", "1")
	if strings.Contains(oldShow.stdout.String(), "On server:") {
		t.Errorf("stdout:\n%s", oldShow.stdout)
	}
}

func TestARetryingRunIsNotFailedYet(t *testing.T) {
	server := fakeAPI(t, map[string]any{
		"/runs": map[string]any{"items": []map[string]any{
			{"id": 5, "workflow": "Deploy", "state": "error", "retrying": true, "namespace": "acme"},
			{"id": 6, "workflow": "Update", "state": "error", "retrying": false, "namespace": "acme"},
		}, "total": 2},
		"/runs/5": map[string]any{"id": 5, "workflow": "Deploy", "state": "error", "retrying": true},
	})
	h := newHarness(t, server)
	if code := h.run("runs", "--color", "always"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	if !strings.Contains(out, output.Yellow+"retrying"+output.Reset) || !strings.Contains(out, output.Red+"error"+output.Reset) {
		t.Errorf("stdout:\n%q", out)
	}
	show := newHarness(t, server)
	show.run("runs", "show", "5")
	if !strings.Contains(show.stdout.String(), "retrying") {
		t.Errorf("stdout:\n%s", show.stdout)
	}
}

var shopProject = map[string]any{
	"id": 2, "name": "Shop", "code": "shop", "namespace": "acme", "plan": "Enterprise", "disabled": false,
	"environments": []map[string]any{
		{"id": 5, "name": "Production", "type": "production", "app_count": 1},
		{"id": 6, "name": "Staging", "type": "staging", "app_count": 2},
	},
}

func TestProjectsListTheirEnvironmentsAndApps(t *testing.T) {
	server := fakeAPI(t, map[string]any{
		"/projects":   map[string]any{"items": []map[string]any{shopProject}, "total": 1},
		"/projects/2": shopProject,
	})
	h := newHarness(t, server)
	if code := h.run("projects"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "production, staging") || !strings.Contains(out, "Enterprise") ||
		!strings.Contains(out, "  3") {
		t.Errorf("stdout:\n%s", out)
	}
	show := newHarness(t, server)
	if code := show.run("projects", "show", "shop"); code != 0 {
		t.Fatalf("exit %d: %s", code, show.stderr)
	}
	if out := show.stdout.String(); !strings.Contains(out, "Environments:") || !strings.Contains(out, "Staging") {
		t.Errorf("stdout:\n%s", out)
	}
}

func TestProjectsHelpDoesNotNeedAFilterToNarrowWith(t *testing.T) {
	h := newHarness(t, nil)
	if code := h.run("projects", "--help"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}

func TestURLsListEveryAppsURLWithItsCertificate(t *testing.T) {
	server, seen := recordedAPI(t, map[string]any{
		"/apps": apps,
		"/urls": map[string]any{"items": []map[string]any{
			{"id": 3, "url": "https://shop.example.com", "namespace": "acme", "app_id": 7, "app": "shop",
				"environment_type": "production", "primary": true, "ssl_status": "expiring_soon",
				"ssl_expires_at": "2099-10-20T08:00:00Z", "public_check": "passed"},
		}, "total": 1},
	})
	h := newHarness(t, server)
	if code := h.run("urls", "--app", "shop", "--ssl", "expiring_soon"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "https://shop.example.com") || !strings.Contains(out, "expiring_soon") {
		t.Errorf("stdout:\n%s", out)
	}
	if got := seen.to("/urls"); len(got) != 1 || got[0] != "/urls?app_id=7&limit=50&offset=0&ssl_status=expiring_soon" {
		t.Errorf("asked %q", got)
	}
	noShow := newHarness(t, server)
	if code := noShow.run("urls", "show", "3"); code != 2 || !strings.Contains(noShow.stderr.String(), "cw apps show") {
		t.Errorf("exit %d: %s", code, noShow.stderr)
	}
}

func TestAPlatformWithoutTheFunctionSaysItNeedsAnUpdate(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{}))
	if code := h.run("projects"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "does not offer cw projects yet") {
		t.Errorf("stderr: %s", h.stderr)
	}
	// A record that is not there is still just that.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"not_found","message":"No such record in this token's namespaces"}}`))
	}))
	t.Cleanup(server.Close)
	missing := newHarness(t, server)
	if code := missing.run("apps", "show", "8"); code != 1 || !strings.Contains(missing.stderr.String(), "No such record") {
		t.Errorf("exit %d: %s", code, missing.stderr)
	}
}

func TestNamespacesAreTheTokensWithTheCallersLevel(t *testing.T) {
	me := map[string]any{"login": "a@b", "name": "A", "namespaces": []map[string]any{
		{"id": 1, "name": "Acme", "code": "acme", "level": "admin"},
		{"id": 2, "name": "Beta", "code": "beta", "level": "viewer"},
	}}
	h := newHarness(t, fakeAPI(t, map[string]any{"/whoami": me}))
	if code := h.run("namespaces"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "acme") || !strings.Contains(out, "viewer") || strings.Contains(out, "a@b") {
		t.Errorf("stdout:\n%s", out)
	}
	asJSON := newHarness(t, fakeAPI(t, map[string]any{"/whoami": me}))
	asJSON.run("namespaces", "--json")
	if out := asJSON.stdout.String(); !strings.HasPrefix(out, "[") || !strings.Contains(out, `"code": "beta"`) {
		t.Errorf("stdout:\n%s", out)
	}
}
