# Architecture

`cw` is one binary (`cmd/cw`) over layered packages in `internal/`. Code only calls **downward**:
`cmd/cw/layers_test.go` fails the build's tests when an import breaks that.

```
cmd/cw                    entry point: builds commands.App with the version, runs it, exits
└── internal/commands     the command line: dispatch, flags, exit codes, hints — and each command
    ├── mcpbridge         cw mcp: stdio ↔ <platform>/mcp, one JSON-RPC message per line
    ├── selfupdate        cw update: the latest release, checked against checksums.txt, swapped in atomically
    ├── config            the platforms logged in to, the one in use, one token each (keychain or 0600 file)
    ├── output            tables aligned by visible width, colours, JSON, times
    └── platform          the /api/v1 client, and the rule for where a token may be sent
```

| Package      | Owns                                                                                     | May import                          |
| ------------ | ---------------------------------------------------------------------------------------- | ----------------------------------- |
| `commands`   | every command (`account`, `resources` + `details`, `restorepoints`, `logs`, `attention`, `completion`, `update`, `mcp`) and `cli.go` | all of the below |
| `mcpbridge`  | `Bridge`: forwards an AI client's messages with the MCP headers the platform checks       | —                                   |
| `selfupdate` | `LatestTag`, `Fetch`, `ReplaceExecutable`, `Newer`                                         | `platform`                          |
| `config`     | `Config`, `Resolve` (flag → `CW_URL` → saved → default), `SecretStore`                     | `platform`                          |
| `output`     | `Clean`, `Text`, `LogLine`, `Table`, `WriteTable`, `Colorize`, `Style`, `Ago`, `LocalTime` | —                                   |
| `platform`   | `Client.Get`/`GetContext`, `APIError`, `NormalizeURL` (never plain http off this machine), no redirects | —                     |

## Where things go

- **A read command over an API function** — a `resource` entry in `commands/resources.go` (list, `show`,
  filters, columns). Anything else gets its own file in `commands/`. Commands reach the platform only through
  `platform.Client`.
- **What a detail view shows beyond its fields** (an app's URLs and repositories, a backup's off-site copies, a
  project's environments) — the resource's `extra`, in `commands/details.go`. A value computed from the whole
  record (a run that is `retrying`, a URL's check with its reason) is a field's `derive`; a field marked
  `hideEmpty` leaves its column out of a table no row fills, and its row out of a detail view — how a newer
  field stays out of sight against an older platform. A resource the API has no single read for is `noShow`.
- **A command the platform may not have yet** — wrap its error in `notOffered`: an older platform's "No API
  function at …" becomes "does not offer cw … yet".
- **Tab completion** — `commands/completion.go`. The scripts ask `cw __complete <words>`, which reads a
  command's flags by running it with `-h` on a quiet copy of the app (`flagsDefined`): a command must define
  its flags before it does anything else. Fixed flag values live in `flagValues`. It never calls the platform.
- **A name lookup** — `resolveID` in `commands/resources.go`, the one place a name becomes an id. It asks the
  list with the lookup's `scope` (namespace, `--all`, the filters given) and, where the resource has a
  `nameSearch` parameter (apps: `search`), only for the names containing it; it keeps the exact matches, any
  case. A filter with a `narrows` text is also offered by the commands that name such a record (`--app`,
  `cw logs <app>`) — give it one only where the flag cannot collide with that command's own filters.
- **A column** — the resource's column list; its colour, keyed by the API field, in `output/color.go`.
- **Anything printed** goes through `output`; **anything kept on the machine** through `config`, whose
  `WritePrivateJSON` writes a synced 0600 temp file and renames it into cw's 0700 directory — never a relative
  path: without a config directory cw keeps nothing rather than use the current one.

## Conventions

- **Exit codes:** `0` success, `1` the platform refused or failed, `2` a mistake on the command line (`usagef`,
  or an HTTP 400), `3` `cw attention` found something.
- **A token goes only where the user sent it:** https (or this machine), no redirects, never on the command line.
- **Colour only in a terminal**, unless `--color always`; `NO_COLOR` is honoured; JSON is never coloured.
- **What the platform says is `output.Clean` before it is printed** — no control characters but tab and
  newline; `Text` (every cell), `LogLine` and error output do it, and cw's own colours come after.
- **Tests sit beside the code.** Command tests run the real command line against a fake API (`newHarness`,
  `fakeAPI` in `commands/cli_test.go`); lower layers are tested directly.
- **Release:** push a tag `vX.Y.Z`. GoReleaser builds `./cmd/cw` and stamps `-X main.version`; `cw update`
  and the install script rely on the version-less archive names in `.goreleaser.yaml`.
