// Types for claude-go's controller API, version 1. The Go controller owns the
// contract (internal/ipc/types.go); this file only mirrors the fields the mod reads.

export const API_VERSION = 1

export type Window = { kind: string; percent_used: number; resets_at?: string }

export type UsageSeen = { seen_at: string; windows: Window[] }

export type AccountView = {
  id: string
  label: string
  default?: boolean
  current?: boolean
  state: 'ready' | 'limit' | 'model_limit' | 'needs_signin' | 'blocked'
  until?: string
  estimated?: boolean
  last_usage?: UsageSeen
}

export type HistoryEntry = {
  at: string
  session_id?: string
  from?: string
  to?: string
  trigger: string
  reason?: string
  result: 'ok' | 'failed' | 'recovered' | 'rejected' | 'unavailable'
  message?: string
}

export type Status = {
  api_version: number
  pid: number
  version: string
  started_at: string
  session: {
    id?: string
    cwd?: string
    account: { id: string; label: string }
    phase: string
    handoff_available: boolean
    handoff_block?: string
    attention?: string
    action?: string
    handoffs: number
  }
  last_handoff?: HistoryEntry
}

export type ApiError = { code: string; message: string; request_id?: string }

export type Reply<T> = { ok: true; data: T } | { ok: false; code: string; error: string }
