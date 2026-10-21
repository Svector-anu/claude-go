import { expect, test } from 'claude-code/testing'

const SOCKET = '/tmp/cg-test.sock'
const NOW = Date.now()

type Fetch = { url: string; method: string; body?: string; socketPath?: string }

const STATUS = {
  api_version: 1, pid: 7, version: 'dev', started_at: new Date(NOW - 60_000).toISOString(),
  session: { id: 'sess-1', account: { id: 'a', label: 'Work' }, phase: 'running', handoff_available: true, handoffs: 0 },
}
const ACCOUNTS = {
  accounts: [
    { id: 'a', label: 'Work', state: 'ready', current: true },
    { id: 'b', label: 'Personal', state: 'limit', until: new Date(NOW + 45 * 60_000).toISOString() },
  ],
}

const BAND = {
  plugin: 'account-flow',
  component: 'AbovePrompt',
  surface: 'terminal',
  props: { hasSurvey: false, isWorking: false, maxRows: 10, bodyColumns: 100, scroll: { offset: 0, bodyRows: 10 }, view: {} },
} as const

type Routes = Record<string, { status?: number; body: unknown }>

// Stands in for the engine: every call the mod makes on `$` is answered here.
function engine(on: any, opts: { socket?: string | undefined; routes?: Routes; throws?: boolean; usageThrows?: boolean } = {}) {
  const fetches: Fetch[] = []
  const registered: string[] = []
  const toasts: string[] = []
  const statuses: (string | undefined)[] = []
  const store = new Map<string, unknown>()
  const routes: Routes = opts.routes ?? {}

  on('env.get', (_: unknown, e: { name: string }) => ({ value: e.name === 'CLAUDE_GO_SOCKET' ? ('socket' in opts ? opts.socket : SOCKET) : undefined }))
  on('command.register', (_: unknown, e: { name: string }) => { registered.push(e.name); return { value: { command: e.name } } })
  on('clock.every', () => new Promise(() => {})) // the poll timer never fires in a test
  on('clock.sleep', () => new Promise(() => {})) // never fires: the controller answers first
  on('session.id', () => ({ value: 'sess-1' }))
  on('session.usage', () => {
    if (opts.usageThrows) throw new Error('usage unavailable')
    return { value: { startedAt: NOW, context: {}, rateLimits: [{ kind: 'five_hour', percentUsed: 23.5, resetsAt: new Date(NOW + 3600_000).toISOString() }] } }
  })
  on('store.get', (_: unknown, e: { key: string }) => ({ value: store.get(e.key) }))
  on('store.set', (_: unknown, e: { key: string; value: unknown }) => { store.set(e.key, e.value); return { value: undefined } })
  on('ui.toast', (_: unknown, e: { text: string }) => { toasts.push(e.text); return { value: undefined } })
  on('ui.status', (_: unknown, e: { text: string | undefined }) => { statuses.push(e.text); return { value: undefined } })
  on('ui.invalidate', () => ({ value: undefined }))
  on('http.fetch', (_: unknown, e: { url: string; init?: { method?: string; body?: string; socketPath?: string } }) => {
    fetches.push({ url: e.url, method: e.init?.method ?? 'GET', body: e.init?.body, socketPath: e.init?.socketPath })
    if (opts.throws) throw new Error('connect ENOENT')
    const path = new URL(e.url).pathname + new URL(e.url).search
    const route = routes[`${e.init?.method ?? 'GET'} ${path}`] ?? { status: 404, body: { error: { code: 'not_found', message: 'unknown path' } } }
    const status = route.status ?? 200
    return { value: { status, ok: status < 300, headers: {}, text: JSON.stringify(route.body) } }
  })
  return { fetches, registered, toasts, statuses, store }
}

const START = { cwd: '/', surface: 'terminal', isInteractive: true } as const
const GOOD: Routes = {
  'GET /v1/status': { body: STATUS },
  'GET /v1/accounts': { body: ACCOUNTS },
  'GET /v1/history?limit=10': { body: { entries: [{ at: new Date(NOW - 120_000).toISOString(), from: 'a', to: 'b', trigger: 'auto', reason: 'usage limit', result: 'ok' }] } },
  'POST /v1/handoff': { status: 202, body: { request_id: 'x', accepted: true, to: 'b' } },
  'POST /v1/usage': { body: { ok: true } },
}

test('registers /accounts, /switch and /flow without clashing with a built-in', async ($, on) => {
  const e = engine(on, { routes: GOOD })
  on('session.start', (_: unknown, ev: { cwd: string }) => ({ cwd: ev.cwd }))
  await $.session.start(START)
  expect(e.registered).toEqual(['accounts', 'switch', 'flow'])
})

