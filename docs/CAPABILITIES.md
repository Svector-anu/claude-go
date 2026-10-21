# Claude Code capability report

Checked against **Claude Code 2.1.304** (macOS arm64), 2026-10-20. Sources, in order of authority:
the installed engine's own declaration file (`claude-code.d.ts`, written by the engine when the
plugin-authoring skill loaded), `claude --help` / `claude plugin --help`, and the official docs at
code.claude.com (hooks, statusline). Nothing below is taken from third-party examples.

"Verified" means read in the installed types or official docs. "Tested" means exercised against the
installed binary (see the test names). "Unverified" means assumed and called out as such.

## Things claude-go relies on

| Capability | Status | Evidence |
|---|---|---|
| `--session-id <uuid>`, `--resume <id>`, `--fork-session` | Verified | `claude --help` |
| `claude auth status` JSON (`loggedIn`, `email`, `orgId`, `configDirectory`) | Tested | `claudebin.WhoAmI`, run against the real binary |
| `CLAUDE_CONFIG_DIR` isolates login; Keychain item `Claude Code-credentials-<sha256(path)[:8]>` | Tested | profile `.../profiles/test1` hashes to `72589d6d`, item exists |
| Settings hook `StopFailure`, `error` ∈ {`rate_limit`, `overloaded`, `authentication_failed`, `oauth_org_not_allowed`, `account_on_hold`, `billing_error`, `invalid_request`, `model_not_found`, `server_error`, `max_output_tokens`, `cloud_credential_error`, `unknown`}; common stdin fields include `session_id` | Verified (docs) | hooks page |
| `StopFailure` extra fields (`error_details`, `last_assistant_message`) | **Unverified** | the docs do not show the payload schema; the parser is lenient and tests use constructed payloads, not captured ones |
| Real usage-limit records | **Verified** (51 real hits in this machine's transcripts, 31 five-hour + 20 weekly) | the assistant line has `isApiErrorMessage: true`, `apiErrorStatus: 429`, `error: "rate_limit"`, and `quotaLimits: { status: "rejected", rateLimitType: "five_hour" \| "seven_day", resetsAt: <epoch seconds>, … }`; text is `You've hit your session limit · resets 8:10am (Africa/Lagos)` (five-hour) or `You've hit your weekly limit · resets Oct 14 at 3am (Africa/Lagos)` |
| Transcript structure | **Verified** (field names and line types only) | ~25 line types (`user`, `assistant`, `system`, `attachment`, `queue-operation`, …); content blocks are mostly `thinking`, `tool_use`, `tool_result`; `accountUuid` is **not** on user/assistant lines |
| `--resume <id>` finds a session by id under `<CLAUDE_CONFIG_DIR>/projects/*/`, from any cwd | **Tested** (no model call; `-p` with a command-only prompt and `--output-format json`) | same `session_id` returned from a second config dir whose `projects/` links to the shared store, and from a different cwd; resume appends to the original file; a made-up id fails with `No conversation found with session ID` |

## Things the mod API provides (installed types are the authority)

| Need | API | Notes |
|---|---|---|
| Talk to the Go controller | `$.http.fetch(url, { socketPath })` | HTTP over a Unix domain socket, documented with an example. Also `$.process.run(argv)` (no shell, 30 s default). |
| Find the controller | `$.env.get("CLAUDE_GO_SOCKET")` | the wrapper exports the variable to Claude; name must be a string literal |
| Slash commands | `$.command.register({ name, description, argumentHint })` + `command.run` hook returning `{ text, exitCode? }` | "a built-in's name is refused" |
| Pinned status line | `$.ui.status(text)` | one per plugin, under the prompt, does not touch the user's `statusLine` setting. the engine draws its own `⚠` before it, so the mod uses it for problems only |
| Indicator band | `ui.render` hook on `{ component: 'AbovePrompt' }` | terminal and desktop only; no marker. the healthy indicator uses this |
| Notifications | `$.ui.toast(text)`, `$.ui.notify(text)` | |
| Pane | `$.ui.open({ id, title })` + `ui.render` hook on `{ component: 'Pane' }` | terminal seats from 144 columns unless the person asks |
| Session id | `$.session.id()` | |
| Current account's usage | `$.session.usage()` → `rateLimits: [{ kind, percentUsed, resetsAt? }]`; event `session.measure` fires when a window moves a whole point | **current session's account only**, "the windows the last API response reported"; empty off a subscription |
| Persistence | `$.store` (across sessions), `$.state` (session, survives reload) | |
| Failure events | `classic.StopFailure` is hookable | not used: the settings hook already feeds the controller, and two detectors would need de-duplication |

## Status line (official docs)

`statusLine` in settings: `{ "type": "command", "command": "...", "padding"?, "refreshInterval"? }`.
Claude passes JSON on stdin. Relevant documented fields: `session_id`, `model.display_name`,
`workspace.current_dir`, `rate_limits.five_hour.{used_percentage,resets_at}`,
`rate_limits.seven_day.{...}`, `rate_limits.spend_limit.{...}`. `resets_at` is **Unix epoch
seconds**. `rate_limits` is present only for claude.ai Pro/Max (or a gateway spend limit), only
after the first API response, each window may be independently absent, and it describes the
**current account only**. There is no data about other registered accounts in any supported
interface.

## Implemented from the verified transcript record

`internal/transcript.CurrentLimit` reads the last 256 KB of the session transcript and reports a limit only
when the last conversation line is an `assistant` line with `isApiErrorMessage`, `error: "rate_limit"` and
`quotaLimits.status == "rejected"`, written at or after the current child started (meta lines are skipped; a
user or assistant line after it means the conversation moved on). Verified against all 51 limit records in
the transcripts on the development machine (`CLAUDE_GO_REAL_TRANSCRIPTS=1 go test ./internal/transcript -run Real -v`).
Precedence for a reset time: transcript record, then last-reported usage window, then text parsing, then a 5 h estimate.

## Continuation after a limit switch (experiment, 2026-10-20)

A throwaway session was given a fact ("code word PELICAN-7"), then a user prompt and a real-shaped limit record
were appended so the transcript ended at the limit, exactly as in real transcripts. Then:

| Check | Result |
|---|---|
| `claude -p --resume <id> "<continuation message>"` with *no mention of the fact* | Claude answered **PELICAN-7**: it recovered the cut-off request from the transcript. Same `session_id`. |
| Interactive TUI, `claude --resume <id> "<prompt>"` (driven through a pty) | the prompt is submitted automatically and answered. |
| Prompt placed right after the id, before later flags: `--resume ID "msg" --model x --add-dir /a /b` | parsed correctly. |
| Prompt placed after a variadic flag: `--add-dir /tmp "msg" --resume ID` | the prompt is swallowed as an `--add-dir` value and Claude fails differently. Hence the prompt always goes right after the id. |
| A trailing limit/API-error line in the transcript | does not stop resume or lose context. |

Cost note: a minimal request on this machine costs roughly $0.12 at first (about 30k tokens of default context, written to the
prompt cache), which is also what the first turn on a new account costs after a handoff.

## Plugin-bundled MCP servers (tested 2026-10-20)

| Check | Result |
|---|---|
| `.mcp.json` in a plugin loaded through `CLAUDE_CODE_PLUGIN_DIRS` | `claude mcp list` shows `plugin:account-flow:memory … ✔ Connected`; without the variable nothing is registered |
| A real Haiku session calling the tools | `remember` stored `project:github.com/acme/webapp`, derived from the session's own working directory (the server starts in the session cwd) |
| A new session in a different clone of the same repository (different folder, different remote URL style) | `recall` returned the note |
| A session in a different repository | "No matching notes in memory." |

Plugin MCP tools are named `mcp__plugin_<plugin>_<server>__<tool>`; allow them with `--allowedTools` (put the prompt
*before* that variadic flag, or it is swallowed).

## Verified on two real accounts (2026-10-20)

| Check | Result |
|---|---|
| Start a session on account A (`-p`), resume the same session id under account B's profile with only the continuation message | B answered the fact A was given; same `session_id`. Resuming across genuinely different accounts works. |
| Live handoff of a running *interactive* Claude (`claude-go handoff`) A → B and back, driven through a pty | SIGTERM stopped the TUI cleanly; the replacement started in ~5 s; B answered from the carried-over context ("KITE-6"). |
| First interactive launch of a newly signed-in profile | Claude runs its **onboarding wizard and a trust dialog per folder** (the sign-in alone does not complete them). It swallowed input and would block an automatic handoff. |
| After `claudejson.Seed` (carries `hasCompletedOnboarding`, `lastOnboardingVersion`, `hasSeenTasksHint` and per-folder `hasTrustDialogAccepted`/`hasCompletedProjectOnboarding`, additively, never identity or caches) | no wizard; the resumed session, the mod's indicator, the handoff toast and the status line all rendered on the second account. |
| Nested runs inside a Claude session | `CLAUDE_CODE_CHILD_SESSION` turns transcript saving off; set `CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1` for such tests. |

## Consequences

1. **Usage for non-current accounts is not obtainable.** claude-go keeps the last windows it saw
   per account (reported by the mod or the status-line command while that account was live) and
   always shows them with their age, never as a current reading.
2. **Verified reset times beat parsed text.** When a limit failure arrives, a recently seen window
   at ≥ 95 % with a future `resetsAt` is used as the reset time (not an estimate). Text parsing
   and the 5 h fallback apply only without it.
3. **Claude pauses by itself at a claude.ai limit** (Notification matchers `quota_auto_resume_*`).
   claude-go's switch preempts the wait; if no other account is eligible the session is left
   alone and Claude's own auto-resume applies.
4. The mod dies with every handoff (it lives inside the Claude process that is stopped). Handoff
   outcomes are therefore recorded by the Go controller and shown by the **next** process's mod.
5. **Mod API is early access** and "moves between releases". The plugin manifest names the
   minimum tested version and the mod checks `$.session.version()`; the Go core never imports it.

## Evaluated and not built

- **MCP server**: the mod can already expose commands, and the model has no need to switch
  accounts. An MCP tool that requests handoffs would let the model trigger account changes.
  Skipped; revisit only for read-only diagnostics.
- **Agent SDK**: the Go wrapper already owns process lifecycle, `--resume`, signals and
  recovery; the SDK would add a second orchestrator with its own session handling. Skipped.
- **Socket auth token**: the socket is `0600` in a `0700` directory owned by the user; a token
  passed through the environment would be readable by exactly the same processes.

## Open questions that need a real second login

1. Does `--resume` of a session created under account A work under account B? **Partly answered:**
   transcript discovery through the shared `projects/` link works from a second config dir (tested without
   a login, no model call). Resuming and *sending a turn* under a genuinely different account is still
   untested and needs a second login. **Now answered: it works** (see "Verified on two real accounts").
2. ~~Does SIGTERM end the interactive UI cleanly?~~ **Answered:** yes (see "Verified on two real accounts").
3. Real `StopFailure` payload. (The limit *message text* and structured `quotaLimits` record are now known,
   from transcripts; the hook payload itself is still undocumented.)
4. ~~Does `$.command.register` accept `accounts`, `switch` and `flow`?~~ **Answered:** yes. The real
   `claude -p "/accounts"`, `/flow` and `/switch` ran the mod's handlers on 2.1.304.
5. ~~Can the mod load under every profile?~~ **Answered:** `CLAUDE_CODE_PLUGIN_DIRS=<folder>` loads it with
   no install and no settings change; a nonexistent entry in the list is ignored.
