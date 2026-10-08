// Command cw reads your apps, servers, installers, backups and runs from the
// platform's customer API (/api/v1) with a token minted in the dashboard, and
// serves them to AI clients over MCP (cw mcp).
package main

import (
	"os"

	"github.com/kingswady/cwcli/internal/commands"
)

// version is set at build time: -ldflags "-X main.version=v1.0.0".
var version = "dev"

func main() {
	os.Exit(commands.New(version).Run(os.Args[1:]))
}
