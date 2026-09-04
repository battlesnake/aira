# Fable backlog simplification sweep

**Date:** 2026-09-04
**Author:** four parallel Fable-model reviews, synthesized by the coordinating session
**Scope:** all 49 open AIRA tickets as of `576bd2d`, split into four clusters
**Method:** for every ticket, read the full git-file body (not a summary), re-verify every
cited line against current source (several tickets had drifted after other fixes landed
tonight), and classify. No ticket's diagnosis was taken on trust.

## Verdict taxonomy

- **SIMPLIFY-AWAY** — the right fix is deleting or radically simplifying the mechanism,
  not patching it.
- **SHARED-ROOT-CAUSE** — the defect is the same class/mechanism as one or more other
  tickets; one structural fix resolves/moots several at once.
- **NEEDS-INDIVIDUAL-FIX** — real, narrow, no clustering or deletion angle. Honestly
  reported as such, not forced into a pattern that isn't there.
- **STALE-OR-WRONG** — the ticket's own diagnosis doesn't hold up against current
  source. Re-scoped or closed, not fixed.
- **BLOCKED/DEFERRED** — correctly waiting on something else; not actionable now.

## Headline numbers

Of 49 open tickets:

- **~15 close via deletion/simplification** with little or no new code (some bundled —
  three of them close together as part of the already-planned xdist retirement).
- **~13 collapse into five structural fixes plus one owner decision**, rather than
  thirteen separate patches.
- **~15 are genuine, narrow, individual defects** — several are small (Tier C), a few
  are real multi-milestone features (`--detach`, the wall-clock-test hardening pass).
- **3 are correctly blocked** on AIRA-91's still-unknown root cause (AIRA-32, AIRA-33,
  and AIRA-91 itself).

Two independent, valuable side-findings fell out of the sweep that aren't captured by
the ticket-count buckets: **AIRA-93's root cause was actually found** (it's a residue
of the already-fixed AIRA-46 incident, not a live allocator bug), and **AIRA-33's
precondition has hardened, not softened**, since the plan was written a few hours ago —
see below.

---

## The five structural fixes (close ~13 tickets)

### 1. Make the cgroup tree the worker-admit ledger

**Closes AIRA-39, AIRA-41, moots AIRA-63.** Bears on AIRA-91 (see below).

`worker_admit.go`'s grant ledger is pure in-memory state (`workerJobState.grants`,
never pruned, ownership permanent) that a daemon restart wipes — defeating the
aggregate over-commit guard exactly like the outer slice ledger did before #74 fixed
it the same way. Every fact the ledger holds is already on disk: each live worker is a
child `<outer>/.aira-worker-N` with a finite `memory.max`. `committed` can become
`Σ memory.max` over existing `.aira-worker-*` children instead of a value that forgets
itself on restart. This also removes the need for a separate concurrency bound on
`worker-admit` (AIRA-63) and the abnormal-relay-death leak (AIRA-41), since "granted"
becomes synonymous with "capped scope exists" rather than a fact tracked in two places
that can drift apart. Needs the grant→scope-creation window closed (daemon creates the
scope itself, or a short TTL'd pending set) — Tier A, same class as the lease-CAS work
already done for the outer ledger.

### 2. A structured daemon↔client outcome channel, not substring matching

**Closes/moots AIRA-42, AIRA-45, most of AIRA-87; also fixes half of AIRA-83.**

The daemon already produces a closed, structured state (`AdmitResponse.State`) that
gets flattened into prose twice on the way out (`worker_admit_client_linux.go:116`,
`main.go:1078-1080`) and then re-derived by regex in Python
(`aitest/supervisor.py:650-659`) — a path patched **six times** by its own comments'
account (AIRA-38 ×3, a Fable gate ×2, AIRA-92 ×2), and still wrong: a genuine
protocol-version mismatch (the AIRA-83 class) is currently misclassified as a sizing
error. Emit the state verbatim as a stable code / `state=` k=v line instead of prose;
Python matches the enum exactly and treats anything unrecognised as an explicit error,
never as silent "unavailable". AIRA-87's exit-code catalogue drift (confirmed both
directions: codes produced but uncatalogued, and codes catalogued but never produced)
is the same disease at the CLI layer — declare each code once in a leaf package with
its exit code beside it, so drift becomes structurally impossible rather than something
a test has to police.

### 3. A captured-subject type for every gate evaluator

**Closes AIRA-80, AIRA-81; the same invariant (not the same code) resolves AIRA-78 if
the ratchet kind survives (see the owner decision below).**

