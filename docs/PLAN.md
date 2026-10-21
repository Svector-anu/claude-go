# claude-go engineering plan

## Workspace assessment (2026-10-20)
Empty directory, no git history, no go.mod. Go 1.26.5 darwin/arm64, Claude Code 2.1.304 at /opt/homebrew/bin/claude.
`git init -b main` was run; no remote is configured; nothing is committed.

## Reference findings
Source read from clones of both repos. "Verified" = read in source; "Unverified" = needs an experiment here.

| Topic | cux (Go, **GPL-3.0**) | claude-swap (Python, **MIT**) |
|---|---|---|
| Account switching | Overwrites Claude's *live* credential (`creds.WriteLive`, `creds.go`) and relaunches | Per-account profile dir, launched with `CLAUDE_CONFIG_DIR` (`session.py`); live login untouched |
| macOS storage | Shells out to `security` (`readMacKeychainSecret`, `writeLiveMacOS`); enumerates items because `find-generic-password -s` returns an arbitrary match; exit 44 = not found; service `Claude Code-credentials` (+ managed variant) | Claude hashes the raw `CLAUDE_CONFIG_DIR` (NFC) into service `Claude Code-credentials-<sha256[:8]>` (`keychain_service_name`); seeds plaintext `.credentials.json` and lets Claude migrate it |
| Backups | OS keystore under own service name; 0600 files on Linux | `security` CLI service `claude-swap` |
| Session continuity | `--resume <id>`; id from SessionStart hook, fallback newest `*.jsonl` in `~/.claude/projects/<cwd>/` (`session.CurrentID`) | Optional `--share-history` symlinks `projects/` and `history.jsonl` across profiles |
| Transcript flush | `waitForTranscript`: exists, non-empty, size stable 200ms; swap on `Stop` hook for proactive swaps | n/a |
| Limit detection | `StopFailure`/`PostToolUseFailure` hook -> `classifyRateLimit` -> signal file; usage API `api.anthropic.com/api/oauth/usage` with `anthropic-beta: oauth-2025-04-20` | Same usage endpoint family; polling policy + cache |
| Signals/IPC | PID-namespaced signal files, atomic 0600 (`signals`) | n/a |
| Writes | temp file in same dir + fsync + rename (`atomicfile`) | `fsutil.atomic_write_text`, `locking.FileLock` |
| Hooks | Edits user's `~/.claude/settings.json`, touching only entries it owns (`hookinstall`) | Menubar/launch agent instead |

Licensing: nothing from cux is copied (GPL-3.0). Design ideas and public facts only. claude-swap is MIT but we are not copying it either.

## Recommended architecture
Per-account **isolated profile directories** (claude-swap model), not live-credential overwriting (cux model):
- Each registered account owns `~/.claude-go/profiles/<id>/` used as `CLAUDE_CONFIG_DIR`. Claude Code owns the credentials inside it (login via `claude-go accounts add` runs `claude auth login`/`/login` under that dir). We never parse or copy token blobs.
- Consequences: no mutation of the user's real `~/.claude` login; a failed handoff cannot corrupt the default login; no token handling in our code, so "never log tokens" is structural.
- Session continuity requires `projects/<encoded-cwd>/<id>.jsonl` to be visible to the replacement profile. Plan: share `projects/` between profiles via symlink to a claude-go-owned shared dir. **Unverified** (Experiment E2).
- Handoff = stop child (graceful), wait for transcript stability, relaunch `claude --resume <id>` with the other profile's `CLAUDE_CONFIG_DIR` and the original flags.

## Package layout
```
cmd/claude-go/         main only
internal/cli/          dispatch, output, exit codes (injected IO/env, fully testable)
internal/buildinfo/    version vars (ldflags)
internal/paths/        state dir resolution
internal/config/       config.json (strict, versioned)
internal/registry/     accounts.json: types, load, (B) atomic save + lock
internal/claudebin/    locate claude, query version via injectable runner
(B) internal/fsx/      atomic write, flock
(B) internal/profile/  profile dir lifecycle, shared projects link
(C) internal/launch/   child process, signals, argv preservation
(C) internal/session/  session id capture, transcript stability
(D) internal/selector/ account state machine + strategy
(D) internal/classify/ limit/auth/transient classification
(E) internal/hooks/    settings.json hook install + signal files
```

