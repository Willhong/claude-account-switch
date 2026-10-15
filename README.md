# cas — Claude & Codex Account Switch

Switch between several Claude Code or Codex CLI logins without repeatedly
signing in and out. Claude and Codex accounts live in separate slot tables, so
the existing Claude commands remain unchanged.

```
$ cas list
   SLOT  EMAIL                 PLAN  ACCESS TOKEN  REFRESH TOKEN  LABEL
*  1     me@work.com           max   in 7h 33m     in 27d 23h     work
   2     me@personal.com       pro   in 6h 02m     in 21d 4h      personal

$ cas switch 2
✓ Switched to slot 2 — me@personal.com

$ cas codex list
   SLOT  ACCOUNT               PLAN  METHOD   ACCESS TOKEN  LABEL
*  1     me@work.com           pro   chatgpt  in 7m 32s     work
   2     me@personal.com       plus  chatgpt  in 4m 18s     personal

$ cas codex switch personal
✓ Switched Codex to slot 2 — me@personal.com
```

macOS only: cas stores parked credentials in the login Keychain. Claude Code's
live credential stays in its Keychain item; Codex's live credential stays in
`CODEX_HOME/auth.json`.

> **Before you start.** Using several accounts to work around rate limits
> conflicts with Anthropic's terms. cas is for people who legitimately hold
> more than one account — a work seat and a personal subscription, say.

## Install

```sh
go install github.com/Willhong/claude-account-switch/cmd/cas@latest
```

or from a clone:

```sh
make install            # builds and installs to ~/.local/bin/cas
```

Then:

```sh
cas adopt               # register the login you already have as slot 1
cas login               # add a second account
cas limit               # see how much quota each one has left
cas daemon install      # keep every account's token refreshed in the background
```

For Codex CLI:

```sh
cas codex adopt                         # register the login Codex already uses
cas codex login --label personal        # sign in again and add another account
cas codex list
cas codex limit
cas codex refresh
cas codex switch work
cas codex daemon install                # keep parked Codex logins refreshed
```

`cas codex login` saves an unregistered current login before Codex replaces it.
API key and enterprise access-token login are also passed through:

```sh
printenv OPENAI_API_KEY | cas codex login --with-api-key --label api
printenv CODEX_ACCESS_TOKEN | cas codex login --with-access-token --label work
```

`make uninstall` removes the binary and the background agent.

## Commands

| Command | What it does |
| --- | --- |
| `cas list` | every registered account with its slot number, email and token expiry |
| `cas login` | run the OAuth flow for another account and register it in a new slot |
| `cas adopt` | register the account Claude Code is already signed in as |
| `cas switch <n>` | make a slot the account Claude Code uses |
| `cas current` | which account is active right now |
| `cas limit` | how much of each account's 5-hour, weekly and per-model quota is spent |
| `cas refresh` | rotate access tokens that are close to expiry |
| `cas clean` | remove accounts whose credentials cannot be revived |
| `cas sessions` | the running Claude Code sessions, how idle they are, and which account each holds |
| `cas reap` | close the idle sessions that keep cycling the credential in the background |
| `cas remove <n>` | forget a slot without touching the account |
| `cas label <n> <name>` | give a slot a short name |
| `cas daemon …` | `install` (`--reap` to close idle sessions too), `uninstall`, `status`, `log`, `run` |
| `cas doctor` | where cas reads and writes, and what looks wrong |
| `cas codex …` | manage a separate set of Codex CLI account slots |

`<n>` accepts a slot number, a label, or an email — `cas switch work` and
`cas switch me@personal.com` both work, as does any unambiguous substring.

Flags may appear anywhere: `cas remove 2 --yes` and `cas remove --yes 2` are
equivalent.

### Codex commands

