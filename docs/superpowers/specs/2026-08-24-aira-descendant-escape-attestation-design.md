# Descendant-migration escape attestation (runner containment hardening)

Status: PLAN v1. Milestone #20. Closes the containment residual explicitly named in
`runner_linux.go:508-516`: a confined descendant that migrates out of the run scope
(writes its pid into a foreign `cgroup.procs`) and outlives the leader escapes the
memory cap, and the current daemonless runner — which verifies only the LEADER's
membership — cannot attest it. Honesty-first: never claim containment we did not
witness.

## 0. Threat + the honest posture

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
  captured start-tick still matches) but **absent** from `cgroup.procs` AND whose
  `/proc/<pid>/cgroup` no longer names the scope path is a **proven descendant
  escape** → record the new state **`ScopeDescendantEscaped`** (with the escaped
  pid/identity + the foreign cgroup for the audit). This is the descendant analogue of
  the existing leader-migration detection, reusing `processIdentityMatches`/
  `processLive`.
- A sub-tick escape (migrate + exit between two samples) remains **inherently
  unobservable** without a persistent supervisor — documented honest limitation
  (matches the `:1391` comment); it is never counted as containment.

## 3. Teardown attestation + reclamation (leader exited)

At the leader's terminal reap, BEFORE scope removal, read `Populated()`:
- **`populated == 0`** — the scope is empty: no descendant remains. Combined with NO
  witnessed escape → the existing `ScopeContained` now honestly covers the whole tree
  (every member exited in-scope). A witnessed escape overrides to
  `ScopeDescendantEscaped` regardless.
- **`populated == 1`** — descendants OUTLIVED the leader but are STILL IN the scope
  (contained, not escaped). These are ours to reclaim: `scope.Kill()` (cgroup.kill,
  atomic) → re-read `Populated()` → `0` confirms reclamation. Record
  **`ScopeDescendantOutlived`** (honest: descendants outlived the leader; contained
  and reclaimed) — distinct from `contained` (clean) and from `escaped` (left).
- **Unreadable `populated` / a monitoring gap** (monitor started after the leader
  already exited, or `cgroup.events` unreadable) → `ScopeResidualUnverified`
  (unevaluated — cannot attest descendant containment), never a fabricated `contained`.

`IsSuccess` stays gated on `contained` only; `descendant-escaped`/`outlived`/
`residual-unverified` are honestly non-green (they are real containment facts, not
failures of the run's own exit).

## 4. States (extend `types.go`)

Add `ScopeDescendantEscaped = "descendant-escaped"`, `ScopeDescendantOutlived =
"descendant-outlived"`, `ScopeResidualUnverified = "residual-unverified"`. Precedence
when multiple apply: **escaped > outlived > residual-unverified > contained** (the
worst witnessed containment fact wins; a proven escape is never masked by a later
clean-empty). Thread through `classifyMembership`/the reap path/`evidence.go` merge +
the RunRecord. AIRA is not live → no migration concern.

## 5. Scope + deferrals

- **In scope:** runner-side full-tree attestation during the leader's lifetime +
  teardown `populated`-based reclamation & honest states, for both foreground `run`
  and `confine` (confine already verifies leader membership at
  `confine_linux.go:349`; add the teardown `populated` check + reclamation there too).
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
  a leader spawns a child that (a) migrates itself to a sibling cgroup and outlives
  the leader → the run is attested `descendant-escaped`, the child's pid/foreign-cgroup
  recorded; (b) a child that outlives the leader but STAYS in the scope → `populated==1`
  → `scope.Kill()` reclaims → `populated==0` → `descendant-outlived`; (c) all children
  exit in-scope → `contained`. PID-reuse guard: a recycled pid (start-tick mismatch)
  is never counted as an escape.
- **No-regression:** a single-process `run`/`confine` with no descendants classifies
  exactly as today (`contained`); the leader-migration path is unchanged.
- `-race` across the monitor goroutine + reap.

## 7. Errors / honesty

No new error codes — the attestation is a RunRecord/ConfineStatus *fact*, not a
failure. Never claim `contained` for a witnessed escape or an unattested residual;
`descendant-escaped`/`outlived` carry the pid + foreign-cgroup evidence for the audit.
