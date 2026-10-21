import type { AccountView, HistoryEntry, Status, Window } from './api'

// Pure text helpers. Nothing here reads the engine, so they are unit-tested directly.

export function ago(iso: string | undefined, now: number): string {
  const t = iso ? Date.parse(iso) : NaN
  if (Number.isNaN(t)) return 'unknown time'
  const s = Math.max(0, Math.round((now - t) / 1000))
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export function until(iso: string | undefined, now: number): string {
  const t = iso ? Date.parse(iso) : NaN
  if (Number.isNaN(t)) return 'an unknown time'
  const m = Math.round((t - now) / 60000)
  if (m <= 0) return 'now'
  if (m < 60) return `${m}m`
  if (m < 48 * 60) return `${Math.floor(m / 60)}h${String(m % 60).padStart(2, '0')}m`
  return `${Math.floor(m / 1440)}d`
}

const WINDOW_NAMES: Record<string, string> = { five_hour: '5h', seven_day: '7d', spend_limit: 'spend' }

export function windowText(w: Window): string {
  const name = WINDOW_NAMES[w.kind] ?? w.kind
  const pct = Math.round(w.percent_used)
  return `${name} ${pct}%${pct >= 90 ? '!' : ''}`
}

export function stateText(a: AccountView, now: number): string {
  switch (a.state) {
    case 'ready':
      return 'ready'
    case 'limit':
      return `at its limit, back in ${until(a.until, now)}${a.estimated ? ' (estimated)' : ''}`
    case 'model_limit':
      return `one model at its limit, back in ${until(a.until, now)}`
    case 'needs_signin':
      return `needs sign-in (claude-go accounts login ${a.id})`
    default:
      return 'unavailable (billing or account hold)'
  }
}

// Usage of a non-current account is only ever the last reading, shown with its age.
export function usageText(a: AccountView, now: number): string {
  const u = a.last_usage
  if (!u || u.windows.length === 0) return ''
  return ` · last seen ${u.windows.map(windowText).join(' ')} (${ago(u.seen_at, now)})`
}

// A ten-cell bar for a percentage.
export function bar(pct: number): string {
  const n = Math.max(0, Math.min(10, Math.round(pct / 10)))
  return '▓'.repeat(n) + '░'.repeat(10 - n)
}

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

// Wall-clock time of a reset in the user's own time zone: "3:40pm", or "Fri 9:00am" on another day.
export function clock(iso: string, now: number): string {
  const d = new Date(iso)
  const h = d.getHours()
  const time = `${h % 12 === 0 ? 12 : h % 12}:${String(d.getMinutes()).padStart(2, '0')}${h < 12 ? 'am' : 'pm'}`
  return d.toDateString() === new Date(now).toDateString() ? time : `${DAYS[d.getDay()]} ${time}`
}

// A window whose reset time has passed describes a period that is over, so its reading is outdated.
export function isCurrent(w: Window, now: number): boolean {
  return !w.resets_at || Date.parse(w.resets_at) > now
}

export function limitLine(w: Window, now: number): string {
  const name = (WINDOW_NAMES[w.kind] ?? w.kind).padEnd(5)
  const pct = Math.round(w.percent_used)
  const reset = w.resets_at ? `  resets ${clock(w.resets_at, now)} (in ${until(w.resets_at, now)})` : ''
  return `${name}${bar(w.percent_used)}  ${String(pct).padStart(3)}%${reset}`
}

// Limit lines for one account. The live account's windows come from Claude itself;
// any other account's are the last reading, labelled with how old it is.
export function limitLines(a: AccountView, live: Window[], now: number): string[] {
  const source = a.current && live.length > 0 ? { windows: live, age: '' } : a.last_usage ? { windows: a.last_usage.windows, age: `seen ${ago(a.last_usage.seen_at, now)}` } : undefined
  if (!source) return ['no reading yet — it appears once this account has been used']
  const windows = source.windows.filter(w => isCurrent(w, now))
  if (windows.length === 0) return [`${source.age ? `${source.age}: ` : ''}its windows have reset since — usage is back to zero`]
  return windows.map(w => `${limitLine(w, now)}${source.age ? `   ${source.age}` : ''}`)
}

export function accountsText(accounts: AccountView[], now: number, live: Window[] = []): string {
  if (accounts.length === 0) return 'No accounts registered. Run: claude-go setup'
  const rows = accounts.flatMap(a => {
    const mark = a.current ? '●' : a.state === 'ready' ? '○' : '✕'
    const name = a.label === a.id ? a.id : `${a.label} (${a.id})`
    return [
      `  ${mark} ${name}${a.current ? ' — current' : ''} — ${stateText(a, now)}`,
      ...limitLines(a, live, now).map(l => `      ${l}`),
    ]
  })
  return ['Accounts and limits:', ...rows, '', 'Only the current account reports live usage; others show their last reading.', 'Switch with /switch <id>'].join('\n')
}

const RESULTS: Record<string, string> = {
  ok: 'switched',
  recovered: 'failed, original account restored',
  failed: 'failed',
  rejected: 'refused',
  unavailable: 'no account available',
}

export function historyLine(e: HistoryEntry, now: number): string {
  const route = e.from || e.to ? `${e.from ?? '?'} → ${e.to ?? '?'}` : ''
  const why = e.reason ? ` (${e.reason})` : ''
  const note = e.result === 'ok' || !e.message ? '' : ` — ${e.message}`
  return `  ${ago(e.at, now)}  ${RESULTS[e.result] ?? e.result}  ${route} ${e.trigger}${why}${note}`.replace(/\s+/g, ' ').replace(/^ /, '  ')
}

export function historyText(entries: HistoryEntry[], now: number): string {
  if (entries.length === 0) return 'No handoffs yet.'
  return ['Recent handoffs (newest last):', ...entries.map(e => historyLine(e, now))].join('\n')
}

export function statusText(s: Status, now: number): string {
  const x = s.session
  const lines = [
    `claude-go controller v${s.version} (pid ${s.pid}, api ${s.api_version})`,
    `Session ${x.id ?? 'unknown'} on ${x.account.label} — ${x.phase}`,
    x.handoff_available
      ? 'Handoff: available'
      : `Handoff: unavailable for this session (${x.handoff_block ?? 'unknown reason'})`,
    `Handoffs this session: ${x.handoffs}`,
  ]
  if (x.attention) {
    lines.push(`Needs attention: ${x.attention}`)
    if (x.action) lines.push(`  Fix: ${x.action}`)
  }
  if (s.last_handoff) lines.push(`Last handoff attempt: ${historyLine(s.last_handoff, now).trim()}`)
  return lines.join('\n')
}

// The compact indicator under the prompt. `windows` are the live account's own
// rate limits as Claude reports them; nothing is claimed about other accounts' usage.
export function indicator(s: Status, accounts: AccountView[], windows: Window[], now: number): string {
  const parts = [`⇄ ${s.session.account.label}`]
  for (const w of windows) parts.push(windowText(w))
  const others = accounts.filter(a => !a.current)
  const ready = others.filter(a => a.state === 'ready' || a.state === 'model_limit').length
  if (others.length > 0) {
    if (ready > 0) parts.push(`${ready} other ready`)
    else {
      const next = others.filter(a => a.until).map(a => Date.parse(a.until as string)).filter(t => !Number.isNaN(t)).sort((a, b) => a - b)[0]
      parts.push(next ? `others back in ${until(new Date(next).toISOString(), now)}` : 'no other account ready')
    }
  }
  if (s.session.attention) parts.push('⚠ needs attention')
  else if (s.session.phase === 'handing_off') parts.push('switching…')
  return parts.join(' · ')
}

export function toastFor(e: HistoryEntry): { text: string; isProblem: boolean } {
  switch (e.result) {
    case 'ok':
      // A switch the user asked for says so; only an automatic one blames a limit.
      if (e.trigger === 'manual') return { text: `Switched to ${e.to}`, isProblem: false }
      return { text: `Continued on ${e.to}${e.reason ? ` — ${e.from} hit ${e.reason}` : ''}`, isProblem: false }
    case 'recovered':
      return { text: `Couldn't switch to ${e.to}; back on ${e.from}. ${e.message ?? ''}`.trim(), isProblem: true }
    case 'unavailable':
      return { text: e.message ?? 'No other account is available.', isProblem: true }
    default:
      return { text: `Handoff ${RESULTS[e.result] ?? e.result}: ${e.message ?? 'see /flow history'}`, isProblem: true }
  }
}
