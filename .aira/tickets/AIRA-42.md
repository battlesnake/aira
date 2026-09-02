---
{"schema":1,"id":"AIRA-42","project":"aira","title":"redesign the Go CLI to Python classification boundary as structured k=v, not substring-matched prose","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-45","to":"AIRA-42"}]}
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
