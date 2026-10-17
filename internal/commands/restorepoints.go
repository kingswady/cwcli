package commands

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
)

// restorePointColumns: what each change did, and the way back from it.
var restorePointColumns = []field{
	{key: "id", title: "ID"}, {key: "at", title: "WHEN", format: output.LocalTime},
	{key: "action", title: "ACTION", derive: pointAction}, {key: "outcome", title: "OUTCOME", derive: pointOutcome},
	{key: "commits", title: "CODE", derive: movedCode}, {key: "modules", title: "MODULES", format: count, hideEmpty: true},
	{key: "backup_verified", title: "BACKUP", derive: pointBackup},
	{key: "instant_undo", title: "UNDO", derive: instantUndo, hideEmpty: true},
	{key: "held", title: "HELD", derive: heldHere, hideEmpty: true},
	{key: "run_id", title: "RUN", hideEmpty: true},
}

// pointAction is the change, and which point a rollback went back to.
func pointAction(r record) any {
	action := output.Text(r["action"])
	if of := output.Text(r["rollback_of_id"]); of != "-" {
		return action + " to #" + of
	}
	return action
}

// pointOutcome says "kept" the way it happened: the update failed and
// production stayed as it was.
func pointOutcome(r record) any {
	if r["outcome"] == "kept" {
		return "failed, kept as it was"
	}
	return r["outcome"]
}

// movedCode is each repository whose commit the change moved: "odoo-ep 972dba2b→4b425981".
func movedCode(r record) any {
	commits := records(r["commits"])
	if len(commits) == 0 {
		return nil
	}
	moves := make([]string, 0, 2)
	for _, c := range commits[:min(2, len(commits))] {
		moves = append(moves, fmt.Sprintf("%s %s→%s", output.Text(c["repository"]), shortSHA(c["before"]), shortSHA(c["after"])))
	}
	if more := len(commits) - len(moves); more > 0 {
		moves = append(moves, fmt.Sprintf("+%d more", more))
	}
	return strings.Join(moves, ", ")
}

// count is how many entries a JSON array has; none is "-".
func count(value any) string {
	list, _ := value.([]any)
	if len(list) == 0 {
		return "-"
	}
	return strconv.Itoa(len(list))
}

func pointBackup(r record) any {
	if output.Text(r["backup_id"]) == "-" {
		return nil
	}
	if verified, _ := r["backup_verified"].(bool); verified {
		return "verified"
	}
	return "not verified"
}

// instantUndo is how long the database from before the change is held, to
// undo it by a rename instead of a restore.
func instantUndo(r record) any {
	if undo, _ := r["instant_undo"].(bool); !undo {
		return nil
	}
	return "instant until " + output.LocalTime(r["kept_until"])
}

func heldHere(r record) any {
	if held, _ := r["held"].(bool); held {
		return "yes"
	}
	return nil
}

func (a *App) restorePoints(args []string) error {
	fs := a.newFlags("restore-points")
	asJSON := fs.Bool("json", false, "print the API response as JSON")
	namespace := fs.String("namespace", "", "look the app up in this namespace only")
	limit := fs.Int("limit", 50, "rows per page (1-200)")
	offset := fs.Int("offset", 0, "rows to skip")
	colorMode := fs.String("color", "auto", "auto (in a terminal, unless NO_COLOR is set), always or never")
	apps := resourceNamed("apps")
	narrow := narrowFlags(fs, apps, "<app>")
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "Usage: cw restore-points <app> [flags]\n\n"+
			"What each change to the app did — its commits, the modules it updated, the backup taken\n"+
			"before it — newest first. \"failed, kept as it was\": the update failed and the app stayed\n"+
			"as it was; \"instant\": the database from before the change is still held.\n\n"+
			"<app> is an id or a name. Where several apps share the name,\n"+
			"%s narrow which one it means.\n\nFlags:\n", joinFlags(narrowing(apps), "and"))
		fs.PrintDefaults()
	}
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one app: cw restore-points <id|name>")
	}
	color, err := a.useColor(*colorMode, *asJSON)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	out := output.Table{Color: color}
	id, err := a.resolveID(c, apps, positional[0], scope{
		namespace: *namespace, filters: given(narrow), offered: narrowing(apps),
	}, out)
	if err != nil {
		return err
	}
	query := url.Values{"limit": {strconv.Itoa(*limit)}, "offset": {strconv.Itoa(*offset)}}
	raw, err := c.Get("/apps/"+id+"/restore-points", query)
	if err != nil {
		return notOffered(c, "cw restore-points", err)
	}
	if *asJSON {
		return output.WriteJSON(a.stdout, raw)
	}
	var page struct {
		Items []record    `json:"items"`
		Total json.Number `json:"total"`
	}
	if err := platform.Decode(raw, &page); err != nil {
		return err
	}
	if len(page.Items) == 0 {
		fmt.Fprintln(a.stderr, "No restore points: none of this app's changes recorded one.")
		return nil
	}
	if err := writeRecords(a.stdout, restorePointColumns, page.Items, out); err != nil {
		return err
	}
	if total, _ := page.Total.Int64(); int(total) > *offset+len(page.Items) {
		fmt.Fprintf(a.stderr, "\nShowing %d-%d of %d — next page: --offset %d\n",
			*offset+1, *offset+len(page.Items), total, *offset+len(page.Items))
	}
	return nil
}
