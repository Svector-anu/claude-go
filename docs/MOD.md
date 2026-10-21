# The claude-go mod

An optional Claude Code *mod* (a plugin of function hooks) named `account-flow`. It is a thin client of
the [controller API](IPC.md): it shows state and requests actions. It holds no credentials, edits no
files, spawns no processes, and makes no network requests other than to the controller socket
(`claude plugin validate` lists exactly the calls it makes).

## Requirements

- Claude Code **2.1.304** (tested). The mods API is early access and "moves between releases"; a
  different version may need the mod updated. Check with `claude plugin validate` and `claude plugin test`.
- A session started by claude-go (`claude-go run`, or `claude` via the alias). Elsewhere the mod stays silent.

## What it adds

| | |
|---|---|
| `/accounts` | accounts, whether each is usable, reset times, and the last-seen usage with its age |
| `/switch <id>` | asks the controller to continue this session on another account; Claude restarts and resumes |
| `/flow [status]` | controller and session state, whether switching is available (and why not), what needs attention |
| `/flow history` | recent handoffs: when, from → to, trigger, reason, result |
| `/flow pane` | a pane with accounts, recent handoffs, and a button that copies the recovery command |
| indicator | `⇄ Work · 5h 24% · 1 other ready`, a band above the prompt (terminal and desktop only), from the controller and `$.session.usage()`. problems use `$.ui.status`, which the engine marks with `⚠` |
| toast | after a handoff (the replacement process shows it once); problems stay longer |

Command names were checked against the installed build: registering `accounts`, `switch` and `flow` was
accepted (a built-in's name would be refused).

## Install and remove

```
claude-go setup --mod       # extracts the mod into ~/.claude-go/mod/account-flow
claude-go setup --undo      # removes it (and the hook / status-line segment / shell block)
```

claude-go loads the mod into the sessions it starts through `CLAUDE_CODE_PLUGIN_DIRS`; it appends to a
value you already set and never edits `~/.claude/settings.json` or Claude's plugin store, and it works
under every account's profile. An installed mod is refreshed automatically when the binary is upgraded.

To use it as an ordinary plugin instead (for example to share it), the repository is a marketplace:

```
claude plugin marketplace add ./integrations/claude-code
claude plugin install account-flow@claude-go-integrations
```

Installed this way it does nothing outside a claude-go session, and is not installed into other profiles.

## Status-line segment (separate, optional)

`claude-go setup --statusline` sets (or wraps) the `statusLine` command in `~/.claude/settings.json`.
Your existing command keeps running; claude-go appends ` │ ⇄ account · 5h 24% …`. It reads only the
documented stdin fields (`rate_limits.five_hour|seven_day|spend_limit`, which describe the current
account only), reports them to the controller, never takes longer than 400 ms, and prints nothing
extra outside a claude-go session. `setup --undo` restores your original command exactly.

## What it cannot do (and does not pretend to)

- It cannot show another account's *current* usage: no supported interface reports it. Other accounts
  show a last-seen reading with its age.
- It cannot transfer a running session itself. It asks the controller, which stops Claude and starts a
  new process; the result appears afterwards.
- `/switch` is refused when the session cannot be resumed with certainty (see `/flow`).
- The pane needs a terminal wide enough to seat it unless asked for with `/flow pane` (asked panes seat at any width).
- Desktop and VS Code surfaces draw the same pane (tested with the mod test kit on all four surfaces),
  but sessions there are not started by `claude-go run`, so the controller is absent and the mod stays silent.

## Development

```
make mod-check    # claude plugin validate + claude plugin test
```
Tests live in `integrations/claude-code/plugin/tests/`. `format.ts` holds pure text helpers; `register.tsx`
the hooks; `api.ts` mirrors the controller types.
