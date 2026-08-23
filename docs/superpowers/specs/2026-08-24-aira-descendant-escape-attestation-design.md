# Descendant-migration escape attestation (runner containment hardening)

Status: PLAN v2 — **Fable code-gate GATE-PASS** (folded 5 must-fixes: subtree-not-
direct-member escape proof [a nested cgroup is contained], reuse `ScopeDescendantKilled`
not a new state, precedence-aware `mergeEvidence`, wire the REAL classifier
`classifyLaunchScopeIntegrity` not the test-only mirror, detach deferral; + `Empty()`/
`CleanSuccess` naming) and **Sol adversarial review** (the load-bearing honesty reframe:
sampling PROVES escape but never whole-tree NON-escape, so `contained` stays leader-only
and a multi-process run is residual `unverified`; pidfd/double-start-time identity;
snapshot-before-kill to distinguish killed-from-escaped; escape/reclaimed/completeness
are orthogonal facts). Milestone #20. Closes the containment residual explicitly named in
`runner_linux.go:508-516`: a confined descendant that migrates out of the run scope
(writes its pid into a foreign `cgroup.procs`) and outlives the leader escapes the
memory cap, and the current daemonless runner — which verifies only the LEADER's
membership — cannot attest it. Honesty-first: never claim containment we did not
witness.

## 0. Threat + the honest posture

**The load-bearing honesty limit (Sol): sampling PROVES escape, never proves
whole-tree non-escape.** A descendant can fork and migrate out BEFORE its first 2ms
sample — it never enters the ever-member set, so a clean-empty scope at teardown
proves only "empty now", NOT "every descendant stayed contained". Therefore this
milestone **never upgrades `ScopeContained` to a whole-tree claim**: `contained` keeps
its existing LEADER-only meaning; a run that spawned descendants has descendant
containment that is inherently **unverified** unless a specific escape was *witnessed*.
The attestation's positive power is bounded to: (1) PROVE a witnessed escape, (2)
reclaim lingering in-scope descendants, (3) honestly label the residual — never
fabricate whole-tree containment.

A confined child spawns a descendant; the descendant migrates itself into another
cgroup (escaping the scope's `memory.max`/`oom.group`) and keeps running. **Prevention
is not tractable unprivileged** — a cgroup namespace (`CLONE_NEWCGROUP`) only
virtualises the `/proc/self/cgroup` view and needs a user namespace to create
unprivileged; it does not stop an absolute-path write to `/sys/fs/cgroup/<other>/
cgroup.procs`. So this milestone is **honest ATTESTATION**, not prevention: detect and
truthfully report descendant escape/containment, and reclaim what we still own. Full
prevention (namespaces / seccomp-blocked cgroup writes) is a recorded deferral.

## 1. What exists (build on it)

- `ScopeIntegrity` states (`types.go:28-38`): `contained`, `migrated`,
  `descendant-killed`, `unverified`, `handoff-unverified`. `IsSuccess`
  (`types.go:136`) requires `contained`.
- The leader monitor `monitorScopeMembership` (`runner_linux.go:1389`) watches the
  LEADER pid via a 2ms ticker + `cgroup.events` and returns "migrated" if the leader
  is live but absent from `cgroup.procs`.
- `scope.Members()` = `cgroup.procs` (`cgroup_linux.go:224`); `Populated()` reads the
  authoritative `cgroup.events populated` bit (`cgroup_linux.go:239-249`);
  `scope.Kill()` = `cgroup.kill` (whole-scope, atomic).
- The runner already refuses to use an unobservable descendant to claim *additional*
  positive containment (the `:508-516` comment). This milestone ADDS witnessed
  descendant attestation + reclamation, without weakening that honesty.

## 2. Full-tree membership attestation (during the leader's lifetime)

Extend the scope monitor from leader-only to **full-membership**:
- Alongside the leader check, snapshot the FULL `cgroup.procs` each tick and maintain
  an **ever-member set** (pids ever seen in the scope, with their PID-identity =
  start-tick to defeat PID reuse).
- A pid in the ever-member set that is later **alive** (`/proc/<pid>` exists AND its
  captured start-tick still matches — `processIdentityMatches`/`processLive`, which
  already maps `Z/X`→dead so an exited-but-lingering `/proc` entry is NOT an escape)
  but whose `/proc/<pid>/cgroup` `0::` path is **neither equal to NOR a descendant of
  the scope path** is a **proven descendant escape** → record the new state
  **`ScopeDescendantEscaped`** (with the escaped pid/identity + the foreign cgroup for
  the audit).
- **Subtree, NOT direct-member (Fable MUST-FIX 1 — load-bearing):** `cgroup.procs`
  lists only DIRECT members, and `memory.max` is HIERARCHICAL, so a descendant placed
  in a **nested cgroup INSIDE the scope** (e.g. a nested AIRA runner — `Create` mkdirs
  `.aira-<id>` under the current cgroup, `cgroup_linux.go:170`) is absent from
  `cgroup.procs` yet fully CONTAINED. The proof therefore compares the descendant's
  cgroup path against the scope **subtree** (equal-or-under, via the mount+rel join at
  `cgroup_linux.go:63-75`) — a path at or under the scope is contained; only a path
  outside it is an escape. A false escape claim is the exact honesty violation this
  milestone exists to prevent.
- **Race-safe identity (Sol):** hold a `pidfd` on each member when first observed (or
  re-validate start-time BEFORE *and* AFTER reading `/proc/<pid>/cgroup` — start-tick
  alone can theoretically collide across a reuse). Attribute the finding as **"observed
  alive outside the scope"**, NOT "self-migration" — we cannot prove the process moved
  itself vs was moved.
- An **unreadable `/proc/<pid>/cgroup`** (hidepid, race) → the proof cannot complete →
  **never** an escape (residual-unverified at worst). A sub-tick escape (migrate +
  exit between two samples) is **inherently unobservable** without a persistent
  supervisor — documented honest limitation (matches `:1391`); never counted as
  containment.

## 3. Teardown attestation + reclamation (leader exited)

At the leader's terminal reap (the site is `runner_linux.go:761` `scope.Empty()` →
integrity classify `:762-776` → terminal `:777-800` → `scope.Remove()` `:844`), use
`scope.Empty()` (the real primitive; **there is no `Populated()`** — `Empty()` reads
`cgroup.events populated` and is HIERARCHICAL, so it correctly covers nested
sub-cgroups at teardown, unlike lifetime `Members()`):
- **Empty (`populated==0`)** — the scope holds nothing now. `ScopeContained` is
  recorded ONLY for a run that never had a descendant (leader-only — sampling then
  fully attests it, unchanged from today). A run that DID spawn any descendant gets the
  residual `ScopeUnverified` for descendant containment (a pre-first-sample
  fork-and-migrate cannot be excluded — §0/Sol), NEVER a whole-tree `contained`. A
  witnessed escape overrides to `ScopeDescendantEscaped` regardless.