This is the direct sibling of tonight's own AIRA-72 fix (a gate's stored-pass digest
covered a narrower slice of the tree than what evaluation actually checked). AIRA-72
fixed the command-checker lane. AIRA-80 and AIRA-81 are the *same* "digest one snapshot,
evaluate another" defect in the two lanes AIRA-72 didn't touch: the dimension-gate lane
re-reads the tree twice independently, and the mutation-canary lane re-indexes the tree
between digesting and evaluating, silently dropping tracked-but-gitignored files in the
process (confirmed as a real, currently-live false "canary fired" hazard). The
structural fix: capture the subject **once** (`stableSubjectEntries`, which already
exists), and change every evaluator's signature from "takes a root path, re-reads it"
to "takes an already-captured subject". This is a net simplification — it removes a
tree-read and a re-index round-trip, it doesn't add one — and makes the whole class
structurally unrepresentable rather than needing a fourth instance to be found and
patched later.

### 4. One deadline-policy seam, applied symmetrically

**The only currently-open instance is AIRA-84**, but it explains why the same class
was already fixed twice this session (AIRA-18, AIRA-92) — worth naming as a durable
convention now rather than re-discovering it a fourth time. The daemon keeps its
original 30-second connect-time deadline in force for the full duration of a
long-running routed verb (a big import, `reconcile --rebuild`, a large gate attest),
so the write can commit and the response frame still fail on an expired deadline —
`RequestOutcomeUnknown`, indistinguishable from "did it happen at all?" **The client
does the exact same thing symmetrically** (`protocol.go:337-341`, unconditional 30s
conn deadline whenever the caller's context carries none), so fixing only the daemon
side would still leave every long verb timing out client-side. One deadline-policy
seam — a short connect/parse deadline, with the response wait driven by context or a
verb-declared budget — applied in both `exchange()` and `serveConnection()`.

### 5. One owner decision resolves AIRA-28 and AIRA-29 together

Not a code fix — a decision that's overdue. AIRA-28 ("bound the delegate-RAM aggregate,
airtight") and AIRA-29 ("charge by live `memory.current`, utilization-first") are two
open P1s answering the same root cause (a delegate job's ledger charge and its actual
`memory.max` ceiling are computed independently and never summed) **from opposite
philosophies**. The daemon already runs both models side by side today — the
restart-adoption path (`admit.get:948-960`) already charges by live current, which is
literally AIRA-29's rule; the steady-state path is what AIRA-28 wants replaced. The
sweep's read: the owner already chose utilization over airtightness once (ticket text,
2026-09-01). Leaving both open as if undecided is the actual defect here. Recommend
closing AIRA-28 as superseded-by-decision and unblocking AIRA-29 (currently on hold) —
which, once built, is also the vehicle that would let AIRA-16's second half (a
slice-internal watchdog trigger) and AIRA-24's fourth ask (better saturation-wait UX)
land as natural extensions rather than separate builds.

---

## Ready to delete or simplify today (no structural fix needed, mostly mechanical)

- **AIRA-89** — 4 confirmed-dead symbols (`hasGateContent`, `FlakyCellStateSummary`,
  `GateAudit.Verify`, `GateAuditRecords`), each re-verified to have zero callers
  repo-wide, ~15 lines total. Everything else on the ticket's original list was already
  removed by tonight's Phase 0 sweep, or is confirmed still live and correctly kept.
- **AIRA-66 + AIRA-88 (site 3)** — one shared defect: `go:embed all:` bakes
  `__pycache__`/`.pytest_cache` (and even the tracked test files) into the shipped
  binary. Confirmed: two of the newest extraction directories on this machine differ
  by nothing but a stale `.pytest_cache` entry — proven, not inferred, and it's why the
  pylib-extraction-directory growth in AIRA-88 exists at all. Fix: declare the embedded
  file list explicitly instead of `all:` (bypasses the need for `all:` entirely);
  convert the existing "does it embed something" test into an equality check against
  the declared manifest. AIRA-88's other two sites (a small append-only registry, and
  zero-byte lock-file inodes that are unsafe to prune while held) are genuinely bounded
  in practice — record that as a decision, add no code.
- **AIRA-35** — stop arming `memory.high` (the kernel throttle) on aitest worker
  scopes at all. The WSL2 D-state convergence problem the ticket chases only exists
  because the throttle is armed; `memory.max` + `memory.oom.group=1` already
  self-contains, faster, without the hazard. The proactive-recycle watermark this
  was also serving is a userspace read and doesn't need the kernel mechanism.
- **AIRA-44** — delete the self-discovery mechanism that misidentifies the outer scope
  on a second pytest invocation; the launcher already has the correct coordinate in
  hand and just isn't passing it down.
- **AIRA-52** — encode the confine job's owner in the scope directory name (which
  already durably carries name/pid/nonce) instead of an in-memory registry that a
  daemon restart wipes.
