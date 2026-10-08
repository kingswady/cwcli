package commands

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kingswady/cwcli/internal/config"
	"github.com/kingswady/cwcli/internal/output"
	"github.com/kingswady/cwcli/internal/platform"
)

const tokenPrefix = "cwk_"

type whoamiData struct {
	Login      string `json:"login"`
	Name       string `json:"name"`
	Namespaces []struct {
		Name  string `json:"name"`
		Code  string `json:"code"`
		Level string `json:"level"`
	} `json:"namespaces"`
}

func (a *App) login(args []string) error {
	fs := a.newFlags("login")
	urlFlag := fs.String("url", "", "platform URL (default "+config.DefaultURL+")")
	withToken := fs.Bool("with-token", false, "read the token from standard input")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if a.configErr != nil {
		return a.configErr // before asking for a token there is nowhere to keep
	}
	base, source, err := a.platform(*urlFlag)
	if err != nil {
		return err
	}
	token, err := a.readToken(base+platformNote(source), *withToken)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		return usagef("that is not an API token — they start with %s (create one in My Settings → API Tokens)", tokenPrefix)
	}
	me, err := checkToken(&platform.Client{Base: base, Token: token, HTTP: a.httpClient, UserAgent: a.userAgent()})
	if err != nil {
		return err
	}
	where, err := a.secrets.Set(base, token)
	if err != nil {
		return fmt.Errorf("saving the token: %w", err)
	}
	cfg := a.loadConfig()
	cfg.URL = base
	cfg.Remember(base)
	if err := a.saveConfig(cfg); err != nil {
		return fmt.Errorf("saving the config: %w", err)
	}
	if me == nil {
		fmt.Fprintf(a.stdout, "Logged in to %s (this token may not read your identity).\n", base)
	} else {
		fmt.Fprintf(a.stdout, "Logged in to %s as %s (%s).\n", base, output.Clean(me.Name), output.Clean(me.Login))
	}
	fmt.Fprintf(a.stdout, "Token saved in %s.\n", where)
	return nil
}

// checkToken asks /whoami; a token without that function is still a valid token.
func checkToken(c *platform.Client) (*whoamiData, error) {
	raw, err := c.Get("/whoami", nil)
	var refused *platform.APIError
	if errors.As(err, &refused) && refused.Code == "function_not_allowed" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var me whoamiData
	if err := platform.Decode(raw, &me); err != nil {
		return nil, err
	}
	return &me, nil
}

// readToken asks for the token; target is the platform as the prompt names it.
func (a *App) readToken(target string, fromStdin bool) (string, error) {
	if fromStdin || a.readSecret == nil {
		raw, err := io.ReadAll(io.LimitReader(a.stdin, 4096))
		return strings.TrimSpace(string(raw)), err
	}
	// Say where the token will go before asking for it (--url picks another platform).
	fmt.Fprintf(a.stderr, "Logging in to %s\n", target)
	token, err := a.readSecret("Paste your API token (My Settings → API Tokens): ")
	return strings.TrimSpace(token), err
}

func (a *App) logout(args []string) error {
	fs := a.newFlags("logout")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	base, err := a.baseURL("")
	if err != nil {
		return err
	}
	if err := a.secrets.Delete(base); err != nil {
		return err
	}
	cfg := a.loadConfig()
	cfg.Forget(base)
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Logged out of %s. Revoke the token in My Settings → API Tokens if it may have leaked.\n", base)
	return nil
}

func (a *App) whoami(args []string) error {
	fs := a.newFlags("whoami")
	asJSON := fs.Bool("json", false, "print the API response as JSON")
	colorMode := fs.String("color", "auto", "auto (in a terminal, unless NO_COLOR is set), always or never")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	color, err := a.useColor(*colorMode, *asJSON)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	raw, err := c.Get("/whoami", nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return output.WriteJSON(a.stdout, raw)
	}
	var me whoamiData
	if err := platform.Decode(raw, &me); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s (%s) on %s\n", output.Clean(me.Name), output.Clean(me.Login), c.Base)
	if expires, ok := output.ParseTime(c.TokenExpires); ok {
		fmt.Fprintf(a.stdout, "Token expires %s (%s)\n", expires.Local().Format("2006-01-02 15:04"), output.Until(a.clock(), expires))
	}
	fmt.Fprintln(a.stdout)
	rows := make([][]string, 0, len(me.Namespaces))
	for _, ns := range me.Namespaces {
		rows = append(rows, []string{output.Clean(ns.Name), output.Clean(ns.Code), output.Clean(ns.Level)})
	}
	return output.WriteTable(a.stdout, output.Table{Color: color}.Headers([]string{"NAMESPACE", "CODE", "LEVEL"}), rows)
}

// use switches the saved platform to one logged in to before.
func (a *App) use(args []string) error {
	fs := a.newFlags("use")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	cfg := a.loadConfig()
	if len(positional) == 0 {
		current, _ := a.baseURL("")
		if len(cfg.Platforms) == 0 {
			fmt.Fprintf(a.stdout, "Using %s. Log in to another with: cw login --url <address>\n", current)
			return nil
		}
		for _, known := range cfg.Platforms {
			marker := "  "
			if known == current {
				marker = "* "
			}
			fmt.Fprintln(a.stdout, marker+known)
		}
		return nil
	}
	if len(positional) != 1 {
		return usagef("usage: cw use [<platform address>]")
	}
	base, err := normalizeURL(positional[0])
	if err != nil {
		return err
	}
	if _, err := a.secrets.Get(base); err != nil {
		return fmt.Errorf("not logged in to %s — run: cw login --url %s", base, base)
	}
	cfg.URL = base
	cfg.Remember(base)
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Now using %s.\n", base)
	return nil
}

// platformNote explains a URL the user did not type on this command line.
func platformNote(source string) string {
	switch source {
	case config.FromEnv:
		return " (from CW_URL)"
	case config.FromSaved:
		return " (your last platform — use --url for another)"
	}
	return ""
}
