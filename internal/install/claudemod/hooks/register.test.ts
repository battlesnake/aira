import { test, expect } from 'claude-code/testing'
import { register } from './register'

// AIRA-284 slice B mod tests. They run against the TEMPLATE: the binary path is
// still the '@@AIRA_BINARY@@' token (the Go installer substitutes it, and its own
// test pins the substitution). The test's hooks stand for the engine beneath the
// plugin; every `process.run` the plugin makes is recorded there.
//
// A hook that throws is SKIPPED by the engine and the chain carries on beneath it,
// so "the result came back fine" cannot tell a swallowed error from a leaked one.
// OBSERVER is a second plugin loaded ABOVE aira-usage: after `await next(e)` it
// reads `next.trace`, the engine's own record of how each link beneath ended, and
// hands it back inside the result text. The chain is intact only when aira-usage
// itself ran to a normal end ('returned'/'passed', never 'skipped'/'kept'/
// 'caught'/'rejected') AND reached the engine ('test' is the last link).

const BINARY = '@@AIRA_BINARY@@'
const ANSWER = 'SECRET-PROMPT-TEXT'
const OK = { exitCode: 0, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false }
// Surfaces a counters-and-ids mod must never touch; any call lands in a hook
// that records it.
const FORBIDDEN = [
  'session.messages', 'session.append', 'session.send', 'fs.read', 'fs.write', 'fs.exists', 'fs.list', 'fs.stat',
  'http.fetch', 'settings.read', 'agent.spawn', 'prompt.read',
]

const OBSERVER = {
  name: 'observer',
  tier: 'prepend' as const,
  register: (on: any) => {
    on('turn.complete', async (_$: any, e: any, next: any) => {
      const r = await next(e)
      const links = next.trace.map((t: any) => ({ plugin: t.plugin, outcome: t.outcome }))
      return { text: JSON.stringify({ text: r.text, links }) }
    })
  },
}

type Call = { argv: readonly string[]; init: any }

const EVENT = {
  answer: ANSWER,
  durationMs: 1200,
  isAborted: false,
  reason: 'answer',
  turnId: 'turn-1',
  usage: { model: 'claude-x', input_tokens: 11, output_tokens: 22, cache_read_input_tokens: 33, cache_creation_input_tokens: 44 },
}

function value(argv: readonly string[], flag: string): string | undefined {
  const at = argv.indexOf(flag)
  return at < 0 ? undefined : argv[at + 1]
}

type Scenario = {
  event?: Record<string, unknown>
  behave?: (call: Call) => Promise<any>
  sessionId?: () => Promise<string>
}

// scenario registers ONE test: hooks stay on the test's engine for its whole
// life, so each test drives exactly one turn.
function scenario(name: string, s: Scenario, check: (r: { calls: Call[]; touched: string[]; text: string; links: any[]; bottom: number }) => void) {
  test(name, { plugins: [OBSERVER] }, async ($: any, on: any) => {
    const calls: Call[] = []
    const touched: string[] = []
    let bottom = 0
    on('session.id', async () => ({ value: await (s.sessionId ?? (async () => 'sess-1'))() }))
    on('session.cwd', async () => ({ value: '/work/tree' }))
    on('process.run', async (_$: any, e: any) => {
      const call = { argv: e.argv, init: e.init }
      calls.push(call)
      return { value: await (s.behave ?? (async () => OK))(call) }
    })
    on('turn.complete', async (_$: any, e: any) => {
      bottom++
      return { text: e.answer }
    })
    for (const forbidden of FORBIDDEN) {
      on(forbidden as any, async () => {
        touched.push(forbidden)
        return { deny: 'the aira-usage mod must not call ' + forbidden }
      })
    }
    const result = await $.turn.complete({ ...EVENT, ...(s.event ?? {}) })
    const seen = JSON.parse(result.text)
    check({ calls, touched, text: seen.text, links: seen.links, bottom })
  })
}

// The plugin changed nothing and the engine ran exactly once.
function expectChainIntact(r: { text: string; links: any[]; bottom: number }) {
  expect(r.text).toBe(ANSWER)
  expect(r.bottom).toBe(1)
  expect(r.links).toHaveLength(2)
  expect(r.links[0].plugin).toBe('aira-usage')
  expect(['returned', 'passed']).toContain(r.links[0].outcome)
  expect(r.links[1]).toEqual({ plugin: 'test', outcome: 'returned' })
}

