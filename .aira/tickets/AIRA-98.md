---
{"schema":1,"id":"AIRA-98","project":"aira","title":"confine --detach's record store and captured output are unpruned and uncapped","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["detach","hygiene","runner"],"hold":false,"relations":[]}
---
Recommended by AIRA-22's own build review (confine --detach, PR #39) but not filed by that agent, to avoid racing ticket-ID allocation.

confine --detach writes a durable per-job record plus captured output under ~/.local/state/aira/confine/<scope-id>/ (internal/runner/detach_control.go / detach_protocol.go). Nothing currently prunes old finished-job records or caps how much output a single detached job can accumulate. Same general class as AIRA-88's other machine-local unbounded stores (registry.jsonl, lock-file inodes), not investigated or scoped here.

Not urgent; recorded so the finding is not lost.

## Resolution (2026-09-05, backlog-completion triage)

Verified against current master (3251bed). The ticket is factually correct that nothing prunes or caps the store — but stale in where it points (real code is internal/runner/confine_detach.go + confine_detach_linux.go, not detach_control.go/detach_protocol.go). confine_detach_linux.go:264-276 hands raw O_APPEND files to the child with no cap; :371-407 rescans every record dir per --status; the only bound is a read-side 8 MiB LimitReader on record.json (:449-452) whose own comment rejects truncation as the wrong conservative direction. Not superseded: AIRA-101 (905d6a6) and AIRA-102 (plan :258, "exactly as ~/.local/state/aira/confine/ already needs pruning") extended the store; AIRA-104 will extend it further.

Why no action is warranted under this project's own bar:
(1) CLAUDE.md requires coverage gaps be "written down and accepted by reviewers; never silent." This gap already is: AIRA-22 plan :65 lists it out of scope, §7 :684-690 states the honest consequence (ENOSPC -> terminal write fails -> outcome-unknown, never a fabricated pass), and review row 17 (:806) records DeepSeek's P1 with disposition "Deferred, named" — passed through the plan gate.
(2) The honesty invariant holds by construction: a filled disk degrades to outcome-unknown, never to a fake pass or fake finish. There is no correctness exposure.
(3) Both proposed fixes add machinery that destroys evidence: a per-job cap silently truncates the captured output the operator asked for (and the foreground form and run --detach are equally uncapped, so it buys no class-level win); a pruner deletes the ONLY record of a job's outcome, turning a later --status into "no such job" for a job that existed — exactly the conflation the store's ReadError design exists to prevent. Architectural-simplicity rule (owner, 2026-08-26): prefer keep-the-primitive + document-the-gap over new machinery.
(4) Direct in-repo precedent: AIRA-88 (d3a6765) closed three same-class unbounded stores (registry.jsonl, locks/, pylib/) with an explicit "stays unbounded, no code" decision, on the identical reasoning. run --detach's <common>/aira/runs/output/ (ledger.go:58) is the parity baseline and is also unpruned.
(5) On-disk today: 0 record dirs, 4 KB. Unlike AIRA-88 there are no growth numbers — stated honestly rather than assumed harmless. Every --status output prints the record/stdout/stderr paths, so manual cleanup of Terminal:true dirs is discoverable and safe.

Not needs-owner-decision: the owner already made this exact class of call (AIRA-88) and the plan gate already accepted the deferral; re-asking is the over-asking the work-autonomously rule forbids. Close with an AIRA-88-style resolution note recording the explicit decision, and name the reopen trigger: measured store growth (multi-GB) or perceptible --status latency from the full-directory rescan.


*Disposition: Closed — not needed, reached via a source-verified triage pass (Fable model) as part of the backlog-completion push, independently spot-checked by the coordinating session before closing.*
