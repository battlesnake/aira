---
{"schema":1,"id":"AIRA-187","project":"aira","title":"aira confine gives no warning when nested inside another confine scope, competing with its own already-covering parent reservation","status":"done","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","ux"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-187","to":"AIRA-191"},{"kind":"relates","from":"AIRA-187","to":"AIRA-194"},{"kind":"relates","from":"AIRA-187","to":"AIRA-195"}]}
---

Peer report (split, 2026-09-08), verified from source. split's own initial
framing ("deterministic self-starvation, can happen alone on an empty
slice") was walked back before this ticket was filed — narrower, confirmed
claim only: a nested `aira confine` call inside an already-admitted parent
scope competes for slice admission as an ordinary, independent request,
which under real contention can consume the full admission wait and
produce an unrun leg. Observed live twice tonight, resolved once (queue
drained, leg ran), not the other time. Their own concrete case (a
`make merge-gate` step shelling out to a nested `aira confine -- go test
./...` for one leg) is their own fix to make in their own gate script, not
an aira defect — see below.

## Verified from source

A nested `aira confine` call does not stay inside its parent's cgroup: it
deliberately moves the child into a brand-new **sibling** scope
(`UseCgroupFD`/`CgroupFD: scope.FD()`, `internal/runner/confine_linux.go`)
and requests independent admission, with no relation to the parent's
existing reservation. AIRA already recognises and solves exactly this
shape of problem for one case: `ExclusiveHolderEnv`
(`AIRA_CONFINE_EXCLUSIVE`, `confine_linux.go:1669-1688`) is a "nesting
token" whose own doc comment names this precise scenario — "a nested
confine resolves to the SAME slice and creates a SIBLING scope — which its
own parent's exclusive hold would block, deadlocking the benchmark against
itself. The token exempts it." That exemption is specific to `--exclusive`
holders. `InheritedConfineScopeID` (`confine_linux.go:1690-1705`), which
reads the coordinate a nested call could use to recognise its own parent,
is consulted only by the separate `aira confine-reserve` verb (the
delegate-ram/aitest sub-reservation path) — an ordinary nested `aira
confine` invocation, like split's, never looks at it at all.

**split's own option (c) is correct and complete for their case.** Cgroup
v2 memory accounting is hierarchical: a plain child process (no `aira
confine` wrapper) launched from inside an already-confined process tree
stays in the parent's own scope by ordinary fork/exec inheritance, and its
memory counts against the parent's already-granted reservation for free —
zero new admission wait, zero competition, since the parent's reservation
is sized for its own peak and by construction covers it. Nesting a second,
independent `aira confine` inside an already-confined command is
unnecessary and is the actual bug, in the caller's own script.

## The narrower gap that survives — a cheap, additive warning, not new admission machinery

`AIRA_CONFINE_SCOPE_ID` (the coordinate exported by
`AppendConfineChildEnvironment`, read by `InheritedConfineScopeID`) is
already available to every nested launch today; a plain `aira confine`
invocation simply never looks at it or says anything about it. A
low-cost, honesty-only addition — matching this project's own precedent
of naming a true, already-detectable condition rather than staying silent
(the same instinct behind `sliceceiling`'s "not applied" clauses and
`aira check`'s `unevaluated`-with-reason) — would be: when a plain,
non-`--exclusive`, non-`confine-reserve` `aira confine` launch detects
`AIRA_CONFINE_SCOPE_ID` set in its own environment, print a warning line
naming the parent scope and stating plainly that this nested request
competes independently for slice admission rather than using the parent's
already-granted reservation. No refusal, no new admission path, no
behaviour change to any existing test — purely a truth-telling addition
at the CLI layer.

## Not designed here

Exact wording, whether the warning should also suggest the fix (run the
command directly instead of wrapping it) or stay purely descriptive, and
whether this belongs as a `confine --list`-adjacent check or purely a
launch-time stderr line, are left for whoever picks this up. Genuine
grant-inheritance machinery (split's options a/b) is explicitly NOT
recommended here — the cheap fix already exists and lives entirely in the
caller's own script for this class of case; building new admission
machinery to route around callers unnecessarily double-confining would be
exactly the kind of complexity this project's architectural-simplicity
preference argues against.


## Review record (2026-09-09)

PR #117 merged (`5a39425`), all CI checks green (build+vet+gofmt, test,
race). The automated review agent hit a session-limit interruption before
recording this on the ticket itself; verified independently against
GitHub (merge SHA, check results) and closed out here.
