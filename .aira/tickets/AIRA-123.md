---
{"schema":1,"id":"AIRA-123","project":"aira","title":"aitest worker-admit: degrade to ledger-only admission when no cgroup sub-scope is available","status":"planned","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","aitest","ci"],"hold":false,"relations":[]}
---
Requested by the owner via peer session 'deploy', 2026-09-06, correcting AIRA-121's
requirement 7 before it was built.

## What requirement 7 got wrong (as a final answer, not as an interim one)

AIRA-121 req. 7 says shim mode must not export AIRA_AITEST_LIB on
--delegate-ram, because today's worker-admit hard-requires a real nested
cgroup sub-scope per worker and none exists in shim mode. That is correct as
a statement about the CURRENT implementation, but wrong as a permanent design
conclusion: it collapses two different things worker-admit currently does
into one. Cgroups give ENFORCEMENT (a kill backstop); a ledger gives
ADMISSION (queue/allow based on a declared-vs-available RAM budget), and only
the first genuinely needs a cgroup. An in-daemon ledger summing admitted
per-worker reservations against a total RAM budget already prevents
over-subscription -- the actual cause of most real OOMs -- without needing
enforcement at all.

That trade is much better on a CI runner than on Mark's shared desktop box:
single-tenant, disposable VM, no desktop to protect, no sibling sessions to
collaterally kill, so losing the kill backstop costs one job rerun, not
someone else's in-flight work. And the ledger's input (total system RAM) is
CLEANER on a dedicated single-tenant VM (a flat number) than reasoning about
a shared slice cap under a dozen contending sessions. Falling back to plain
pytest-xdist -n auto (unset AIRA_AITEST_LIB, per-worker RAM invisible to
anything) gives up real value: this project's own AIRA-11 class of incident
is exactly a heavy parallel leg spiking RAM with no per-test awareness --
observed for real, a local engine leg peaking ~35GB RSS.

## What to build

Extend `worker-admit` (the daemon RPC aitest's supervisor.py calls to admit
each forked worker) to support a DEGRADED mode: when no cgroup v2 subtree is
available (shim/CI mode, per AIRA-121), still make a real ADMISSION decision
-- admit now or make the caller wait -- based on the worker's declared RAM
need against an in-daemon ledger of currently-admitted workers' declared
needs vs the outer job's total RAM budget (the same shim-mode ledger AIRA-121
already builds at the job level; this is that same primitive one level
deeper, per worker) -- but do NOT attempt to create/nest a cgroup sub-scope,
and do NOT claim one exists. supervisor.py's worker.py side needs the
equivalent of today's granted-scope-path handling to instead accept an
admission-only grant (no scope_path, or a distinct 'admitted, no scope'
marker) and skip the cgroup-dependent pieces (place_self's cgroup.procs
write, the memory.current/memory.max watermark read in _should_recycle --
time/test-count recycling still applies, exactly like today's daemon-down
path) while still being GOVERNED by admission (unlike daemon-down, which is
fully ungoverned and only reached when the daemon itself is unreachable).

Once this exists: AIRA_AITEST_LIB should be exported in shim mode's
--delegate-ram path whenever this degraded backend can actually function --
NOT flatly withheld. Update AIRA-121 requirement 7 to this conditional rule
once this ticket lands (or build them together if that is the more natural
shape -- decide at build time; they are tightly coupled but this is real
Python+Go cross-boundary work in a different subsystem (aitest's supervisor,
not confine/install), so keeping it a separate ticket avoids ballooning
AIRA-121's already-large scope).

## Honesty requirement (same discipline as AIRA-121 throughout)

Every place that currently reports a worker's cgroup-backed state (any
diagnostic surface reading scope_path, memory.current, placement status) must
distinguish 'admission-only, no enforcement backstop' from a real granted
cgroup sub-scope -- never let admission-only look like the real thing.

## Interim state until this lands

AIRA-121 requirement 7's 'leave AIRA_AITEST_LIB unset in shim mode' stands as
the correct INTERIM behaviour (a broken backend -- today's cgroup-requiring
worker-admit with no cgroup -- is worse than plain xdist), but is a deliberate
DEFERRAL pending this ticket, not the intended end state. Do not let a future
reader treat requirement 7 as case-closed.
