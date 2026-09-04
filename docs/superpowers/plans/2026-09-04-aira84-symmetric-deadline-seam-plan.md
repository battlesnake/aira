# AIRA-84 — Symmetric deadline-policy seam (backlog remediation Phase 1, Fix 4)

Executes §3.4 of `docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md`.
Tier A (full two-loop). Closes **AIRA-84**. Built on `origin/master` `4d51b23`
(the Fix 2 merge, PR #24 — confirmed present before starting).

## 1. The defect, re-verified against source (not against the plan's line numbers)

`internal/daemon/server.go:535` stamps every accepted connection with

```go
_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
```

— one constant covering **read and write, for the whole connection lifetime**.
Handlers that know better replace it for their own path:

| path | what it does | site |
|---|---|---|
| store-ops | `SetReadDeadline(zero)` then `SetWriteDeadline(storeOpWriteTimeout)` before the write | `server.go:556,558` |
| admit / governor / worker-admit | `SetReadDeadline(zero)`, own their frame, stamp `admitWriteTimeout`/`governorWriteTimeout` before each write | `server.go:592,603,615`; `admit.go:780,1254,1263,1272`; `governor.go:644`; `worker_admit.go:749,871` |
| watch | `SetReadDeadline(zero)`, stamps `watchWriteTimeout` before the write | `server.go:643,651` |
| **generic routed verbs** | **nothing — writes at `server.go:676` under the connect-time deadline** | — |

So a routed verb whose work outlives the connect stamp (`aira import` of a large
tree, `gate attest` over a big subject digest, `reconcile --rebuild`) **commits
durably and then fails the response-frame write**, which the client can only
report as `RequestOutcomeUnknownError`. That is an honesty failure, not a
performance one.

**The client end has the identical defect, and fixing only the daemon would not
fix the user-visible symptom.** `internal/daemon/protocol.go:357-361`:

```go
if deadline, ok := ctx.Deadline(); ok {
    _ = conn.SetDeadline(deadline)
} else {
    _ = conn.SetDeadline(time.Now().Add(30 * time.Second))
}
```

Every CLI entry point builds its request context from `context.Background()`
(`cmd/aira/main.go` — no deadline), so the `else` arm is the production path:
the client gives up on the response after 30 s and returns
`RequestOutcomeUnknownError` even when the daemon is about to answer correctly.
Both ends must move together; that is what "symmetric" means here.

### Line-number drift from the parent plan — recorded honestly

The parent plan's §3.4 cites `server.go:527,155,668` and `protocol.go:337`.
Against `4d51b23` those are `server.go:535` (connect stamp), `server.go:163`
(`storeOpWriteTimeout` initialiser; `:129` is its field), `server.go:676` (the
routed write), and `protocol.go:339` (`exchange`'s signature; `:357-361` is the
deadline itself). Same four sites, ~8 lines of drift from the intervening Fix 1
and Fix 2 merges. No claimed site was missing or misidentified.

## 2. The convention (the actual deliverable)

This is the third fix of this class tonight (AIRA-18, AIRA-92), so the output is
a **named convention with one shared type**, not a third one-off patch:

```go
type deadlinePolicy struct {
    Connect      time.Duration // the HANDSHAKE only
    ResponseWait time.Duration // the client's wait, when the caller declares none
    Write        time.Duration // stamped immediately before each response write
}
```

Three rules, applied on both ends of the socket:

1. **Connect bounds the handshake only** — the client's request-frame write and
   the daemon's request-frame read. Framing a request is never slow work, so one
   short fixed budget is right for it. It must never survive frame parse.
2. **The response wait is never the connect deadline.** On the client it is the
   caller's own context deadline when there is one, else `ResponseWait`. On the
   daemon there is no response wait — the daemon *produces* the response; its
   work budget is verb-declared and lives in the handler (`storeOpAppendTimeout`
   / `storeOpHeavyTimeout` chosen per op at `storeops.go:186-189` is the existing
   example).
3. **A write deadline is stamped immediately before each response write**, sized
   by the writer, so it bounds the write and never the handler's work. Violating
   (3) is precisely the AIRA-84 defect.

## 3. Scope

**In scope (the parent plan's four sites):**

- `server.go:535` — connect stamp becomes `s.deadlines.Connect`, and is
  explicitly narrowed to the handshake by hoisting the read-deadline clear (§4.1).
- `server.go:676` — the routed write, stamped under rule (3).
- `server.go:129,163` — `storeOpWriteTimeout` becomes `deadlines.Write`, the one
  daemon-owned response-write budget the routed path now shares. This is the
  "same treatment as store-ops" AIRA-84's own text points at.
- `protocol.go:357-361` — `exchange()` splits its single `SetDeadline` into a
  request-phase write deadline and a response-phase read deadline.

**Deliberately widened beyond the four sites, flagged for the reviewer.** Rule
(3) is applied to *every* response write in `serveConnection` that happens after
handler work, through one helper, rather than to the single line AIRA-84 names.
Reason: `confine-report` (`:567`), `confine-list`/`confine-kill` (`:574`),
`eject` (`:581`) and `supervisor-lease-*` (`:631`) sit in the same function and
have the same latent defect for the same reason; `eject` in particular tears down
a project and is the most likely of them to outrun a connect stamp. Fixing one
line and leaving four siblings would re-create the exact asymmetry the ticket
calls "the tell". This introduces **no fifth constant** — it reuses
`deadlines.Write` — so the parent plan's stated boundary ("four constants, so
symmetric has a defined boundary") is respected.

**Out of scope, unchanged:**

- `internal/daemon/storeops.go:186-192` and `server.go:161-162`
  (`storeOpAppendTimeout` / `storeOpHeavyTimeout`) — `context.WithTimeout`
  execution *budgets* for an op's own work, a different question ("how long may
  this operation run") from a socket deadline ("how long may this connection sit
  idle"). Verified against source: `:190-192`'s `timeout <= 0` fallback is
  unreachable in the normal case, both defaults being positive.
- `cmd/aira/watch.go:18` (`watchExchangeTimeout`) — a long-lived streaming
  connection; needs its own design. It is also the one caller that *does* declare
  a context deadline, so it keeps today's exact behaviour for free under §2 rule (2).
- `internal/daemon/watch.go:20` (`watchWriteTimeout`) at `server.go:651` — already
  correct under rule (3), deliberately tighter for a streaming path. Untouched.
- `admit.go` / `governor.go` / `worker_admit.go` write deadlines — already
  correct under rule (3), each with its own justified budget. Untouched.
- `internal/runner/admission_linux.go`, `worker_admit_client_linux.go`,
  `supervisor_lease_linux.go` — separate transports with their own deadline
  handling, not named by AIRA-84 or the parent plan.

## 4. Design

### 4.1 Daemon (`server.go`)

- `Server` gains `deadlines deadlinePolicy`; the `storeOpWriteTimeout` field is
  **deleted** and its single use becomes `s.deadlines.Write`.
- Defaults: `Connect: 30s`, `Write: 30s`. **Both values are unchanged from what
  they replace.** What changes is the connect deadline's *scope*, not its size:
  shortening it would be an unforced behaviour change (a 16 MiB
  `MaxFrameBytes` request frame is legal, and store-op bodies are framed
  separately), and the defect is that the connect deadline outlives the
  handshake, not that it is too long.
- **One hoisted read-deadline clear.** Immediately after the inbound frame is
  parsed and its proto/state-identity checks pass, `conn.SetReadDeadline(zero)`
  runs once. This *deletes* the four scattered clears at `:556,592,603,615`
  (`:643`'s watch clear too) — verified by reading `:554-676`: no path reads the
  connection between the hoist point and those sites, so the behaviour is
  identical and the invariant "a connect deadline never survives frame parse"
  becomes locally provable. Net −4 lines.
- Two small helpers stamp rule (3):

```go
func (s *Server) reply(conn net.Conn, frame any) bool           // writeFrame
func (s *Server) replyStoreOp(conn net.Conn, frame ResponseFrame) bool // writeResponse (+body)
```

  Applied at `:538,548,552` (early error frames — no behaviour change, they are
  inside the connect window anyway; uniformity), `:559` (store-op, exactly
  today's behaviour), `:567,574,581,622,631,635`, and `:676` (the AIRA-84 site).

  **The panic writer at `:531-532` is deliberately NOT changed** (revised after
  plan-review). It borrows `watchWriteTimeout` (5 s), which has nothing to do
  with watch — but it already satisfies rule (3) (it stamps immediately before
  its write), and moving it to `deadlines.Write` would triple the worst-case
  linger of a post-panic write from 5 s to 30 s against `DrainTimeout`'s 10 s
  shutdown budget. That is a behaviour change nobody asked for, in exchange for
  naming tidiness. The borrowed constant is recorded as a wart in §7, not
  silently fixed under this ticket.

### 4.2 Client (`protocol.go`)

`exchange` takes the policy as a parameter (unexported; `Exchange` /
`ExchangeStoreOp` pass `defaultDeadlines`), so tests drive it with a small policy
instead of mutating a package-level var.

```go
// request phase — the connect budget, or the caller's deadline when sooner
requestDeadline := now.Add(policy.Connect)
if d, ok := ctx.Deadline(); ok && d.Before(requestDeadline) { requestDeadline = d }
conn.SetWriteDeadline(requestDeadline)
... write frame ...
// response phase — the caller's own deadline, else the declared budget
responseDeadline := now.Add(policy.ResponseWait)
if d, ok := ctx.Deadline(); ok { responseDeadline = d }
conn.SetReadDeadline(responseDeadline)
... read response ...
```

`ResponseWait = 10 * time.Minute`, **derived, not picked**: it must exceed the
daemon's largest sanctioned unit of work (`storeOpHeavyTimeout`, 5 min) plus its
response-write allowance (30 s), or the client re-creates AIRA-84's defect from
the other end — giving up on work the daemon is still legitimately doing and
reporting `OUTCOME_UNKNOWN` for a mutation that lands seconds later.

**Accepted cost, stated rather than hidden:** against a *wedged* daemon (one
holding the connection open without answering — not a crashed one, which closes
the socket and yields an immediate EOF) a CLI call now blocks up to 10 min
instead of 30 s. SIGINT still terminates it (only `aira watch` installs a signal
handler; every other path takes the default disposition), so the user retains
control. This trade is the point of the fix: a rare pathological hang is a
better failure than routinely fabricating `OUTCOME_UNKNOWN` for work that
committed.

**Not built: a per-verb budget table on the daemon.** The parent plan says "the
caller's context/signal **or** a verb-declared budget"; this implements the
former plus a derived default, and records the latter as a deliberate
non-goal — routed verbs declare no daemon-side work budget today, inheriting the
serve context, and inventing a table would be new machinery against the owner's
standing simplicity preference. Named here as a documented gap, not omitted
silently.

## 5. Tests (TDD — each written failing first)

**Honesty note on "fails first", corrected after self-review.** These tests
cannot literally be run against `master`: they need the injectable
`server.deadlines` / `policy` parameter, which master does not have, so they do
not compile there. Their falsifiability is established by **mutation** instead —
build the seam type, then remove the fix and confirm the test goes red. Claiming
"fails on master" would be false; the mutation table below is the real evidence.

Slow handlers are injected through the **existing `OnRequest` seam**, which
`serveConnection` calls immediately before every one of the widened handlers and
before the generic routed path. This is deterministic — not "hope the handler is
slow" — which was a porousness finding from plan-review.

1. `TestRoutedReplySurvivesAHandlerThatOutlivesTheConnectDeadline` — real socket;
   `deadlines.Connect` ~50 ms, `OnRequest` sleeps ~300 ms, generic routed verb;
   assert the response frame arrives.
2. `TestConfineReportReplySurvivesTheConnectDeadline` — same mechanism on a
   *widened* site (`:567`), proving the helper is really used there and not only
   on the one line AIRA-84 names.
3. `TestExchangeResponseWaitOutlivesTheConnectBudget` — stub listener reads the
   request, sleeps past `policy.Connect`, then replies; assert the client returns
   the response rather than `RequestOutcomeUnknownError`.
4. `TestExchangeStillHonoursACallerDeadline` — a caller ctx deadline shorter than
   `ResponseWait` still bounds the wait against a listener that never replies
   (this is `aira watch`'s contract; a mutation that always uses `ResponseWait`
   makes this test hang and fail).
5. `TestExchangeDeadlinePhases` — table test over the two pure derivation
   helpers (`requestPhaseDeadline`, `responsePhaseDeadline`): no-deadline caller,
   caller deadline shorter than `Connect`, caller deadline between `Connect` and
   `ResponseWait`, caller deadline beyond `ResponseWait`. Replaces an earlier
   "listener that never reads" idea, which was porous: a small frame fits in the
   socket buffer, so the request-phase write succeeds with nobody reading.
6. `TestStoreOpReplyStillUsesTheDaemonOwnedWriteDeadline` — the store-op path
   keeps today's behaviour after `storeOpWriteTimeout` → `deadlines.Write`.
7. `TestDefaultResponseWaitExceedsTheDaemonsLargestWorkBudget` — pins §4.2's
   derivation (`ResponseWait > storeOpHeavyTimeout + Write`) so a future edit to
   either constant cannot silently re-open the defect.

**Mutation testing** (adversarial pass, run and recorded in the PR): revert each
of (a) the hoisted read clear, (b) `reply`'s `SetWriteDeadline`, (c) the client's
response-phase `SetReadDeadline`, (d) the client's request-phase deadline, (e)
`responsePhaseDeadline`'s caller-deadline arm; each must turn at least one test
red. A test that stays green against the wrong implementation proves nothing.

## 6. Risks

| risk | mitigation |
|---|---|
| CLI hangs longer against a wedged daemon | §4.2 accepted cost; SIGINT still works; a crashed daemon still yields immediate EOF |
| Hoisting the read clear changes a path that *does* read again | Verified by reading `server.go:554-676` in full; `watch`'s post-response read goroutine and the admit/governor/worker-admit handlers all clear it themselves today at the same effective point |
| Fresh write deadline masks a genuinely stuck client | The write deadline is per-write and finite (30 s), so a stuck reader still frees the connection |
| Widening to sibling writes is out of the plan's stated boundary | No new constant; §3 flags it explicitly for the reviewer to reject if unwanted |
| Protocol/behaviour drift vs. the deployed daemon | Wire format is untouched; **no `ProtocolVersion` bump is needed or made**. A mixed old-client/new-daemon pair simply keeps the old client's 30 s response cap |

## 7. Deferrals

- Per-verb daemon-side work budgets for routed verbs (§4.2).
- `cmd/aira/watch.go:18` and the streaming path generally (parent plan §3.4).
- A `forbidigo` rule pinning the convention — parent plan Phase 2, tooling.
- **The panic writer at `server.go:531` borrows `watchWriteTimeout`** for a
  non-watch path. Cosmetic; changing it is a behaviour change (§4.1), so it is
  recorded here rather than folded in.

## 8. Plan-review record

Codex/Sol was **unavailable** for this plan (OpenAI usage limit, "try again
Sep 7"), so the external lineages are Gemini and DeepSeek-pro rather than the
usual Sol+one. Recorded, not papered over.

**Gemini — `VERDICT: APPROVE-WITH-CHANGES`:**
- *[P1] `ResponseWait=10m` hangs scripted CLI callers against a deadlocked
  daemon; add a flag/env override or drop the default to 2 m.* **Partly
  accepted, partly rejected.** The hang is real and is now stated as an accepted
  cost (§4.2). The proposed remedies are rejected: an env/flag knob is exactly
  the new machinery the owner's simplicity rule forbids, and a 2-minute default
  would re-break the daemon's own 5-minute heavy store-op budget — re-creating
  AIRA-84 from the client end, which is the defect being fixed.
- *[P1] Early error frames must write under `deadlines.Write`.* **Accepted** —
  already in §4.1 (`:538,548,552`).
- *[P2] Test 3 is porous — it passes if the handler simply finishes fast.*
  **Accepted**, and it was right: §5 now drives the delay through the existing
  `OnRequest` seam instead of assuming handler slowness.
- *GAP: moving the panic writer from 5 s to 30 s increases post-crash linger.*
  **Accepted** — the panic writer is no longer touched (§4.1).

**DeepSeek-pro — `VERDICT: APPROVE-WITH-CHANGES`:**
- *[P1] `ResponseWait=10m` is overbroad; use the minimal derived headroom.*
  **Accepted** — 10 m → **6 m** (`storeOpHeavyTimeout` 5 m + `Write` 30 s + 30 s
  margin), the smallest the derivation permits, cutting the accepted worst-case
  hang by 40%.
- *[P1] The "caller deadline still bounds the wait" test is porous: `exchange`'s
  pre-existing `context.AfterFunc` closes the connection at the caller's
  deadline, so an implementation that never set a read deadline returns fast
  anyway and the test passes against the wrong code.* **Accepted, and it was
  right** — this is exactly the class the project cares about. The integration
  test was rebuilt around a caller with NO deadline and a tiny `ResponseWait`
  (nothing can mask it), asserting a socket **timeout** specifically rather than
  "it returned"; caller-deadline precedence moved to the pure table test, where
  no transport behaviour can mask a wrong answer.
- *[P2] Add a guard for the "no post-parse reads" invariant.* **Partly
  accepted:** stated as an explicit INVARIANT comment at the hoist rather than a
  brittle source-scanner (simplicity rule).
- *[P2] Early error frames should not get a fresh 30 s window.* **Accepted, and
  it reverses Gemini's opposite P1** — resolved in DeepSeek's favour: handshake
  rejections keep the handshake deadline, which is both the smaller change and
  the crisper rule ("a handshake rejection is part of the handshake").
- *[P2] Mixed new-client/old-daemon still fails past 30 s.* **Accepted** —
  documented in the ticket and commit as a deployment note.

## 9. Build-review record

Sol unavailable (usage limit) and Gemini's daily quota exhausted before the
build review, so DeepSeek-pro was the only external lineage on the built diff,
alongside a repo-internal `/code-review` pass. Recorded, not papered over.

**DeepSeek-pro — `VERDICT: APPROVE-WITH-CHANGES`:**
- *[P1] `ResponseWait` is derived from `storeOpHeavyTimeout`, but generic routed
  verbs have no daemon-side budget at all, so a legitimate >6 m handler still
  commits and still reports `OUTCOME_UNKNOWN`.* **Accepted — this is the
  sharpest finding of the whole review.** It exposes a real hole in the
  derivation's own justification: the client budget is derived from a budget
  that does not bound the path being fixed. Not fixed (a per-verb routed budget
  is the new machinery this change deliberately does not build), but the
  overclaim is removed: `defaultDeadlines`' doc comment now states plainly that
  the fix **narrows the window from 30 s to 6 m and does not close it**, and the
  commit and ticket say the same. Claiming closure would have been the exact
  dishonesty this project exists to prevent.
- *[P1] The structural guard is porous — it skips the reply helper bodies.*
  **Half-accepted, half-refuted with evidence.** Refuted: mutation M removed
  `reply`'s own `SetWriteDeadline` and two behavioural tests went red, so helper
  bodies ARE covered — the guard covers call sites, the behavioural tests cover
  bodies, and the two are complementary. Accepted: a NEW helper hiding a bare
  write is genuinely uncovered until the Phase 2 lint rule lands. The test's doc
  comment now states this boundary instead of implying full coverage.
- *[P2] `supervisorLeaseRequest` receives `conn`, and a read there would now hang
  forever.* **Verified hypothetical, and hardened anyway** — that function
  touches `conn` only for `SO_PEERCRED` (no read), so there is no live defect;
  but it is the one post-handshake handler handed a connection without owning a
  framed reader, so it now carries an explicit doc note naming the invariant.
- *[P2] A handshake rejection can fail if parsing consumed the Connect budget.*
  **Acknowledged, not changed** — behaviour is unchanged from before this fix,
  and the reviewer's own alternative ("or add an explicit test accepting that
  failure mode") is what was done, as a documented rationale at the site.
- *[P2] `cmd/aira/watch.go` still caps at 30 s.* **Out of scope** by the parent
  plan (§3.4); unchanged.
- *Porous: `TestDefaultResponseWait...` only pins arithmetic.* **True by
  design** — it is a drift guard for the derivation, and says so.

**Mutation results** (each fix reverted in turn; every one must turn a test red):

| # | mutation | result |
|---|---|---|
| A | routed write reverted to bare `writeFrame` | caught |
| B | confine-report write reverted | caught |
| C | hoisted read-deadline clear deleted | caught |
| E | client response-phase `SetReadDeadline` deleted | caught |
| F | full pre-fix client revert (one `SetDeadline` for both phases) | caught (2 tests) |
| G | `responsePhaseDeadline` ignores the caller deadline | caught (3 cases) |
| H | `requestPhaseDeadline` ignores a sooner caller deadline | caught |
| I | `ResponseWait` shrunk below the daemon work budget | caught |
| J | store-op reply loses its write deadline | **SURVIVED** → test was porous (fast `ensure-scope` replied inside the connect window); test rebuilt on a genuinely slow op, then caught (J2) |
| K | eject write reverted | caught |
| L | supervisor-lease write reverted | caught |
| M | `reply`'s OWN `SetWriteDeadline` removed | caught (2 tests) |
| D | request-phase `SetWriteDeadline` → `SetDeadline` | **SURVIVED — inert mutation**, not a porous test: the response-phase `SetReadDeadline` immediately overrides it, so the two forms are genuinely equivalent and no test can distinguish them. `SetWriteDeadline` is a clarity choice there, not a behavioural one. |
