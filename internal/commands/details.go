package commands

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kingswady/cwcli/internal/output"
)

// What a detail view shows beyond its fields, and the values derived from a
// whole record. Each reads only keys the record has: an older platform's answer
// lacks the newer ones, and then the view is what it was.

// runState is a run's state, "retrying" while a failed step is about to be
// retried: it has not failed yet.
func runState(r record) any {
	if retrying, _ := r["retrying"].(bool); retrying {
		return "retrying"
	}
	return r["state"]
}

// unlessListed is the field's own value, or nothing once the record carries
// list (the app's every URL replaces its primary one).
func unlessListed(list string) func(record) any {
	return func(r record) any {
		if _, ok := r[list]; ok {
			return nil
		}
		return r["url"]
	}
}

// upgradeText is an app's open version upgrade, in one line.
func upgradeText(r record) any {
	upgrade, ok := r["upgrade"].(record)
	if !ok {
		return nil
	}
	text := fmt.Sprintf("%s → %s, %s", output.Text(upgrade["from_version"]), output.Text(upgrade["to_version"]),
		strings.ReplaceAll(output.Text(upgrade["state"]), "_", " "))
	if purpose := output.Text(upgrade["purpose"]); purpose == "test" {
		text += " (test run)"
	}
	if output.Text(upgrade["new_app_id"]) == output.Text(r["id"]) {
		return text + " — upgraded from " + appRef(upgrade, "source_app")
	}
	if output.Text(upgrade["new_app_id"]) != "-" {
		return text + " — new app " + appRef(upgrade, "new_app")
	}
	return text
}

// appRef is "shop-18 (2)": an app's name and the id commands take.
func appRef(r record, key string) string {
	return fmt.Sprintf("%s (%s)", output.Text(r[key]), output.Text(r[key+"_id"]))
}

// records is a JSON array of objects as records; anything else is none.
func records(value any) []record {
	list, _ := value.([]any)
	items := make([]record, 0, len(list))
	for _, item := range list {
		if r, ok := item.(record); ok {
			items = append(items, r)
		}
	}
	return items
}

// printTable prints a titled table of items, or nothing when there are none.
func (a *App) printTable(title string, columns []field, items []record, out output.Table) error {
	if len(items) == 0 {
		return nil
	}
	fmt.Fprintln(a.stdout, "\n"+out.Label(title))
	return writeRecords(a.stdout, columns, items, out)
}

var (
	tlsField     = field{key: "ssl_status", title: "TLS"}
	expiresField = field{key: "ssl_expires_at", title: "EXPIRES", format: output.Expires}
	checkField   = field{key: "public_check", title: "CHECK", derive: publicCheck}
)

// publicCheck is the last outside-in check of a URL: passed, failed, or not
// checked — with the platform's reason when it has one.
func publicCheck(r record) any {
	state, code := output.Text(r["public_check"]), output.Text(r["public_check_code"])
	if state == "-" {
		if code == "-" {
			return nil
		}
		state = "not checked"
	}
	if code != "-" && state != "passed" {
		return state + " (" + code + ")"
	}
	return state
}

var appURLColumns = []field{{key: "url", title: "URL"}, {key: "primary", title: "PRIMARY"}, tlsField, expiresField, checkField}

var repositoryColumns = []field{
	{key: "repository", title: "REPOSITORY"}, {key: "branch", title: "BRANCH"}, {key: "main", title: "MAIN"},
	{key: "deployed_sha", title: "DEPLOYED", format: shortSHA}, {key: "head_sha", title: "HEAD", format: shortSHA},
	{key: "up_to_date", title: "STATUS", derive: branchStatus},
	{key: "deployed_message", title: "DEPLOYED COMMIT", format: output.FirstLine, hideEmpty: true},
}

// shortSHA is a commit as the dashboard shows it: its first 8 characters.
func shortSHA(value any) string {
	sha := output.Text(value)
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// branchStatus says whether the server runs the branch's newest commit.
func branchStatus(r record) any {
	if deleted, _ := r["deleted_upstream"].(bool); deleted {
		return "deleted upstream"
	}
	switch r["up_to_date"] {
	case true:
		return "up to date"
	case false:
		return "behind"
	}
	return "unknown"
}

// printAppExtras lists an app's every URL, and the repositories it runs at
// which commit.
func printAppExtras(a *App, app record, out output.Table) error {
	if err := a.printTable("URLs:", appURLColumns, records(app["urls"]), out); err != nil {
		return err
	}
	return a.printTable("Repositories:", repositoryColumns, records(app["repositories"]), out)
}

// offsiteCount is "1/2": a backup's copies on external storage, of those made.
func offsiteCount(value any) string {
	copies := records(value)
	if len(copies) == 0 {
		return "-"
	}
	stored := 0
	for _, c := range copies {
		if on, _ := c["on_storage"].(bool); on {
			stored++
		}
	}
	return fmt.Sprintf("%d/%d", stored, len(copies))
}

var offsiteColumns = []field{
	{key: "storage", title: "STORAGE"}, {key: "type", title: "TYPE"}, {key: "on_storage", title: "ON STORAGE"},
	{key: "state", title: "STATE"}, {key: "uploaded_at", title: "UPLOADED", format: output.LocalTime},
	{key: "error", title: "ERROR", format: output.FirstLine, hideEmpty: true},
}

func printOffsiteCopies(a *App, backup record, out output.Table) error {
	return a.printTable("Off-site copies:", offsiteColumns, records(backup["offsite"]), out)
}

// environmentTypes is a project's environments by tier: "production, staging".
func environmentTypes(value any) string {
	var types []string
	for _, env := range records(value) {
		types = append(types, output.Text(env["type"]))
	}
	if len(types) == 0 {
		return "-"
	}
	return strings.Join(types, ", ")
}

// appTotal is the apps across a project's environments.
func appTotal(value any) string {
	var total int64
	for _, env := range records(value) {
		if n, ok := env["app_count"].(json.Number); ok {
			count, _ := n.Int64()
			total += count
		}
	}
	return fmt.Sprint(total)
}

var environmentColumns = []field{
	{key: "id", title: "ID"}, {key: "name", title: "NAME"},
	// Keyed like the other tables' environment, so it is coloured like them.
	{key: "environment_type", title: "TYPE", derive: func(r record) any { return r["type"] }},
	{key: "app_count", title: "APPS"},
}

func printEnvironments(a *App, project record, out output.Table) error {
	return a.printTable("Environments:", environmentColumns, records(project["environments"]), out)
}
