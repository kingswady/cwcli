# cw — the platform from your terminal

`cw` reads your projects, apps, servers, installed services, backups, restore points and runs through the
platform's customer API, and backs an app up, restarts it or retries a failed run when you ask it to (see
[Changes](#changes)). Nothing else it does changes anything.

## Install

macOS and Linux:

```bash
curl -fsSL https://www.cloudwady.com/install.sh | sh
curl -fsSL https://www.cloudwady.com/install.sh | sh -s -- --version v1.0.0 --dir "$HOME/bin"
```

The script downloads the release for your machine, checks its SHA-256 against the release's `checksums.txt`,
and installs `cw` into `/usr/local/bin` if you can write there, `~/.local/bin` otherwise. It never uses sudo.
Your platform serves the same script at its own address, and then tells you the matching `cw login --url`.

Homebrew (macOS):

```bash
brew install kingswady/tap/cw
```

Windows (PowerShell):

```powershell
irm https://www.cloudwady.com/install.ps1 | iex
```

It checks the download against `checksums.txt` too, installs `cw.exe` into `%LOCALAPPDATA%\Programs\cw` and puts
that folder on your PATH (open a new terminal afterwards); `$env:CW_VERSION = "v1.0.0"` before it picks a
release. By hand: download `cw_windows_amd64.zip` from the [releases](https://github.com/kingswady/cwcli/releases).

From source (Go 1.26+, or Docker): `make build` → `bin/cw`; `make docker-dist` builds every platform. How the code is laid out:
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Update

```bash
cw update            # to the latest release, checked and swapped in place
cw update --check    # only look
```

When a newer release is out, cw says so after a command, at most once a day, and in a terminal asks
`Update now? [Y/n]`. Never in CI or when its output goes to another program; `CW_NO_UPDATE_CHECK=1` turns it
off. Installed with Homebrew, it tells you to run `brew upgrade cw` instead.

Every release's `checksums.txt` is signed. `cw update` installs a release only when that signature verifies
against the release key built into cw, and the archive matches its checksum, so a release file replaced on
the way, or on the release page, is refused.

**From cw 0.7.0 or older:** those versions look for releases at the repository's old address and cannot find
them. Update once by hand, then `cw update` works again:

```bash
CW_DOWNLOAD_BASE=https://github.com/kingswady/cwcli/releases cw update
```

## Tab completion

```bash
source <(cw completion bash)                           # in ~/.bashrc
source <(cw completion zsh)                            # in ~/.zshrc, after compinit
cw completion fish > ~/.config/fish/completions/cw.fish
```

Tab completes commands, `show`, each command's flags and the values some flags take (`--env`, `--edition`,
`--source`, `--ssl`, `--color`). It never asks the platform, so it answers at once, also offline.

## Log in

Create a token in the dashboard under **My Settings → API Tokens**: choose its namespaces and what it may
read. The token is shown once.

```bash
cw login                                   # paste the token; it is not echoed
cw login --url https://your.platform.host  # a platform other than www.cloudwady.com
echo "$TOKEN" | cw login --with-token      # from a script
cw whoami
cw namespaces                              # the codes --namespace takes, and your level in each
```

A later `cw login` goes back to the platform you last logged in to, and says so; `--url` picks another.
`cw use` lists the platforms you logged in to and `cw use <address>` switches between them — each keeps its own
token. `cw whoami` shows when the token expires, and every command warns in its last week.

The token is kept in the system keychain (macOS Keychain, Windows Credential Manager, the Secret Service on
Linux). Where there is none, it goes to a file only you can read, next to `cw`'s config. `cw logout` forgets
it; revoke it in the dashboard if it may have leaked.

## Read

```bash
cw projects                      # with their environments and how many apps run there
cw projects show acme
cw apps                          # the token's apps (deleted and disabled ones only with --all)
cw apps --search shop            # name contains "shop", any case
cw apps --env production --version 19.0 --edition enterprise
cw apps --server prod-1 --project acme --show-url
cw apps show shop                # by name or id: every URL, every repository at its commit
cw urls --ssl expiring_soon      # apps' URLs: certificate and the outside-in check
cw servers
cw installers --server prod-1
cw backups --app shop            # OFFSITE: copies on external storage, of those made
cw backups show 9482             # on the server or not, verified, each off-site copy
cw runs --app shop --state error
cw runs show 4812                # steps, and why a failed step failed
```

`cw apps show` lists each URL with its certificate (TLS, when it expires) and the platform's last check from
outside (CHECK: passed, or failed with its reason), and each repository with its branch, the commit the server
runs (DEPLOYED), the branch's newest one (HEAD) and whether they are the same. An app being upgraded to a new
Odoo version says so, and which app the upgrade made.

A run that failed but will be retried automatically shows as `retrying`, not `error`: it has not failed yet.

## Restore points

```bash
cw restore-points shop           # newest first
cw restore-points shop --json
```

What each change to the app did — the commits it moved, how many modules its update migrated, the backup taken
before it — and the way back. OUTCOME `failed, kept as it was` is an update that failed and left the app as it
was; UNDO `instant until …` means the database from before the change is still held, so undoing it is a rename,
not a restore. Roll back from the dashboard (cw does not roll back).

### Names several records share

An app, server or installer can be named by id or by name. When several share a name, cw lists them and asks
for one — or narrow the name with the same filters the list takes:

```bash
cw apps show v19-0 --project internal --env production
cw servers show prod-1 --namespace acme
cw installers show grafana --server prod-1
```

Commands that name an app — `--app` on `backups`, `runs` and `urls`, `cw logs <app>` and
`cw restore-points <app>` — take `--project`, `--env`,
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
cw logs shop --source upgrade        # the log of its version upgrade's stages
cw logs shop --source main,setup     # several logs at once, each line named by its log
```

The app's logs, from the platform's log store — only that app's lines, secrets masked. `--source` picks which:
`main` (the Odoo log, the default), `setup`, `restore`, `backup`, `transfer`, `upgrade`, `script`, or `all`. `--grep`
matches text, any case. The token needs **Logs** in what it can read.

## Changes

```bash
cw backup shop                   # back the app up now (--format dump: the database only)
cw restart shop                  # restart it
cw runs retry 4812               # run a failed run again, from the step that failed
cw apps update shop              # latest code: pulled, rebuilt, the modules the pull changed updated
cw apps update shop --quick      # pull and restart on the same image (--modules none: no module update)
cw apps update-modules shop sale stock  # odoo -u of those modules, the code as it is
cw maintenance shop on           # the maintenance page for its users, until: cw maintenance shop off
cw restart shop --wait           # …and follow the run until it ends
```

Each asks first, naming the app, its project and environment: `Restart shop (Shop / staging)? [y/N]`. A
production app is confirmed by typing its name. From a script — no terminal to ask in — pass `--yes`, or cw
refuses (exit 2).

- **Backup** does what the dashboard's Backup does. When a backup of the app already runs, cw says so and
  starts nothing; `--wait` then follows that one.
- **Restart** is refused while an update, a backup or a restore of the app runs: it never cancels them.
- **Retry** is refused while an automatic retry is due in a few minutes (`--now` retries anyway), or when a
  newer run of the same record exists.
- **`cw apps update`**, **`cw apps update-modules`** and **`cw maintenance`** are refused while something runs on the app, as restart
  is, and while a version upgrade holds it. `--modules` picks what an update's module update covers:
  `changed` (the default, what the pull changed), `configured` (the app's own scope) or `none` (`--quick`
  only). A production app is backed up first, in the same queue — the dashboard's default; `--no-backup`
  skips it, `--backup` asks for it elsewhere (`--backup-format dump`: the database only). With `--wait`, cw
  follows the backup, then the change; a failed backup leaves the app unchanged. `cw update` is cw's own
  update only: `cw update shop` says to use `cw apps update shop`.

`--wait` prints each step as its state changes — and, for a long backup, what the watcher on the server
reports until it is done ("Watcher: Dumping the database") — and exits `0` when the run is done, `1` when it failed, `4`
when it is still going after `--timeout` (default `30m`); Ctrl-C stops waiting, not the run. Without
`--wait`, cw prints the run's id: `cw runs show <id>` follows it.

A change is sent once: cw gives each command one request key and reuses it when it has to ask again (a rate
limit, a lost connection), so the platform acts once however often the request arrives.

The token needs the function ticked in My Settings → API Tokens (they are listed under Changes, unticked by
default, and a token that can make changes lives at most 30 days). What a change may touch is your access
level in that environment, as in the dashboard. Platforms open changes through the API to their customers
separately from reading; until then cw says they are not open to your account.

## AI agents (MCP)

`cw mcp` serves the platform's tools to an AI client over stdio, with the token you logged in with:

```bash
claude mcp add -s user cloudwady -- cw mcp   # Claude Code, in every project
```

```json
{ "mcpServers": { "cloudwady": { "command": "cw", "args": ["mcp"] } } }
```

The agent gets the same functions as the CLI — projects, apps and their URLs, servers, installed services,
backups, restore points, runs, logs and what needs attention — limited to the token's namespaces and what it
may read, plus prompts such as `triage` and `why_failed`. A client that speaks HTTP can connect to
`https://www.cloudwady.com/mcp` directly, with the header `Authorization: Bearer cwk_…`.

**Changes (back up, restart, retry)** are offered only to clients that can show a confirmation form (MCP
elicitation), e.g. Claude Code, and only when the token holds them. Each one asks you in your client first —
"Restart shop (Shop / staging)?", and for a production app its name typed — and runs only once you said yes;
declining does nothing. The answer comes from that form, never from the agent. A client speaking MCP
2026-07-28 does the round trip itself; for an older (2025) client `cw mcp` puts the platform's question to it
as an `elicitation/create` request and hands the answer back.

## In CI

```bash
export CW_URL=https://www.cloudwady.com
export CW_TOKEN=cwk_…        # from your CI secret store
cw runs --app shop --state error --json
```

`CW_TOKEN` overrides the saved token; `CW_URL` the saved platform. A change from CI needs `--yes`
(`cw backup shop --yes --wait`). A command the platform does not offer yet
(it runs an older version) says so. A token made before a function existed does not hold it: edit the token in
My Settings → API Tokens and tick it. Exit codes: `0` success, `1` the platform
refused or failed (or a run followed with `--wait` failed), `2` a mistake on the command line, `3` `cw attention`
found something, `4` a run followed with `--wait` was still going at `--timeout`.

## License

Apache License 2.0 — see [LICENSE](LICENSE). The CloudWady name and logo are not covered by it.
