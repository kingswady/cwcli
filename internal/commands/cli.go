// Package commands is the cw command line: each command, and the dispatch,
// flags, exit codes and hints around them. It uses the layers below it —
// platform, config, output, mcpbridge, selfupdate — and nothing uses it but cmd/cw.
package commands

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"golang.org/x/term"

	"github.com/kingswady/cwcli/internal/config"
	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
	"github.com/kingswady/cwcli/internal/selfupdate"
)

// App holds everything a command touches, so tests can swap each piece.
type App struct {
	// version is this build's (set by the release; "dev" otherwise).
	version   string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	getenv    func(string) string
	configDir string
	// configErr says why there is no configDir: nothing is kept then, never
	// in the current directory, and commands that must keep something fail with it.
	configErr  error
	secrets    config.SecretStore
	httpClient *http.Client
	// readSecret prompts for the token without echo; nil when stdin is not a terminal.
	readSecret func(prompt string) (string, error)
	// interrupt and wait pace --follow; tests replace them.
	interrupt func() (context.Context, func())
	wait      func(ctx context.Context, d time.Duration) bool
	// stdoutIsTerminal decides whether logs are coloured by default.
	stdoutIsTerminal func() bool
	// stdinIsTerminal tells a person running cw mcp by hand from an AI client.
	stdinIsTerminal func() bool
	// executable is the file cw update replaces.
	executable func() (string, error)
	// current is the client the command used, for its token's expiry.
	current *platform.Client
	// now is swapped in tests.
	clock func() time.Time
}

// New is cw as the terminal runs it: this machine's config, keychain and streams.
func New(version string) *App {
	dir, dirErr := configDir()
	fallback := config.FileStore{}
	if dirErr == nil {
		fallback.Path = filepath.Join(dir, "credentials.json")
	}
	a := &App{
		version:    version,
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		getenv:     os.Getenv,
		configDir:  dir,
		configErr:  dirErr,
		secrets:    config.KeychainStore{Fallback: fallback},
		httpClient: platform.NewHTTPClient(),
		interrupt:  signalContext,
		wait:       sleepOrDone,
		stdoutIsTerminal: func() bool {
			return term.IsTerminal(int(os.Stdout.Fd()))
		},
		stdinIsTerminal: func() bool {
			return term.IsTerminal(int(os.Stdin.Fd()))
		},
		executable: selfupdate.ExecutablePath,
		clock:      time.Now,
	}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		a.readSecret = func(prompt string) (string, error) { return readHidden(fd, a.stderr, prompt) }
	}
	return a
}

// exitInterrupted is the exit code of a command stopped by Ctrl-C (128 + SIGINT).
const exitInterrupted = 130

// readHidden reads a line from the terminal without echo. The default Ctrl-C
// would end cw with echo still off; this one restores the terminal first.
func readHidden(fd int, stderr io.Writer, prompt string) (string, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	stop := onInterrupt(func() {
		_ = term.Restore(fd, state)
		fmt.Fprintln(stderr)
		os.Exit(exitInterrupted)
	})
	defer stop()
	fmt.Fprint(stderr, prompt)
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(stderr)
	return string(raw), err
}

// onInterrupt runs fn on Ctrl-C instead of the default handler, until stop.
func onInterrupt(fn func()) (stop func()) {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(signals, os.Interrupt)
	go func() {
		select {
		case <-signals:
			fn()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}

// configDir is cw's directory in the user's config directory ($XDG_CONFIG_HOME
// or ~/.config, ~/Library/Application Support, %AppData%), else in ~/.config —
// never a relative one, which would put the token in the current directory.
func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err == nil && filepath.IsAbs(base) {
		return filepath.Join(base, "cw"), nil
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && filepath.IsAbs(home) {
		return filepath.Join(home, ".config", "cw"), nil
	}
	if err == nil {
		err = fmt.Errorf("%q is not an absolute path", base)
	}
	return "", fmt.Errorf("cw has nowhere private to keep its config and token (%v) — set HOME", err)
}

func (a *App) userAgent() string { return "cw/" + a.version }

func (a *App) loadConfig() config.Config { return config.Load(a.configDir) }

func (a *App) saveConfig(cfg config.Config) error {
	if a.configErr != nil {
		return a.configErr
	}
	return config.Save(a.configDir, cfg)
}

// platform is the platform to talk to and where that came from; a URL the
// user got wrong is a command-line mistake.
func (a *App) platform(explicit string) (string, string, error) {
	base, source, err := config.Resolve(explicit, a.getenv, a.configDir)
	if err != nil {
		return "", "", usagef("%v", err)
	}
	return base, source, nil
}

func (a *App) baseURL(explicit string) (string, error) {
	base, _, err := a.platform(explicit)
	return base, err
}

// normalizeURL is platform.NormalizeURL with a wrong URL as a command-line mistake.
func normalizeURL(raw string) (string, error) {
	base, err := platform.NormalizeURL(raw)
	if err != nil {
		return "", usagef("%v", err)
	}
	return base, nil
}

// client builds an authenticated client from CW_TOKEN or the saved token.
func (a *App) client() (*platform.Client, error) {
	base, err := a.baseURL("")
	if err != nil {
		return nil, err
	}
	token := a.getenv("CW_TOKEN")
	if token == "" {
		if token, err = a.secrets.Get(base); err != nil {
			return nil, fmt.Errorf("not logged in to %s — run: cw login", base)
		}
	}
	a.current = &platform.Client{Base: base, Token: token, HTTP: a.httpClient, UserAgent: a.userAgent()}
	return a.current, nil
}

// usageError is a mistake on the command line: exit code 2, not 1.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// Run runs one command line and returns its exit code.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		a.usage(a.stderr)
		return 2
	}
	name, rest := args[0], args[1:]
	code := a.dispatch(name, rest)
	a.afterCommand(name, code)
	return code
}