- **AIRA-56** — delete `hasGateContent` (zero callers) and the "does this project even
  have gates" ledger-memory idea it motivated; `ready` can surface the honest
  `U_GATE_SET_EMPTY` primitive that already exists as an advisory line instead.
- **AIRA-74** — the machine-wide flock + full write-transaction rebuild on every
  `grep` query can become a per-query temporary FTS table on its own connection: no
  lock, no persistent-table maintenance surface, same result.
- **AIRA-75** — the ticket's own premise ("voids the journal's gap-detection") doesn't
  exist in the code at all — there is no gap-detection mechanism to void. The real fix
  is smaller than the ticket: stop minting project ticket-sequence numbers for daemon
  telemetry events in the first place.
- **AIRA-17, AIRA-25, AIRA-26, AIRA-65, AIRA-69, AIRA-77** — close as superseded by the
  already-planned xdist-stack retirement (AIRA-33) or, for AIRA-69, as cosmetic
  (confirmed the mis-placed test cgroups are invisible to every sweeper and correctly
  charged against real memory — not the leak risk originally suspected).

---

## Two findings worth surfacing on their own, outside the ticket-bucket counts

### AIRA-93's root cause was actually found, not just re-described

The `reconcile --rebuild` journal-corruption error is real and still reproduces, but
it's not a live allocator race. The two colliding receipts
(`AIRA-1`/`LIFE-1`) are dated inside the exact incident window already documented and
fixed as **AIRA-46**: a Go test that leaked an inherited `GIT_DIR` and wrote into this
repository's real audit directory instead of its own throwaway one. `AIRA-46`'s fix
scrubbed `GIT_*` in the git **hooks**; the `aira` binary's own git-invoking helper
(`gitValue`/`runGitRevParse`) still inherits the environment unscrubbed, so the same
class of leak remains possible from the binary itself, not just from hooks. Fix is
small: scrub `GIT_*` in that one helper, add a `TestMain` guard, and do a one-time
removal of the two stale receipts. `--rebuild`'s refuse-and-report behaviour was
correct throughout and should stay — the fix is to the leak, not the refusal.

### AIRA-33 (retire the xdist stack) is now more blocked than the plan thought, not less