| Command | What it does |
| --- | --- |
| `cas codex list` | every registered Codex account, login method and access-token expiry |
| `cas codex login` | run the official Codex login flow and register its result |
| `cas codex adopt` | register the account in `CODEX_HOME/auth.json` |
| `cas codex switch <n>` | replace the live Codex credential with a stored slot |
| `cas codex current` | show which Codex account is active |
| `cas codex limit` | show current rate-limit usage and reset times for every slot |
| `cas codex refresh` | refresh parked ChatGPT credentials near access-token expiry |
| `cas codex clean` | remove only credentials proven dead or missing |
| `cas codex sessions` | list standalone CLI, desktop app-server and BB provider processes |
| `cas codex reap` | close idle standalone CLI sessions; managed app-servers are always protected |
| `cas codex daemon …` | manage the separate Codex refresh/reap launchd agent |
| `cas codex doctor` | diagnose credential storage, active account, slots, processes and daemon |
| `cas codex remove <n>` | forget a slot without logging the live account out |
| `cas codex label <n> <name>` | give a slot a short name |

Codex support currently requires file-backed credentials. Set
`cli_auth_credentials_store = "file"` in `~/.codex/config.toml` when your Codex
installation is configured for Keychain-only or ephemeral storage. The active
credential remains in `CODEX_HOME/auth.json`, exactly where Codex writes it;
parked credentials are stored in macOS Keychain under `cas-codex-credentials`.
See the [official Codex authentication documentation](https://developers.openai.com/codex/auth/)
for the storage setting and supported login methods.

Codex refreshes the active login during use. Each `cas codex` command copies a
newly rotated live credential back into its slot. `cas codex refresh` starts an
isolated local app-server for each parked ChatGPT credential and asks Codex to
refresh it. The separate `cas codex daemon` can run that pass in the background.
Restart Codex sessions that were already running when you switched because they
still hold the previous account in memory.

`cas codex limit` starts a short-lived local Codex app-server for each parked
credential and reads the same rate-limit snapshot used by Codex `/status`. It
does not switch the live account while checking the slots.

BB sessions configured with `bb-account-pool` authenticate through BB's own
provider and ignore `CODEX_HOME/auth.json`. `cas codex switch` therefore does
not change an already running BB thread; switch that provider separately or
start a standalone Codex process after changing the slot.

`cas codex sessions` reports managed desktop and BB app-servers for visibility,
but `cas codex reap` never signals them, even with `--all` or `--pid`. Only
standalone Codex CLI processes can be selected. The unattended daemon also
requires at least a 15-minute idle threshold and only sends `SIGTERM`.

The Codex daemon is independent from the Claude daemon:

```sh
cas codex daemon install --interval 1800
cas codex daemon install --reap stale --reap-idle 2h
cas codex daemon status
cas codex daemon log
cas codex daemon uninstall
```

## How it works

Claude Code keeps its OAuth credential in a single macOS Keychain item —
service `Claude Code-credentials`, account your macOS username — holding
`{"claudeAiOauth":{accessToken, refreshToken, expiresAt, …}}`. It also records
who you are in the `oauthAccount` block of `~/.claude.json`.

cas keeps one Keychain item per registered account (service `cas-credentials`,
account `slot-N`) and a metadata index at `~/.cas/state.json`. **No token is
ever written to a plain file** — `state.json` holds only slot numbers, emails,
plan names and expiry timestamps, so `cas list` needs no Keychain access.

A switch does three things: install the slot's credential into the Claude Code
Keychain item, rewrite the `oauthAccount` block in `~/.claude.json`, and record
the new active slot. The config file is patched surgically — only that one key's
bytes change, so the other ~150 KB of your settings are left exactly as they
were. The first rewrite saves a copy at `~/.cas/backups/.claude.json.original`.

### Usage limits

`cas limit` reads `/api/oauth/usage` for every registered account — the same
source Claude Code's own `/usage` screen uses — and reports the buckets side by
side, so you can see which account has room before switching to it:

```
   SLOT  EMAIL                    5H             WK             FABLE
*  1     me@work.com               11% · 3h 25m   88% · 1d 12h   59% · 1d 12h
   2     me@personal.com          100% · 5m       57% · 2d 14h   34% · 2d 14h
```

`5H` is the rolling session window, `WK` the weekly total, and any further
column is a per-model weekly budget the API reports (`Fable` today). The
columns are built from the response, so a bucket Anthropic adds later shows up
without a cas release. Each cell is the percentage spent and the time until
that window rolls over, coloured by the severity the API itself assigns.

`--wide` prints one block per account with usage bars and wall-clock reset
times; `--json` emits a stable shape for scripting. Pass slot numbers, labels
or emails to report on just those accounts.

### Keeping tokens alive

Claude Code access tokens last about eight hours; refresh tokens about thirty
days. A parked account would go stale, so cas refreshes on two schedules:

- **On every command.** Any slot within 45 minutes of expiry is rotated before
  the command reports anything. `--no-refresh` skips it; `CAS_REFRESH_THRESHOLD`
  changes the window.
- **In the background.** `cas daemon install` registers a launchd agent that
  runs `cas refresh` every 30 minutes (`--interval` to change it), logging to
  `~/.cas/daemon.log`. If your login Keychain is locked when it fires, the run
  fails and is logged; the next `cas` command picks the work back up.

#### Reaping from the agent

The agent can also close idle sessions each run, which is off unless you ask
for it:

```sh
cas daemon install --reap stale               # idle sessions on the previous account
cas daemon install --reap idle                # every idle session
cas daemon install --reap stale --reap-idle 4h
```

`stale` is the mode worth having: it closes only the sessions that both sat
idle past the threshold *and* started before the last account switch — exactly
the ones that can undo it. `idle` closes any idle session, whichever account it
holds. `cas daemon status` reports which mode is installed.

Unattended reaping is deliberately more cautious than the interactive command,
because nothing is there to answer a prompt and, running under launchd, the
agent has no terminal or parent session of its own to recognise:

- the idle threshold is the only protection left, so it must be at least 15
  minutes — `--reap-idle 5m` is refused at install time;
- a session whose idle time could not be measured is never closed, so a
  detached session with no transcript is always left alone;
- only `SIGTERM` is ever sent. A session that ignores it is logged and left
  running rather than killed;
- in `stale` mode, if cas has never recorded a switch there is nothing to judge
  staleness against, so the run closes nothing and says so in the log.

The reap happens before the refresh, so a session holding a superseded
credential is gone before any token is rotated and cannot race the write.

cas also watches for drift in the other direction. Claude Code rotates its own
token, so the live credential is often newer than cas's copy; each command
notices and folds it back into the active slot. If the live token turns out to
belong to a different account — you ran `claude /login` yourself, or a session
that predates a switch wrote its own token back — cas confirms the owner with
Anthropic's `/api/oauth/validate` endpoint (which does not rotate anything)
before touching any slot, and re-points the active marker instead of corrupting
one.

### Sessions that fight over the credential

A running Claude Code session reads the credential once, at startup, and then
keeps it alive itself: when its access token nears expiry it spends the refresh
token and writes the rotated pair back over the Keychain item. That is fine for
the session you are working in. It is not fine for one you left open in a
terminal tab yesterday — that session still holds whichever account was active
when it started, so its next refresh quietly reinstalls that account, and the
refresh token cas holds for it is invalidated in the same breath. The symptom
is auth breaking for no visible reason some time after a switch.

`cas sessions` finds them:

```
$ cas sessions
PID    TTY      IDLE  UPTIME  CREDENTIAL  DIRECTORY
67268  ttys004  46m   1d      stale       ~/workspace/api
5180   ttys007  14m   45m     current     ~/work/site
88905  ttys003  0s    8m      current     ~/workspace/cas       this session

cas switched to slot 2 (me@personal.com) 20m ago.
warning: 1 session(s) started before the last switch: each still holds the
previous account and can reinstall it on its next token refresh.
```

`IDLE` is how long the session has gone without activity, measured the way `w`
measures it: the mtime of its terminal, which moves on both keystrokes and
output. A session with no terminal — an SDK or subagent run — has no such
signal, so cas falls back to the mtime of the transcripts under its working
directory, and shows `?` when even that is unavailable. `CREDENTIAL` is `stale`
when the process started before the last account switch.

`cas reap` closes them. By default it takes the sessions idle for two hours or
more (`--idle`, `CAS_REAP_IDLE`), asks for confirmation, and sends `SIGTERM` so
Claude Code can save its transcript on the way out:

```sh
cas reap                      # everything idle for 2h+
cas reap --idle 30m           # a tighter window
cas reap --stale              # only the ones holding the previous account
cas reap --stale --idle 0     # …however recently they were used
cas reap --pid 67268          # one specific session
cas reap --dry-run            # show the selection and stop
cas reap --force              # SIGKILL instead of SIGTERM
```

Two sessions are never candidates, with or without `--all`: the one cas is
running inside (found by walking cas's own parent chain, so `cas reap` is safe
to run from inside Claude Code) and any session sharing this terminal.

The background agent can do the same sweep on its own — see below.

### `cas clean`

A slot is removed only when its credential is genuinely unrecoverable: the
refresh token has expired, the Keychain item is gone, or Anthropic rejects the
refresh token with `invalid_grant`. A slot that is merely stale gets renewed,
and an unreachable API is never treated as proof that an account is dead.
`--dry-run` reports without removing.

## Notes and limits

- **Running sessions do not notice a switch.** Claude Code caches the credential
  in memory at startup, so restart any open session after switching. `cas switch`
  says how many are still on the previous account; `cas sessions` names them and
  `cas reap` closes the ones nobody is using.
- **`cas login` speaks the OAuth flow directly**, using the same public client id
  and endpoints Claude Code itself uses, so it never disturbs the account you are
  currently signed in as. If the browser round-trip is blocked, `cas login
  --manual` prints the URL and takes the code by paste.
- **These endpoints are not a published API.** If Anthropic moves them, override
  with `CAS_OAUTH_TOKEN_URL`, `CAS_OAUTH_AUTHORIZE_URL`, `CAS_OAUTH_CLIENT_ID`
  and `CAS_API_BASE` rather than waiting for a release.

## Environment

| Variable | Meaning |
| --- | --- |
| `CAS_HOME` | where cas keeps its state (default `~/.cas`) |
| `CAS_REFRESH_THRESHOLD` | how close to expiry a token is refreshed (default `45m`) |
| `CAS_REAP_IDLE` | how long untouched a session must be for `cas reap` (default `2h`) |
| `CAS_KEYCHAIN_SERVICE` | the Claude Code Keychain item to target |
| `CAS_KEYCHAIN_ACCOUNT` | the Keychain account name (default: your macOS username) |
| `CLAUDE_CONFIG_DIR` | honoured exactly as Claude Code honours it |
| `CODEX_HOME` | where Codex reads `auth.json` (default `~/.codex`) |
| `CAS_CODEX_BIN` | exact Codex executable used by refresh and daemon runs |
| `CAS_CODEX_REFRESH_THRESHOLD` | how close to expiry a parked Codex token is refreshed (default `45m`) |
| `CAS_CODEX_REAP_IDLE` | how long a standalone Codex CLI session must be idle for reap (default `2h`) |
| `NO_COLOR` | disable colour output |

If you use `CLAUDE_CONFIG_DIR`, Claude Code appends a hash suffix to its
Keychain service name. `cas doctor` lists every `Claude Code-credentials*` item
it can see so you know which one to point `CAS_KEYCHAIN_SERVICE` at.

## Development

```sh
make check     # gofmt, go vet, go test
make build     # ./bin/cas
```

The tests cover the parts that would be expensive to get wrong: the surgical
`~/.claude.json` patch, credential round-tripping (including fields a future
Claude Code might add), slot numbering, the OAuth request shapes, and the
process-table parsing and self-detection behind `cas reap`. Nothing in the test
suite touches your real Keychain, makes a network call, or signals a process.

## Licence

MIT. cas is an independent project and is not affiliated with or endorsed by
Anthropic.
