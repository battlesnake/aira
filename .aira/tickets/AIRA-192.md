---
{"schema":1,"id":"AIRA-192","project":"aira","title":"aira top's RAM bar sums scope CAP, not granted reserve -- a delegate-ram scope's ceiling inflates the displayed claim far past what is actually reserved","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["confine","delegate-ram","observability","tui"],"hold":false,"relations":[]}
---

Owner observation (2026-09-09, live): "aira top currently shows 90GB of
reservations on a system with 80GB of RAM. No wonder jobs keep dying and
the mem usage bar in aira top is flooded." Investigated immediately.

## Verified from source -- confirmed real, root cause identified precisely

At the moment of the report, `aira confine --list` (the real admission
ledger) showed `slice reserve: 40086514564 granted / 52928M ceiling
across 8 admitted jobs` -- healthy, nowhere near over-subscribed. But the
SAME listing's own `slice scope caps:` line showed `94990824K across live
scopes` -- ~93 GiB, matching the owner's "90GB" observation almost
exactly. Two different numbers, and `aira top` draws the wrong one.

`topReserveFor` (`cmd/aira/tui_top.go:109-125`) is the RAM bar's per-scope
width function. Its own doc comment states the (now-false) assumption
directly: **"The cap IS the granted reserve since AIRA-67 (the daemon
writes the grant as the scope's memory.max hard sub-cap), so this needs
no second source and invents nothing."** That was true when AIRA-67
shipped. `internal/daemon/admit.go:953-954` documents precisely when it
stopped being true: **"a delegate scope's memory.max is its scope
ceiling, not its pinned framework reserve, and that is the largest cap
population on the machine."** AIRA's own admission ledger (AIRA-114)
already reads `scopeCeiling` correctly and knows not to conflate it with
the pinned reserve for its own aggregate-bound arithmetic -- `aira top`
never got the same correction, and sums every scope's `cap`
indiscriminately, delegate-ram scopes included, into `bar.Claimed`
(`tui_top.go:576-611`, accumulated into `offset` per scope, passed as
`claimed` to `topBarFor`).

**Effect:** a handful of generously-capped `--delegate-ram` scopes (whose
whole design point is a ceiling wider than any single pinned framework
reserve, to give aitest/xdist workers room without individually admission-
gating each one) inflate the bar's `Claimed` total far past what is
actually reserved on the slice, producing exactly the "more reserved than
the machine has RAM" reading the owner saw -- a display artifact, not
evidence of genuine over-admission. This is closely related to, but
sharper than, [[AIRA-191]] (filed minutes earlier from a different
report, about the SAME cap-vs-granted gap causing per-scope attribution
confusion in `--json` output) -- AIRA-191 is about an operator having to
reason it out by hand; this ticket is about the flagship dashboard
rendering the wrong number outright, including feeding
`bar.Overcommitted` (`tui_top.go:678`, `Claimed+Outside > Total`) a
falsely-inflated `Claimed`, which can fire the `OVER-SUBSCRIBED` banner
on a healthy slice.

## Owner's explicit correction (2026-09-09): the fix is the real one, not an interim workaround

"The bar should show the reservation-size for each job, with the used
portion brighter." This is the bar's own existing structural intent
(`topUsedWithin` already computes a used sub-span within each region's
`Size`, shaded via `ShadeColour`) -- the only defect is that `Size` is
sourced from `cap` instead of the real per-scope granted reserve.
Excluding delegate-ram scopes from the bar (the interim option originally
sketched above) is explicitly NOT what is wanted; it must show the real
number.

**Investigated further: this needs a genuine new per-scope data source,
not just a rendering change.** `ListConfines`/`listConfinesWithDeps`
(`internal/runner/confine_manage_linux.go:140-144`) is a PURE cgroupfs
scan -- zero daemon round-trip, reads `cap`/`rss`/`command` live from
`/sys/fs/cgroup/.../aira.slice/.aira-CONFINE-*` and `/proc/<pid>/cmdline`.
The real per-job charged reserve (`admitWaiter.reserve`,
`internal/daemon/admit.go:410,1175` etc.) lives only in the daemon's
in-memory admission-evaluator state, keyed by scope-id, and is currently
handed to the CLIENT ONLY at the moment of admission
(`AdmitResponse{Reserve: waiter.reserve, ScopeCeiling: waiter.scopeCeiling}`,
`admit.go:2164`) -- never persisted or exposed anywhere a LATER listing
(from a different process, like `aira top`) can read it back.

**However, `confine --list`'s response ALREADY does a daemon round-trip**
for the slice-wide aggregate (`internal/daemon/confine_manage.go:140-230`,
`s.admitSliceSnapshotFor`) -- this is not a new RPC to invent, it is an
existing one to extend. The daemon already tracks per-scope-id reserve
for every outstanding admitted job (needed for the very
`GrantedBytes`/`outstanding` aggregate this same response already
carries) -- the fix is to also return a `map[scope_id]reserve_bytes` (or
equivalent per-row list, matching the existing `ConfineReservationHold`
row shape at `internal/runner/confine_manage.go:453-467`, which is the
closest existing precedent but currently scoped to "scope-less"
reservations only, e.g. `confine-reserve --pinned` -- this ticket needs
the same treatment for ordinary CGROUP-SCOPED jobs, a different
population) on the wire, merged client-side by scope-id into each
`ConfineRecord` at listing time. `aira top`'s `topReserveFor` then reads
this real reserve when present, falling back to `unevaluated` (never
cap, never a fabricated number) when the daemon does not know it (daemon
down, or a job the daemon lost track of across a restart) -- matching
this project's honesty convention throughout.

## Not designed here

Exact wire shape for the new per-scope reserve data (a map vs. a list of
rows matching `ConfineReservationHold`'s pattern), and whether
non-delegate-ram scopes should also switch to reading this real field
(they should be numerically identical to `cap` today, per the
`topReserveFor` comment's own now-partially-false claim, but reading the
SAME source for every scope rather than branching on scope type is
simpler and removes the whole cap-vs-reserve distinction as a rendering
concern going forward) -- left for whoever builds this. [[AIRA-191]]
(the per-scope `reserve_bytes` field for `--json` output generally) and
this ticket are now the same underlying fix; build together.

## Built (2026-09-09) -- the two questions this ticket left open, decided

**Wire shape: a per-scope field on `ConfineRecord`, not a fourth row type.**
`ConfineRecord.ReserveBytes *int64` (`"reserve_bytes"`, always on the wire,
`null` when unestablished), merged server-side by scope id in
`runner.ApplyConfineScopeReserves`. Rows on `ConfineSliceReserve` in the
`ConfineReservationHold` mould were considered and rejected: every
scope-backed granted waiter ALREADY has a row in the Scopes table (a
missing cgroup directory surfaces as a `Pending` row via the registry
merge), so a parallel row list would duplicate data that can then
disagree with itself, and it would not serve [[AIRA-191]], whose ask is a
per-scope field in `--json` output. One field, one source.

**Which number: the LEDGER CHARGE (`admitWaiter.ledgerCharge()`), not the
frozen grant and not the cap.** Under AIRA-29 a scope's charge is
re-derived from live usage, and it is the charge -- not the grant payload
-- that occupies the slice, sums to `GrantedBytes`, and gates the next
admission. Publishing the frozen `reserve` would have fixed the delegate
symptom while leaving `aira top` disagreeing with `confine --list`'s own
`slice reserve:` line for every dynamically charged job, which is the
same class of complaint in a new place. The invariant is now pinned by
test: the per-scope reserves in one listing sum to that listing's own
`ScopeBytes + AdoptedBytes`.

**Point 4's premise ("non-delegate cap and reserve are numerically
identical today") is FALSE, and was checked rather than assumed.** At
least five ways they differ for a non-delegate scope: `floorMemoryPage`
rounds the WRITTEN cap down to a page (observed live on this branch's own
build job -- granted reserve 931227238, `scope-memory.max` 931225600, a
1638-byte gap); AIRA-29's dynamic charge sits BELOW the cap for any
tracked scope; an explicit `--memory-max` sets the cap independently of
the reserve; `ConfineCapSourceMemoryReserve` writes the DECLARED reserve
rather than the daemon's resolved one; and the flock fallback leaves the
cap `max` with no ledger record at all. So the unified path is not a
behavioural no-op for that population -- it is a correction there too,
in the same direction. Where the two genuinely do coincide the drawn
output is unchanged, and that case has its own test.

**Adopted scopes are now named per scope.** `queue.adoptedScopes` breaks
the post-restart adopted scalar down by scope id in the same locked block
and the same loop that sums it. Without it every scope on the machine
would render unevaluated across every daemon restart -- trading a wrong
number for a blank bar. It moves exactly as the scalar does, including
being RETAINED (not cleared) across a failed scan.

**Accepted, documented gaps.** A scope the ledger charges nothing for
renders unevaluated rather than 0 (an operator must not read "not
tracked" as "claims nothing"). Rows and totals can skew by up to one
scan interval for adopted scopes, the same bound the adopted scalar
already carries. And a `--delegate-ram` suite holding per-test
`confine-reserve` leases is charged in two places by design -- the
parent's small pinned reserve plus the workers in the scope-less region
-- so its region is narrower than its RAM column; drawing the parent at
its own `memory.current` would count its children twice.


## Review record (2026-09-09)

PR #119 merged (`dd86e94`), all CI checks green (build+vet+gofmt, test,
race). Verified independently against GitHub (merge SHA, check results)
and closed out here.
