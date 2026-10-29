package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestARecordIsNamedByItsIdNotLookedUpAsAName(t *testing.T) {
	app := map[string]any{"id": "app_7k2m9xq4pzta", "name": "shop", "namespace": "acme"}
	server, seen := recordedAPI(t, map[string]any{"/apps/app_7k2m9xq4pzta": app})
	h := newHarness(t, server)
	if code := h.run("apps", "show", "app_7k2m9xq4pzta"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if lookups := seen.to("/apps"); len(lookups) != 0 {
		t.Errorf("an id is not a name to look up: %q", lookups)
	}
	if !strings.Contains(h.stdout.String(), "app_7k2m9xq4pzta") {
		t.Errorf("stdout:\n%s", h.stdout)
	}
}

func TestIsIDTakesThePlatformsIdsAndNumbers(t *testing.T) {
	for ref, want := range map[string]bool{
		"app_7k2m9xq4pzta": true,
		"run_4n8x2k7q9mzt": true,
		"1101":             true,
		"shop":             false,
		"app_7k2m9xq4pzt":  false, // eleven characters
		"app_7k2m9xq4pzti": false, // i is not in the alphabet
		"APP_7K2M9XQ4PZTA": false,
		"my_shop_staging":  false,
	} {
		if got := isID(ref); got != want {
			t.Errorf("isID(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestAChangesAnswerTakesIdsAndNumbers(t *testing.T) {
	for raw, want := range map[string]writeAnswer{
		`{"state":"started","run_id":"run_4n8x2k7q9mzt","backup_id":null}`: {State: "started", RunID: "run_4n8x2k7q9mzt"},
		`{"state":"started","run_id":4812,"backup_id":77,"backup_run_id":4811}`: {
			State: "started", RunID: "4812", BackupID: "77", BackupRunID: "4811",
		},
	} {
		var got writeAnswer
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got != want {
			t.Errorf("%s: %+v, want %+v", raw, got, want)
		}
	}
}

func TestARecordNamedByItsNumberIsWarnedAbout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Deprecation", "@1793232000")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "app_7k2m9xq4pzta", "name": "shop"}})
	}))
	t.Cleanup(server.Close)
	h := newHarness(t, server)
	if code := h.run("apps", "show", "1101"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "Records are named by their id now") {
		t.Errorf("stderr %q", h.stderr)
	}
}
