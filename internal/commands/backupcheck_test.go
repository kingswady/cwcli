package commands

import (
	"strings"
	"testing"
)

func checked(status, message string, verified bool) map[string]any {
	return map[string]any{"status": status, "message": message, "backup": map[string]any{
		"id": "bkp_kmsq1fkwf1fb", "name": "shop_20261110", "app": "shop", "size_mb": 3.82, "state": "done",
		"on_server": true, "verified": verified, "offsite": []any{},
	}}
}

func TestBackupsCheckSaysWhatTheServerFoundAndExitsByIt(t *testing.T) {
	for _, tc := range []struct {
		status string
		code   int
	}{{"completed", 0}, {"skipped", 0}, {"waiting", 4}, {"checked_recently", 4}, {"failed", 1}} {
		t.Run(tc.status, func(t *testing.T) {
			server, seen := recordedAPI(t, map[string]any{
				"/backups/bkp_kmsq1fkwf1fb/check": checked(tc.status, "The server says: "+tc.status+".", tc.status == "completed"),
			})
			h := newHarness(t, server)
			if code := h.run("backups", "check", "bkp_kmsq1fkwf1fb"); code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, h.stderr)
			}
			out := h.stdout.String()
			if !strings.Contains(out, "The server says: "+tc.status+".") || !strings.Contains(out, "shop_20261110") {
				t.Errorf("stdout:\n%s", out)
			}
			if got := seen.to("/backups/bkp_kmsq1fkwf1fb/check"); len(got) != 1 {
				t.Errorf("asked %q", got)
			}
		})
	}
}

func TestBackupsCheckOnAnOlderPlatform(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{}))
	if code := h.run("backups", "check", "bkp_kmsq1fkwf1fb"); code != 1 || !strings.Contains(h.stderr.String(), "does not offer cw backups check yet") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}

func TestBackupsCheckNamesOneBackup(t *testing.T) {
	h := newHarness(t, fakeAPI(t, nil))
	if code := h.run("backups", "check"); code != 2 {
		t.Fatalf("exit %d", code)
	}
}
