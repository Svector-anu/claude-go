# Project memory (optional)

Short notes that Claude can save and look up, shared by **every account, every session and every machine**.
Because the notes live outside Claude's own config, switching accounts never loses them.

```
claude-go setup --memory      # once; nothing per project afterwards
```

Claude then has three tools (`remember`, `recall`, `forget`). It decides when to use them, guided by the
tool descriptions: look notes up when starting work in a project, save a decision and its reason, never
save secrets.

## How it is wired (and why nothing per project is needed)

`claude-go mcp-memory` is a stdio MCP server. `setup --memory` writes a `.mcp.json` into the `account-flow`
plugin that claude-go already loads into every session (`CLAUDE_CODE_PLUGIN_DIRS`), so the server is available
under **every account's profile** without touching `~/.claude.json` or any Claude setting. Claude Code lists it as
`plugin:account-flow:memory`; `claude mcp list` health-checks it. It is only present in sessions started by claude-go.

## Scopes and project identity

- **project** (default): notes about the current repository. A repository is identified by its normalised `origin`
  URL (`git@github.com:Acme/App.git`, `https://github.com/acme/app` → `project:github.com/acme/app`), so a clone in
  any folder, on any machine, shares notes. Without a remote, the folder path is hashed. The server reads
  `.git/config` directly (worktrees included); it never runs git.
- **global**: preferences and facts true everywhere.

Recall only ever returns the current project's notes plus global ones; another repository's notes are never shown.

## Storage and portability

`~/.claude-go/memory/` (mode `0700`), one append-only JSONL log **per machine** (`<machine id>.jsonl`, `0600`).
A machine only appends to its own file; readers merge every `*.jsonl` and the newest operation per (scope, key)
wins. Every new operation is stamped strictly later than the state its author saw, so a write or deletion can
never lose to something it just replaced, even with skewed clocks.

To follow you between machines, point `memory_dir` at a synced folder (Syncthing, iCloud, Dropbox, a git repo):

```json
{ "version": 1, "memory_dir": "/Users/me/Dropbox/claude-go-memory" }
```

or move notes explicitly: `claude-go memory export notes.jsonl` / `claude-go memory import notes.jsonl`
(import is idempotent and keeps the newest write).

There is no database, no network and no account. Encryption at rest is not included in this version; the
notes folder has the same protection as your other dotfiles. Do not put the folder somewhere world-readable.

## Safety

- **Secrets are refused.** API keys, tokens, private keys, JWTs and `password: …`-style assignments in a value,
  tag or import line are rejected with a message telling the model to store *where* the secret lives instead.
  This is a safety net, not a guarantee: do not rely on it to make storing secrets acceptable.
- **Recalled text is framed as data.** Output is wrapped in `<memory>` with a note that the entries are
  information, not instructions, and stored text cannot close the wrapper (angle brackets are neutralised).
  A note saved from untrusted content therefore cannot masquerade as a system instruction, although a model can
  still be influenced by what it reads; treat the memory as you would any file Claude reads.
- **Bounded.** Key ≤ 128 chars, value ≤ 2000, ≤ 8 tags, ≤ 5000 notes, recall output ≤ ~6,000 characters.
- **Hostile files are tolerated.** Lines from other machines are validated on read; bad, oversized or
  unknown lines are skipped.

## Commands

```
claude-go memory list [--all] [--project DIR]
claude-go memory forget [--global] [--project DIR] KEY
claude-go memory export FILE | import FILE | path
claude-go setup --undo        # turns memory off; your notes are kept
```

## Not done

Semantic (embedding) search, encryption at rest, and conversation upload (deliberately omitted: it sends full chat
text off the machine). Recall is keyword scoring over tags, key and value text plus recency, which is enough for
thousands of short notes.
