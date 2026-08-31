# cas — Claude Account Switch

Switch between several Claude Code logins without signing in and out, and keep
every parked account's token alive so a switch never lands on a dead session.

```
$ cas list
   SLOT  EMAIL                 PLAN  ACCESS TOKEN  REFRESH TOKEN  LABEL
*  1     me@work.com           max   in 7h 33m     in 27d 23h     work
   2     me@personal.com       pro   in 6h 02m     in 21d 4h      personal

$ cas switch 2
✓ Switched to slot 2 — me@personal.com
```

macOS only: cas stores credentials in the login Keychain, exactly where Claude
Code does.

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
| `cas remove <n>` | forget a slot without touching the account |
| `cas label <n> <name>` | give a slot a short name |
| `cas daemon …` | `install`, `uninstall`, `status`, `log`, `run` |
| `cas doctor` | where cas reads and writes, and what looks wrong |

`<n>` accepts a slot number, a label, or an email — `cas switch work` and
`cas switch me@personal.com` both work, as does any unambiguous substring.

Flags may appear anywhere: `cas remove 2 --yes` and `cas remove --yes 2` are
equivalent.

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

cas also watches for drift in the other direction. Claude Code rotates its own
token, so the live credential is often newer than cas's copy; each command
notices and folds it back into the active slot. If the live token turns out to
belong to a different account — you ran `claude /login` yourself, or a session
that predates a switch wrote its own token back — cas confirms the owner with
Anthropic's `/api/oauth/validate` endpoint (which does not rotate anything)
before touching any slot, and re-points the active marker instead of corrupting
one.

### `cas clean`

A slot is removed only when its credential is genuinely unrecoverable: the
refresh token has expired, the Keychain item is gone, or Anthropic rejects the
refresh token with `invalid_grant`. A slot that is merely stale gets renewed,
and an unreachable API is never treated as proof that an account is dead.
`--dry-run` reports without removing.

## Notes and limits

- **Running sessions do not notice a switch.** Claude Code caches the credential
  in memory at startup, so restart any open session after switching. `cas switch`
  says how many are running.
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
| `CAS_KEYCHAIN_SERVICE` | the Claude Code Keychain item to target |
| `CAS_KEYCHAIN_ACCOUNT` | the Keychain account name (default: your macOS username) |
| `CLAUDE_CONFIG_DIR` | honoured exactly as Claude Code honours it |
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
Claude Code might add), slot numbering, and the OAuth request shapes. Nothing in
the test suite touches your real Keychain or makes a network call.

## Licence

MIT. cas is an independent project and is not affiliated with or endorsed by
Anthropic.