## Invariants
1. Token bytes never enter our logs, output, tests or state.
2. State files 0600, dirs 0700; every write is temp+fsync+rename under a lock.
3. Corrupt/unknown-version state is an error that names the file; it is never overwritten.
4. Never delete/replace a profile or credential without an explicit user command.
5. Exactly one child per wrapper; a handoff is "successful" only once the replacement process has started.
6. Resume only the captured session id; if unknown, relaunch fresh and say so.
7. A bare 429/5xx is not an account limit.

## Phases and acceptance
- **A** CLI foundation: `--help`, `version`, `status`, `accounts list`; config/state loading; tests for each; `make build`.
- **B** Registry write path, locking, atomic writes, profile dirs, 0600/0700, corruption tests, concurrent-writer test; macOS Keychain behavior validated by experiment E1 before any "works on macOS" claim.
- **C** Launcher preserving argv/cwd/env/exit status, signal forwarding; session id capture; manual `claude-go handoff <account>` proven end to end (E2/E3).
- **D** Selector state machine (eligible/cooling/exhausted/auth-failed), classification fixtures, no-eligible-account message.
- **E** Hooks (`Stop`, `StopFailure`, `SessionStart`) triggering handoff; only after C/D tests pass.

## Experiments needed on Claude Code 2.1.304
- E1: does `CLAUDE_CONFIG_DIR=<dir> claude auth login` create a hashed Keychain item, and does `security` behave under a non-interactive wrapper (ACL prompts)?
- E2: does a symlinked shared `projects/` let a different profile `--resume` a session created under another?
- E3: exact payloads of `SessionStart`/`Stop`/`StopFailure` hooks; is the session id exposed in env?
- E4: rate-limit message shape and whether the usage endpoint is stable enough to depend on.

## Stage C decisions (2026-10-20)
- **Session identity:** fresh launches get `--session-id <uuid>` injected (flag confirmed in `claude --help` 2.1.304), so the session is known without hooks or transcript sniffing. Invocations where the id is unknowable (`--continue`, `--resume` by name/picker, `--fork-session`) or the arguments cannot be rebuilt (`--worktree`, `--bg`, unknown flags that may take a value) run normally but refuse handoff, with the reason.
- **Arguments:** `internal/claudeargs` mirrors Claude's flag arity (`flagArity`); a test diffs it against the installed `claude --help` to catch drift.
- **Handoff:** SIGTERM, wait for exit (SIGKILL after 5s), relaunch `claude --resume <id> <original flags minus prompt>` under the target profile. One child alive at a time; success is reported only after the replacement starts; a failed start restarts the original account.
- **Trigger:** `claude-go handoff <account>` writes a request under `<state>/runtime/<pid>.handoff`; the wrapper answers via `<pid>.result`. Automatic triggers are Stage E.
- **Transcripts:** every profile's `projects/` links to `<state>/shared/projects`. A profile that already holds its own history is never merged; `run` refuses until the user resolves it.
- **Signals:** SIGINT is not forwarded (the terminal already delivers it to the foreground group, i.e. Claude); SIGTERM/SIGHUP/SIGQUIT are.

## Verified vs unverified after Stage C
Verified (fake `claude`, real processes): argument/cwd/env/exit-status preservation, handoff and return handoff, duplicate-session refusal, failed-handoff recovery, control-file cleanup.
Unverified against real Claude Code (needs a logged-in profile): E1 (login under `CLAUDE_CONFIG_DIR`, Keychain item), E2 (resume across profiles through the shared `projects/` symlink), SIGTERM being a clean exit for the interactive UI, and whether `claude auth status` reports per-profile login as assumed.

## Setup, isolation and automatic switching (2026-10-20)
User-facing flow: `claude-go setup` once, `claude-go accounts add NAME --login` once per extra account, then `claude` / `claude-go run`.

**Corrections to earlier assumptions (verified here):**
- A fresh `CLAUDE_CONFIG_DIR` is *not* seeded from the default login: an interactive start stayed logged out and created no Keychain item. An earlier "profile came up logged in" observation was the user signing in during that session.
- The Keychain service name for a profile is `Claude Code-credentials-` + sha256(path)[:8] (observed: `.../profiles/test1` -> `72589d6d`).