func (a *App) dispatch(name string, rest []string) int {
	switch name {
	case "help", "-h", "--help":
		a.usage(a.stdout)
		return 0
	case "version", "--version":
		fmt.Fprintln(a.stdout, "cw", a.version)
		return 0
	case "login":
		return a.exit(a.login(rest))
	case "logout":
		return a.exit(a.logout(rest))
	case "whoami":
		return a.exit(a.whoami(rest))
	case "logs":
		return a.exit(a.logs(rest))
	case "attention":
		return a.exit(a.attention(rest))
	case "mcp":
		return a.exit(a.mcp(rest))
	case "update":
		return a.exit(a.update(rest))
	case "use":
		return a.exit(a.use(rest))
	}
	if res := resourceNamed(name); res != nil {
		return a.exit(a.resource(res, rest))
	}
	fmt.Fprintf(a.stderr, "cw: unknown command %q\n\n", name)
	a.usage(a.stderr)
	return 2
}

func (a *App) exit(err error) int {
	var usage *usageError
	var refused *platform.APIError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errNeedsAttention):
		return exitAttention
	case errors.As(err, &usage), errors.As(err, &refused) && refused.Status == http.StatusBadRequest:
		// A 400 is the platform saying the command line was wrong (--limit 0).
		fmt.Fprintln(a.stderr, "cw:", output.Clean(err.Error()))
		return 2
	default:
		// An error may carry the platform's own message: Clean, like all it says.
		fmt.Fprintln(a.stderr, "cw:", output.Clean(err.Error()))
		return 1
	}
}

func (a *App) usage(w io.Writer) {
	fmt.Fprint(w, `cw reads your apps, servers, backups and runs from the terminal.

Usage: cw <command> [flags]

Account
  login        Save an API token (create one in My Settings → API Tokens)
  logout       Forget the saved token
  whoami       Show who the token acts as, and its namespaces
  use          Switch between platforms you logged in to
  update       Update cw to the latest release (--check only looks)

Read
  attention    What needs attention now   exit code 3 when something does
  apps         List apps                  cw apps show <id|name>
  servers      List servers               cw servers show <id|name>
  installers   List installed services    cw installers show <id|name>
  backups      List backups               cw backups show <id>
  runs         List recent runs           cw runs show <id>
  logs         An app's Odoo log          cw logs <app> [--since 1h] [--grep …] [--follow]

AI agents
  mcp          Serve the platform's tools to an AI client (stdio): claude mcp add -s user cloudwady -- cw mcp

Flags on every read command
  --json              Print the API response as JSON
  --namespace <code>  Only this namespace of the token
  --limit <n>         Rows per page (1-200, default 50)
  --offset <n>        Rows to skip

Names
  An app, server or installer is an id or a name. A name several share is narrowed
  with the list's filters: cw apps show v19-0 --project internal --env production
  --app (backups, runs) and cw logs <app> take --project, --env, --server, --version
  and --edition for that; they narrow only which app is meant (runs --state is the runs').

Environment
  CW_URL     Platform URL (default `+config.DefaultURL+`)
  CW_TOKEN   API token; overrides the saved one (for CI)

Run "cw <command> --help" for a command's own flags.
`)
}

// newFlags is a flag set that reports to stderr and returns errors instead of exiting.
func (a *App) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("cw "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	return fs
}

// parseArgs lets flags follow positional arguments ("cw apps show shop --json"),
// which the flag package alone stops at.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// useColor decides --color: auto colours a terminal unless NO_COLOR is set
// (https://no-color.org); JSON is never coloured.
func (a *App) useColor(mode string, asJSON bool) (bool, error) {
	switch mode {
	case "always":
		return !asJSON, nil
	case "never":
		return false, nil
	case "auto":
		return !asJSON && a.getenv("NO_COLOR") == "" && a.stdoutIsTerminal(), nil
	}
	return false, usagef("--color is auto, always or never, not %q", mode)
}

// signalContext ends on Ctrl-C.
func signalContext() (context.Context, func()) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// sleepOrDone waits d; false when ctx ended first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
