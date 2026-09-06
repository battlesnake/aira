---
{"schema":1,"id":"AIRA-110","project":"aira","title":"aira confine scopes: memory.max does not bound swap, so a confined job's cap is not the bound it appears to be","status":"in-review","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","cgroup","confine","oom"],"hold":false,"relations":[]}
---
Found while measuring AIRA-35 (aitest worker scopes), and deliberately NOT
fixed there: the same latent property applies to every `aira confine` scope
on the machine, which is a materially larger blast radius than one ticket's
worth of change.

## The property

cgroup-v2's `memory.max` bounds a cgroup's *memory* (anon + file + kernel),
not memory + swap. Swap is bounded separately by `memory.swap.max`, which
defaults to `max` (inherited, i.e. unbounded up to the nearest ancestor's
limit). **No production AIRA code path writes `memory.swap.max` on any
scope** — a repo-wide grep for it returns only `_test.go` files.

`aira.slice` itself carries `MemorySwapMax=8G`
(`~/.config/systemd/user/aira.slice`), so the aggregate is bounded, but each
individual confine scope's `scope-memory.max` is not the containment bound
its own status line implies: a job that exceeds its granted reserve is
reclaimed into swap rather than being killed by its scope, until the shared
8 GiB slice-wide swap budget is exhausted.

## Measured (AIRA-35's probe, this host: WSL2 6.18.33.2, 20 GiB swap active)

A 32 MiB-capped scope with `memory.oom.group=1`, running a process that
allocates and touches 512 MiB:

- swap uncapped: **never OOM-killed, child exits 0**, ~520 MiB written to
  swap (identical at 256 MiB cap / 1 GiB allocation: exits 0, 820 MiB
  swapped; identical for a slow 8 MiB-per-100 ms leaker).
- `memory.swap.max=0` on the scope: `oom_group_kill` in 0.03-0.48 s.

## Why this matters beyond aitest

1. The reserve ledger and `Σgranted ≤ cap−headroom` accounting are stated
   over `memory.max`. A job whose real footprint escapes into swap is not
   bounded by the number the ledger reserved for it.
2. `confine_peak_history` / `resolveAdmitReserve` size future reserves from
   observed peak RSS. A job that swaps records a *deflated* peak, so the
   estimate for the next run of the same signature is systematically low —
   a feedback loop toward under-reservation.
3. The confine status line reports `scope-memory.max=enforced=N`, which is
   true as written but reads as a containment guarantee it does not make on
   a host with swap.

## Not obviously "just set it to 0"

Unlike an aitest worker (a disposable pytest process whose OOM the design
already treats as normal + requeue-once), a confined job is arbitrary user
work — a build, a test suite, a training run. Killing one that would have
completed by swapping is a behaviour change with real cost, and the right
answer may be a per-job policy (`--swap-max`, default inherit) rather than a
blanket 0. That design call is exactly why this is its own ticket rather
than a line in AIRA-35.

relates AIRA-35, AIRA-29, AIRA-67.

## Resolution (in-review)

Branch `aira110-swap-bound-memory-max`, off `bd807e6`.

### The decision

`memory.swap.max=0`, by default, on **every** `aira confine` scope. No
`--allow-swap`, no `--swap-max`, no per-job policy — the "not obviously just set
it to 0" section above was put to the coordinator and settled the other way, and
the reasoning is worth recording because it is the reason a whole policy surface
was *not* built:

- AIRA's entire mission is protecting a shared machine from uncontrolled memory
  pressure. A confined job that swaps defeats that mission rather than softening
  it: swap thrash degrades the **whole box** — every other session's build, test
  run and merge gate — measurably worse than one clean in-scope OOM-kill of the
  job that actually outran its own reserve.
- The cost side of the trade is real and is accepted with eyes open: a job that
  would have limped to completion by paging out is now killed. That is the
  intended behaviour. It is also the behaviour the reserve the job was *granted*
  already described, so the change makes the product honest rather than stricter
  than advertised.
- AIRA is pre-release with zero compatibility obligations ([[aira-not-live-no-compat]]),
  so tightening a default costs nothing that a migration would otherwise have to
  pay for. A flag would be a permanent surface bought for a hypothetical.

### What changed

