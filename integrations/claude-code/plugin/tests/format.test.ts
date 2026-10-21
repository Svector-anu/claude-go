import { expect, test } from 'claude-code/testing'

import type { AccountView, HistoryEntry, Status } from '../hooks/api'
import { accountsText, ago, bar, clock, isCurrent, historyLine, historyText, indicator, stateText, statusText, toastFor, until, usageText, windowText } from '../hooks/format'

const NOW = Date.parse('2026-10-20T12:00:00Z')
const iso = (minutes: number) => new Date(NOW + minutes * 60_000).toISOString()

const status = (over: Partial<Status['session']> = {}): Status => ({
  api_version: 1,
  pid: 42,
  version: 'dev',
  started_at: iso(-30),
  session: { id: 's-1', account: { id: 'a', label: 'Work' }, phase: 'running', handoff_available: true, handoffs: 0, ...over },
})

test('relative times', () => {
  expect(ago(iso(0), NOW)).toBe('just now')
  expect(ago(iso(-5), NOW)).toBe('5m ago')
  expect(ago(iso(-180), NOW)).toBe('3h ago')
  expect(ago(iso(-3000), NOW)).toBe('2d ago')
  expect(ago(undefined, NOW)).toBe('unknown time')
  expect(until(iso(45), NOW)).toBe('45m')
  expect(until(iso(90), NOW)).toBe('1h30m')
  expect(until(iso(-1), NOW)).toBe('now')
  expect(until(iso(60 * 72), NOW)).toBe('3d')
  expect(until('garbage', NOW)).toBe('an unknown time')
})

test('window text marks nearly spent windows', () => {
  expect(windowText({ kind: 'five_hour', percent_used: 23.4 })).toBe('5h 23%')
  expect(windowText({ kind: 'seven_day', percent_used: 93 })).toBe('7d 93%!')
  expect(windowText({ kind: 'mystery', percent_used: 1 })).toBe('mystery 1%')
})

test('every account state has an honest description', () => {
  const base: AccountView = { id: 'b', label: 'b', state: 'ready' }
  expect(stateText(base, NOW)).toBe('ready')
  expect(stateText({ ...base, state: 'limit', until: iso(90) }, NOW)).toBe('at its limit, back in 1h30m')
  expect(stateText({ ...base, state: 'limit', until: iso(90), estimated: true }, NOW)).toContain('(estimated)')
  expect(stateText({ ...base, state: 'model_limit', until: iso(10) }, NOW)).toContain('one model')
  expect(stateText({ ...base, state: 'needs_signin' }, NOW)).toContain('claude-go accounts login b')
  expect(stateText({ ...base, state: 'blocked' }, NOW)).toContain('billing')
})

test('usage of another account is labelled as the last reading, with its age', () => {
  const a: AccountView = {
    id: 'b', label: 'b', state: 'ready',
    last_usage: { seen_at: iso(-12), windows: [{ kind: 'five_hour', percent_used: 80 }] },
  }
  expect(usageText(a, NOW)).toBe(' · last seen 5h 80% (12m ago)')
  expect(usageText({ ...a, last_usage: undefined }, NOW)).toBe('')
  expect(usageText({ ...a, last_usage: { seen_at: iso(0), windows: [] } }, NOW)).toBe('')
})

test('the accounts list marks the current account and unavailable ones', () => {
  const text = accountsText([
    { id: 'a', label: 'Work', state: 'ready', current: true },
    { id: 'b', label: 'b', state: 'limit', until: iso(30) },
    { id: 'c', label: 'c', state: 'ready' },
  ], NOW)
  expect(text).toContain('● Work (a) — current — ready')
  expect(text).toContain('✕ b — at its limit, back in 30m')
  expect(text).toContain('○ c — ready')
  expect(text).toContain('/switch <id>')
  expect(accountsText([], NOW)).toContain('claude-go setup')
})

test('history lines say what happened and why', () => {
  const ok: HistoryEntry = { at: iso(-3), from: 'a', to: 'b', trigger: 'auto', reason: 'usage limit', result: 'ok' }
  expect(historyLine(ok, NOW)).toBe('  3m ago switched a → b auto (usage limit)')
  const bad: HistoryEntry = { at: iso(-1), from: 'a', to: 'b', trigger: 'manual', result: 'rejected', message: 'not signed in' }
  expect(historyLine(bad, NOW)).toContain('refused')
  expect(historyLine(bad, NOW)).toContain('not signed in')
  expect(historyText([], NOW)).toBe('No handoffs yet.')
  expect(historyText([ok, bad], NOW).split('\n')).toHaveLength(3)
})

