---
{"schema":1,"id":"AIRA-183","project":"aira","title":"confine --list's LIVE=no is ambiguous between a genuinely dead supervisor pending reap and a momentarily-idle-but-alive scope","status":"done","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["confine","reaper","ux"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-183","to":"AIRA-195"}]}
---

Peer report (split, 2026-09-08), verified from source. After `kill <supervisor-pid>`
on a stuck job, `aira confine --list` kept showing a row for the killed PID
(`LIVE=no`) alongside a second, unrelated row also `LIVE=no` — split could
not tell from the listing whether the kill had failed, a scope had leaked,
or this was ordinary re-enqueue churn, and killed a second, unrelated PID
on that mistaken basis. Split later resolved this specific instance
correctly themselves (both rows were ~12s apart and far younger than the
435s the original gate had been waiting, proving neither belonged to the
original job — ordinary churn, not a leak), but named the remaining gap
precisely: a killed-and-pending-reap scope reads identically to a
merely-idle-but-alive one.

## Verified from source

`LIVE` in `aira confine --list` (`cmd/aira/main.go:2706-2708`) renders
`SubtreePopulated` — the kernel's `cgroup.events populated` signal for that
scope's own subtree (AIRA-102's face-only fix, comment at `:2691-2699`). It
answers "does this scope currently have any live leaf processes", nothing
about the SUPERVISOR process itself.

The orphan reaper (`internal/daemon/confine_reaper.go`,
`reapOrphanedScopesPass`/`ReapOrphanedConfineScopes`) separately already
checks supervisor-PID liveness as one of its own reap predicates (alongside
emptiness, age ≥ `defaultScopeReapGrace`, and no live admit lease) — that
check exists, but only inside the reaper's own sweep; it is never surfaced
to the listing. A row with `LIVE=no` therefore conflates two genuinely
different states that the daemon can already tell apart internally: (a)
supervisor PID confirmed dead — orphaned, will be reaped once past grace,
and (b) supervisor PID still alive, scope subtree just transiently empty
(e.g. between fork and exec, or a legitimately idle moment) — not
orphaned, nothing to investigate.

## Not designed here

Whether to add a distinct rendered state (e.g. a `REAPING`/`ORPHANED`
value alongside `LIVE`'s yes/no, or a separate column) and whether the
listing should reuse the reaper's own liveness-check helper directly or a
read-only equivalent, is left for whoever picks this up. Split's own
suggested minimal fix: distinguish the two states in the existing LIVE
column rather than changing behaviour.

## Build review and merge (2026-09-09)

**Verdict: MERGE.** PR #115 merged into master as `0548c19` (branch
`aira183-live-status-clarity`, head `83c7527`, merge base `5a39425`). This
was a fresh, independent final review from source: the automated reviewer
first assigned to the PR hit a session limit mid-task and never completed,
and nothing from it was carried over.

### Gates (exact exit codes, all under `aira confine`)

- Merged tree `83c7527` + origin/master `5be2869` (throwaway detached
  worktree, clean merge `94cb29a`): `make build` 0, `make vet` 0,
  `make fmt-check` 0; `make test` (`go test ./... -count=1`) exit 0, all 15
  packages `ok` (store 365.0s, runner 167.9s, daemon 134.0s, cmd/aira 73.9s,
  core 72.1s, pylib 42.4s). Admission immediate; peak-rss 1.1G;
  terminated-by=normal.
- origin/master moved to `58f1c2f` between that run and the merge.
  `git diff --name-only 5be2869 58f1c2f` contains zero `.go` files (ticket
  files and one plan doc), so the tested tree is code-identical to the tree
  that landed.
- GitHub CI on the PR head `83c7527`: build + vet + gofmt, test, and race all
  `success` (run 34288530619).
- `GOOS=darwin go build ./...` fails in `internal/pylib/extract.go`
  (`unix.Renameat2`): pre-existing, untouched by this PR, not a CI gate.
  Noted only.

### Mutation run (false-pass direction)

Merged tree, `-run 'AIRA183|RenderConfineListLiveColumnUsesSubtreePopulation'`
over `./internal/runner/ ./cmd/aira/`. Baseline 13 pass / 0 fail. Every
mutant exits 1 and is killed by the test that claims the property:

