import type { Register } from 'claude-code'

// AIRA-284. Reports one row per finished turn to the local aira daemon:
// COUNTERS AND IDS ONLY (the four Anthropic token counters, model, session id,
// agent id, turn id, working directory). It never reads the transcript, the
// prompt, tool arguments or files, and it never changes the session: every path
// returns next(e) unchanged, exactly once, and every failure is swallowed.
//
// The AIRA_BINARY literal below is replaced by `aira install --claude-usage-mod`
// with the absolute path of the aira binary that installed this mod, so a
// PATH-planted `aira` is never run per turn. The argv goes straight to
// $.process.run (no shell).
const AIRA_BINARY = '@@AIRA_BINARY@@'
const TIMEOUT_MS = 2000
const COUNTER_FIELDS = ['input_tokens', 'cache_read_input_tokens', 'cache_creation_input_tokens', 'output_tokens']

// Only counters the event really carries, as whole non-negative numbers. An
// absent or malformed counter is omitted (aira stores NULL), never sent as 0.
function counters(usage: any): Record<string, number> {
  const out: Record<string, number> = {}
  for (const field of COUNTER_FIELDS) {
    const value = usage?.[field]
    if (typeof value === 'number' && Number.isInteger(value) && value >= 0) out[field] = value
  }
  return out
}

async function deliver($: any, e: any): Promise<void> {
  const model = e?.usage?.model
  const turnId = e?.turnId
  const sent = counters(e?.usage)
  // No usage, model, turn id or counter means nothing countable happened (an
  // interrupt, an API error): leave a gap, never a fabricated row.
  if (typeof model !== 'string' || model === '' || typeof turnId !== 'string' || turnId === '') return
  if (Object.keys(sent).length === 0) return
  const session = await $.session.id()
  const cwd = await $.session.cwd()
  if (typeof session !== 'string' || session === '' || typeof cwd !== 'string' || cwd === '') return
  const agent = typeof e.agentId === 'string' ? e.agentId : ''
  await $.process.run(
    [
      AIRA_BINARY, '--scope-dir', cwd, 'spend', 'add',
      '--provider', 'anthropic', '--model', model, '--source', 'claude-mod',
      '--session', session, '--agent', agent, '--turn-id', turnId, '--resolve-ticket',
    ],
    { stdin: JSON.stringify(sent), timeoutMs: TIMEOUT_MS },
  )
}

export const register: Register = on => {
  on('turn.complete', async ($, e, next) => {
    try {
      await deliver($, e)
    } catch {}
    return next(e)
  })
}
