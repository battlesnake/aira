---
{"schema":1,"id":"AIRA-125","project":"aira","title":"Two catalogue entries sit on the wrong side of the state-conflict/bad-request rule","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty"],"hold":false,"relations":[]}
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
