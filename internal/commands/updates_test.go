package commands

import (
	"flag"
	"reflect"
	"strings"
	"testing"
)

// updatesAPI answers the two apps' lookups and the change at path with reply.
func updatesAPI(t *testing.T, path string, reply answer, more map[string][]answer) (*writeAPI, func(bool, string) *harness) {
	t.Helper()
	answers := map[string][]answer{
		"GET /apps":    {appsList},
		"GET /apps/7":  {ok(200, stagingApp)},
		"GET /apps/8":  {ok(200, productionApp)},
		"POST " + path: {reply},
	}
	for key, list := range more {
		answers[key] = list
	}
	server, api := newWriteAPI(t, answers)
	return api, func(terminal bool, typed string) *harness { return writeHarness(t, server, terminal, typed) }
}

var started = ok(202, map[string]any{"state": "started", "run_id": 41977, "app_id": 7})

func TestMaintenanceAsksAndSaysOnOrOff(t *testing.T) {
	api, harnessFor := updatesAPI(t, "/apps/7/maintenance", started, nil)
	h := harnessFor(true, "y\n")
	if code := h.run("maintenance", "shop", "on"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	question := "Put shop in maintenance (Shop / staging)? Its users see the maintenance page until you take it out. [y/N]"
	if !strings.Contains(h.stderr.String(), question) {
		t.Errorf("asked %q", h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "Maintenance of shop started: run 41977.") {
		t.Errorf("stdout %q", h.stdout)
	}
	h = harnessFor(false, "")
	if code := h.run("maintenance", "shop", "off", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if len(api.posts) != 2 || api.posts[0].body["on"] != true || api.posts[1].body["on"] != false {
		t.Errorf("posts %+v", api.posts)
	}
	for _, args := range [][]string{{"maintenance", "shop"}, {"maintenance", "shop", "maybe", "--yes"}} {
		if code := harnessFor(false, "").run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if len(api.posts) != 2 {
		t.Errorf("a mistake sent something: %+v", api.posts)
	}
}

func TestAProductionMaintenanceIsConfirmedByItsName(t *testing.T) {
	api, harnessFor := updatesAPI(t, "/apps/8/maintenance", started, nil)
	h := harnessFor(true, "shop\n")
	if code := h.run("maintenance", "erp", "on"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	want := "Put erp in maintenance (ERP / production) — a production app. Its users see the maintenance page " +
		"until you take it out. Type erp to go on:"
	if !strings.Contains(h.stderr.String(), want) || len(api.posts) != 0 {
		t.Errorf("asked %q, posts %+v", h.stderr, api.posts)
	}
}

func TestUpdateSendsTheBackupOnlyWhenAFlagSaysSo(t *testing.T) {
	cases := []struct {
		args  []string
		typed string
		ask   string
		body  map[string]any
	}{
		{[]string{"apps", "update", "shop"}, "y\n",
			"Update shop: pull the latest code, rebuild and update the modules the pull changed (Shop / staging)? [y/N]",
			map[string]any{"quick": false, "modules": "changed"}},
		{[]string{"apps", "update", "shop", "--backup", "--backup-format", "dump"}, "y\n",
			"Update shop: pull the latest code, rebuild and update the modules the pull changed, after a backup (Shop / staging)?",
			map[string]any{"quick": false, "modules": "changed", "backup": true, "backup_format": "dump"}},
		{[]string{"apps", "update", "erp"}, "erp\n",
			"Update erp: pull the latest code, rebuild and update the modules the pull changed, after a backup (ERP / production) — a production app.",
			map[string]any{"quick": false, "modules": "changed"}},
		{[]string{"apps", "update", "erp", "--no-backup", "--modules", "configured"}, "erp\n",
			"Update erp: pull the latest code, rebuild and update its configured modules (ERP / production) — a production app.",
			map[string]any{"quick": false, "modules": "configured", "backup": false}},
		{[]string{"apps", "update", "shop", "--quick", "--modules", "none"}, "y\n",
			"Quick-update shop: pull the latest code and restart, without a module update (Shop / staging)? [y/N]",
			map[string]any{"quick": true, "modules": "none"}},
		{[]string{"apps", "update", "shop", "--quick"}, "y\n",
			"Quick-update shop: pull the latest code and restart, updating the modules the pull changed (Shop / staging)?",
			map[string]any{"quick": true, "modules": "changed"}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			path := "/apps/7/update"
			if c.args[2] == "erp" {
				path = "/apps/8/update"
			}
			api, harnessFor := updatesAPI(t, path, started, nil)
			h := harnessFor(true, c.typed)
			if code := h.run(c.args...); code != 0 {
				t.Fatalf("exit %d: %s", code, h.stderr)
			}
			if !strings.Contains(h.stderr.String(), c.ask) {
				t.Errorf("asked %q, want %q", h.stderr, c.ask)
			}
			if len(api.posts) != 1 || !reflect.DeepEqual(api.posts[0].body, c.body) {
				t.Errorf("posts %+v, want body %v", api.posts, c.body)
			}
		})
	}
}

func TestUpdateFlagMistakesAreRefusedBeforeAnything(t *testing.T) {
	api, harnessFor := updatesAPI(t, "/apps/7/update", started, nil)
	for _, args := range [][]string{
		{"apps", "update", "shop", "--modules", "none", "--yes"},
		{"apps", "update", "shop", "--backup", "--no-backup", "--yes"},
		{"apps", "update", "shop", "--modules", "everything", "--yes"},
		{"apps", "update", "shop", "--backup-format", "tar", "--yes"},
		{"apps", "update", "shop", "erp", "--yes"},
		{"apps", "update-modules", "shop", "Sale", "--yes"},
		{"apps", "update-modules", "shop", "--yes"},
	} {
		h := harnessFor(false, "")
		if code := h.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, h.stderr)
		}
	}
	if len(api.posts) != 0 {
		t.Errorf("a mistake sent something: %+v", api.posts)
	}
	h := harnessFor(false, "")
	h.run("apps", "update-modules", "shop", "sale", "Sale", "--yes")
	if !strings.Contains(h.stderr.String(), `"Sale" is not a module name`) {
		t.Errorf("stderr %q", h.stderr)
	}
}

func TestUpdateModulesNamesTheModules(t *testing.T) {
	api, harnessFor := updatesAPI(t, "/apps/7/update-modules", started, nil)
	h := harnessFor(true, "y\n")
	if code := h.run("apps", "update-modules", "shop", "sale,stock", "purchase", "sale"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "Update the modules sale, stock, purchase of shop (Shop / staging)? [y/N]") {
		t.Errorf("asked %q", h.stderr)
	}
	if len(api.posts) != 1 || !reflect.DeepEqual(api.posts[0].body, map[string]any{"modules": "sale,stock,purchase"}) {
		t.Errorf("posts %+v", api.posts)
	}
	if !strings.Contains(h.stdout.String(), "Module update of shop started: run 41977.") {
		t.Errorf("stdout %q", h.stdout)
	}
}

var backedUp = ok(202, map[string]any{
	"state": "started", "run_id": 41977, "app_id": 8, "backup_id": 9482, "backup_run_id": 41976,
})

func TestWaitFollowsTheBackupThenTheChange(t *testing.T) {
	_, harnessFor := updatesAPI(t, "/apps/8/update", backedUp, map[string][]answer{
		"GET /runs/41976": {run("done", false, step("Backup", "successful", ""))},
		"GET /runs/41977": {run("done", false, step("Update Odoo", "successful", ""))},
	})
	h := harnessFor(false, "")
	if code := h.run("apps", "update", "erp", "--yes", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
	want := strings.Join([]string{
		"Update of erp started: run 41977. A backup goes first: run 41976 (backup 9482).",
		"Backup first: run 41976",
		"  1. Backup  successful",
		"Run 41976 done.",
		"Then run 41977:",
		"  1. Update Odoo  successful",
		"Run 41977 done.",
	}, "\n")
	if !strings.Contains(h.stdout.String(), want) {
		t.Errorf("want\n%s\nin\n%s", want, h.stdout)
	}
}

func TestAFailedBackupLeavesTheChangeUnstarted(t *testing.T) {
	api, harnessFor := updatesAPI(t, "/apps/8/update-modules", backedUp, map[string][]answer{
		"GET /runs/41976": {run("error", false, step("Backup", "failed", "no space left on device"))},
		"GET /runs/41977": {run("draft", false, step("Update modules", "draft", ""))},
	})
	h := harnessFor(false, "")
	if code := h.run("apps", "update-modules", "erp", "sale", "--yes", "--wait"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stdout)
	}
	if !strings.Contains(h.stdout.String(), "Backup: no space left on device") ||
		!strings.Contains(h.stdout.String(), "The backup failed, so run 41977 did not start: the app was not changed.") {
		t.Errorf("stdout %q", h.stdout)
	}
	if api.served["GET /runs/41977"] != 0 {
		t.Errorf("followed the change the backup held back")
	}
}

func TestAnOlderPlatformDoesNotOfferUpdates(t *testing.T) {
	for _, args := range [][]string{
		{"apps", "update", "shop", "--yes"},
		{"apps", "update-modules", "shop", "sale", "--yes"},
		{"maintenance", "shop", "on", "--yes"},
	} {
		// Nothing answers the POST: the platform's 404 "No API function at …".
		_, harnessFor := updatesAPI(t, "/apps/7/elsewhere", started, nil)
		h := harnessFor(false, "")
		if code := h.run(args...); code != 1 {
			t.Errorf("%v: exit %d", args, code)
		}
		command := args[0]
		if command == "apps" {
			command += " " + args[1]
		}
		if want := "does not offer cw " + command + " yet"; !strings.Contains(h.stderr.String(), want) {
			t.Errorf("%v: stderr %q, want %q", args, h.stderr, want)
		}
	}
}

func TestNewRefusalsSayWhatToDo(t *testing.T) {
	_, harnessFor := updatesAPI(t, "/apps/7/update",
		refuse(409, "upgrade_standby", "The app waits for its version upgrade to switch.", ""), nil)
	h := harnessFor(false, "")
	if code := h.run("apps", "update", "shop", "--yes"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.stderr.String(), "finish or cancel it in the dashboard first") {
		t.Errorf("stderr %q", h.stderr)
	}
}

func TestCwUpdateIsCwsOwnAndSaysWhereAnAppsUpdateWent(t *testing.T) {
	for args, want := range map[string]bool{
		"":                 false,
		"--check":          false,
		"--version v0.3.0": false,
		"--force --check":  false,
		"shop":             true,
		"-- shop":          true,
	} {
		if got := namesSomething(strings.Fields(args)); got != want {
			t.Errorf("namesSomething(%q) = %v, want %v", args, got, want)
		}
	}
	for _, args := range [][]string{{"update", "shop"}, {"update", "--quick"}, {"update", "shop", "--modules", "none"}} {
		h := newHarness(t, nil)
		if code := h.run(args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
		if !strings.Contains(h.stderr.String(), "to update an app: cw apps update <app>") {
			t.Errorf("%v: stderr %q", args, h.stderr)
		}
	}
	h := newHarness(t, nil)
	if code := h.run("update", "--check"); code != 2 || !strings.Contains(h.stderr.String(), "development build") {
		t.Errorf("cw update --check went elsewhere: exit %d, %q", code, h.stderr)
	}
}

func TestUpdateValueFlagsAreAllKnown(t *testing.T) {
	h := newHarness(t, nil)
	fs := h.app.flagsOf("update")
	if fs == nil {
		t.Fatal("cw update: no flags")
	}
	fs.VisitAll(func(f *flag.Flag) {
		if takesValue := !isBoolFlag(f); takesValue != updateValueFlags[f.Name] {
			t.Errorf("update --%s: takes a value %v, updateValueFlags says %v", f.Name, takesValue, updateValueFlags[f.Name])
		}
	})
}

func TestAppsShowSaysMaintenanceWhenThePlatformDoes(t *testing.T) {
	inMaintenance := map[string]any{}
	for key, value := range stagingApp {
		inMaintenance[key] = value
	}
	inMaintenance["maintenance"] = true
	for _, c := range []struct {
		app  map[string]any
		want string
	}{{inMaintenance, "on"}, {stagingApp, ""}} {
		server, _ := newWriteAPI(t, map[string][]answer{"GET /apps": {appsList}, "GET /apps/7": {ok(200, c.app)}})
		h := newHarness(t, server)
		if code := h.run("apps", "show", "7"); code != 0 {
			t.Fatalf("exit %d: %s", code, h.stderr)
		}
		line := ""
		for _, l := range strings.Split(h.stdout.String(), "\n") {
			if strings.HasPrefix(l, "Maintenance:") {
				line = strings.TrimSpace(strings.TrimPrefix(l, "Maintenance:"))
			}
		}
		if line != c.want {
			t.Errorf("Maintenance line %q, want %q in\n%s", line, c.want, h.stdout)
		}
	}
}

func TestUpdateCommandsComplete(t *testing.T) {
	h := newHarness(t, nil)
	for words, want := range map[string][]string{
		"maintenance shop |":             {"on", "off"},
		"apps |":                         {"show", "update", "update-modules"},
		"apps update shop --qu|":         {"--quick"},
		"update --che|":                  {"--check"},
		"apps update shop --modules |":   {"changed", "configured", "none"},
		"apps update-modules shop --no|": {"--no-backup"},
		"upd|":                           {"update"},
	} {
		typed := strings.Fields(strings.TrimSuffix(words, "|"))
		if strings.HasSuffix(words, " |") {
			typed = append(typed, "")
		}
		if got := h.app.candidates(typed); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, want %v", words, got, want)
		}
	}
}
