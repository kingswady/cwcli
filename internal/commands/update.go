package commands

import (
	"fmt"
	"strings"

	"github.com/kingswady/cwcli/internal/selfupdate"
)

func (a *App) update(args []string) error {
	fs := a.newFlags("update")
	check := fs.Bool("check", false, "only say whether a newer release exists")
	want := fs.String("version", "", "install this release (e.g. v0.3.0) instead of the latest")
	force := fs.Bool("force", false, "update even a development build, or reinstall the same version")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if a.version == "dev" && !*force {
		return usagef("this is a development build — use --force to replace it with a release")
	}
	if installed, err := a.executable(); err == nil && installedByHomebrew(installed) {
		return fmt.Errorf("cw was installed with Homebrew — update it with: brew upgrade cw")
	}
	tag := *want
	if tag != "" {
		tag = selfupdate.Tag(tag) // --version 0.3.0 is v0.3.0
	} else {
		var err error
		if tag, err = selfupdate.LatestTag(strings.TrimRight(a.releases(), "/")); err != nil {
			return err
		}
	}
	current := a.currentTag()
	if !*force && !selfupdate.Newer(tag, current) {
		fmt.Fprintf(a.stdout, "cw is up to date (%s).\n", current)
		return nil
	}
	if *check {
		fmt.Fprintf(a.stdout, "cw %s is available (this is %s) — run: cw update\n", tag, current)
		return nil
	}
	return a.updateTo(tag)
}

// updateTo replaces this cw with release tag, its archive checked against the
// release's signed checksums.
func (a *App) updateTo(tag string) error {
	target, err := a.executable()
	if err != nil {
		return fmt.Errorf("cannot find this program's file: %w", err)
	}
	binary, err := selfupdate.Fetch(strings.TrimRight(a.releases(), "/"), tag)
	if err != nil {
		return err
	}
	if err := selfupdate.ReplaceExecutable(target, binary); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Updated cw %s → %s (%s)\n", a.currentTag(), tag, target)
	return nil
}