test('registers a turn.complete hook', () => {
  expect(typeof register).toBe('function')
})

scenario('delivers exactly the ids and four counters, by the recorded absolute path', {}, r => {
  expectChainIntact(r)
  expect(r.calls).toHaveLength(1)
  const { argv, init } = r.calls[0]
  expect(argv[0]).toBe(BINARY)
  expect(argv.slice(1, 5)).toEqual(['--scope-dir', '/work/tree', 'spend', 'add'])
  expect(value(argv, '--provider')).toBe('anthropic')
  expect(value(argv, '--model')).toBe('claude-x')
  expect(value(argv, '--source')).toBe('claude-mod')
  expect(value(argv, '--session')).toBe('sess-1')
  expect(value(argv, '--agent')).toBe('')
  expect(value(argv, '--turn-id')).toBe('turn-1')
  expect(argv).toContain('--resolve-ticket')
  expect(argv).not.toContain('--at')
  expect(JSON.parse(init.stdin)).toEqual({ input_tokens: 11, output_tokens: 22, cache_read_input_tokens: 33, cache_creation_input_tokens: 44 })
  expect(init.timeoutMs).toBe(2000)
  // counters and ids only: the answer text reaches neither argv nor stdin
  expect(JSON.stringify(argv)).not.toContain(ANSWER)
  expect(init.stdin).not.toContain(ANSWER)
  expect(Object.keys(init).sort()).toEqual(['stdin', 'timeoutMs'])
  expect(r.touched).toEqual([])
})

scenario('a subagent turn passes its agent id', { event: { agentId: 'agent-7' } }, r => {
  expect(r.calls).toHaveLength(1)
  expect(value(r.calls[0].argv, '--agent')).toBe('agent-7')
})

scenario('an absent counter is omitted, never sent as 0', { event: { usage: { model: 'claude-x', input_tokens: 5, output_tokens: 6 } } }, r => {
  expect(r.calls).toHaveLength(1)
  expect(JSON.parse(r.calls[0].init.stdin)).toEqual({ input_tokens: 5, output_tokens: 6 })
})

scenario('a genuine zero counter is kept as 0', { event: { usage: { model: 'claude-x', input_tokens: 0, output_tokens: 6 } } }, r => {
  expect(JSON.parse(r.calls[0].init.stdin)).toEqual({ input_tokens: 0, output_tokens: 6 })
})

scenario(
  'malformed counters are omitted, not coerced',
  { event: { usage: { model: 'claude-x', input_tokens: '7', output_tokens: -1, cache_read_input_tokens: 1.5, cache_creation_input_tokens: 9 } } },
  r => {
    expect(JSON.parse(r.calls[0].init.stdin)).toEqual({ cache_creation_input_tokens: 9 })
  },
)

// Nothing countable happened (an interrupt, an API error, a usage-less answer):
// leave a gap, never a fabricated row.
const NO_ROW: Array<[string, Record<string, unknown>]> = [
  ['no usage', { usage: undefined }],
  ['no counters', { usage: { model: 'claude-x' } }],
  ['no model', { usage: { input_tokens: 1 } }],
  ['empty model', { usage: { model: '', input_tokens: 1 } }],
  ['no turn id', { turnId: '' }],
]
for (const [name, event] of NO_ROW) {
  scenario(`${name} leaves no row, and the chain is intact`, { event }, r => {
    expect(r.calls).toHaveLength(0)
    expectChainIntact(r)
  })
}

scenario('a non-zero exit is swallowed and the chain is intact', { behave: async () => ({ ...OK, exitCode: 3, stderr: 'E_NOT_PROJECT' }) }, r => {
  expect(r.calls).toHaveLength(1)
  expectChainIntact(r)
})

scenario(
  'a rejected process.run (timeout or cannot start) is swallowed and the chain is intact',
  {
    behave: async () => {
      throw new Error('timed out after 2000 ms')
    },
  },
  r => {
    expect(r.calls).toHaveLength(1)
    expectChainIntact(r)
  },
)

scenario('next(e) runs exactly once on a clean delivery', {}, expectChainIntact)

scenario(
  'a session.id failure is swallowed, nothing is delivered, the chain is intact',
  {
    sessionId: async () => {
      throw new Error('no id')
    },
  },
  r => {
    expect(r.calls).toHaveLength(0)
    expectChainIntact(r)
  },
)
