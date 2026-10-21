# Controller API (v1)

The Go controller (the `claude-go run` process) serves a small API for Claude-side extensions: the mod,
the status-line command, diagnostics. The controller stays authoritative: extensions ask questions and
request actions, they never switch accounts themselves.

## Transport and access control

- HTTP/1.1 with JSON bodies over a **Unix domain socket** at `<state>/runtime/<controller pid>.sock`.
- Socket mode `0600` inside a `0700` directory owned by the user; no TCP listener exists.
- Claude is started with `CLAUDE_GO_SOCKET=<path>`; outside a claude-go session the variable is absent.
- Stale sockets from dead processes are replaced; a socket that still answers is never taken over.
- macOS limits socket paths to 104 bytes. If the state directory is too deep the controller serves no
  socket, says why in `claude-go status`, and the core keeps working (the mod then reports "not reachable").
- No credentials appear in any request or response.

## Rules

- Paths are versioned (`/v1/…`). Responses carry `api_version`; clients must refuse a different major version.
- Bodies: ≤ 64 KiB, `application/json`, unknown fields rejected, trailing data rejected.
- Errors: non-2xx with `{"error":{"code","message","request_id?"},"api_version":1}`.
  Codes: `invalid_request` (400/415), `too_large` (413), `method_not_allowed` (405), `not_found` (404),
  `rejected` (422), `busy` (409), `timeout` (504), `internal` (500).

## Endpoints

| | | |
|---|---|---|
| `GET /v1/health` | `{ok, api_version, pid, version}` | liveness |
| `GET /v1/status` | session (id, cwd, account, `phase`, `handoff_available`, `handoff_block`, `attention`, `action`, `handoffs`), `last_handoff` | phases: `starting`, `running`, `handing_off`, `ended` |
| `GET /v1/accounts` | `{accounts:[{id,label,default?,current?,state,until?,estimated?,last_usage?}]}` | states: `ready`, `limit`, `model_limit`, `needs_signin`, `blocked` |
| `GET /v1/history?limit=N` | `{entries:[…]}` (1–200, default 10), oldest first | results: `ok`, `failed`, `recovered`, `rejected`, `unavailable` |
| `POST /v1/handoff` | `{request_id, to}` → `202 {request_id, accepted, to, replayed?}` | see below |
| `POST /v1/usage` | `{windows:[{kind,percent_used,resets_at?}]}` → `{ok}` | kinds `five_hour`, `seven_day`, `spend_limit`; attributed to the live account |

`last_usage` on an account is the last reading Claude reported *while that account was live*, with
`seen_at`. It is never a current value for an account the session is not running on.

## Handoff semantics

1. `request_id` is 8–64 chars of `A-Za-z0-9_-`; `to` must be a valid account id.
2. The controller validates (registered, profile ready, signed in, session resumable) **before** touching
   Claude. A refusal is `422 rejected` with the reason and the session keeps running.
3. Acceptance (`202`) means validation passed and Claude is about to be stopped; the final outcome is
   recorded in the history and shown by the replacement process. A caller must not expect to survive it.
4. Duplicates: the same `request_id` within 2 minutes returns the first answer (`replayed: true`) without
   a second attempt. A different `request_id` while one is in progress gets `409 busy` naming it.
   Concurrent requests admit exactly one.
5. If the replacement cannot start, the original account is restarted (`recovered`); if that fails too
   the failure is reported (`failed`) and the controller exits with an error.

Types are in `internal/ipc/types.go`; the mod's mirror is `integrations/claude-code/plugin/hooks/api.ts`.
