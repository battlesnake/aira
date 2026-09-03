---
{"schema":1,"id":"AIRA-81","project":"aira","title":"Canary re-materialization drops tracked-but-ignored files, so a canary can fire on the drop rather than on its mutation","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["gate","honesty"],"hold":false,"relations":[]}
---
Found by the Fable plan gate during the AIRA-72 two-loop, as a pre-existing defect adjacent to but distinct from AIRA-72. Not fixed there.

## Defect

The mutation-canary path materializes the subject (`materializeTrackedSnapshot`), applies the mutation, re-stages with `git add -A`, and then `runCommandChecker` materializes *again* from that mutated root. A file that is both tracked in the source and matched by the source's own `.gitignore` (or the user's `core.excludesFile`) is copied to disk by the first materialization but is not picked up by the intermediate `git add -A`, so it is absent from the second materialization's `git ls-files --cached` view.

The canary can therefore fire because a file **disappeared**, not because the declared mutation perturbed anything. `E_GATE_CANARY_DID_NOT_FIRE` is a hard fail, so the failure mode here is the opposite: a canary that fires for the wrong reason still mints proof-of-fire, and proof-of-fire is what licenses a trusted pass. The proof is real but it attests to the wrong perturbation.

Note AIRA-55's `injectFile` doc comment already documents the sibling case — an injected file matched by git excludes is dropped and surfaces as `E_GATE_CANARY_DID_NOT_FIRE`, loudly. This is the same interaction in the direction that is *not* loud.

## Direction

Preserve source index membership across materialization rather than re-deriving it: stage the copied entries explicitly (`git add -f` for the captured set, or construct the index directly) so the materialized tree's tracked set is exactly the source's tracked set. AIRA-72 already removed the digest half of this divergence by digesting the captured entries instead of the re-indexed tree; this is the remaining execution half.
