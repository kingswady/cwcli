package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
// a day, says when a newer cw exists. Both go to stderr and never change the
// exit code.
func (a *App) afterCommand(name string, code int) {
	a.warnExpiry()
	if (code == 0 || code == exitAttention) && name != "update" && name != "version" {
		a.hintUpdate()
	}
}

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
}

func (a *App) updateStatePath() string { return filepath.Join(a.configDir, "update-check.json") }

// hintUpdate prints one line when a newer release exists: in a terminal only,
// never in CI, and without asking GitHub more than once a day.
func (a *App) hintUpdate() {
	if a.version == "dev" || a.configErr != nil || a.getenv("CI") != "" || a.getenv("CW_NO_UPDATE_CHECK") != "" ||
		!a.stdoutIsTerminal() {
		return
	}
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
	current := a.currentTag()
	if state.Latest != "" && selfupdate.Newer(state.Latest, current) {
		fmt.Fprintf(a.stderr, "\ncw %s is available (you have %s) — run: cw update\n", state.Latest, current)
	}
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
