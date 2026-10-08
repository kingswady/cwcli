package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kingswady/cwcli/internal/output"
)

var mixedApps = map[string]any{
	"items": []map[string]any{
		{"id": 7, "name": "shop", "namespace": "acme", "environment_type": "production", "state": "deploy",
			"url": "https://shop.example", "backup_health": "healthy"},
		{"id": 9, "name": "lab", "namespace": "beta", "environment_type": "development", "state": "error",
			"url": nil, "backup_health": "critical"},
	},
	"total": 2,
}

func TestAppsAreColouredLikeTheDashboard(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{"/apps": mixedApps}))
	if code := h.run("apps", "--color", "always"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{
		output.Bold + "ENV" + output.Reset,
		output.Red + "production" + output.Reset,
		output.Cyan + "development" + output.Reset,
		output.Green + "deploy" + output.Reset,
		output.Red + "error" + output.Reset,
		output.Green + "healthy" + output.Reset,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestShowURLAddsTheColumnBeforeTheNamespace(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{"/apps": mixedApps}))
	if code := h.run("apps", "--show-url"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	header := strings.Fields(strings.SplitN(h.stdout.String(), "\n", 2)[0])
	if header[len(header)-2] != "URL" || header[len(header)-1] != "NAMESPACE" {
		t.Errorf("header %v", header)
	}
	if !strings.Contains(h.stdout.String(), "https://shop.example") {
		t.Errorf("stdout %s", h.stdout)
	}
	plain := newHarness(t, fakeAPI(t, map[string]any{"/apps": mixedApps}))
	plain.run("apps")
	if strings.Contains(plain.stdout.String(), "URL") {
		t.Errorf("URL is opt-in:\n%s", plain.stdout)
	}
}

func TestDeletedAppsAreHiddenUnlessAll(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		json.NewEncoder(w).Encode(map[string]any{"data": mixedApps})
	}))
	t.Cleanup(server.Close)
	h := newHarness(t, server)
	h.run("apps")
	h.run("apps", "--all")
	if strings.Contains(queries[0], "include_deleted") || !strings.Contains(queries[1], "include_deleted=true") {
		t.Errorf("queries %q", queries)
	}
}

func TestAllOnAnOlderPlatformFallsBackToItsFullList(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Has("include_deleted") {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":"unknown_parameter","message":"Unknown parameter(s): include_deleted"}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": mixedApps})
	}))
	t.Cleanup(server.Close)
	h := newHarness(t, server)
	if code := h.run("apps", "--all"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if len(queries) != 2 || strings.Contains(queries[1], "include_deleted") {
		t.Errorf("queries %q", queries)
	}
}

func TestShowAllFindsADeletedAppByName(t *testing.T) {
	deleted := map[string]any{"items": []map[string]any{{"id": 5, "name": "old", "namespace": "acme", "state": "delete"}}, "total": 1}
	server := fakeAPI(t, map[string]any{
		"/apps?include_deleted=true&limit=200&offset=0&search=old": deleted,
		"/apps":   map[string]any{"items": []map[string]any{}, "total": 0},
		"/apps/5": map[string]any{"id": 5, "name": "old", "state": "delete"},
	})
	h := newHarness(t, server)
	if code := h.run("apps", "show", "old"); code != 1 {
		t.Fatalf("a deleted app is not found by name without --all: exit %d", code)
	}
	h = newHarness(t, server)
	if code := h.run("apps", "show", "old", "--all"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}

func TestAppFiltersBecomeQueryParameters(t *testing.T) {
	servers := map[string]any{"items": []map[string]any{{"id": 3, "name": "prod-1", "namespace": "acme"}}, "total": 1}
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/servers" {
			json.NewEncoder(w).Encode(map[string]any{"data": servers})
			return
		}
		got = r.URL.RawQuery
		json.NewEncoder(w).Encode(map[string]any{"data": mixedApps})
	}))
	t.Cleanup(server.Close)
	h := newHarness(t, server)
	code := h.run("apps", "--search", "shop", "--server", "prod-1", "--version", "19.0", "--edition", "enterprise", "--project", "acme")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	for _, want := range []string{"search=shop", "server_id=3", "version=19.0", "edition=enterprise", "project=acme"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %q", want, got)
		}
	}
}

var apps = map[string]any{
	"items": []map[string]any{
		{"id": 7, "name": "shop", "namespace": "acme", "environment_type": "production", "version": "19.0",
			"state": "deploy", "server": "prod-1", "backup_health": "healthy", "updated_at": nil},
		{"id": 8, "name": "shop-staging", "namespace": "acme", "environment_type": "staging", "version": "19.0",
			"state": "deploy", "server": nil, "backup_health": "none", "updated_at": "2026-09-24 09:00:00"},
	},
	"total": 2,
}

func TestAppsTable(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{"/apps": apps}))
	if code := h.run("apps"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{"ID", "NAME", "shop-staging", "production", "prod-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NAMESPACE") {
		t.Errorf("one namespace: the column is noise\n%s", out)
	}
}

func TestShowResolvesAName(t *testing.T) {
	server := fakeAPI(t, map[string]any{
		"/apps":   apps,
		"/apps/7": map[string]any{"id": 7, "name": "shop", "state": "deploy", "deployed_at": "2026-09-01 08:00:00"},
	})
	h := newHarness(t, server)
	if code := h.run("apps", "show", "SHOP"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "Name:") || !strings.Contains(h.stdout.String(), "shop") {
		t.Errorf("stdout: %s", h.stdout)
	}
}

