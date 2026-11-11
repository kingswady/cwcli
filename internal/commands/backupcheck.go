package commands

import (
	"fmt"

	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
)

// checkBackup asks the server now whether a backup that waits for its report is
// done (the dashboard's Check status) and prints what it found. A script loops on
// its exit code: 0 once there is nothing to wait for, 4 while the server is still at
// work, 1 when the backup failed.
func (a *App) checkBackup(args []string) error {
	fs := a.newFlags("backups check")
	asJSON := fs.Bool("json", false, "print the API response as JSON")
	colorMode := fs.String("color", "auto", "auto (in a terminal, unless NO_COLOR is set), always or never")
	fs.Usage = func() {
		fmt.Fprint(a.stderr, "Usage: cw backups check <id> [flags]\n\n"+
			"A backup stays running after its steps until its server reports it done. This asks\n"+
			"the server now, as the dashboard's Check status does, and the backup keeps the answer.\n"+
			"One backup is checked at most every 20 seconds; sooner, it says it was checked moments ago.\n\n"+
			"Exit code: 0 done (or nothing to check), 4 the server is still at it, 1 the backup failed.\n\n"+
			"Flags:\n")
		fs.PrintDefaults()
	}
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return usagef("name one backup: cw backups check <id>")
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
	backups := resourceNamed("backups")
	id, err := a.resolveID(c, backups, positional[0], scope{}, out)
	if err != nil {
		return err
	}
	raw, err := c.Get("/backups/"+id+"/check", nil)
	if err != nil {
		return notOffered(c, "cw backups check", err)
	}
	var answer struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Backup  record `json:"backup"`
	}
	if err := platform.Decode(raw, &answer); err != nil {
		return err
	}
	if *asJSON {
		if err := output.WriteJSON(a.stdout, raw); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(a.stdout, "%s\n\n", answer.Message)
		if err := a.writeDetails(backups, answer.Backup, out); err != nil {
			return err
		}
	}
	return checkOutcome(answer.Status)
}

// checkOutcome is the exit a check ends with; its message is already printed.
func checkOutcome(status string) error {
	switch status {
	case "failed", "timeout":
		return errRunFailed
	case "waiting", "busy", "deferred", "checked_recently":
		return errStillRunning
	}
	return nil
}
