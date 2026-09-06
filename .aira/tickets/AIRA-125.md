---
{"schema":1,"id":"AIRA-125","project":"aira","title":"Two catalogue entries sit on the wrong side of the state-conflict/bad-request rule","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty"],"hold":false,"relations":[]}
---
Filed as an AIRA-107 review follow-up. Out of that ticket's scope: neither code
is one of the eleven AIRA-107 decided, and moving a code AIRA-87 did *not* leave
at the default is a separate contract change.

AIRA-107 wrote the catalogue's implicit 1-versus-2 rule into the `codes` package
doc comment, because deciding four of the eleven required stating it:

- **1** — the request is well formed and some durable state refuses it *now*
  (already exists, held, conflicted, not in the state the operation needs).
  Change the state and the same request succeeds.
- **2** — no change of state would let that request through as written (a
  malformed argument, or a named target that can never serve it).

Most of the catalogue follows it: `E_RELATION_EXISTS`, `E_LEASE_HELD`,
`E_WRITE_CONFLICT`, `E_PREFIX_OWNERSHIP_CONFLICT`, `E_TRANSITION_INVALID`,
`E_INTENT_NOT_PENDING` are all 1. Two entries predate it and sit on the other
side:

- `E_ALREADY_INITIALIZED` (2) — `aira init` on an initialised project is an
  already-exists refusal about durable state, which the rule puts at 1.
- `E_RANT_IDEMPOTENCY_CONFLICT` (2) — a replayed idempotency key conflicting with
  a stored rant is a state conflict, which the rule puts at 1.

Either move them to 1, or record at each entry why the rule does not apply (an
argument does exist for `E_ALREADY_INITIALIZED`: `init` is the bootstrap surface,
below any record, and a caller that has not initialised has nothing to observe).
What is not acceptable is leaving them as unexplained counter-examples, because
the next author to bucket a conflict code will find two precedents pointing
opposite ways — which is how AIRA-107's eleven ended up undecided in the first
place.

Whichever is chosen, pin it in `internal/codes/produced_test.go`.

## Resolution

**Both codes move to 1.** The ticket offered two acceptable outcomes — move them,
or record at each entry why the rule does not apply. Moving them is chosen for
both, deliberately including `E_ALREADY_INITIALIZED`, whose counter-argument the
ticket names.

That counter-argument is real but does not survive contact with the refusal it is
about. `aira init` *is* the bootstrap surface, below any record, and it is true
that a caller who has not initialised has nothing to observe — but this code is
never raised in that situation. Both emitters fire only once the record exists:
`app.Init` finds `.aira/config` already on disk (`internal/app/project.go:422`),
and `store.PreflightAdoption` finds a `projects` row for the project
(`internal/store/lifecycle.go:162`). At the moment of the refusal the state is
present and observable, the request is well formed, and removing the config or
ejecting the project makes the identical request succeed. That is the 1 side of
the rule, and the same already-exists shape as `E_RELATION_EXISTS`,
`E_GATE_EXISTS` and `E_PREFIX_OWNERSHIP_CONFLICT` — the last of which is raised by
the very same `PreflightAdoption` call, two branches apart, and already exits 1.

`E_RANT_IDEMPOTENCY_CONFLICT` is split off the `E_RANT_*` bad-request family it
was catalogued with. Its neighbours there — `E_RANT_INVALID`, `E_RANT_TOO_LARGE`,
`E_RANT_REF_INVALID` — reject what the caller wrote, and no state change lets any
of them through as written. This one refuses a well-formed rant solely because a
stored row already claims that idempotency key with different input
(`internal/store/rant.go:66`); the key is the caller's to choose and the stored
row is observable state, so it belongs with `E_WRITE_CONFLICT` and
`E_RUN_TELEMETRY_CONFLICT` at 1.

Carving a one-off exception for either would have left exactly the artefact the
ticket exists to remove — a documented counter-example the next author can cite
against the rule — so consistency with the stated rule wins over the special case.

### What this cost the catalogue before

The two entries were not inert. AIRA-107 had to name and dismiss each of them in
prose at an unrelated entry: `E_RUN_TELEMETRY_CONFLICT` records
`E_RANT_IDEMPOTENCY_CONFLICT` as "the lone precedent the other way", and
`E_GATE_EXISTS` records `E_ALREADY_INITIALIZED` as "the 2-precedent ... deliberately
not followed". Both of those passages are now rewritten as agreements rather than
dismissals. That relitigation is the concrete cost the ticket predicted.

### Changes

- `internal/codes/codes.go` — `E_ALREADY_INITIALIZED` 2 → 1 and
  `E_RANT_IDEMPOTENCY_CONFLICT` 2 → 1, each with its reasoning inline at the
  entry. The package doc's 1-versus-2 paragraph no longer records two standing
  counter-examples; it records that AIRA-125 removed them and why an exception was
  rejected. The two dismissal passages above are updated.
- `internal/codes/produced_test.go` — new `TestStateConflictCodesExitOne` pins
  both buckets, kept separate from `TestRebucketedCodesFollowTheKindConvention`
  because that table's exhaustiveness over AIRA-107's eleven is load-bearing and
  neither code here is one of them.
- `internal/core/check_test.go` — `TestAlreadyInitializedUsesInvalidInvocationExitCode`
  asserted exit 2 and is renamed to `TestAlreadyInitializedUsesStateConflictExitCode`
  asserting 1. Its old name encoded the premise this ticket rejects: the
  invocation is not invalid.

No non-test behaviour outside the catalogue changed. The phase-1 spec's code table
(`docs/superpowers/specs/2026-08-08-aira-phase1-design.md:614`) classifies
`E_ALREADY_INITIALIZED` as `error` and carries no exit number, so it stays correct
as written.

### Tests

`TestStateConflictCodesExitOne` asserts three things rather than one, because a
bare value pin would be porous here in a specific way:

1. **Membership in `ExitCodes`, separately from the value.** `ExitForCode`
   defaults to 1, so deleting either entry outright would leave `ExitForCode`
   still answering 1 and a value-only assertion still passing, while the published
   contract silently lost the code. Verified by deleting the
   `E_ALREADY_INITIALIZED` entry: the test fails on the membership branch.
2. **The family neighbours the move was argued from** (`E_RELATION_EXISTS`,
   `E_GATE_EXISTS`, `E_PREFIX_OWNERSHIP_CONFLICT`, `E_WRITE_CONFLICT`,
   `E_RUN_TELEMETRY_CONFLICT`, all 1) — if one moves, the recorded reasoning stops
   holding.
3. **The bad-request neighbours the rant code was split away from**
   (`E_RANT_INVALID`, `E_RANT_TOO_LARGE`, `E_RANT_REF_INVALID`, all 2). A lazy
   "move the whole `E_RANT_*` family to 1" would satisfy a one-sided pin while
   destroying the distinction the split exists to carry.

Non-porousness was verified by reverting: with both catalogue values put back to
2, `TestStateConflictCodesExitOne` fails on all four assertions
(`E_ALREADY_INITIALIZED exits 2, want 1` and the `ExitForCode` pair for each code)
and `TestAlreadyInitializedUsesStateConflictExitCode` fails with
`already initialized exit=2, want 1`. Both were then restored and re-run green.

### Verification (exact exit codes)

- `aira confine -- go build ./...` → exit 0
- `aira confine -- go vet ./...` → exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` → exit 0 (all 15
  packages `ok`; `internal/cgrouptest` has no test files)