| mutant | killed by |
| --- | --- |
| M1 renderer swaps `idle`/`orphaned` | `…SeparatesAnOrphanFromAnIdleScope`, `…LiveColumnStates`, `…LegendNamesOnlyThePresentStates` |
| M2 `EPERM` read as dead | `…SupervisorLivenessFromSignalError/exists_but_not_ours` |
| M3 lease veto removed | `…ALiveAdmitLeaseVetoesADeadSupervisorReading` |
| M4 unestablished supervisor rendered `idle` instead of `no` | `TestRenderConfineListLiveColumnUsesSubtreePopulation` (the tightened AIRA-102 assertion), `…NeverCallsAnUnestablishedSupervisorAnOrphan`, `…LiveColumnStates/empty_with_no_supervisor_reading` |
| M5 veto widened to `if held` | `…ALiveAdmitLeaseLeavesALiveReadingAlone` |
| M6 `top` wired to the split | `…TopKeepsTheUnsplitLivenessCell` |

### What the review examined (from source, not the PR body)

- **Probe.** `probeSupervisorLive` refuses `pid <= 0` before the syscall
  (`kill(0, 0)` and a negative PID address a process group);
  `parseConfineScopeID` already rejects a non-positive PID, so the guard is
  belt-and-braces. Tri-state: nil error / `EPERM` → alive, `ESRCH` → dead,
  anything else → unestablished. Consistent with the reaper's `pidIsDead`
  (`ESRCH`-only): every `orphaned` row is a PID the reaper also calls dead,
  and `EPERM`/`EINVAL` are non-dead on both sides (listing: `idle` / `no`;
  reaper: not a candidate). The listing's `orphaned` additionally requires
  `SubtreePopulated == false`, which is stricter than the reaper's
  leaf-count predicate, so no row reads `orphaned` while its subtree has
  processes.
- **Veto.** `if held && !*state → nil, else record = state`. Lease
  membership can only nil a DEAD reading; `idle` needs a positive live
  probe, so a stale lease never fabricates life (M3, M5). A vetoed reading
  renders the pre-change `no`, never `idle`: a real orphan under a
  still-held lease (connection-close release pending, or the AIRA-49 stale
  case) is not masked as healthy, and the reaper will not reap it either,
  so the two faces agree. Both are built from the same set,
  `Server.activeConfines(path)` (granted waiters carrying a scope ID).
- **Pending rows.** `mergeConfineRegistry` names `supervisor_live`
  unevaluated rather than borrowing the lease as proof of life; a scanned
  record wins over a registry row (`exists → continue`);
  `ApplyConfineScopeReserves` mutates in place, so the field survives the
  daemon path.
- **Renderer.** Population is asked first (nil → `unevaluated`, true →
  `yes`); the supervisor reading refines only the empty case; the legend is
  derived from the rendered cell and printed only for words on screen.
- **`aira top`.** The change is eight comment lines; `topLiveCell` still
  reads `SubtreePopulated` through `confineBoolYesNo` exactly as before.
  No data-field semantics changed and the RAM bar is untouched, so there is
  no AIRA-192-class cap-vs-reserve conflation. The divergence is documented
  at the function and pinned by a test (M6): a decision, not an oversight.
- **Tightened AIRA-102 assertion.** Genuine. `liveCell` reads field 4 of
  the row whose field 0 is the scope name (NAME OWNER SUPERVISOR-PID
  SCOPE-ID LIVE …; name and owner are validated to contain no whitespace),
  fails loudly when the row is missing, and cannot be satisfied by the
  header or the `LIVE:` legend line. The old `Contains(output, "no")` was
  vacuous via OWNER `unknown`. On the merged tree the old
  `Contains(output, "unevaluated")` was ALSO vacuous, via the RESERVE column
  (a nil `ReserveBytes` renders `unevaluated` since AIRA-191); the test's
  comment names only the first, but the fix covers both. Doc nit, not
  actioned.

### Accepted gaps (non-blocking, written down)

- PID reuse: a dead supervisor whose PID was recycled probes alive and
  renders `idle`. The reaper has the identical blind spot (it will not reap
  until the recycled PID exits). Self-heals.
- Zombie supervisor: `kill(pid, 0)` succeeds on a zombie, so it renders
  `idle`; the reaper likewise treats it as not dead. Self-heals when the
  parent reaps it.
- Daemon-down listing (`dispatcher.go`, empty registry): no lease knowledge
  by construction, so a PID-namespace-local supervisor would read
  `orphaned` in that face only; the daemon-served listing vetoes it.
- Registry snapshot: the listing snapshots granted leases before the scan
  where the reaper queries live per check; a lease granted in that window
  for a namespace-local supervisor could read `orphaned` for one read-only
  listing and self-corrects on the next.
- `top` horizon: "goes static, then vanishes" spans reap grace 2m plus the
  5m sweep, so a dead scope can sit at `no` in `top` for up to ~7 minutes.
  Accepted as the documented decision; if the owner wants the split there
  it is one call (`confineLiveStatus`) plus flipping the pin test.
- Sidecar: the DeepSeek second-opinion call failed (agentmux exit 4); not
  retried, not a gate.