The plan's snapshot recorded AIRA-33 as waiting on a dogfood precondition the owner
then resolved. Since then, fastest-ee actually merged its full migration to aitest
(#1124) — and then had to **pin three legs (`hosted`/`services`/`pipeline`) back onto
the xdist stack**, specifically because of tonight's AIRA-91 findings, with a comment
in their own Makefile: *"Remove `FASTEST_NO_AITEST=1` once AIRA-91/92 are fixed and
re-verified."* Eleven fastest-ee conftests still register the xdist governor whenever
aitest isn't explicitly selected, and AIRA's own CLI still exports the coordinates that
arm it unconditionally on every `--delegate-ram` launch. So the xdist stack is
currently functioning as fastest-ee's safety net *against* AIRA-91, not legacy cruft
waiting to be swept — the honest precondition for deleting it is "AIRA-91 fixed", full
stop, no narrower reading is defensible. This sharpens AIRA-91's priority rather than
AIRA-33's: fixing the truncation bug is now also the unblock for a backlog item, not
just a standalone honesty concern.

---

## Recurring bug classes and concrete prevention proposals

Independently rediscovered by more than one cluster, which is itself signal that these
are real, general classes rather than one-off coincidences:

1. **Unstructured/substring-matched error classification.** Appears in AIRA-42, 45,
   83(b), 87, and a cousin in AIRA-73/93 (`strings.Contains` on `"unique"`,
   `"database is locked"`, raw error prose in `install.go`, `gitremote/client.go`,
   `runner_linux.go`). *Prevention:* a semgrep rule flagging
   `strings.(Contains|HasPrefix|HasSuffix)($ERR.Error(), ...)` in Go (require
   `errors.Is`/`errors.As`/a typed code comparison instead), and its Python mirror —
   `"E_..." in message` — with a required table test that feeds every value the Go
   side can actually emit through the Python classifier and asserts each maps to
   exactly one outcome, unknown values always erroring rather than silently degrading.

2. **Unbounded blocking I/O with no timeout.** The exact class AIRA-92 fixed tonight;
   recurs as smaller residues in AIRA-37 and as the deadline-mismatch class above.
   *Prevention:* semgrep for Python (`$P.stdout.readline()`, `$P.wait()`,
   `os.waitpid($PID, 0)` with no `timeout=`) and for Go
   (`$CONN.Read(...)`/`$CONN.Write(...)` in a function with no `SetDeadline` and not
   inside a `context.WithTimeout`), plus a `forbidigo` rule restricting
   `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline` calls to one designated seam file
   so the policy has exactly one place to audit.

3. **A ledger/reservation charged or held independently of the kernel object it's
   supposed to track.** AIRA-28, 29, 39, 41, 16(2), and historically #74 and AIRA-68.
   *Prevention:* one generic property test per ledger, run against a fake cgroupfs:
   for any interleaving of {grant, create, release, kill, restart, rmdir}, the charged
   total must never fall below the sum of actually-existing capped children. One test
   catches every future instance of this shape instead of needing one per ledger.

4. **Digest one snapshot, evaluate another.** AIRA-72 (fixed), 78, 80, 81.
   *Prevention:* the captured-subject type described above (structural fix #3) plus one
   generic property test across every gate/canary kind: "for a stored pass, mutating
   any tracked file — including a `.gitignore`-matched one, a mode bit, a symlink
   target — invalidates it; it is never possible to obtain a digest of anything other
   than the bytes actually handed to the evaluator."

5. **"Default is green."** AIRA-56, AIRA-86, sibling of the already-fixed AIRA-54.
   Recurring seed-then-demote pattern found at four separate sites tonight
   (`check.go:130`, `gate_eval.go:99`, `gate_eval.go:597`, `gate_ratchet.go:80`).
   *Prevention:* every verdict/dimension must seed `unevaluated`, never `pass`, at
   every site that initializes one — this is a convention, not a tool, but it's cheap
   to grep for as a review checklist item (`grep -n '"pass"' internal/store/*.go`
   near any seeding/initialization code).

6. **Writes to `state.db` outside the daemon's single-writer discipline.** AIRA-85 (a
   detached supervisor opens its own read-write handle), and structurally, nothing
   currently prevents a second one appearing. *Prevention:* `depguard`/`forbidigo`
   forbidding `store.Open(` (the RW constructor) outside `internal/daemon` and the
   documented bootstrap path, backed by a daemon-held flock on `state.db` that any RW
   opener must observe at runtime, not just by convention.

---

## What's left as genuine individual engineering

Real, narrow, no shortcut — honestly reported as such by the sweep rather than
force-fit into a pattern: AIRA-24 (queue-position UX, small), AIRA-40 (replace a proxy
liveness signal — pipe EOF — with a real one, `pidfd` in the same `select()`, small),
AIRA-43 (a missing test harness for an existing behaviour), AIRA-60 (collapse three
near-identical path-safety predicates into one, ~4 lines), AIRA-70 (attribute a
confined job's SIGKILL to its actual cause — signal handler, remote kill, or OOM — all
currently byte-identical from the outside; the OOM half is a pre-existing computed
value dropped by a presentation gate, so partly free), AIRA-79 (a git-submodule gate
digest gap — genuinely no submodule-bearing project exists yet, correctly deferred),
AIRA-85 (fold the detached supervisor's writes into the existing relay path), AIRA-86
(seed all fourteen honesty dimensions `unevaluated`, add the per-dimension tests the
programme plan already calls for), AIRA-20 (a real package-wide pass to replace fixed
wall-clock test deadlines with condition-based waiting or an environment-scalable
deadline helper — the biggest single remaining item in this bucket), AIRA-22 (`confine
--detach` is a genuine multi-milestone feature, not cleanup — should converge with the
existing detached-run machinery rather than duplicate it), AIRA-23 (owner falls back to
a literal "unknown" string; replace with a stable non-colliding identity), AIRA-34
(mechanism confirmed but a real production consumer only exists on one launch path;
correct call is to document and keep, not build, unless that path starts gating on it),
AIRA-82 (re-scoped: not fabricated metadata as originally framed — the daemon correctly
records the cwd it was given; the actual gap is that a sub-agent or MCP-server cwd can
silently select the wrong project scope — small, individual fix), and the two small
residues left in AIRA-37 after four of its six original sub-items turned out to already
be fixed by AIRA-30 and AIRA-92.

## Correctly blocked, not actionable now

**AIRA-32** and **AIRA-33** both wait on AIRA-91's root cause (see above — the
precondition has hardened, not softened). **AIRA-91** itself remains open; this sweep
adds one negative result worth keeping (`uv run`, which sits in every affected
fastest-ee leg's process chain, correctly propagates a SIGKILLed child as exit 137, not
0 — ruling out that specific intermediary as the source of the exit-0 mystery) but does
not close it. The cgroup-tree-as-ledger fix (structural fix #1) removes one plausible
contributing mechanism but, on its own terms, predicts the wrong exit code for what's
been observed — so it's worth building for its own sake, but shouldn't be mistaken for
an AIRA-91 fix once it lands.
