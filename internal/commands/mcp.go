package commands

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kingswady/cwcli/internal/mcpbridge"
)

// mcp serves the platform's MCP tools to a local AI client over stdio: each
// line on stdin is one JSON-RPC message, forwarded to <platform>/mcp with the
// saved token; each answer goes back as one line on stdout.
func (a *App) mcp(args []string) error {
	fs := a.newFlags("mcp")
	fs.Usage = func() {
		fmt.Fprint(a.stderr, `Usage: cw mcp

Serves the platform's tools to an AI client over stdio, with the token "cw login"
saved (or CW_TOKEN). Add it to your client:

  Claude Code   claude mcp add -s user cloudwady -- cw mcp
  Cursor, …     {"mcpServers": {"cloudwady": {"command": "cw", "args": ["mcp"]}}}
  Codex         [mcp_servers.cloudwady]
                command = "cw"
                args = ["mcp"]

A client that speaks HTTP can also connect to <platform>/mcp directly, with the
header "Authorization: Bearer <token>".
`)
	}
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		fs.Usage()
		return usagef("unexpected arguments: %s", strings.Join(positional, " "))
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	if a.stdinIsTerminal() {
		// A person ran it: say what it is instead of waiting in silence. stderr only —
		// stdout carries the protocol.
		fmt.Fprintf(a.stderr, "cw mcp serves %s's tools to an AI client and is waiting for one on stdin.\n"+
			"Add it to Claude Code: claude mcp add -s user cloudwady -- cw mcp   (other clients: cw mcp --help)\n"+
			"Ctrl-D to quit.\n", c.Base)
	}
	b := &mcpbridge.Bridge{
		Endpoint:  c.Base + "/mcp",
		Token:     c.Token,
		HTTP:      &http.Client{Timeout: 2 * time.Minute, CheckRedirect: a.httpClient.CheckRedirect},
		UserAgent: a.userAgent(),
		Out:       a.stdout,
		Log:       a.stderr,
	}
	return b.Serve(a.stdin)
}
