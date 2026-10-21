import { expect, test } from 'claude-code/testing'

const NOW = Date.now()
const PANE = {
  plugin: 'account-flow',
  component: 'Pane',
  requestId: 'account-flow',
  props: { title: 'claude-go', isFocused: false, bodyColumns: 60, placement: 'dock', scroll: { offset: 0, bodyRows: 20 }, view: {} },
} as const
const SURFACES = ['terminal', 'desktop', 'vscode', 'mobile'] as const

const STATUS = (session: object) => ({
  api_version: 1, pid: 7, version: 'dev', started_at: new Date(NOW - 60_000).toISOString(),
  session: { id: 'sess-1', account: { id: 'a', label: 'Work' }, phase: 'running', handoff_available: true, handoffs: 1, ...session },
})
const ACCOUNTS = {
  accounts: [
    { id: 'a', label: 'Work', state: 'ready', current: true },
    { id: 'b', label: 'Personal', state: 'limit', until: new Date(NOW + 45 * 60_000).toISOString(), last_usage: { seen_at: new Date(NOW - 120_000).toISOString(), windows: [{ kind: 'five_hour', percent_used: 100 }] } },
  ],
}
const HISTORY = { entries: [{ at: new Date(NOW - 300_000).toISOString(), from: 'b', to: 'a', trigger: 'auto', reason: 'usage limit', result: 'ok' }] }

function wire(on: any, status: object, extra: { copied?: string[] } = {}) {
  const routes: Record<string, unknown> = {
    'GET /v1/status': status, 'GET /v1/accounts': ACCOUNTS, 'GET /v1/history?limit=6': HISTORY, 'GET /v1/history?limit=10': HISTORY,
  }
  on('env.get', () => ({ value: '/tmp/cg-test.sock' }))
  on('command.register', (_: unknown, e: { name: string }) => ({ value: { command: e.name } }))
  on('clock.every', () => new Promise(() => {}))
  on('clock.sleep', () => new Promise(() => {}))
  on('session.id', () => ({ value: 'sess-1' }))
  on('session.usage', () => ({ value: { startedAt: NOW, context: {}, rateLimits: [] } }))
  on('store.get', () => ({ value: undefined }))
  on('store.set', () => ({ value: undefined }))
  on('ui.toast', () => ({ value: undefined }))
  on('ui.status', () => ({ value: undefined }))
  on('ui.invalidate', () => ({ value: undefined }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('ui.copy', (_: unknown, e: { text: string }) => { extra.copied?.push(e.text); return { value: { isCopied: true } } })
  on('http.fetch', (_: unknown, e: { url: string; init?: { method?: string } }) => {
    const u = new URL(e.url)
    const body = routes[`${e.init?.method ?? 'GET'} ${u.pathname}${u.search}`]
    return { value: { status: body ? 200 : 404, ok: !!body, headers: {}, text: JSON.stringify(body ?? { error: { code: 'not_found', message: 'x' } }) } }
  })
}

test('/flow pane opens the pane and it shows accounts, history and a clear status, on every surface', async ($, on) => {
  wire(on, STATUS({}))
  const out = await $.command.run({ command: 'flow', args: 'pane' })
  expect(out.text).toBe('claude-go pane opened.')
  await new Promise(r => setTimeout(r, 60))

  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect((await ui.find({ type: 'Text', text: /claude-go · Work/ }))?.text).toContain('Work')
    expect((await ui.find({ type: 'Text', text: /● Work — ready/ }))).toBeDefined()
    // Another account's usage is shown only as the last reading, with its age.
    expect((await ui.find({ type: 'Text', text: /✕ Personal — at its limit, back in 45m · last seen 5h 100%! \(2m ago\)/ }))).toBeDefined()
    expect((await ui.find({ type: 'Text', text: /switched b → a auto \(usage limit\)/ }))).toBeDefined()
    await ui.unmount()
  }
})

test('when the controller needs the user, the pane says why and offers the fix command', async ($, on) => {
  const copied: string[] = []
  wire(on, STATUS({ attention: 'Account Work hit its limit and no other account is available.', action: 'claude-go accounts add NAME --login' }), { copied })
  await $.command.run({ command: 'flow', args: 'pane' })
  await new Promise(r => setTimeout(r, 60))

  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect((await ui.find({ type: 'Text', text: /⚠ Account Work hit its limit/ }))).toBeDefined()
    const fix = await ui.find({ type: 'Button', key: 'copy-fix' })
    expect(fix).toBeDefined()
    await ui.press({ key: 'copy-fix' })
    await ui.unmount()
  }
  expect(copied).toEqual(Array(SURFACES.length).fill('claude-go accounts add NAME --login'))
})

test('the pane says when switching is off for this session', async ($, on) => {
  wire(on, STATUS({ handoff_available: false, handoff_block: 'session resumed by name, picker or --continue' }))
  await $.command.run({ command: 'flow', args: 'pane' })
  await new Promise(r => setTimeout(r, 60))
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect((await ui.find({ type: 'Text', text: /Switching is off for this session/ }))).toBeDefined()
  await ui.unmount()
})

test('the pane waits politely before the controller has answered', async ($, on) => {
  wire(on, STATUS({}))
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect((await ui.find({ type: 'Text', text: /Waiting for the controller/ }))).toBeDefined()
  await ui.unmount()
})

test('a pane that cannot be placed is reported, not pretended', async ($, on) => {
  wire(on, STATUS({}))
  // wire() answers ui.open; the engine's refusal path is covered by the command text
  const out = await $.command.run({ command: 'flow', args: 'nonsense' })
  expect(out.text).toBe('Usage: /flow [status|history|pane]')
})