test('shows the live account, its own rate limits and the other accounts', async ($, on) => {
  const e = engine(on, { routes: GOOD })
  on('session.start', (_: unknown, ev: { cwd: string }) => ({ cwd: ev.cwd }))
  await $.session.start(START)
  await new Promise(r => setTimeout(r, 50))
  const ui = await $.ui.mount(BAND)
  const line = (await ui.find({ type: 'Text', text: /⇄ Work/ }))?.text
  expect(line).toContain('5h 24%')
  expect(line).toContain('others back in')
  // A healthy indicator is a plain band, not a pinned status line (which the engine marks with a warning sign).
  expect(e.statuses.filter(Boolean)).toHaveLength(0)
  await ui.unmount()
})

test('every request goes to the controller socket, never to the network', async ($, on) => {
  const e = engine(on, { routes: GOOD })
  on('session.start', (_: unknown, ev: { cwd: string }) => ({ cwd: ev.cwd }))
  await $.session.start(START)
  await $.command.run({ command: 'accounts' })
  await $.command.run({ command: 'flow', args: 'history' })
  expect(e.fetches.length).toBeGreaterThan(2)
  for (const f of e.fetches) {
    expect(f.socketPath).toBe(SOCKET)
    expect(f.url.startsWith('http://claude-go/v1/')).toBe(true)
  }
})

test('/accounts lists accounts and their availability', async ($, on) => {
  engine(on, { routes: GOOD })
  const out = await $.command.run({ command: 'accounts' })
  expect(out.text).toContain('● Work (a) — current')
  expect(out.text).toContain('✕ Personal (b) — at its limit, back in 45m')
})

test('/switch asks the controller and does not pretend the switch has happened', async ($, on) => {
  const e = engine(on, { routes: GOOD })
  const out = await $.command.run({ command: 'switch', args: 'b' })
  const post = e.fetches.find(f => f.method === 'POST' && f.url.endsWith('/v1/handoff'))
  expect(post).toBeDefined()
  const body = JSON.parse(post?.body ?? '{}')
  expect(body.to).toBe('b')
  expect(body.request_id).toMatch(/^req-[a-z0-9-]{8,}$/)
  expect(Object.keys(body).sort()).toEqual(['request_id', 'to'])
  expect(out.text).toContain('Switching to b')
  expect(out.text).toContain('resumes this session')
})

test('/switch with a bad or missing account never reaches the controller', async ($, on) => {
  const e = engine(on, { routes: GOOD })
  for (const args of ['', '../etc', 'a b', 'UPPER', 'x'.repeat(40)]) {
    const out = await $.command.run({ command: 'switch', args })
    expect(out.text).toContain('Usage: /switch <account id>')
  }
  expect(e.fetches.some(f => f.method === 'POST')).toBe(false)
})

test('/switch reports a refusal from the controller with its reason', async ($, on) => {
  engine(on, {
    routes: { ...GOOD, 'POST /v1/handoff': { status: 422, body: { error: { code: 'rejected', message: 'account "b" is not signed in (run: claude-go accounts login b)' } } } },
  })
  const out = await $.command.run({ command: 'switch', args: 'b' })
  expect(out.text).toContain('Not switched')
  expect(out.text).toContain('not signed in')
  expect(out.text).not.toContain('Switching to')
})

test('/switch while another handoff runs says so', async ($, on) => {
  engine(on, { routes: { ...GOOD, 'POST /v1/handoff': { status: 409, body: { error: { code: 'busy', message: 'another handoff is in progress' } } } } })
  const out = await $.command.run({ command: 'switch', args: 'b' })
  expect(out.text).toContain('another handoff is in progress')
})

test('/flow shows status and history, and rejects unknown subcommands', async ($, on) => {
  engine(on, { routes: GOOD })
  const status = await $.command.run({ command: 'flow', args: '' })
  expect(status.text).toContain('Session sess-1 on Work')
  expect(status.text).toContain('Handoff: available')
  const history = await $.command.run({ command: 'flow', args: 'history' })
  expect(history.text).toContain('switched a → b auto (usage limit)')
  const bad = await $.command.run({ command: 'flow', args: 'delete everything' })
  expect(bad.text).toBe('Usage: /flow [status|history|pane]')
})

test('with the controller down every command says so instead of pretending', async ($, on) => {
  const e = engine(on, { throws: true })
  for (const c of [{ command: 'accounts' }, { command: 'flow', args: 'status' }, { command: 'flow', args: 'history' }, { command: 'switch', args: 'b' }] as const) {
    const out = await $.command.run(c)
    expect(out.text).toContain('not reachable')
    expect(out.text).not.toContain('Switching to')
  }
  expect(e.fetches.length).toBeGreaterThan(0)
})

