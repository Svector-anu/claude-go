---
name: claude-go
description: Diagnose and fix claude-go account problems: why a session did not switch accounts, an account that needs signing in again, no account available, or a handoff that failed. Use when the user mentions claude-go, switching accounts, hitting a usage limit, or /accounts, /switch, /flow.
---

# claude-go troubleshooting

claude-go is a controller outside Claude Code that runs several Claude accounts as one and moves a
session to another account when one hits its limit. It owns accounts and handoffs; this plugin and
you only read its state and ask it to act.

## Look before acting

Run these (read-only) and read the output before suggesting anything:

- `claude-go status` — setup state, the hook, running sessions, and each account's state.
- `claude-go history -n 10` — recent handoffs with the reason and the result.
- `claude-go accounts list` — each account and whether it is ready, at its limit (with the reset
  time, "estimated" when no exact time was known), or needs signing in.
- Inside a claude-go session: `/flow` (controller and session), `/flow history`, `/accounts`.

## What the states mean

| You see | Meaning | What to do |
|---|---|---|
| `ready` | usable | nothing |
| `at its limit … until HH:MM` | usage window spent | wait for the reset, or use another account; `claude-go accounts enable <id>` only if the reset time is a known guess and the user knows it is wrong |
| `needs sign-in` | login expired or missing | the user runs `claude-go accounts login <id>` (opens a browser; you cannot do this for them) |
| `unavailable: billing or account hold` | Anthropic reports a billing problem or hold | the user must fix it on claude.ai; claude-go cannot |
| handoff `rejected` / `unavailable` | the switch did not happen | read the message in `claude-go history`; the session kept running on the old account |
| handoff `recovered` | the new account could not start; the old one was restarted | check the target account with `claude-go accounts list` |

## "Why didn't it switch?"

1. `claude-go status`: is the hook `installed`? If not, `claude-go setup`.
2. Was this session started by `claude-go run` (or the `claude` alias)? A plain `claude` is not
   supervised. `/flow` says so.
3. `/flow` shows `Handoff: unavailable for this session (…)` when the session was started with
   `--continue`, `--resume` by name, `--fork-session`, `--worktree` or similar: claude-go cannot know
   which conversation to resume. Start a fresh `claude-go run` or resume by full session id.
4. Is another account ready? If not, claude-go leaves the session alone and Claude's own wait applies.

## Rules

- Never read, print or edit credential files, Keychain items or anything under a profile directory.
  claude-go never needs you to.
- Do not run `claude-go accounts remove --purge-profile` or `claude-go setup --undo` unless the user
  asked for exactly that; tell them what it removes first.
- Handoffs restart Claude: `/switch <id>` ends the current process and resumes the same session on
  the other account. Do not suggest it mid-task without saying so.