test('status text explains why a handoff is unavailable and what to do', () => {
  expect(statusText(status(), NOW)).toContain('Handoff: available')
  const blocked = statusText(status({ handoff_available: false, handoff_block: 'resumed by name' }), NOW)
  expect(blocked).toContain('unavailable for this session (resumed by name)')
  const attention = statusText(status({ attention: 'no other account', action: 'claude-go accounts add NAME --login' }), NOW)
  expect(attention).toContain('Needs attention: no other account')
  expect(attention).toContain('Fix: claude-go accounts add NAME --login')
})

test('the indicator shows the live account, its own windows and the others', () => {
  const accts: AccountView[] = [
    { id: 'a', label: 'Work', state: 'ready', current: true },
    { id: 'b', label: 'b', state: 'ready' },
  ]
  expect(indicator(status(), accts, [{ kind: 'five_hour', percent_used: 23.5 }], NOW)).toBe('⇄ Work · 5h 24% · 1 other ready')
  expect(indicator(status(), [accts[0]], [], NOW)).toBe('⇄ Work')
  const waiting: AccountView[] = [accts[0], { id: 'b', label: 'b', state: 'limit', until: iso(75) }]
  expect(indicator(status(), waiting, [], NOW)).toBe('⇄ Work · others back in 1h15m')
  expect(indicator(status(), [accts[0], { id: 'b', label: 'b', state: 'needs_signin' }], [], NOW)).toContain('no other account ready')
  expect(indicator(status({ attention: 'x' }), accts, [], NOW)).toContain('⚠ needs attention')
  expect(indicator(status({ phase: 'handing_off' }), accts, [], NOW)).toContain('switching…')
})

test('toasts: success is quiet, problems last longer and say why', () => {
  const ok = toastFor({ at: iso(0), from: 'a', to: 'b', trigger: 'auto', reason: 'usage limit', result: 'ok' })
  expect(ok).toEqual({ text: 'Continued on b — a hit usage limit', isProblem: false })
  // A manual switch must not claim a limit was hit.
  expect(toastFor({ at: iso(0), from: 'a', to: 'b', trigger: 'manual', reason: 'requested', result: 'ok' }).text).toBe('Switched to b')
  const rec = toastFor({ at: iso(0), from: 'a', to: 'b', trigger: 'manual', result: 'recovered', message: 'x failed' })
  expect(rec.isProblem).toBe(true)
  expect(rec.text).toContain('back on a')
  expect(toastFor({ at: iso(0), trigger: 'auto', result: 'unavailable', message: 'No other account is available.' }).text).toBe('No other account is available.')
})

test('the accounts view shows each account\'s limits: live for the current one, last reading for the others', () => {
  const live = [{ kind: 'five_hour', percent_used: 24, resets_at: iso(130) }, { kind: 'seven_day', percent_used: 41, resets_at: iso(60 * 50) }]
  const text = accountsText([
    { id: 'a', label: 'Work', state: 'ready', current: true },
    { id: 'b', label: 'Personal', state: 'ready', last_usage: { seen_at: iso(-12), windows: [{ kind: 'five_hour', percent_used: 80, resets_at: iso(40) }] } },
    { id: 'c', label: 'c', state: 'ready' },
  ], NOW, live)
  expect(text).toContain('5h   ▓▓░░░░░░░░   24%  resets')
  expect(text).toContain('in 2h10m)')
  expect(text).toMatch(/5h\s+▓▓▓▓▓▓▓▓░░\s+80%  resets .* \(in 40m\)   seen 12m ago/)
  expect(text).toContain('no reading yet')
  expect(text).toContain('Only the current account reports live usage')
})

test('a window whose reset has passed is not shown as current usage', () => {
  const a = { id: 'b', label: 'b', state: 'ready' as const, last_usage: { seen_at: iso(-300), windows: [{ kind: 'five_hour', percent_used: 99, resets_at: iso(-5) }] } }
  expect(accountsText([a], NOW)).toContain('its windows have reset since')
  expect(isCurrent({ kind: 'five_hour', percent_used: 1, resets_at: iso(-1) }, NOW)).toBe(false)
  expect(isCurrent({ kind: 'five_hour', percent_used: 1 }, NOW)).toBe(true)
})

test('bar and clock', () => {
  expect(bar(0)).toBe('░░░░░░░░░░')
  expect(bar(100)).toBe('▓▓▓▓▓▓▓▓▓▓')
  expect(bar(240)).toBe('▓▓▓▓▓▓▓▓▓▓')
  expect(clock(new Date(NOW + 3600_000).toISOString(), NOW)).toMatch(/^\d{1,2}:\d{2}(am|pm)$/)
  expect(clock(new Date(NOW + 36 * 3600_000).toISOString(), NOW)).toMatch(/^(Sun|Mon|Tue|Wed|Thu|Fri|Sat) \d{1,2}:\d{2}(am|pm)$/)
})