test('outside claude-go the mod is silent and makes no requests', async ($, on) => {
  const e = engine(on, { socket: undefined })
  on('session.start', (_: unknown, ev: { cwd: string }) => ({ cwd: ev.cwd }))
  await $.session.start(START)
  await new Promise(r => setTimeout(r, 30))
  const out = await $.command.run({ command: 'accounts' })
  expect(out.text).toContain('not started by claude-go')
  expect(e.fetches).toHaveLength(0)
  expect(e.statuses.filter(Boolean)).toHaveLength(0)
})

test('a controller speaking another API version is reported, not trusted', async ($, on) => {
  const e = engine(on, { routes: { ...GOOD, 'GET /v1/status': { body: { ...STATUS, api_version: 2 } } } })
  on('session.start', (_: unknown, ev: { cwd: string }) => ({ cwd: ev.cwd }))
  await $.session.start(START)
  await new Promise(r => setTimeout(r, 30))
  expect(e.statuses.filter(Boolean).at(-1)).toContain('update claude-go')
})

test('a garbled controller answer is reported as such', async ($, on) => {
  engine(on, { routes: { 'GET /v1/accounts': { body: undefined } } })
  const out = await $.command.run({ command: 'accounts' })
  expect(out.text).toMatch(/unreadable|not_found|unknown path|controller/)
})

test('a handoff done for this session is toasted once, a problem for longer', async ($, on) => {
  const last = { at: new Date(NOW - 5_000).toISOString(), session_id: 'sess-1', from: 'a', to: 'b', trigger: 'auto', reason: 'usage limit', result: 'ok' }
  const e = engine(on, { routes: { ...GOOD, 'GET /v1/status': { body: { ...STATUS, last_handoff: last } } } })
  on('session.start', (_: unknown, ev: { cwd: string }) => ({ cwd: ev.cwd }))
  await $.session.start(START)
  await new Promise(r => setTimeout(r, 30))
  await $.session.start(START)
  await new Promise(r => setTimeout(r, 30))
  expect(e.toasts).toEqual(['Continued on b — a hit usage limit'])
})

async function announces($: any, on: any, last: unknown): Promise<string[]> {
  const e = engine(on, { routes: { ...GOOD, 'GET /v1/status': { body: { ...STATUS, last_handoff: last } } } })
  on('session.start', (_: unknown, ev: { cwd: string }) => ({ cwd: ev.cwd }))
  await $.session.start(START)
  await new Promise(r => setTimeout(r, 30))
  return e.toasts
}

test("another session's handoff is not announced here", async ($, on) => {
  const other = { at: new Date(NOW - 5_000).toISOString(), session_id: 'other', from: 'a', to: 'b', trigger: 'auto', result: 'ok' }
  expect(await announces($, on, other)).toHaveLength(0)
})

test('a handoff from hours ago is not announced as news', async ($, on) => {
  const old = { at: new Date(NOW - 3 * 3600_000).toISOString(), session_id: 'sess-1', from: 'a', to: 'b', trigger: 'auto', result: 'ok' }
  expect(await announces($, on, old)).toHaveLength(0)
})

test('a failed handoff is announced with its reason', async ($, on) => {
  const bad = { at: new Date(NOW - 2_000).toISOString(), session_id: 'sess-1', from: 'a', to: 'b', trigger: 'manual', result: 'recovered', message: 'could not start' }
  const toasts = await announces($, on, bad)
  expect(toasts).toHaveLength(1)
  expect(toasts[0]).toContain('back on a')
})

test('a refresh that throws is shown, not lost', async ($, on) => {
  const e = engine(on, { routes: GOOD, usageThrows: true })
  on('session.start', (_: unknown, ev: { cwd: string }) => ({ cwd: ev.cwd }))
  await $.session.start(START)
  await new Promise(r => setTimeout(r, 30))
  expect(e.statuses.filter(Boolean).at(-1)).toContain('status unavailable')
})

test('rate-limit changes are reported to the controller once, and only the live account\'s', async ($, on) => {
  const e = engine(on, { routes: GOOD })
  const limits = [{ kind: 'five_hour', percentUsed: 40, resetsAt: '2026-10-20T15:00:00.000Z' }]
  on('session.measure', (_: unknown, ev: unknown) => ev)
  const input = { context: {}, rateLimits: limits, changed: ['rateLimits'] }
  await $.session.measure(input as never)
  await $.session.measure(input as never)
  const posts = e.fetches.filter(f => f.url.endsWith('/v1/usage'))
  expect(posts).toHaveLength(1)
  expect(JSON.parse(posts[0].body ?? '{}')).toEqual({ windows: [{ kind: 'five_hour', percent_used: 40, resets_at: '2026-10-20T15:00:00.000Z' }] })
  await $.session.measure({ ...input, changed: ['context'] } as never)
  expect(e.fetches.filter(f => f.url.endsWith('/v1/usage'))).toHaveLength(1)
})
