---
{"schema":1,"id":"AIRA-42","project":"aira","title":"redesign the Go CLI to Python classification boundary as structured k=v, not substring-matched prose","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-45","to":"AIRA-42"}]}
---
Found by Fable build-review (final gate, AIRA-30). Explicitly flagged as
"recommended redesign as a dedicated follow-up ticket, not a merge-blocker
by itself."

**The problem.** The `aira worker-admit` CLI's non-grant responses are
flattened to prose uniformly prefixed `E_CONFINE_UNAVAILABLE` with one flat
exit code; `supervisor.py`'s `acquire_worker` re-derives the real
classification via FIVE substring probes ("worker-admit denied",
"worker-admit timeout", "worker-admit unevaluated", "reject:exceeds-
ceiling", "worker-admit local-placement-failed", "E_CONFINE_ARGUMENT_
INVALID") whose fallthrough default is the maximally-unsafe outcome
(`WorkerAdmitUnavailable` -> strip containment for the rest of the run).

AIRA-38's three patched instances plus two more found in this same final
gate (the `--estimated-bytes` floor rejection, the daemon's own upper-
bound `admitMaxReserve` rejection) make FIVE recurrences of the identical
bug class, each "fixed" by adding one more substring to match. The pattern
itself is the fragility, not any one instance of it.

**Failure scenario.** Any future unrecognized stderr shape -- a new Go
error path, a reworded message, a wrapped errno string -- silently
classifies a reachable-daemon condition as daemon-unavailable and runs
the remaining suite unconfined (loudly warned once, but never recovering
containment). The boundary today holds together only via same-binary
lockstep (the CLI and the plugin are compiled into the same `aira`
binary) plus a small number of hand-copied test pinnings, not by
construction.

**Recommended direction (Fable's own suggestion).** The CLI already emits
a structured `k=v` shape for the grant case (`granted scope=... worker_id=...
memory_max=... memory_high=...`). Emit the same shape for non-grants too
(`declined state=denied reason=...`, `state=placement-failed reason=...`,
`state=argument-invalid reason=...`), have the Python side match exact
`state=` values instead of substrings, and make "unavailable" an
EXPLICITLY produced state rather than the classifier's implicit
fallthrough default -- so an unrecognized/garbage response can never
silently default to stripping containment.

Not urgent enough to block Slice 1 merge (Fable's own gate verdict), but a
real structural weakness worth closing before this pattern grows a sixth
substring. relates AIRA-30, AIRA-38.

## Resolution — done (Phase 1 Fix 2, backlog-remediation plan §3.3)

Plan: `docs/superpowers/plans/2026-09-04-aira42-45-structured-outcome-channel-plan.md`.

The whole substring cascade is **deleted, not supplemented**. `aira
worker-admit` now writes exactly one machine-readable line to stdout in EVERY
outcome — grant or not, including the pre-dispatch argument failures that used
to produce no outcome at all — carrying `state`, `class`, `reason` and a
query-escaped `detail`. `class` is the load-bearing disposition, defined once in
Go (`internal/runner/worker_admit_outcome.go`) and mapped to an exception by
ONE exact dictionary lookup on the Python side. There is no fallthrough default:
`_parse_worker_admit_outcome` refuses any value outside the catalogue, so the
old behaviour ("an unrecognised shape means the daemon is gone; run the rest of
the suite unconfined") is not representable.

The daemon's own copy of the same defect went with it —
`workerAdmitConnection`'s `strings.HasPrefix(response.Reason, "reject:")` poll
break is now `response.Class == request-invalid`.

The ticket's own recommended direction is what was built, with one addition it
did not name: "unavailable" is not merely explicitly produced, it is one of
exactly TWO containment-stripping classes, and an unrecognised outcome now
routes to a new terminal `contract-violation` class instead — loud, honest,
and never a silent fallback.

Guards against recurrence: `TestSupervisorClassifiesWorkerAdmitByEnumNotBySubstring`
fails if any retired prose token reappears anywhere in `supervisor.py` or if
`acquire_worker` grows a message-inspection idiom;
`TestWorkerAdmitOutcomeVocabularyMatchesTheSupervisor` holds the Go and Python
catalogues equal in both directions. Seven mutations were applied and all seven
were killed (plan §9).

Bumps `daemon.ProtocolVersion` **6 → 7** (and its pinned runner copy,
`runner.DaemonProtocolVersion`). Originally written as 5 → 6; AIRA-39 landed
first and took 6 for its own semantics change, so this shape change takes 7.
Both branches independently wrote `= 6`, so the runner's copy auto-merged with
no conflict — the pin test only holds the two sides equal, not correct.
