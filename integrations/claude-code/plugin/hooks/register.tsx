import type { EngineInterface, Register } from 'claude-code'

import { API_VERSION } from './api'
import type { AccountView, ApiError, HistoryEntry, Reply, Status, Window } from './api'
import { accountsText, historyLine, historyText, indicator, stateText, statusText, toastFor, usageText } from './format'

// The claude-go mod is a thin client. The Go controller owns accounts,
// process lifecycle and handoffs; this module only asks it questions and
// requests actions, over the Unix socket named by CLAUDE_GO_SOCKET.

const POLL_MS = 5000
const CALL_TIMEOUT_MS = 4000
const NOT_WRAPPED = 'This session was not started by claude-go. Start it with `claude-go run`.'

type Api = EngineInterface

async function call<T>(
  $: Api,
  method: 'GET' | 'POST',
  path: string,
  body?: unknown,
): Promise<Reply<T>> {
  const socket = await $.env.get('CLAUDE_GO_SOCKET')
  if (!socket) return { ok: false, code: 'not_wrapped', error: NOT_WRAPPED }
  try {
    const request = $.http.fetch(`http://claude-go${path}`, {
      method,
      headers: { 'content-type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      socketPath: socket,
    })
    const timeout = $.clock.sleep(CALL_TIMEOUT_MS).then(() => 'timeout' as const)
    const res = await Promise.race([request, timeout])
    if (res === 'timeout') {
      return { ok: false, code: 'timeout', error: 'The claude-go controller did not answer in time.' }
    }
    let parsed: unknown
    try {
      parsed = JSON.parse(res.text)
    } catch {
      return { ok: false, code: 'bad_response', error: 'The claude-go controller sent an unreadable answer.' }
    }
    if (!res.ok) {
      const detail = (parsed as { error?: ApiError }).error
      return { ok: false, code: detail?.code ?? 'error', error: detail?.message ?? `controller answered ${res.status}` }
    }
    return { ok: true, data: parsed as T }
  } catch {
    return {
      ok: false,
      code: 'unreachable',
      error: 'The claude-go controller is not reachable. Is `claude-go run` still running?',
    }
  }
}

const requestId = () => `req-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`

async function liveWindows($: Api): Promise<Window[]> {
  const usage = await $.session.usage()
  return usage.rateLimits.map(w => ({
    kind: w.kind,
    percent_used: w.percentUsed,
    ...(w.resetsAt ? { resets_at: w.resetsAt } : {}),
  }))
}

async function reportUsage($: Api, windows: Window[]): Promise<void> {
  if (windows.length > 0) await call($, 'POST', '/v1/usage', { windows })
}

async function toastNewHandoff($: Api, last: HistoryEntry | undefined, startedAt: number): Promise<void> {
  if (!last) return
  const sessionId = await $.session.id()
  if (last.session_id && last.session_id !== sessionId) return
  if (Date.parse(last.at) < startedAt - 60_000) return // an older session's news
  const key = `toasted:${sessionId}`
  if ((await $.store.get(key)) === last.at) return
  await $.store.set(key, last.at)
  const t = toastFor(last)
  $.ui.toast(t.text, { timeoutMs: t.isProblem ? 10_000 : 6_000 })
}

// Module state. A hot reload starts it over, which only means one extra refresh.
const startedAt = Date.now()
let lastWindowsKey = ''

const PANE = 'account-flow'

// What the pane draws: the last answers from the controller. A drawing never
// calls the controller itself, so the pane stays responsive when it is slow.
type View = { status?: Status; accounts: AccountView[]; history: HistoryEntry[]; problem?: string }
let view: View = { accounts: [], history: [] }
let isPaneOpen = false

let isRefreshing = false

// The healthy indicator is drawn as a band above the prompt: the engine pins
// $.ui.status lines with its own warning marker, which suits problems only.
let indicatorText: string | undefined

function showIndicator($: Api, text: string): void {
  $.ui.status(undefined)
  if (text === indicatorText) return
  indicatorText = text
  $.ui.invalidate('ui.render')
}

function showProblem($: Api, text: string | undefined): void {
  $.ui.status(text)
  if (indicatorText === undefined) return
  indicatorText = undefined
  $.ui.invalidate('ui.render')
}

// One refresh at a time: overlapping runs could both announce the same handoff.
async function refresh($: Api): Promise<void> {
  if (isRefreshing) return
  isRefreshing = true
  try {
    await refreshOnce($)
  } catch (err) {
    showProblem($, `⇄ claude-go: status unavailable (${err instanceof Error ? err.message : 'unknown error'})`)
  } finally {
    isRefreshing = false
  }
}

async function refreshOnce($: Api): Promise<void> {
  const [status, accounts] = await Promise.all([
    call<Status>($, 'GET', '/v1/status'),
    call<{ accounts: AccountView[] }>($, 'GET', '/v1/accounts'),
  ])
  if (!status.ok) {
    // Outside claude-go the mod stays out of the way; inside it, say what is wrong.
    showProblem($, status.code === 'not_wrapped' ? undefined : `⇄ claude-go: ${status.error}`)
    view = { accounts: [], history: [], problem: status.error }
    if (isPaneOpen) $.ui.invalidate('ui.render')
    return
  }
  if (status.data.api_version !== API_VERSION) {
    showProblem($, `⇄ claude-go: controller speaks API ${status.data.api_version}, this mod needs ${API_VERSION}; update claude-go`)
    return
  }
  const windows = await liveWindows($)
  showIndicator($, indicator(status.data, accounts.ok ? accounts.data.accounts : [], windows, Date.now()))
  await toastNewHandoff($, status.data.last_handoff, startedAt)
  if (isPaneOpen) {
    const history = await call<{ entries: HistoryEntry[] }>($, 'GET', '/v1/history?limit=6')
    view = {
      status: status.data,
      accounts: accounts.ok ? accounts.data.accounts : [],
      history: history.ok ? history.data.entries : [],
      problem: accounts.ok ? undefined : accounts.error,
    }
    $.ui.invalidate('ui.render')
  }
}

function renderPane($: Api, e: Parameters<Api['ui']['resolve']>[0]) {
  const { Box, Text, Button } = $.ui.resolve(e)
  const now = Date.now()
  const s = view.status
  if (!s) {
    return (
      <Box flexDirection="column">
        <Text bold>claude-go</Text>
        <Text dimColor>{view.problem ?? 'Waiting for the controller…'}</Text>
      </Box>
    )
  }
  const fix = s.session.action
  return (
    <Box flexDirection="column">
      <Text bold>claude-go · {s.session.account.label}</Text>
      <Text dimColor>
        {s.session.phase} · {s.session.handoffs} handoff{s.session.handoffs === 1 ? '' : 's'} this session
      </Text>
      {!s.session.handoff_available && (
        <Text dimColor>Switching is off for this session: {s.session.handoff_block}</Text>
      )}
      {s.session.attention && <Text color="yellow">⚠ {s.session.attention}</Text>}
      {fix && (
        <Button key="copy-fix" label={`Copy: ${fix}`} onPress={press => void $.ui.copy({ text: fix, surface: press.surface })} />
      )}
      <Text bold>Accounts</Text>
      {view.accounts.map(a => (
        <Text dimColor={!a.current && a.state !== 'ready'}>
          {a.current ? '●' : a.state === 'ready' ? '○' : '✕'} {a.label} — {stateText(a, now)}
          {usageText(a, now)}
        </Text>
      ))}
      <Text bold>Recent handoffs</Text>
      {view.history.length === 0 && <Text dimColor>None yet.</Text>}
      {view.history.map(entry => (
        <Text dimColor>{historyLine(entry, now).trim()}</Text>
      ))}
      <Button key="refresh" label="Refresh" onPress={() => void refresh($)} />
    </Box>
  )
}

export const register: Register = on => {
  on('ui.render', { component: 'AbovePrompt' }, ($, e, next) => {
    if (indicatorText === undefined) return next(e)
    const { Text } = $.ui.resolve(e)
    return <Text dimColor>{indicatorText}</Text>
  })

  on('session.start', async ($, e, next) => {
    await $.command.register({ name: 'accounts', description: 'List claude-go accounts and whether each is available' })
    await $.command.register({
      name: 'switch',
      description: 'Continue this session on another claude-go account',
      argumentHint: '<account id>',
    })
    await $.command.register({
      name: 'flow',
      description: 'claude-go controller status and recent handoffs',
      argumentHint: '[status|history|pane]',
    })
    void refresh($)
    $.clock.every(POLL_MS, () => void refresh($))
    return next(e)
  })

  // Claude reports the live account's rate-limit windows; pass them on, because
  // no supported interface reports any other account's.
  on('session.measure', async ($, e, next) => {
    if (e.changed.includes('rateLimits')) {
      const windows: Window[] = e.rateLimits.map(w => ({
        kind: w.kind,
        percent_used: w.percentUsed,
        ...(w.resetsAt ? { resets_at: w.resetsAt } : {}),
      }))
      const key = JSON.stringify(windows)
      if (key !== lastWindowsKey) {
        lastWindowsKey = key
        await reportUsage($, windows)
      }
    }
    return next(e)
  })

  on('command.run', { command: 'accounts' }, async $ => {
    const r = await call<{ accounts: AccountView[] }>($, 'GET', '/v1/accounts')
    if (!r.ok) return { text: r.error }
    const live = await liveWindows($).catch(() => [] as Window[])
    return { text: accountsText(r.data.accounts, Date.now(), live) }
  })

  on('command.run', { command: 'switch' }, async ($, e) => {
    const target = e.args.trim()
    if (!/^[a-z0-9][a-z0-9_-]{0,31}$/.test(target)) {
      const r = await call<{ accounts: AccountView[] }>($, 'GET', '/v1/accounts')
      const names = r.ok ? r.data.accounts.filter(a => !a.current).map(a => a.id).join(', ') : ''
      return { text: `Usage: /switch <account id>${names ? `\nOther accounts: ${names}` : ''}` }
    }
    const r = await call<{ accepted: boolean; to: string }>($, 'POST', '/v1/handoff', {
      request_id: requestId(),
      to: target,
    })
    if (!r.ok) {
      return { text: `Not switched: ${r.error}`, exitCode: 1 }
    }
    // The controller stops this Claude process right after answering, so the
    // result is shown by the replacement: a toast, and /flow history.
    return { text: `Switching to ${r.data.to}. Claude restarts and resumes this session in a moment.` }
  })

  on('command.run', { command: 'flow' }, async ($, e) => {
    const sub = e.args.trim().toLowerCase() || 'status'
    if (sub === 'history') {
      const r = await call<{ entries: HistoryEntry[] }>($, 'GET', '/v1/history?limit=10')
      return { text: r.ok ? historyText(r.data.entries, Date.now()) : r.error }
    }
    if (sub === 'pane') {
      isPaneOpen = true
      void refresh($)
      const opened = await $.ui.open({ id: PANE, title: 'claude-go' })
      return { text: opened.isPlaced ? 'claude-go pane opened.' : `Could not open the pane (${opened.reason}). Try /flow status.` }
    }
    if (sub === 'status') {
      const r = await call<Status>($, 'GET', '/v1/status')
      return { text: r.ok ? statusText(r.data, Date.now()) : r.error }
    }
    return { text: 'Usage: /flow [status|history|pane]' }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, ($, e) => renderPane($, e))
}
