---
{"schema":1,"id":"AIRA-53","project":"aira","title":"aira gate add/set do not materialize anything; skill docs describe a working verb","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["docs","dogfood","gate"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-54","to":"AIRA-53"}]}
---
## Symptom

`aira gate add <id> --checker command --predicate exit-zero --argv true --cwd root` (the exact example from the skill/dispatch docs) does not create a gate. On a fresh `aira init`ed project it returns `E_NOT_FOUND: gate not found`, `aira gate ls` stays `[]`, and no `.aira/gates/` directory is ever created. `set` behaves identically. Reported by a peer session (`field`) dogfooding gates on a real (non-Go) project; independently verified against source below, not taken on trust.

## Root cause (verified by direct source read)

`internal/core/core.go`'s dispatch table (~line 2016) declares `add` with `Summary: "Add a gate definition"` and a full flag example (`--checker`, `--predicate`, `--argv`, `--cwd`, `--env-allow`, `--timeout-ms`, `--output-cap-bytes`, `--parser`). The handler (core.go ~1877-1943) does parse all of these into `inputFields` via `gateDefinitionInputFields` and passes them to `gs.GateActionWithFields(ctx, subverb, gate_id, canary_id, inputFields)` when the store implements `gateActionInputStore` — which the real store does.

But `internal/store/gate_eval.go:463-466`:
```go
func (s *Store) GateActionWithFields(ctx context.Context, operation, gateID, canaryID string, fields map[string]any) (any, error) {
	_ = fields
	return s.GateAction(ctx, operation, gateID, canaryID)
}
```
`fields` is explicitly discarded, and `GateAction`'s `"add"` case (gate_eval.go:365-375) is a pure `ListGates()` + linear-scan-by-ID lookup that returns `E_NOT_FOUND: gate not found` when the gate doesn't already exist on disk as a hand-authored `.aira/gates/<id>.json` file. There is no code path anywhere that writes a gate definition file. A doc comment directly above `GateActionWithFields` (gate_eval.go:459-462) says "Gate files remain the authenticated source of truth; fields are passed through for callers that materialize content changes and are not used to mint a verdict directly" — suggesting `add`/`set` were deliberately left as a plumbing seam for some not-yet-built materialization path, not that they were meant to be inert forever. Whatever the intent, the CURRENT behavior does not match the CURRENT docs.

## Impact

Any agent (or person) planning off the skill/dispatch documentation will write a plan around `aira gate add` and have it silently do nothing useful — worse, it fails with `E_NOT_FOUND`, which reads like "you targeted the wrong gate" rather than "this verb doesn't do what its docs say." This is exactly the kind of documentation-reality mismatch that erodes trust in the whole tool for an agent operating on doc-trust alone.

## Suggested direction

Either (a) implement `add`/`set` to actually write `.aira/gates/<id>.json` from the parsed fields (schema-validated, matching `gate.GateDefinition`), or (b) change the dispatch-table `Summary`/`Example` to say gates are hand-authored JSON files under `.aira/gates/`, and that `add`/`show` are a read-back/validation check against an existing file rather than a creation verb. (a) is probably the right fix long-term since a "definition creation verb that doesn't create" is a strange permanent API shape, but (b) is the immediate, cheap fix to stop misleading agents today.

## Done — merged `cf81344` (PR #8), 2026-09-03

`gate add` / `gate set` now materialize a real definition. `GateActionWithFields`
no longer discards `fields`: it builds a `gate.GateDefinition`, validates it via
`gate.RenderGate` (so nothing is written unless it validates), and writes
`.aira/gates/<id>.json`. When the input carries a mutation seed it also writes
`.aira/gates/canaries/<canary-id>.json`.

Direction (a) from the ticket was taken. It completed existing machinery rather
than adding new: the flags were already declared, already parsed by
`gateDefinitionInputFields`, and already plumbed to this seam — only the terminal
write was missing. The `--mutation-*` flags already present in `add`'s own
argument list were the evidence that gate + canary materialization was the
original intent.

Safety properties, each covered by a test:
- The canary is written FIRST. Only the gate file is discoverable, so a failure
  between the two writes can never leave a discoverable gate whose declared
  canary is missing.
- `add` refuses `E_GATE_EXISTS` instead of silently overwriting, and a refused
  add leaves the original bytes untouched.
- `set` re-checks the digest of what it parsed under the path lock and returns
  `E_WRITE_CONFLICT` on a concurrent rewrite (matching `materialiseIntent`).
- Where a field has no safe default the verb REFUSES rather than guessing:
  ratchet gates, a missing `--dimension`, a `check-dimension` value other than
  `traceability` (which `EvaluateDimension` cannot evaluate), a missing
  `--timeout-ms`, non-numeric integers, and a derived canary id that breaks the
  64-char slug limit.
- The result reports `CanaryStatus` / `IndexStatus` honestly and warns that a
  canary-less gate cannot be proven and holds readiness.
- `GateAction` now refuses bare `add`/`set` rather than returning a lookup that
  looks like a successful creation.

One new flag (`--dimension`) was added, wired at all three required sites, and
`canary_id` is now advertised per-operation.

Also fixed here, same fabricated-green class, found by adversarial plan review:
`gate prove` returned an unevaluated result unwrapped, so the response defaulted
to OK/exit 0 for a gate that was never proven; and `prove`'s summary claimed
"Record proof of fire" when it records nothing (only RunGate/AttestGate mint
proof from real canary evidence, which is the point of the security model, so
the docs moved to match reality). Its `Safety` was deliberately left unchanged,
since re-classifying it changes request routing.

Merged after `cd02dda` (AIRA-55), so materialization also round-trips the new
`inject-file` kind including `mutation_content` — dropping that field would have
been the same silent-drop defect this ticket fixed.

Evidence: build 0, vet 0, targeted 0, full suite 0 (12/12 packages, zero
failures). Every fix mutation-tested — reintroduced in a throwaway copy with the
matching test required to fail; all 7 failed as required, no porous tests.
Plan and full review record: `docs/plans/2026-09-03-aira53-54-gate-honesty-plan.md`.
