package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kingswady/cwcli/internal/config"
	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/selfupdate"
)

const (
	expiryWarning     = 7 * 24 * time.Hour
	updateCheckPeriod = 24 * time.Hour
)

// afterCommand warns about a token that is about to expire and, at most once
// a day, offers a newer cw. Both go to stderr and never change the exit code.
func (a *App) afterCommand(name string, code int) {
	a.warnExpiry()
	if (code == 0 || code == exitAttention) && !noUpdateOffer[name] {
		a.offerUpdate()
	}
}

// noUpdateOffer: commands after which cw never asks — they are about
// updating already, or another program reads their output.
var noUpdateOffer = map[string]bool{"update": true, "version": true, "--version": true, "mcp": true, "completion": true}

func (a *App) warnExpiry() {
	if a.current == nil {
		return
	}
	expires, ok := output.ParseTime(a.current.TokenExpires)
	if !ok || expires.Sub(a.clock()) > expiryWarning {
		return
	}
	fmt.Fprintf(a.stderr, "\nThis API token expires %s — create a new one in My Settings → API Tokens.\n", output.Until(a.clock(), expires))
}

type updateState struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
	// OfferedAt and Offered: when cw last told about a release, and which,
	// so it asks once a day and not after every command.
	OfferedAt time.Time `json:"offered_at"`
	Offered   string    `json:"offered"`
}

func (a *App) updateStatePath() string { return filepath.Join(a.configDir, "update-check.json") }

// offerUpdate tells about a newer release once a day: in a terminal only,
// never in CI, without asking GitHub more than once a day. Where cw can
// update itself and someone is there to answer, it asks; otherwise it says
// what to run.
func (a *App) offerUpdate() {
	if a.version == "dev" || a.configErr != nil || a.getenv("CI") != "" || a.getenv("CW_NO_UPDATE_CHECK") != "" ||
		!a.stdoutIsTerminal() {
		return
	}
	state := a.refreshedUpdateState()
	current := a.currentTag()
	if state.Latest == "" || !selfupdate.Newer(state.Latest, current) {
		return
	}
	if state.Offered == state.Latest && a.clock().Sub(state.OfferedAt) < updateCheckPeriod {
		return
	}
	state.Offered, state.OfferedAt = state.Latest, a.clock()
	_ = config.WritePrivateJSON(a.updateStatePath(), state)
	if a.executable != nil && homebrewOwns(a.executable) {
		fmt.Fprintf(a.stderr, "\ncw %s is available (you have %s) — run: brew upgrade cw\n", state.Latest, current)
		return
	}
	if !a.stdinIsTerminal() {
		fmt.Fprintf(a.stderr, "\ncw %s is available (you have %s) — run: cw update\n", state.Latest, current)
		return
	}
	fmt.Fprintf(a.stderr, "\ncw %s is available (you have %s). Update now? [Y/n] ", state.Latest, current)
	if !a.answersYes() {
		fmt.Fprintln(a.stderr, "Not now — run cw update when you want it.")
		return
	}
	if err := a.updateTo(state.Latest); err != nil {
		fmt.Fprintln(a.stderr, "cw:", output.Clean(err.Error()), "— run cw update to try again")
	}
}

// refreshedUpdateState is what cw knows about the latest release, asking
// GitHub when the last answer is a day old.
func (a *App) refreshedUpdateState() updateState {
	var state updateState
	if raw, err := os.ReadFile(a.updateStatePath()); err == nil {
		_ = json.Unmarshal(raw, &state)
	}
	if a.clock().Sub(state.CheckedAt) >= updateCheckPeriod {
		state.CheckedAt = a.clock()
		if latest, err := selfupdate.LatestTagWith(quickClient, a.releases()); err == nil {
			state.Latest = latest
		}
		_ = config.WritePrivateJSON(a.updateStatePath(), state)
	}
	return state
}

// answersYes reads one line: empty, y or yes is yes; anything else, or
// nothing to read, is no.
func (a *App) answersYes() bool {
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(a.stderr)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

func homebrewOwns(executable func() (string, error)) bool {
	target, err := executable()
	return err == nil && installedByHomebrew(target)
}

// installedByHomebrew: Homebrew owns this file (a formula's Cellar, a cask's
// Caskroom) and updates it itself.
func installedByHomebrew(path string) bool {
	return strings.Contains(path, "/Cellar/") || strings.Contains(path, "/Caskroom/")
}

// currentTag is this build's version as a release tag, however the build stamped it.
func (a *App) currentTag() string { return selfupdate.Tag(a.version) }

// quickClient bounds the daily check: a slow network must not slow a command down.
var quickClient = &http.Client{
	Timeout: 3 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (a *App) releases() string {
	if base := a.getenv("CW_DOWNLOAD_BASE"); base != "" {
		return base
	}
	return selfupdate.DefaultReleases
}