- **Not-empty (`populated==1`)** — descendants OUTLIVED the leader but are STILL in the
  scope SUBTREE (contained, not escaped). **Snapshot the current member identities
  BEFORE `Kill()` and re-validate after (Sol #3):** `populated→0` alone cannot
  distinguish "killed" from a member that migrated OUT between the read and the kill —
  only snapshotted members confirmed absent-and-dead are `descendant-killed`; a
  snapshotted member found alive-outside is an escape; any enumeration/read gap →
  residual `unverified`, never a claimed reclamation. Reclaim: `scope.Kill()`
  (cgroup.kill, atomic) → re-check. Record the **EXISTING `ScopeDescendantKilled`**
  (`types.go:38`) — the PTY-quiesce (`:686`) and capture-kill (`:752`) paths ALREADY
  stamp this exact fact (descendants outlived, in-scope, killed by us), so we UNIFY on
  it rather than add a duplicate `outlived` state (Fable MUST-FIX 2). **This REPLACES
  the current `:768-770` `!empty → ScopeHandoffUnverified + E_RUN_SCOPE_HANDOFF`
  branch** — an explicit behaviour change: a still-populated scope at reap is now
  actively reclaimed + attested `descendant-killed`, not left `handoff-unverified`.
- **Unreadable `Empty()` / a monitoring gap** (monitor started after the leader already
  exited) → the existing `ScopeUnverified`/`ScopeHandoffUnverified` (residual — cannot
  attest descendant containment), never a fabricated `contained`.
- **PTY no-regression:** a `--pty` run reaches teardown AFTER `quiescePTYScope`
  (`:1213`) has already Killed + emptied the scope, so teardown must NOT reclassify it
  — it is already `descendant-killed`/empty; the new branch is a no-op there.

`CleanSuccess` (`types.go:134`, the real name — not `IsSuccess`) stays gated on
`ScopeContained` only, so `descendant-escaped`/`descendant-killed`/residual are
automatically non-green — real containment facts, not failures of the run's own exit.

## 4. States (extend `types.go`)

Add exactly ONE new state — `ScopeDescendantEscaped = "descendant-escaped"` — and
REUSE the existing states for the other facts (Fable): `ScopeDescendantKilled` for
outlived-then-reclaimed (§3), `ScopeUnverified`/`ScopeHandoffUnverified` for the
residual/gap. Define a **total precedence** over ALL states (the worst witnessed
containment fact wins; a proven escape is NEVER masked by a later clean-empty):
`descendant-escaped` > `migrated` > `descendant-killed` > `unverified` >
`handoff-unverified` > `contained`. **Orthogonality caveat (Sol #4):** only
`contained` (leader-only, no descendants) is a POSITIVE containment attestation;
`descendant-killed` records that we reclaimed a lingering in-scope survivor but does
NOT erase the residual (other descendants may have escaped pre-first-sample), and
`unverified` IS that residual — none of the non-`contained` states claims whole-tree
containment. The precedence only decides which single honest label a record carries,
never upgrades confidence.

- **Wire the pure classifier into the REAL path (Fable MUST-FIX 4):** the shipped
  logic is `classifyLaunchScopeIntegrity` (`runner_linux.go:1330`) + the inline reap
  block (`:761-807`); `classifyMembership` (`decisions.go:34`) is a **test-only mirror
  with no production caller** — §6's pure tests MUST target the classifier the launch
  path actually calls, or they prove nothing.
- **Precedence-aware merge (Fable MUST-FIX 3):** `mergeEvidence` (`evidence.go:78-80`)
  is **last-writer-wins** today — a later `contained` would clobber an earlier
  `escaped`. Change it to take the max-precedence state, with a test proving a
  later-`contained` cannot mask an earlier-`escaped`.
- Which codes stamp the records: `descendant-escaped` reuses the migration-class code
  (`E_RUN_SCOPE_MIGRATION`, `:764`); `descendant-killed` reuses `E_RUN_DESCENDANT_KILLED`
  (`:687`). AIRA is not live → no migration concern.

## 5. Scope + deferrals

- **In scope:** runner-side full-tree attestation during the leader's lifetime +
  teardown `populated`-based reclamation & honest states, for both foreground `run`
  and `confine` (confine already verifies leader membership at
  `confine_linux.go:349`; add the teardown `populated` check + reclamation there too).
- **Detach — explicitly DEFERRED (Fable MUST-FIX 5):** the detached path has NO
  membership monitor (only a one-shot check at `detach_linux.go:452`) and deliberately
  never asserts `ScopeDescendantKilled`; full-tree attestation for a detached run whose
  supervisor outlives this process belongs to the persistent daemon (the scope owner),
  not this runner-lifetime increment. Detached runs keep today's behaviour; silence is
  not implied attestation.
- **Deferred (recorded):** the DAEMON post-leader-exit attestation (a descendant that
  escapes AFTER the runner's monitor stops — the persistent daemon that owns the scope
  is the right place, a follow-up subsystem like the reconciler); `CLONE_NEWCGROUP` +
  mount/seccomp PREVENTION (needs userns / a bigger sandbox); per-descendant rusage.

## 6. Tests (TDD; pure decision + gated real-cgroup)

- **Pure (extend `decisions.go` + a new classifier):** full-membership → integrity
  mapping — a live ever-member absent from the scope with a foreign cgroup → escaped;
  populated-after-leader → outlived; clean empty + no escape → contained; monitor gap
  / unreadable populated → residual-unverified; the precedence order
  (escaped>outlived>residual>contained). Proven RED against the wrong impl.
- **Real-cgroup (gated `AIRA_REAL_CGROUP=1`, `cgrouptest.IsolatedScopeParent`):**
  a leader spawns a child that (a) migrates itself to a **sibling** cgroup (outside the
  scope subtree) and outlives the leader → attested `descendant-escaped`, child's
  pid/foreign-cgroup recorded; (b) a child that outlives the leader but STAYS in the
  scope → `Empty()` false → `scope.Kill()` reclaims → `Empty()` true → the existing
  `ScopeDescendantKilled`; (c) a **leader-only** run (no descendants) → `contained`
  (fully attested); a multi-process run whose children all exit in-scope → the residual
  `unverified` (NOT `contained` — a pre-first-sample escape cannot be excluded, Sol).
- **Adversarial (Sol):** a child that forks + migrates out BEFORE the monitor's first
  sample and outlives the leader → the run is `unverified` (or `escaped` if later
  witnessed), NEVER `contained` — proves sampling can't fabricate whole-tree
  containment. A member that migrates out between the pre-kill snapshot and `Kill()` →
  NOT falsely `descendant-killed` (the snapshot-verify catches it → residual or escape).
- **Nested-cgroup NOT escaped (Fable MUST-FIX 1 regression):** a child placed in a
  cgroup NESTED under the scope (e.g. `.aira-<id>/child`) is absent from the scope's
  direct `cgroup.procs` but its cgroup path is UNDER the scope → attested **contained,
  NOT escaped**. Prove RED against a naive exact-path compare.
- **PID-reuse guard:** a recycled pid (start-tick mismatch) is never an escape.
  **Unreadable `/proc/<pid>/cgroup`** mid-lifetime → never an escape (residual at worst).
- **No-regression:** a single-process `run`/`confine` with no descendants classifies
  exactly as today (`contained`); the leader-migration path is unchanged; a `--pty` run
  (scope already quiesced/emptied) is NOT reclassified by the new teardown branch.
- **Precedence-merge test:** a record that saw `escaped` then a later `contained` merge
  stays `escaped` (proves the max-precedence `mergeEvidence`).
- `-race` across the monitor goroutine + reap.

## 7. Errors / honesty

No new error codes — the attestation is a RunRecord/ConfineStatus *fact*, not a
failure. Never claim `contained` for a witnessed escape or an unattested residual;
`descendant-escaped`/`outlived` carry the pid + foreign-cgroup evidence for the audit.
