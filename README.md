# cw — the platform from your terminal

`cw` reads your apps, servers, installed services, backups and runs through the platform's customer API. It is
read-only: nothing you run with it can change, deploy or delete anything.

## Install

macOS and Linux:

```bash
curl -fsSL https://www.cloudwady.com/install.sh | sh
curl -fsSL https://www.cloudwady.com/install.sh | sh -s -- --version v1.0.0 --dir "$HOME/bin"
```

The script downloads the release for your machine, checks its SHA-256 against the release's `checksums.txt`,
and installs `cw` into `/usr/local/bin` if you can write there, `~/.local/bin` otherwise. It never uses sudo.
Your platform serves the same script at its own address, and then tells you the matching `cw login --url`.

Windows: download `cw_windows_amd64.zip` from the [releases](https://github.com/kingswady/cwcli/releases).

From source (Go 1.26+, or Docker): `make build` → `bin/cw`; `make docker-dist` builds every platform. How the code is laid out:
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Update

```bash
cw update            # to the latest release, checked and swapped in place
cw update --check    # only look
```

In a terminal, cw mentions a newer release at most once a day (never in CI; `CW_NO_UPDATE_CHECK=1` turns it off).

## Log in

Create a token in the dashboard under **My Settings → API Tokens**: choose its namespaces and what it may
read. The token is shown once.

```bash
cw login                                   # paste the token; it is not echoed
cw login --url https://your.platform.host  # a platform other than www.cloudwady.com
echo "$TOKEN" | cw login --with-token      # from a script
cw whoami
```

A later `cw login` goes back to the platform you last logged in to, and says so; `--url` picks another.
`cw use` lists the platforms you logged in to and `cw use <address>` switches between them — each keeps its own
token. `cw whoami` shows when the token expires, and every command warns in its last week.

The token is kept in the system keychain (macOS Keychain, Windows Credential Manager, the Secret Service on
Linux). Where there is none, it goes to a file only you can read, next to `cw`'s config. `cw logout` forgets
it; revoke it in the dashboard if it may have leaked.

## Read

```bash
cw apps                          # the token's apps (deleted and disabled ones only with --all)
cw apps --search shop            # name contains "shop", any case
cw apps --env production --version 19.0 --edition enterprise
cw apps --server prod-1 --project acme --show-url
cw apps show shop                # by name or id
cw servers
cw installers --server prod-1
cw backups --app shop
cw runs --app shop --state error
cw runs show 4812                # steps, and why a failed step failed
```

### Names several records share

An app, server or installer can be named by id or by name. When several share a name, cw lists them and asks
for one — or narrow the name with the same filters the list takes:

```bash
cw apps show v19-0 --project internal --env production
cw servers show prod-1 --namespace acme
cw installers show grafana --server prod-1
```

Commands that name an app — `--app` on `backups` and `runs`, and `cw logs <app>` — take `--project`, `--env`,
`--server` (id or name), `--version` and `--edition` to narrow which app the name means:

```bash
cw backups --app v19-0 --project internal
cw runs --app v19-0 --env production --state error
cw logs v19-0 --project internal --follow
```

They only pick the app; they never filter the backups or runs themselves, and `--state` stays the command's own
(the runs' state, not the app's). `--namespace` narrows both. An id is taken as given. If nothing matches, cw
says which filters it applied; if several still do, it lists them and the flags left to narrow with.

Every read command takes `--json` (the API's own response, for `jq`), `--namespace <code>`, `--limit`,
`--offset` and `--color auto|always|never` (coloured in a terminal unless `NO_COLOR` is set). `apps`, `servers`
and `installers` leave deleted and disabled records out unless `--all`. A token never sees more than its namespaces and your own access, whatever you ask for.

## What needs attention

```bash
cw attention                    # the dashboard's "Needs attention", with the apps, URLs and runs behind it
cw attention --namespace acme --json
```

Apps in error, production backups critical, missing or stale, TLS certificates expired or expiring, and runs
that failed in the last day — the same rules and counts as the dashboard, so the two never disagree. It exits
`3` when something needs attention and `0` when nothing does, so a script or a cron job can act on it:

```bash
cw attention > report.txt || mail -s "CloudWady needs attention" ops@example.com < report.txt
```

The token needs **Attention** in what it can read; add it to an existing token by editing the token.

## Logs

```bash
cw logs shop                         # the newest 200 lines of the last hour
cw logs shop --since 15m --grep ERROR
cw logs shop --limit 2000            # up to 2 000 lines, up to a day back
cw logs shop --follow                # keep printing new lines (Ctrl-C to stop)
cw logs shop --source restore        # the log of its last restore
cw logs shop --source main,setup     # several logs at once, each line named by its log
```

The app's logs, from the platform's log store — only that app's lines, secrets masked. `--source` picks which:
`main` (the Odoo log, the default), `setup`, `restore`, `backup`, `transfer`, `script`, or `all`. `--grep`
matches text, any case. The token needs **Logs** in what it can read.

## AI agents (MCP)

`cw mcp` serves the platform's tools to an AI client over stdio, with the token you logged in with:

```bash
claude mcp add -s user cloudwady -- cw mcp   # Claude Code, in every project
```

```json
{ "mcpServers": { "cloudwady": { "command": "cw", "args": ["mcp"] } } }
```

The agent gets the same read-only functions as the CLI — apps, servers, installed services, backups, runs, logs
and what needs attention — limited to the token's namespaces and what it may read, plus prompts such as
`triage` and `why_failed`. A client that speaks HTTP can connect to `https://www.cloudwady.com/mcp` directly,
with the header `Authorization: Bearer cwk_…`.

## In CI

```bash
export CW_URL=https://www.cloudwady.com
export CW_TOKEN=cwk_…        # from your CI secret store
cw runs --app shop --state error --json
```

`CW_TOKEN` overrides the saved token; `CW_URL` the saved platform. Exit codes: `0` success, `1` the platform
refused or failed, `2` a mistake on the command line, `3` `cw attention` found something.

## License

Apache License 2.0 — see [LICENSE](LICENSE). The CloudWady name and logo are not covered by it.