`writeScopeSwapCap` (`internal/runner/swap_cap_linux.go`) is AIRA-35's worker
swap-cap writer, moved out of `worker_scope_linux.go` unchanged in behaviour and
promoted to the shared primitive. It writes `0`, verifies the read-back, and
reports one of the three honest `WorkerAdmitSwapCap*` dispositions — `enforced`,
`not-applicable` (control absent AND `/proc/swaps` proved absent inside a mounted
`/proc`, i.e. a kernel that cannot swap at all), or `unavailable` (a swap-capable
host that will not let AIRA bound it). Every other failure — permission, failed
write, read-back mismatch — is an error and fails the launch closed.

It is now called from every site that creates a scope AIRA claims to contain:

1. **`confineWithDeps`** (`internal/runner/confine_linux.go`), immediately after
   the `memory.oom.group` write and **unconditionally** — before, and independent
   of, the cap decision below it. The cap is conditional (an unpinned,
   non-daemon-admitted launch is deliberately left uncapped); the swap bound is
   not, because an *uncapped* scope that swaps still evades the slice ledger and
   still records a deflated peak. This is the site the ticket is about.
2. **`Runner.applyScopeMemoryCap`** (`internal/runner/runner_linux.go`), paired
   with the `memory.max` write, which covers both remaining AIRA-57 launch sites
   — foreground `aira run` (`runner_linux.go`) and detached `aira run --detach`
   (`detach_linux.go`) both reach the kernel through that one helper. Fail-closed
   as `E_RUN_CAP_UNAVAILABLE`; the target never starts.
3. **`CreateWorkerScope`** already did this (AIRA-35) and is unchanged apart from
   the rename.

Ordering is load-bearing at all three, and is documented at each: the swap write
runs only *after* another `memory.*` write on the same scope has succeeded. A
cgroup with no `+memory` in its parent's `subtree_control` exposes no `memory.*`
files at all, so an ENOENT there would otherwise be misread as "this kernel has
no swap support" instead of "this cgroup has no memory controller".

Harm #3 in the report above is addressed too: `ConfineStatus` gained
`ScopeSwapCap`, rendered on **every** trailer as `scope-swap.max=<disposition>`
on the same always-rendered discipline as `terminated-by`. `scope-memory.max=
enforced=N` is true as written but reads as a containment guarantee, and on a
host where swap could not be bounded it is not one. An unset value renders
`unevaluated`, never as a claim that swap is bounded.

### Tests, and why they are not porous

Both real-cgroup tests were run against a mutant `writeScopeSwapCap` that returns
`enforced` without touching the kernel, and both went RED:

- `TestConfineRealScopeBoundsSwapToZero` (capped **and** uncapped sub-cases):
  scope `memory.swap.max="max"`, want `"0"`. The uncapped sub-case is what pins
  the write as unconditional — no `memory.max` is written on that path at all.
- `TestConfineRealSwapBoundMakesScopeCapContainARunaway`: **exit=0** for a
  256 MiB allocation inside a 32 MiB `memory.max`. That is the ticket's measured
  bug reproduced through the production launch path; with the fix it is 137.

The fixture is `swapUncappedMemoryParent`, which deliberately does **not** reuse
`confineMemoryParent`/`writableMemoryParent`: both of those write
`memory.swap.max=0` on the *parent*, so a containment test under them proves the
harness rather than the product — the exact false pass AIRA-35 found in the
aitest e2e test. The containment test additionally skips when there is no online
swap area, no swap control, or an ancestor already bounding swap below the
allocation, because in each of those cases the old code would OOM-kill too and a
green result would prove nothing.

Also added: `TestConfineSwapCapFailureDoesNotLaunch` and
`TestRunScopeMemoryCapPairsSwapCapAndFailsClosed` (fail-closed at each launch
site, target never started; the latter also asserts the writer is *called* at
all, which is what fails against master), plus hermetic
`TestWriteScopeSwapCapFailsClosedOnUnwritableControl` /
`...AbsentControlIsADispositionNotAFailure` — a pair, because with only the first
of them collapsing ENOENT into an error would look correct and would break every
confined job on a `CONFIG_SWAP=n` host.

### Not done, deliberately

No `--allow-swap`/`--swap-max` escape hatch (above). No swap facet on
`RunRecord`: nothing on that record claims swap containment for a disposition to
contradict. `aira.slice`'s own `MemorySwapMax=8G` is untouched — it is now a
backstop for processes placed in the slice by hand rather than the only bound on
AIRA's own jobs.

### Gate

`go build ./...` exit 0 · `go vet ./...` exit 0 ·
`AIRA_REAL_CGROUP=1 go test ./... -count=1` exit 0.