**Design:**
- Default account = Claude's own `~/.claude`, launched with `CLAUDE_CONFIG_DIR` *unset* (an explicit value would select a different Keychain item). Registered by `setup` without copying anything. Never purged.
- Extra accounts = `~/.claude-go/profiles/<id>`, signed in by the real `claude auth login` under that directory. claude-go never reads, copies or stores a credential. Twin logins (same email+org) are rejected.
- Profiles symlink `projects/` to `~/.claude/projects` (one shared history) and `settings.json`, `CLAUDE.md`, `commands`, `agents`, `skills`, `rules`, `keybindings.json` to the default ones, so the hook installed once in `~/.claude/settings.json` applies to every account.
- The hook is a signature-keyed `StopFailure` entry (`<abs>/claude-go hook stop-failure`); other tools' hooks, unknown keys and large numbers are preserved, a backup is written once, invalid JSON is never touched. It does nothing outside a wrapped session.
- Hook -> wrapper: `limits.Classify` -> `<state>/runtime/<pid>.event` -> wrapper stops Claude (SIGTERM), picks the next eligible account (`availability.Next`), relaunches `--resume <id>` and prints one line. Account state lives in `state.json` (exhausted / model_limited / auth_failed / blocked).
- When no account is left the session is left running and one explanatory line (earliest reset) is printed when it ends.

**Unverified against the real Claude Code (assumptions to confirm with a real second login):**
- exact `StopFailure` payload fields and the usage-limit wording (parser is lenient; a bare `rate_limit` is treated as an account limit with an estimated 5h reset);
- that SIGTERM ends the interactive UI cleanly;
- `claude auth login` from a custom `CLAUDE_CONFIG_DIR` creating the hashed Keychain item and `auth status` reading it;
- `--resume` of a session created under another account through the shared `projects/` link.

## Claude Code integrations (2026-10-20)
Evidence and API facts are in [CAPABILITIES.md](CAPABILITIES.md); the controller interface is [IPC.md](IPC.md); the mod is [MOD.md](MOD.md).

**Built:** controller socket (`internal/ipc`), handoff history (`internal/history`), last-seen usage and
verified reset times (`availability.UsageSeen`/`VerifiedReset`), `claude-go statusline`, the `account-flow`
mod (commands, indicator, toast, pane), a diagnostics skill, reversible install (`setup --mod`, `--statusline`, `--undo`).

**Decisions made from what the installed build proved:**
- The mod is loaded through `CLAUDE_CODE_PLUGIN_DIRS` by the wrapper, not installed with `claude plugin install`:
  install writes into the *config dir's* settings and plugin store, which differ per profile. The env route
  works under every account and edits nothing of the user's.
- Plugin names starting with `claude-` are reserved by Claude Code, hence `account-flow`.
- The mod validator only allows `$` to flow into top-level functions; `register` therefore delegates to
  module-level helpers.
- A JSX loop variable named `h` shadows the JSX factory; caught by the mod's pane test.
- Handoffs kill the mod with the process it lives in, so outcomes are recorded by the Go side and announced
  by the replacement process.
- The Go controller stays the only place that decides: the hook, the status-line command and the mod all feed
  or query it; none switches accounts.

**Not built, with reasons:** an MCP server (a model-callable handoff tool would let the model change
accounts) and Agent SDK orchestration (the wrapper already owns process lifecycle). `classic.StopFailure`
inside the mod is unused: the settings hook already reports failures and a second detector would need de-duplication.

**Verified against the installed build (2.1.304):** `claude plugin validate`/`test` (33 mod tests); the real
`claude -p "/accounts"`, `/flow`, `/switch` against a live controller socket; a real handoff requested by the real
mod, executed by the Go controller; the full chain wrapper → `CLAUDE_CODE_PLUGIN_DIRS` → real claude → mod → socket.
**Not verified:** the mod rendered in an interactive terminal (indicator, toast, pane were tested with the mod
test kit and `-p`, not on screen); and everything in the "Open questions" list of CAPABILITIES.md that needs a
real second login (cross-profile `--resume`, SIGTERM on the interactive UI, a genuine limit message).
