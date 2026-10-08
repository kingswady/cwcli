package commands

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
)

// exitAttention is cw attention's exit code when something needs attention,
// so a script can act on it: `cw attention || notify-team`.
const exitAttention = 3

// errNeedsAttention says the findings are printed; a.exit turns it into exitAttention.
var errNeedsAttention = errors.New("something needs attention")

type attentionSection struct {
	Rule     string   `json:"rule"`
	Severity string   `json:"severity"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Count    int      `json:"count"`
	Items    []record `json:"items"`
}

// attentionColumns are the columns of each kind of section.
var attentionColumns = map[string][]field{
	"app": {
		{key: "id", title: "ID"}, {key: "name", title: "APP"}, projectField, {key: "environment_type", title: "ENV"},
		{key: "state", title: "STATE"}, {key: "backup_health", title: "BACKUPS"},
		{key: "last_backup_at", title: "LAST BACKUP", format: output.Ago}, {key: "server", title: "SERVER"},
		namespaceField,
	},
	"url": {
		{key: "id", title: "ID"}, {key: "url", title: "URL"}, {key: "ssl_status", title: "SSL"},
		{key: "ssl_expires_at", title: "EXPIRES", format: output.Ago}, {key: "owner", title: "FOR"}, namespaceField,
	},
	"failure": {
		{key: "run_id", title: "RUN"}, {key: "workflow", title: "WORKFLOW"}, {key: "step", title: "STEP"},
		{key: "record", title: "FOR"}, projectField, envField, {key: "failed_at", title: "FAILED", format: output.Ago},
		{key: "reason", title: "WHY", format: output.FirstLine}, namespaceField,
	},
}

// genericColumns are for a kind of section this cw does not know yet (a
// newer platform's rule): what every record has.
var genericColumns = []field{{key: "id", title: "ID"}, {key: "name", title: "NAME"}, namespaceField}

var severityColor = map[string]string{"danger": output.Red, "warning": output.Yellow}

func (a *App) attention(args []string) error {
	fs := a.newFlags("attention")
	asJSON := fs.Bool("json", false, "print the API response as JSON")
	namespace := fs.String("namespace", "", "only this namespace (code or id)")
	colorFlag := fs.String("color", "auto", "auto (in a terminal, unless NO_COLOR is set), always or never")
	fs.Usage = func() {
		fmt.Fprint(a.stderr, "Usage: cw attention [flags]\n\n"+
			"What needs attention now: the dashboard's Needs attention rules, with their records.\n"+
			"Exit code 3 when something does, 0 when nothing does.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if positional, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(positional) > 0 {
		fs.Usage()
		return usagef("unexpected arguments: %s", strings.Join(positional, " "))
	}
	color, err := a.useColor(*colorFlag, *asJSON)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	query := url.Values{}
	if *namespace != "" {
		query.Set("namespace", *namespace)
	}
	raw, err := c.Get("/attention", query)
	var refused *platform.APIError
	if errors.As(err, &refused) && refused.Code == "not_found" {
		return fmt.Errorf("%s does not answer cw attention yet — it needs a newer platform version", c.Base)
	}
	if err != nil {
		return err
	}
	var result struct {
		Sections []attentionSection `json:"sections"`
	}
	if err := platform.Decode(raw, &result); err != nil {
		return err
	}
	if *asJSON {
		err = output.WriteJSON(a.stdout, raw)
	} else {
		err = a.printAttention(result.Sections, output.Table{Color: color})
	}
	if err == nil && len(result.Sections) > 0 {
		return errNeedsAttention
	}
	return err
}

func (a *App) printAttention(sections []attentionSection, out output.Table) error {
	if len(sections) == 0 {
		msg := "Nothing needs attention."
		if out.Color {
			msg = output.Paint(output.Green, msg)
		}
		fmt.Fprintln(a.stdout, msg)
		return nil
	}
	failures := false
	for i, section := range sections {
		if i > 0 {
			fmt.Fprintln(a.stdout)
		}
		title := fmt.Sprintf("%-8s%d %s", strings.ToUpper(output.Clean(section.Severity)), section.Count, output.Clean(section.Label))
		if out.Color {
			title = output.Paint(output.Bold+severityColor[section.Severity], title)
		}
		fmt.Fprintln(a.stdout, title)
		if err := a.printSection(section, out); err != nil {
			return err
		}
		failures = failures || section.Kind == "failure"
	}
	if failures {
		fmt.Fprintln(a.stdout, "\n"+out.Label("Why a run failed, step by step: cw runs show <RUN>"))
	}
	return nil
}

func (a *App) printSection(section attentionSection, out output.Table) error {
	if len(section.Items) == 0 {
		return nil
	}
	columns, known := attentionColumns[section.Kind]
	if !known {
		columns = genericColumns
	}
	var table bytes.Buffer
	if err := writeRecords(&table, columns, section.Items, out); err != nil {
		return err
	}
	for _, line := range strings.SplitAfter(strings.TrimSuffix(table.String(), "\n"), "\n") {
		fmt.Fprint(a.stdout, "  "+line)
	}
	fmt.Fprintln(a.stdout)
	if more := section.Count - len(section.Items); more > 0 {
		fmt.Fprintln(a.stdout, "  "+out.Label(fmt.Sprintf("… and %d more", more)))
	}
	return nil
}