func TestAnAmbiguousNameListsTheCandidatesAsATable(t *testing.T) {
	twins := map[string]any{"items": []map[string]any{
		{"id": 1113, "name": "shop", "namespace": "acme", "environment_type": "production", "server": "prod-1"},
		{"id": 1183, "name": "shop", "namespace": "beta", "environment_type": "staging", "server": "stage-1"},
	}, "total": 2}
	h := newHarness(t, fakeAPI(t, map[string]any{"/apps": twins}))
	if code := h.run("backups", "--app", "shop"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	lines := strings.Split(h.stderr.String(), "\n")
	if lines[0] != `2 apps are named "shop":` {
		t.Errorf("first line %q", lines[0])
	}
	table := strings.Join(lines[2:5], "\n")
	for _, want := range []string{"ID", "ENV", "SERVER", "NAMESPACE", "1113", "production", "prod-1", "acme", "1183", "stage-1", "beta"} {
		if !strings.Contains(table, want) {
			t.Errorf("missing %q in the table:\n%s", want, table)
		}
	}
	if !strings.Contains(h.stderr.String(), `cw: use one of these ids instead of "shop", or narrow it with --project, --env, --server, --version, --edition or --namespace`) {
		t.Errorf("stderr:\n%s", h.stderr)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("stdout stays clean for --json: %q", h.stdout)
	}
}

func TestBackupsFilterByAppName(t *testing.T) {
	server := fakeAPI(t, map[string]any{
		"/apps": apps,
		"/backups?app_id=7&limit=50&offset=0": map[string]any{"items": []map[string]any{
			{"id": 3, "app": "shop", "namespace": "acme", "size_mb": 2048, "automated": true, "taken_at": "2026-09-24 01:00:00"},
		}, "total": 1},
	})
	h := newHarness(t, server)
	if code := h.run("backups", "--app", "shop"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "2.0 GB") || !strings.Contains(h.stdout.String(), "yes") {
		t.Errorf("stdout: %s", h.stdout)
	}
}

func TestRunShowListsStepsAndWhyOneFailed(t *testing.T) {
	server := fakeAPI(t, map[string]any{"/runs/5": map[string]any{
		"id": 5, "workflow": "Deploy", "state": "error",
		"steps": []map[string]any{
			{"name": "Prepare", "state": "success"},
			{"name": "Deploy", "state": "error", "reason": "Pull image: deploy failed\ndb_password=***REDACTED***"},
		},
	}})
	h := newHarness(t, server)
	if code := h.run("runs", "show", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "Steps:") || !strings.Contains(out, "2. Deploy\n     Pull image: deploy failed\n     db_password") {
		t.Errorf("stdout:\n%s", out)
	}
}

func TestRunsAreLookedUpByIDOnly(t *testing.T) {
	h := newHarness(t, fakeAPI(t, nil))
	if code := h.run("runs", "show", "deploy"); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestJSONPrintsTheAPIData(t *testing.T) {
	h := newHarness(t, fakeAPI(t, map[string]any{"/apps": apps}))
	if code := h.run("apps", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	var got struct{ Total int }
	if err := json.Unmarshal(h.stdout.Bytes(), &got); err != nil || got.Total != 2 {
		t.Errorf("not the API data: %v %s", err, h.stdout)
	}
}

func TestBackupsAndRunsShowTheAppsProjectAndEnvironment(t *testing.T) {
	server := fakeAPI(t, map[string]any{
		"/backups": map[string]any{"items": []map[string]any{
			{"id": 3, "app": "shop", "project": "webshop", "environment_type": "production", "namespace": "acme", "taken_at": nil},
		}, "total": 1},
		"/runs": map[string]any{"items": []map[string]any{
			{"id": 5, "workflow": "Deploy", "state": "error", "record": "shop", "project": "webshop", "environment_type": "staging", "namespace": "acme"},
		}, "total": 1},
	})
	for command, env := range map[string]string{"backups": "production", "runs": "staging"} {
		h := newHarness(t, server)
		if code := h.run(command, "--color", "always"); code != 0 {
			t.Fatalf("%s: exit %d: %s", command, code, h.stderr)
		}
		for _, want := range []string{"ENV", "PROJECT", "webshop"} {
			if !strings.Contains(h.stdout.String(), want) {
				t.Errorf("%s: missing %s:\n%s", command, want, h.stdout)
			}
		}
		if !strings.Contains(h.stdout.String(), output.Style("environment_type", env)) {
			t.Errorf("%s: %s is not coloured like the dashboard:\n%s", command, env, h.stdout)
		}
	}
}

func TestColumnsNoRowHasAreLeftOut(t *testing.T) {
	// An older platform sends no environment_type; server runs have none.
	server := fakeAPI(t, map[string]any{"/runs": map[string]any{"items": []map[string]any{
		{"id": 5, "workflow": "Server update", "state": "done", "record": "prod-1", "namespace": "acme"},
	}, "total": 1}})
	h := newHarness(t, server)
	if code := h.run("runs"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	for _, noise := range []string{"ENV", "PROJECT"} {
		if strings.Contains(h.stdout.String(), noise) {
			t.Errorf("an empty %s column is noise:\n%s", noise, h.stdout)
		}
	}
}
