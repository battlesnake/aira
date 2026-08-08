# Adversarial verification

Passing an author's own tests is not evidence of correctness. AIRA's durable
coordination state has failure modes at process, filesystem, database, and git
boundaries, so correctness-critical work is red-teamed after its ordinary tests
pass.

## Two-loop method

### Loop 1: attack the delivered design or implementation

Give reviewers distinct attack angles and require runnable counterexamples:

- crash recovery: kill between every persistence step and inspect the repaired
  state;
- concurrency: use several short-lived processes and worktrees, including
  overlapping allocation, lease claim, heartbeat, steal, and release;
- identity and integrity: forge tokens, duplicate IDs, create dangling
  relations, and submit ambiguous selectors;
- determinism and completeness: vary process order, filesystem order, and
  partial state; no valid record may disappear and no invalid state may become a
  pass;
- boundary and scope: look for a per-project state accidentally replacing the
  machine-wide state, or Phase-1 implementation leaking into later phases.

The adversary's default is to refute. A plausible concern becomes a finding only
after a minimal executable reproduction or a precise counterexample against the
design invariant.

### Loop 2: attack the fixes

Review each correction as new work. Check that the fix did not introduce a
second source of truth, order sensitivity, a widened selector, a forged holder,
a lost journal event, or a new race. Re-run the original counterexample and add
the corrected case to the regression corpus.

## Required invariant families

- **Atomicity:** one allocation/lease winner is observable, never two winners or
  none after a committed operation.
- **Durability:** every allocated ID resolves to a live or explicitly retired
  record, and every missing derived artifact is reconciled visibly.
- **Completeness:** all records in the source of truth are indexed or honestly
  reported as unevaluated; no silent truncation.
- **Fail-closed integrity:** duplicate IDs, dangling ticket relations, forged
  lease holders, and ambiguous selectors are refused.
- **Honest verdicts:** an unrun or uncomputable check is `unevaluated`, never a
  fake pass.
- **Determinism:** equivalent inputs and replay order produce the same canonical
  state and event interpretation.

## Record of a review

The review record names the scope, attack angle, command or reproduction, result,
and exact fix. Confirmed findings become tests or explicit changes to the design
spec. A clean review is not evidence that a path was exercised unless the test
or probe proves that it ran.
