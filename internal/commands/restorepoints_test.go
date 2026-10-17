package commands

import (
	"strings"
	"testing"
)

var amodooPoints = map[string]any{"items": []map[string]any{
	{"id": 57, "app_id": 1849, "at": "2026-10-14T17:24:00Z", "action": "update", "outcome": "applied", "run_id": 41976,
		"commits": []map[string]any{{"repository": "odoo-ep-staging", "before": "972dba2bee01", "after": "4b4259817b02"}},
		"modules": []string{"sale", "stock", "account"}, "backup_id": 9482, "backup": "amodoo_20261014", "backup_verified": true,
		"rollback_of_id": nil, "held": false, "instant_undo": true, "kept_until": "2026-10-21T17:24:00Z"},
	{"id": 56, "app_id": 1849, "at": "2026-10-14T15:02:00Z", "action": "update", "outcome": "kept", "run_id": 41950,
		"commits": []any{}, "modules": []any{}, "backup_id": 9480, "backup": "amodoo_20261014b", "backup_verified": false,
		"rollback_of_id": nil, "held": false, "instant_undo": false, "kept_until": nil},
}, "total": 3}

func TestRestorePointsSayWhatEachChangeDidAndTheWayBack(t *testing.T) {
	server, seen := recordedAPI(t, map[string]any{
		"/apps":                     map[string]any{"items": []map[string]any{{"id": 1849, "name": "amodoo-staging", "namespace": "acme"}}, "total": 1},
		"/apps/1849/restore-points": amodooPoints,
	})
	h := newHarness(t, server)
	if code := h.run("restore-points", "amodoo-staging", "--limit", "2"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{"failed, kept as it was", "applied", "odoo-ep-staging 972dba2b→4b425981", "verified",
		"not verified", "instant until", "41976"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "HELD") {
		t.Errorf("no point is held: the column is noise\n%s", out)
	}
	if got := seen.to("/apps/1849/restore-points"); len(got) != 1 || got[0] != "/apps/1849/restore-points?limit=2&offset=0" {
		t.Errorf("asked %q", got)
	}
	if !strings.Contains(h.stderr.String(), "next page: --offset 2") {
		t.Errorf("stderr: %s", h.stderr)
	}
}

func TestRestorePointsOnAnOlderPlatform(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{}))
	if code := h.run("restore-points", "1849"); code != 1 || !strings.Contains(h.stderr.String(), "does not offer cw restore-points yet") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}

func TestRestorePointsNameOneApp(t *testing.T) {
	h := newHarness(t, fakeAPI(t, nil))
	if code := h.run("restore-points"); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestNoRestorePointsSaysSo(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{"/apps/7/restore-points": map[string]any{"items": []any{}, "total": 0}}))
	if code := h.run("restore-points", "7"); code != 0 || !strings.Contains(h.stderr.String(), "No restore points") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}
