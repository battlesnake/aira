# Fix 2 — structured daemon↔client worker-admit outcome channel

**Date:** 2026-09-04
**Status:** BUILT AND MERGE-READY. Two-loop complete:
plan review by Codex/Sol (GATE-FAIL) and DeepSeek-pro (GATE-PASS-WITH-CHANGES)
→ §8; TDD build; adversarial build review of the merged diff by Codex/Sol
(BLOCK, 1×P0) and DeepSeek-pro (BLOCK) → §10, with the P0 and four other
findings fixed and mutation-verified. Nine mutations applied, nine killed (§9).
**Parent plan:** [`2026-09-04-backlog-remediation-plan.md`](2026-09-04-backlog-remediation-plan.md) §3.3 (Phase 1, Fix 2, Tier A)
**Closes/moots:** AIRA-42, AIRA-45; closes AIRA-83(b)
**Explicitly NOT in this PR:** AIRA-87's `store.ExitCodes` leaf-package move
(separate mechanical follow-on, per §3.3 — folding a repo-wide move into a
Tier-A wire-shape change would make the PR unreviewable).

> **Read §12 before §2.3 and §5.** Fix 1 (AIRA-39, `11221d3`) merged to master
> while this fix was in build review, and it rewrote the same evaluator this
> fix reclassifies. §12 records the integration: which rows of the §2.3 daemon
> table no longer exist, which four rows Fix 1 added, and the two places where
> the merge changed a decision this plan had already made. Where §2.3/§5 and
> §12 disagree, §12 is what was built.

## 1. The defect, restated against current source

`aira worker-admit` is the only channel between the Go admission stack and the
Python aitest supervisor. Today it is **two** channels with **prose** on both:

- **stdout** carries the grant only: `granted scope=… worker_id=… memory_max=…
  memory_high=…` (`cmd/aira/main.go:1129`).
- **stderr** carries every non-grant as a free-text sentence uniformly prefixed
  `E_CONFINE_UNAVAILABLE:` with one flat exit code
  (`worker_admit_client_linux.go:103,116`, `main.go:1108,1126,1131`).

`supervisor.py`'s `acquire_worker` (`:534-704`) re-derives the real
classification from that prose with **eleven** substring probes across five
branches, whose fallthrough default (`:704`) is the maximally-unsafe outcome
(`WorkerAdmitUnavailable` → strip RAM containment for the rest of the run).
The daemon's own poll loop does the same thing to its own reason string
(`worker_admit.go:405`, `strings.HasPrefix(response.Reason, "reject:")`).

Two live consequences, both already ticketed:

- **AIRA-42** — six recurrences of "add one more substring". Any reworded Go
  error silently becomes "daemon unavailable" and the suite runs unconfined.
- **AIRA-45 / AIRA-83(b)** — `"E_DAEMON_PROTOCOL" in message` (`:655`) buckets
  the daemon's *protocol-version mismatch* (`server.go:540`, "daemon protocol
  is N, client requested M") together with its *argument* rejection
  (`validateWorkerAdmitArgs`). After a version bump — **which this very PR
  performs** — a stale client marks every queued test unevaluated with the
  factually wrong diagnostic "cannot be admitted at this sizing".

## 2. Design: one channel, exact-match enums

### 2.1 The channel

`aira worker-admit` writes **exactly one machine-readable line to stdout in
every outcome**, grant or not, and nothing else to stdout. stderr keeps a
human diagnostic that **nothing parses**.

```
aira-worker-admit state=granted class=granted scope=<p> worker_id=<n> memory_max=<n> memory_high=<n>
aira-worker-admit state=denied class=request-invalid reason=exceeds-ceiling detail=<query-escaped>
```

`aira-worker-admit` is a frame marker compared with `==` on the first
whitespace token, not a prefix search. Remaining tokens are `k=v`; `detail` is
`url.QueryEscape`d in Go and `urllib.parse.unquote_plus`ed in Python, so free
text can never break tokenisation and can never be mistaken for a field.

### 2.2 The vocabulary (single Go definition, `internal/runner/worker_admit_outcome.go`)

`internal/daemon` already imports `internal/runner`; `cmd/aira` imports both;
`internal/runner` cannot import `internal/daemon` (layering). So the shared
worker-admit outcome vocabulary is defined **once** in `internal/runner`, and
the daemon uses those constants — no second hand-copied table (the AIRA-83
item-3 class).

**`state`** — what happened (diagnostic, and the granted/non-granted switch):

| value | produced by | meaning |
|---|---|---|
| `granted` | daemon | admitted; grant fields present |
| `denied` | daemon | the daemon declined |
| `timeout` | daemon / client | a wait budget expired |
| `unevaluated` | daemon / client | the daemon answered but could not establish a result |
| `unavailable` | client | no usable daemon exchange happened |
| `argument-invalid` | CLI | this request's own arguments are wrong, pre-dial |
| `placement-failed` | CLI | the daemon granted; local scope creation failed |

**`class`** — the load-bearing disposition. This is the field the supervisor
acts on, and it replaces the `reject:`/`fallback:` prefix convention outright:

| value | supervisor exception | supervisor behaviour |
|---|---|---|
| `granted` | — | use the grant |
| `contended` | `WorkerAdmitDenied` | retriable; keep containment |
| `request-invalid` | `WorkerAdmitRequestTooLarge` | terminal for the queued work; daemon stays available |
| `admission-unusable` | `WorkerAdmitUnavailable` | fall back unconfined for the rest of the run |
| `placement-failed` | `WorkerPlacementFailed` | fall back unconfined |
| `contract-violation` | `WorkerAdmitContractViolation` | terminal, loud; **never** silently unconfined |

**`reason`** — a stable token, exact-match, never parsed for substrings.
**`detail`** — free text for humans, never parsed.

### 2.3 The mapping (every producible outcome, explicitly)

Daemon (`evaluateWorkerAdmit`, `workerAdmitConnection`):

| condition | state | class | reason |
|---|---|---|---|
| outer-scope read/parse failure | `unevaluated` | `contended` | `outer-scope-unreadable` |
| outer-scope `memory.max == "max"` | `unevaluated` | `admission-unusable` | `outer-scope-unbounded` |
| `estimated > ceiling` | `denied` | `request-invalid` | `exceeds-ceiling` |
| outer scope owned by another job | `denied` | `request-invalid` | `outer-scope-owned-by-another-job` |
| no live headroom right now | `denied` | `contended` | `insufficient-headroom` |
| supervisor scope unreadable | `unevaluated` | `contended` | `supervisor-scope-unreadable` |
| Σ caps + supervisor RSS over ceiling | `denied` | `contended` | `aggregate-cap-exceeded` |
| poll deadline expired | `timeout` | `contended` | `saturated` |
| grant | `granted` | `granted` | — |

Client (`RequestWorkerAdmit`) — **structural** discriminators only, no message
matching:

| condition | detected by | state | class | reason |
|---|---|---|---|---|
| dial failed | dial error | `unavailable` | `admission-unusable` | `dial-failed` |
| request write failed | write error | `unavailable` | `admission-unusable` | `request-send-failed` |
| response read timed out | `net.Error.Timeout()` | `timeout` | `contended` | `response-timeout` |
| response read broke | `io.EOF`/`ErrUnexpectedEOF`/`ECONNRESET`/`ECONNABORTED`/`EPIPE` | `unevaluated` | `contended` | `response-interrupted` |
| response frame unparseable | frame-size sentinel / `*json.SyntaxError` / `*json.UnmarshalTypeError` | `unevaluated` | `contract-violation` | `malformed-response` |
| any other read error (unreachable by construction — see §8 D2) | — | `unavailable` | `admission-unusable` | `response-failed` |
| **protocol-version mismatch** | `Code == E_DAEMON_PROTOCOL && Proto != 0 && Proto != DaemonProtocolVersion` | `unavailable` | `admission-unusable` | `protocol-version-mismatch` |
| other `E_DAEMON_PROTOCOL` | code, `Proto == 0` | `denied` | `request-invalid` | `request-rejected` |
| any other non-OK code | code | `unevaluated` | `contract-violation` | `daemon-error` |
| grant JSON unparseable | unmarshal error | `unevaluated` | `contract-violation` | `malformed-response` |
| daemon state/class not in catalogue | exact-match lookup | `unevaluated` | `contract-violation` | `unknown-daemon-outcome` |

**AIRA-45/83(b) is the `protocol-version-mismatch` row.** The discriminator is
structural: `protocolMismatchFrame` (`protocol.go:137-141`) is the only daemon
frame that sets `Proto`; `errorFrame` leaves it zero. `runnerAdmitResponseFrame`
gains a `proto` field so the client can see it. Both directions are pinned by a
test (a version-mismatched request carries `Proto`; a bad-argument request does
not).

CLI (`runWorkerAdmitCommand`):

| condition | state | class | reason |
|---|---|---|---|
| `--estimated-bytes` outside [1MiB, 1PiB], bad `--max-wait` | `argument-invalid` | `request-invalid` | `estimated-bytes-out-of-range` / `max-wait-invalid` |
| **pre-dispatch: unknown/missing option, or `--json`** | `argument-invalid` | `request-invalid` | `arguments-invalid` |
| daemon paths unavailable | `unavailable` | `admission-unusable` | `daemon-paths-unavailable` |
| `CreateWorkerScope` failed after a grant | `placement-failed` | `placement-failed` | `worker-scope-create-failed` |
| stdout write failed | (no line can be written) | — | — |

The pre-dispatch row is added on plan review: `parseArgs` failures
(`main.go:101`) and the `--json` refusal (`main.go:147`) run one layer ABOVE
`runWorkerAdmitCommand` and used to render an ordinary error, leaving the
supervisor with no outcome line at all — which it can only read as "the relay
produced nothing" and therefore as a reason to run unconfined. Both now speak
the channel.

### 2.4 The Python side — deletion, not addition

`acquire_worker`'s entire stderr-substring cascade (`supervisor.py:534-704`,
eleven `in message` probes) is **deleted**. It is replaced by:

1. parse the single stdout line into a `k=v` dict (frame marker `==`-checked);
2. `state == "granted"` → validate and return the grant, exactly as today;
3. otherwise look `class` up in one dict of exact enum values;
4. an unknown/missing `state` or `class`, a `state`/`class` disagreement about
   grantedness, or a malformed line → `WorkerAdmitContractViolation`;
5. **no outcome line at all** (the relay died, printed nothing, or was killed)
   → `WorkerAdmitUnavailable`, with the stderr text attached **as a diagnostic
   string only**.

Step 5 is the one place stderr is still read, and it is read to *quote*, never
to *classify* — the message is never inspected. That preserves today's
behaviour for a dead relay (the tested `worker-admit exited without a grant`
path) while making it an explicitly named condition rather than the
fallthrough default of a substring chain.

`WorkerAdmitContractViolation` is a new sibling of `WorkerAdmitRequestTooLarge`
under a new `WorkerAdmitTerminal` base; the three `except
WorkerAdmitRequestTooLarge` sites become `except WorkerAdmitTerminal`, and
`_fail_queue_too_large` is renamed `_fail_queue_terminal` with a message that
no longer asserts "at this sizing" for a condition that may not be a sizing
problem.

The two locally-raised denials keep their behaviour but move to the same
vocabulary: `fallback:fork-unavailable` → `class=contended
reason=fork-unavailable`, `fallback:relay-unresponsive` /
`fallback:placement-ack-timeout` likewise.

### 2.5 Protocol version

`daemon.ProtocolVersion 5 → 6` (`protocol.go:21`) and
`runner.DaemonProtocolVersion 5 → 6` (`admission_linux.go:84`), which
`TestRunnerDaemonProtocolVersionMatchesTheDaemon` already pins equal. **This
forces a coordinated atomic reinstall+restart at deploy time** (parent plan §8)
— the orchestrating session does the deploy, not this PR.

### 2.6 Behaviour changes, named rather than smuggled

1. **Version skew no longer reports "too large".** It reports
   `protocol-version-mismatch` → fall back unconfined with an honest warning.
   This matches `aira confine`'s own admission client for the same condition
   (AIRA-45's own note) and is the point of AIRA-83(b).
2. **`state=unevaluated` with `reason=supervisor-scope-unreadable` and
   `outer-scope-unreadable` stay retriable** — unchanged, but now by class
   rather than by the absence of a substring.
3. **`timeout`'s reason token is now `saturated`, not `reject:saturated`.**
   The coincidental-`reject:`-wording hazard that `supervisor.py:646-651` and
   `worker_admit.go:398-404` both had to document away disappears by
   construction.
4. **An unrecognised outcome is now terminal-and-loud, not silently
   unconfined.** This is a deliberate direction change: the old default
   stripped containment. It covers an uncatalogued `state`/`class`, a
   self-contradicting pair, an unparseable line, an unintelligible daemon
   frame, and an error code this client does not know.
5. **A malformed GRANT is terminal too, where it used to fall back
   unconfined.** `internal/pylib/aitest/test_supervisor.py`'s
   `test_malformed_worker_grant_falls_back_without_losing_collected_results`
   asserted the old behaviour and is rewritten (and renamed) to assert the new
   one. Justification: `WorkerAdmitOutcomeLine` REFUSES to render a granted
   line without placement fields, so seeing one means the two sides are out of
   lockstep — a contract violation, not evidence about the daemon. What did
   not change: no collected result is lost and no `KeyError` escapes
   `spawn_worker`.
6. **The pre-dispatch argument failures now emit an outcome** (§2.3). They
   previously emitted none, which the supervisor could only read as "the relay
   produced nothing" → run unconfined.

## 3. Invariants

- **I1.** Every code path in `RequestWorkerAdmit`, `runWorkerAdmitCommand` and
  `evaluateWorkerAdmit` produces a `class` from the catalogue. There is no
  unclassified error path (the Go client returns an outcome value, not an
  `error`, so this is enforced by the type).
- **I2.** No component classifies by substring on a message. `strings.Contains`
  /`HasPrefix`/`HasSuffix` over a reason/error/message is absent from this
  channel in Go, and `in message` is absent from `acquire_worker` in Python.
- **I3.** The containment-stripping classes are **exactly two** —
  `admission-unusable` and `placement-failed` — and each is produced only from
  positive evidence of its own named condition, never as a default for an
  unrecognised input. (Corrected on plan review: an earlier draft claimed
  `admission-unusable` was the only one, which is false — `WorkerPlacementFailed`
  reaches `_disable_daemon` at `supervisor.py:1095,1160,1492` too.)
- **I4.** The Go catalogue and the Python dispatch table hold the same class
  and state sets, pinned by a test that reads the embedded Python source.

## 4. Tests (as built)

**Go, `internal/daemon`** (`worker_admit_outcome_test.go`, `worker_admit_cli_boundary_test.go`):
- `TestEvaluateWorkerAdmitClassifiesEveryOutcome` — table over every daemon
  condition asserting `(state, class, reason)` exactly, plus catalogue
  membership.
- `TestWorkerAdmitPollLoopBreaksOnClassNotOnReasonSpelling` — a `request-invalid`
  verdict whose reason token no longer reads like a rejection must break on the
  FIRST evaluation; a `contended` one must poll to its deadline. Mutation-kill
  for the deleted `HasPrefix(reason, "reject:")` (M1 below).
- `TestProtocolMismatchFrameCarriesProtoAndArgumentRejectionDoesNot` — pins the
  structural discriminator in both directions.
- `TestWorkerAdmitResponseCarriesClassOnTheWire` — the class survives the JSON
  round trip, and no retired prefix appears in the wire shape.
- `TestWorkerAdmitCLIOutcomeChannelMatchesTheSupervisorBoundary` — the real
  binary against a real daemon, six sub-cases, all asserted on **stdout**.

**Go, `internal/runner`** (`worker_admit_outcome_test.go`, `worker_admit_classify_linux_test.go`,
and the pre-existing `worker_admit_client_linux_test.go`, kept and strengthened):
- `TestClassifyWorkerAdmitReadFailureSortsByType` — ten transport rows.
- `TestClassifyWorkerAdmitDaemonErrorSplitsVersionSkewFromArgumentRejection` —
  **the AIRA-45/83(b) regression test**, six rows including "matching proto is
  not a skew".
- `TestRequestWorkerAdmitClassifiesEndToEnd` — nine sub-cases over real unix
  sockets.
- `TestWorkerAdmitOutcomeLineRoundTrips` / `…RefusesInconsistentOutcomes` /
  `TestParseWorkerAdmitOutcomeLineRefusesUncataloguedInput`.
- `TestWorkerAdmitContainmentStrippingClassesAreExactlyTwo` — a new class must
  declare whether it strips containment.
- The three pre-existing `TestRequestWorkerAdmit*` tests keep their scenarios and
  now additionally assert the class each produces.

**Go, `cmd/aira`** (`worker_admit_outcome_test.go`):
- `TestRunWorkerAdmitCommandAlwaysWritesOneStructuredOutcome` — four
  argument-failure rows.
- `TestWorkerAdmitPreDispatchFailuresSpeakTheOutcomeChannel` — unknown option,
  missing option, `--json`.

**Go, cross-language pin (`internal/pylib/worker_admit_channel_test.go`,
external `pylib_test`):**
- `TestWorkerAdmitOutcomeVocabularyMatchesTheSupervisor` — set equality in both
  directions between the Go catalogue and `supervisor.py`'s tables (I4).
- `TestSupervisorClassifiesWorkerAdmitByEnumNotBySubstring` — every retired
  prose token is absent from `supervisor.py`, and `acquire_worker` contains no
  message-inspection idiom and does dispatch through the exact-match table.

**Python (`internal/pylib/aitest/test_supervisor.py`):**
- `test_acquire_worker_maps_every_class_to_its_exception` — a 13-row table
  replacing eleven separate substring-probe tests; several rows deliberately
  use reason tokens the retired cascade would have MISCLASSIFIED.
- `test_only_two_classes_are_containment_stripping` — I3, from the live table.
- `test_acquire_worker_refuses_an_unparseable_or_uncatalogued_outcome` — eight
  rows including the PRE-AIRA-42 grant line (an old relay must fail loudly).
- `test_acquire_worker_never_classifies_from_stderr` — stderr says
  "exceeds-ceiling"/"protocol"/"dial", the outcome says contended; the outcome
  wins.
- `test_acquire_worker_detail_cannot_forge_a_grant` — a hostile `detail`.
- `test_acquire_worker_raises_unavailable_when_the_relay_produces_no_outcome`.
- `test_transport_outcomes_arrive_classified_and_never_strip_containment` —
  replaces the two prose-shaped transport tests, pointing at the Go test that
  now owns that discrimination.
- The malformed-grant tests keep their scenarios; their expected exception
  changes to `WorkerAdmitContractViolation` (§2.6 item 5).

**Mutation testing.** Seven mutations were applied to the merged
implementation, each reverting one load-bearing decision; every one was killed
by a named test (see §9).

## 5. Files touched

| File | Change |
|---|---|
| `internal/runner/worker_admit_outcome.go` | **new** — the vocabulary, the outcome type, format/parse |
| `internal/runner/worker_admit_client_linux.go` | returns `WorkerAdmitOutcome`; structural transport classification |
| `internal/runner/worker_admit_client_stub.go` | non-linux stub follows the new signature |
| `internal/runner/admission_linux.go` | `DaemonProtocolVersion` 5→6; `proto` on the response frame; frame-size sentinel error |
| `internal/daemon/worker_admit.go` | `WorkerAdmitResponse` gains `class`/`detail`; reasons lose their prefixes; the `HasPrefix` poll-loop check is deleted |
| `internal/daemon/protocol.go` | `ProtocolVersion` 5→6 |
| `cmd/aira/main.go` | `runWorkerAdmitCommand` emits the outcome line in every path |
| `internal/pylib/aitest/supervisor.py` | the substring cascade is deleted and replaced |
| `internal/pylib/aitest/__init__.py` | the stale "classifier does not recognize E_DAEMON_PROTOCOL" comment (AIRA-45's own item 2) |
| `internal/daemon/protocol_test.go` | `TestStoreWriteRelayUsesProtocolVersionFive` → `TestProtocolVersionIsPinned`, now at 6 |
| `cmd/aira/dispatcher_inprocess_test.go` | the protocol-4/5 replacement test made version-RELATIVE so future bumps do not edit it |
| `internal/pylib/aitest/test_init.py` | two docstrings that asserted a classifier mechanism that no longer exists |
| `docs/superpowers/specs/2026-09-01-aitest-design.md` | §3.3 wire sketch + "Reason-string convention" rewritten to the structured shape |
| tickets AIRA-42, AIRA-45, AIRA-83 | resolutions recorded |

Build-review additions (§10): `workerAdmitGrantProblem` and
`classifyWorkerAdmitDialFailure` in `worker_admit_client_linux.go`, a scope
`os.Remove` on the undeliverable-outcome path in `main.go`, the
terminate-before-stderr-read reordering in `supervisor.py`, and
single-definition guards in `internal/pylib/worker_admit_channel_test.go`.

## 6. Risks

- **R1 — protocol bump on a shared machine.** Every session's installed
  binary mismatches the new daemon the instant it restarts. Mitigated by
  AIRA-83(a) (already merged: a mismatched client now refuses cleanly instead
  of restarting the shared daemon) and by the parent plan §8 atomic
  reinstall+restart. **This PR performs no deploy.**
- **R2 — Fix 1 has NOT merged; the parent plan's 1 → 2 order did not hold.**
  Verified at build time: `origin/master` is `8996999` (Phase 0) and branch
  `aira39-worker-admit-cgroup-ledger` carries only a plan doc (`8f4a73f`) and a
  probe test (`2751ad3`) — no implementation. This PR is therefore built on
  `origin/master`. Two conflict points for whoever rebases:
  1. `cmd/aira/main.go`'s `runWorkerAdmitCommand` — Fix 1 removes the CLI-side
     `runner.CreateWorkerScope` call. When it does, the `placement-failed`
     branch moves into the daemon; the vocabulary needs no change, because
     `worker-scope-create-failed` is already a catalogued reason and
     `placement-failed` is already a catalogued class/state.
  2. `internal/daemon/worker_admit.go` — Fix 1 deletes the
     `outer-scope-owned-by-another-job` verdict (ownership tracking becomes
     unnecessary) and adds an `admitSlots`-saturation denial. The latter is a
     pure addition: it needs one new reason token
     (`admit-slots-saturated`) with `class=contended`. It is deliberately NOT
     pre-added here, because a catalogued-but-never-produced token is the
     AIRA-87 drift class this repo is trying to close.
  Fix 1's §3.1 note about emitting the busy denial with a non-`reject:`-prefixed
  reason is **moot** after this PR: the prefix convention no longer exists, and
  the correct expression is `class=contended`.
- **R3 — cross-language duplication.** The class vocabulary exists in Go and
  in Python. Mitigated by the I4 set-equality test, which is the same
  prevention shape AIRA-66 used for `go:embed`.
- **R4 — behaviour changes.** Four, all listed in §2.6 with reasons. None
  widens the containment-stripping class; two narrow it.

## 7. Explicit deferrals

- AIRA-87's `store.ExitCodes` leaf-package move — separate follow-on PR (§3.3).
- The `aitest-bootstrap` stdout channel (`outer=`/`supervisor_scope=`) is
  already `k=v` and is not reshaped here.
- The confine-admission `basis` strings (`reject:saturated`,
  `fallback:daemon-unavailable`) are **descriptive telemetry**, not a
  classification channel — verified: no code branches on their prefix. Out of
  scope, and named here so a reviewer does not read their survival as a miss.
- `workerAdmitWaitCeilingMs` vs `runner.AdmitWaitCeiling` unification stays
  deferred (parent plan §3.1).


## 8. Plan-review disposition

Two independent lineages reviewed the plan before the build. Every finding is
recorded here with its disposition; the ones declined are declined for a stated
reason, not silently.

**Accepted (built):**

- **A1 (Sol P1, the class-model contradiction).** `WorkerPlacementFailed` also
  reaches `_disable_daemon`, so "admission-unusable is the only
  containment-stripping class" was false. I3 corrected; the Go test
  `TestWorkerAdmitContainmentStrippingClassesAreExactlyTwo` and the Python
  `test_only_two_classes_are_containment_stripping` now pin the set at two.
- **A2 (Sol P0-1, restated).** Sol asked for `outer-scope-unbounded` to stop
  being a fallback. The DISPOSITION is declined (see D1) but the naming defect
  Sol identified is real: the class was called `daemon-unusable` while that row
  reaches it with a healthy daemon. Renamed `admission-unusable`, which names
  the disposition rather than diagnosing the daemon.
- **A3 (Sol P0-3 + DeepSeek P0-2).** An error code this client does not know,
  an unintelligible frame, and an unparseable grant payload are all
  `contract-violation` now, not `admission-unusable`. Both reviewers
  independently reached the same place, and it is what the parent plan's
  "anything unrecognised is an explicit error" requires.
- **A4 (Sol P1, runner-side coverage).** The unknown-`state`, unknown-`class`,
  self-contradicting-pair and unknown-code rows are all tested inside
  `RequestWorkerAdmit`, not only on the Python side, so a Go-side mutation is
  killed too. Verified: mutation M2.
- **A5 (Sol P1, pre-dispatch outcomes).** `main.go:101` (parseArgs) and
  `main.go:147` (`--json`) now emit the outcome line. Verified: mutation M7.
- **A6 (Sol P1, guard scope + poisoned detail).** The anti-substring test
  covers the whole `supervisor.py` module for every retired prose token, not
  just `acquire_worker`'s body, and there are hostile-`detail` tests on both
  sides. Verified: mutation M5.
- **A7 (Sol P0-4, partial).** The Fix-1 rebase surface is enumerated concretely
  in R2, including which vocabulary additions Fix 1 needs and why they are not
  pre-added.
- **A8 (DeepSeek P0-1).** The daemon's own poll-loop check moves to the class.
  It was already in the plan; naming it as a P0 was right and it is verified by
  mutation M1.
- **A9 (DeepSeek P1, proto decode).** `runnerAdmitResponseFrame` now decodes
  `proto`; without that the discriminator is inert and the AIRA-45 bug
  survives. Verified: mutation M3.

**Declined, with reasons:**

- **D1 (Sol P0-1's disposition).** `outer-scope-unbounded` STAYS
  fallback-triggering. The design spec (`2026-09-01-aitest-design.md` §3.7)
  decides this explicitly and argues it: the workers remain hierarchically
  bounded by the real outer confine job's cap either way, so falling back is
  safe, and it beats hanging forever against a scope that can never become
  capped. Changing it is a design decision about admission policy, not a
  channel-shape fix, and this PR is not the place to overturn a spec decision
  as a side effect. A2 addresses the part of the finding that was really a
  naming defect.
- **D2 (Sol P0-2 / DeepSeek P1, retry policy).** Sol asked for bounded retry
  before terminal handling on the contended rows; DeepSeek asked for the
  opposite change on the "any other read error" row. Both are retry-POLICY
  changes. `_wait_for_admission_or_disable`'s indefinite retry is a deliberate,
  documented decision ("a daemon that stays reachable but saturated forever
  means the run genuinely waits forever, which is the honest outcome, not this
  method's job to silently degrade safety"), and every row Sol lists is already
  retriable on master. This PR changes the CHANNEL, not the policy: no row's
  retriability moves. The "any other read error" row keeps master's disposition
  (`admission-unusable`) and is documented as unreachable by construction —
  `readRunnerAdmitFrame` produces exactly a net error, an EOF/errno, the
  frame-size sentinel, or a JSON error, all of which are classified above it.
- **D3 (Sol P0-4's other half).** `admit-slots-saturated` is not pre-added to
  the reason catalogue. A catalogued-but-never-produced code is exactly the
  AIRA-87 drift this repo is closing; Fix 1 adds the token with its call site.

## 9. Mutation testing

Each mutation reverts one load-bearing decision in the merged implementation.
All seven were killed; the harness is `~/tmp/aira-fix2/mutate.py`.

| # | Mutation | Killed by |
|---|---|---|
| M1 | daemon poll loop back to `HasPrefix(reason, "reject:")` | `TestWorkerAdmitPollLoopBreaksOnClassNotOnReasonSpelling` (258 evaluations vs 1) |
| M2 | an uncatalogued daemon outcome becomes `admission-unusable` | `TestRequestWorkerAdmitClassifiesEndToEnd` ×3 sub-cases |
| M3 | drop the `proto != 0` version-skew discriminator (the AIRA-45 bug) | `TestClassifyWorkerAdmitDaemonErrorSplitsVersionSkewFromArgumentRejection` ×2, `…EndToEnd/a_protocol-version_mismatch_is_not_a_sizing_verdict` |
| M4 | Python resolves an unknown class into `WorkerAdmitUnavailable` | 2 pytest failures in `test_supervisor.py` |
| M5 | stop query-escaping `detail` | `TestWorkerAdmitOutcomeLineRoundTrips` (hostile detail broke the line) |
| M6 | `outer-scope-unbounded` classed `contended` | `TestEvaluateWorkerAdmitClassifiesEveryOutcome`, `TestWorkerAdmitCLIOutcomeChannelMatchesTheSupervisorBoundary` |
| M7 | pre-dispatch argument errors emit no outcome line | `TestWorkerAdmitPreDispatchFailuresSpeakTheOutcomeChannel` ×2 |
| M8 | drop the grant-payload validation (build-review P0) | `TestRequestWorkerAdmitClassifiesEndToEnd` ×6 sub-cases |
| M9 | every dial error is `admission-unusable` again | `…EndToEnd/local_resource_exhaustion_on_dial_is_retriable` |
| M10 | stop query-escaping `detail` (re-run post-merge, §12.3) | `TestWorkerAdmitOutcomeLineRoundTrips` ×2 sub-cases |

**M10 is M5 re-applied after the merge**, because §12.3 deletes the `"unbounded"`
mangling and the escaping is now the ONLY thing standing between
operator-controlled text and the classifier. Recorded honestly: M10 was already
killed by the assertions M5 killed on — the new token-count assertion added
alongside it is a **precision and diagnostic** improvement, not a new kill. It
asserts the STRUCTURE (a hostile detail must not split the line into extra
tokens) rather than only the resulting field values, which is the property the
mangling's deletion actually rests on. The new
`detail carrying the token the retired classifier keyed on` sub-case uses the
two real strings that used to trigger the hazard: an operator's
`aira confine --name unbounded-suite` echoed into a cgroup path, and raw
`memory.max` bytes quoted back through `%q`.

## 10. Build-review disposition

Two independent adversarial lineages read the merged diff: Codex/Sol against the
repo (**BLOCK**, 1×P0 + 3×P1 + 3×P2) and DeepSeek-pro against the Python half
inline (**BLOCK**, 1×P0 + 2×P1 + 3×P2).

**Accepted and fixed (each with a new mutation-verified test):**

- **B1 (Sol P0 — real, and in the dangerous direction).** `RequestWorkerAdmit`
  validated only catalogue membership, so a grant with
  `memory_high >= memory_max` — exactly what `CreateWorkerScope` refuses
  (`worker_scope_linux.go:29`) — reached the CLI's `CreateWorkerScope` call,
  failed there, and was reported as `placement-failed`, which STRIPS RAM
  containment for the rest of the run. A nonsensical grant is a contract
  problem, not a local one. `workerAdmitGrantProblem` now checks worker_id,
  scope_path, both limits positive, and `memory_high < memory_max`, and reports
  `contract-violation` / `malformed-grant`. Also closes DeepSeek's P2 on the
  same field pair. Mutation M8.
- **B2 (Sol P1 — dial errors).** A dial that fails on THIS process's own
  `EMFILE`/`ENFILE`/`ENOMEM`/`EAGAIN` is now `contended`, not
  `admission-unusable`. Precedent: `supervisor.py` already applies the identical
  reasoning to an `EAGAIN`/`ENOMEM` fork failure when launching this very relay.
  This only ever moves cases OUT of a containment-stripping class. Mutation M9.
- **B3 (DeepSeek P1 — an unbounded read on the dispatch loop).** The
  malformed-grant path read the relay's stderr to EOF after closing stdin but
  BEFORE terminating it, so a relay that ignores its own stdin EOF wedged the
  single-threaded dispatch loop — the AIRA-92 hazard, pre-existing but on a path
  this change already rewrites. `_terminate_process` now runs before the read.
  Both original ordering constraints are preserved and the reasoning is in the
  code.
- **B4 (Sol P2 — a leaked scope).** When the outcome line cannot be written
  after `CreateWorkerScope` succeeded, nobody will ever learn the scope's path,
  so the CLI now removes it instead of leaving it for the #72 reaper's 5-minute
  sweep.
- **B5 (Sol P2 — porous guards).** `pythonSetLiteral` and `functionBody` picked
  the FIRST textual match; both now require the name to be defined exactly once,
  so a dead earlier definition cannot satisfy the guard while a later one runs.

**Declined, with reasons:**

- **DeepSeek P0 — false positive.** It claimed `Popen` failures escape
  uncaught; `acquire_worker` catches `OSError` (which `FileNotFoundError`
  subclasses) and has since before this change. DeepSeek was given the function
  inline and misread it; verified against source.
- **Sol P2 — the poll-loop test.** Sol read it as unable to distinguish "polled
  twice then broke early" from "polled to the deadline". It can: only reaching
  the deadline produces `state=timeout`, and the test asserts the state in the
  written payload. A comment now says so explicitly rather than leaving the
  reader to derive it. No code change; a tighter wall-clock bound would be the
  AIRA-20 flake class.
- **Sol P1 — pre-ack child exits labelled placement failures**
  (`supervisor.py` `spawn_worker`). Real, pre-existing, and untouched by this
  change; it is about the fork/ack handshake, not the outcome channel. Filing
  territory, not this PR's.
- **Sol P1 / DeepSeek P2 — permanently-unreadable cgroup files and an
  unresponsive relay retried forever.** Both are retry POLICY. `unevaluated` is
  retriable by explicit design-spec mandate (§3.3: "a future client must treat
  `unevaluated` as retriable, the same as `denied`/`timeout`"), and
  `_wait_for_admission_or_disable`'s indefinite retry is a documented decision.
  This PR changes the channel, not the policy; no row's retriability moves.
- **DeepSeek P1 — "no outcome line" should be terminal.** Kept as
  `WorkerAdmitUnavailable`, preserving master's behaviour for a dead relay. The
  distinction held: an outcome we cannot UNDERSTAND is a contract violation; the
  ABSENCE of one is not a classification at all, and the honest reading is that
  this relay cannot deliver admission. It is a named branch, not a fallthrough.
- **DeepSeek P2 — cross-check every state/class pair, not just grantedness.**
  Declined: `class` is the authority by design, `state` is diagnostic, and
  re-deriving the legal pairs on the consumer side would duplicate the mapping
  table — reintroducing the two-sources-of-truth shape this fix removes. The
  grantedness check exists only because a `None` sentinel sits in the table.

**Sol's verified non-findings** (recorded as evidence, not as absence of
review): every current producer emits a catalogued non-empty class; the
connection is released on every non-granted path and the lease on every
post-grant failure; the daemon's deferred grant release is still correct; the
terminal exceptions are caught by all scheduling paths; `unquote_plus` does not
raise on runtime-decoded input; no prior scenario was deleted without equivalent
coverage; and no prose-substring classifier survives, with the bootstrap `outer=`
parse and the worker-result framing correctly out of scope.

## 11. Verification

`aira confine -- make ci` (`fmt-check vet build test`, `go test ./... -count=1`)
— **exit code 0**, all 12 packages `ok`.

One earlier full run failed on `TestGovernorSlotReconnectsWithSameUUID`
(`internal/runner/governor_slot_test.go:453`, "relay did not reconnect"). That
is a known wall-clock flake on the xdist-governor path — AIRA-20's flake
surface, and a file the parent plan schedules for deletion under AIRA-33. It
did not reproduce in isolation (`-count=3`), in a full package re-run, or in
the confirming `make ci`, and it touches nothing this PR changes. Recorded
rather than hidden.

## 12. Integration with Fix 1 (AIRA-39), merged after this fix was built

Fix 1 landed on master (`11221d3`) while this fix was in build review. It
rewrote `evaluateWorkerAdmit` — the exact function this fix reclassifies — so
the merge was a re-derivation, not a textual resolution: `worker_admit.go` was
taken from master wholesale and the structured vocabulary re-applied onto Fix
1's shape, verdict by verdict. What follows is what changed relative to §2.3
and §5, and why.

### 12.1 The protocol version is 7, not 6

**Fix 1 also bumped the protocol, 5 → 6**, for its own reason (daemon-side
worker-scope creation changes wire SEMANTICS without changing wire SHAPE). This
fix changes the shape on top of that, so it bumps **6 → 7**.

This is the merge's one genuinely dangerous spot, and git could not have caught
it: both branches independently wrote `= 6`, so `internal/runner/
admission_linux.go` auto-merged with **no conflict marker** and would have
shipped a daemon and a client that both claimed 6 while speaking different
shapes — silently, since `TestRunnerDaemonProtocolVersionMatchesTheDaemon` only
pins the two sides EQUAL, not correct. Caught by reading the version on both
branches rather than trusting a clean merge. `TestProtocolVersionIsPinned` (the
version-agnostic rename this fix made) now pins 7.

### 12.2 The §2.3 daemon table, re-derived

**Row deleted — no longer a reachable condition.** `outer-scope-owned-by-
another-job` is gone: Fix 1 replaced the per-job grants map with a sum over the
outer scope's real `.aira-worker-*` children, so two jobs sharing one outer
scope are now counted together rather than the second refused.
`WorkerAdmitReasonOuterScopeOwnedByAnother` is deleted from the vocabulary.

**Four rows added, each classified here for the first time:**

| condition (Fix 1) | state | class | reason |
|---|---|---|---|
| `.aira-worker-*` scan unreadable | `unevaluated` | `contended` | `worker-scopes-unreadable` |
| scope-id collision (`fs.ErrExist`) | `denied` | `contended` | `worker-scope-id-collision` |
| scope creation failed | `denied` | `request-invalid` | `worker-scope-create-failed` |
| worker-id space exhausted | `denied` | `request-invalid` | `worker-id-space-exhausted` |
| admit-slots saturated (pre-existing, now classified) | `denied` | `contended` | `admit-slots-saturated` |

Every one preserves the disposition Fix 1 chose via its `reject:`/`fallback:`
prefix — this fix changes the ENCODING, not any verdict's behaviour.

**The `request-invalid` class doc was widened, honestly.** Two of its members
are now daemon-side infrastructure facts, not facts about the request. The
class doc previously asserted "a permanent, static fact about THIS request";
it now names the DISPOSITION (terminal, daemon healthy, containment kept) and
records why those two land there — `contended` would retry a broken cgroupfs
forever and stall every aitest run on the machine, and `admission-unusable`
would strip containment over a daemon answering perfectly well. This is the
same correction §2.2 already applied to `admission-unusable`'s own name.

### 12.3 A deletion this fix earns from Fix 1

Fix 1 shipped `workerScopesUnreadableReason`, which MANGLED the token
`"unbounded"` into `"un-bounded"` in its diagnostic — a defensive hack against
the very substring classifier this fix deletes (an operator's `aira confine
--name unbounded-suite`, echoed into a cgroup path, was otherwise read as an
uncapped outer scope and ran the whole suite unconfined). Fix 1's own comment
predicted "Fix 2's structured outcome channel removes the need for substring
matching altogether", and it does: the function is now
`workerScopesUnreadableDetail` and reports the error verbatim.

`TestWorkerAdmitUnevaluatedReasonNeverCarriesTheUnboundedToken` guarded that
workaround, so its assertions **invert** rather than disappear — it is now
`TestWorkerAdmitHostileDiagnosticTextCannotForgeAVerdict`, and asserts that
hostile text reaches the wire unmangled AND changes no part of the
classification, end to end through the real renderer and parser. Deleting it
outright would have been a masking test rewrite.

### 12.4 Two decisions the merge overturned

- **`placement-failed` has no producer left, and is deliberately KEPT.** Fix 1
  removed the CLI's `CreateWorkerScope` call — the daemon creates the scope
  inside the same critical section that grants — so the §2.3 CLI row that
  produced this class is gone. The class stays in both catalogues because
  `supervisor.py` still raises `WorkerPlacementFailed` from its own fork/ack
  path, and that disposition should have ONE name across both sides rather
  than a channel name and a Python-only name — the two-vocabularies shape this
  fix exists to remove. The "exactly two containment-stripping classes"
  invariant (I3) is unchanged; what changed is that only the supervisor
  produces the second one, and only locally. Recorded in the class doc, the
  exception docstring, the spec, and the Python table row.
- **Build-review finding B4 (§10) is mooted, and its fix was removed.** B4
  removed a leaked scope when the CLI could not deliver its outcome line. Post
  Fix 1 the CLI does not own that scope — the daemon created it — and Fix 1
  made the SYMMETRIC decision at its own failed-response-write site
  deliberately, on plan-review adjudication: *"Removing it here is NOT safe:
  writeFrameBytes surfaces the underlying Write error and discards n, so 'the
  client provably never learned the path' is false"*, preferring a loud,
  retriable over-charge to a silent unconfined run. Having the relay
  unilaterally `rmdir` a daemon-created cgroup would re-litigate that call
  inside a merge resolution, where it would be under-reviewed. The over-charge
  is accepted, and the reasoning is recorded at the call site.

### 12.5 One rename beyond the original plan

`WorkerAdmitRequestTooLarge` → `WorkerAdmitRequestInvalid`, matching its class
token exactly. AIRA-45's own complaint is that "cannot be admitted at this
sizing" is the factually wrong diagnostic for a protocol skew; Fix 1 then added
two members that are not about sizing OR about the request. Keeping the old
name would have left this fix's headline vocabulary claim false at its most
visible point. No compatibility alias was added, per the project's zero-back-
compat rule.

### 12.6 Post-merge verification

- `aira confine -- go build ./...` — **exit 0**.
- `aira confine -- make fmt-check vet build` — **exit 0**.
- `aira confine -- go test ./... -count=1` — **exit 0**, all 13 packages `ok`
  (`cmd/aira`, `internal/app`, `core`, `daemon`, `domain`, `gate`,
  `gitcontext`, `gitremote`, `install`, `pylib`, `runner`, `store`;
  `internal/cgrouptest` has no test files).
- `aira confine -- python3 -m pytest internal/pylib/aitest/ -q` — **exit 0**,
  130 passed.
- The AIRA-42 requirement re-verified against the MERGED tree, not the
  pre-merge one: `grep` for `in message` / prose classification across
  `supervisor.py` returns nothing, and the only surviving `strings.HasPrefix`
  in `worker_admit.go` is on a cgroup DIRECTORY NAME (`.aira-worker-`), which
  is structural framing on a filesystem entry, not classification of a
  message.

### 12.7 Review coverage of the integration — a stated gap

The pre-merge diff got the full two-loop: two plan-review lineages and two
adversarial build-review lineages (§8, §10). **The merge integration itself
(§12.1–§12.5) did not.**

- **Codex/Sol: UNAVAILABLE.** The account hit its usage limit mid-review
  (`ERROR: You've hit your usage limit ... try again at Sep 7th`). Per this
  project's sidecar policy an unavailable lineage is noted and not blocking,
  but the loss is real: Sol is the lineage that found the P0 in the pre-merge
  round.
- **DeepSeek-pro: attempted three times, no usable response.** A 98 KB payload
  returned empty at exit 0 twice (once via the CLI, once via MCP); a focused
  ~4 KB prompt was still running at the time of writing. A small ping to the
  same model succeeded, so the model is up and the payload size is the
  suspected cause. If the focused run lands before merge its findings belong
  in this section; if not, this gap stands as written.
- **What DID cover the integration:** my own structured pass, which found and
  fixed three staleness spots (comments in `workerAdmitGrantProblem`, its call
  site, and its test, all describing the CLI `CreateWorkerScope` path AIRA-39
  deleted), plus the empirical escaping probe that became the M10 mutation and
  the token-count assertion above.

**The areas a reviewer should weight most**, because they are exactly what the
missing lineages would have attacked: the five re-derived daemon
classifications in §12.2 (especially the two TERMINAL ones, rows 9 and 10, and
whether `contended` on the EEXIST and slot-saturation rows should instead
return promptly rather than poll); the deleted `"unbounded"` mangling in §12.3;
and the two overturned decisions in §12.4.

### 12.8 Integration build review — DeepSeek-pro (BLOCK), disposition

A focused re-run of the DeepSeek-pro lineage on the re-derived classification
table did land, after three failed attempts on larger payloads (§12.7).
**Verdict: BLOCK, 2×P1 + 2×P2.** Every finding was checked against source
before being accepted or declined; two were checked by writing a probe.

**Accepted as an observation, recorded, not fixed here:**

- **P2 — rows 9/10 are terminal but `request-invalid` is the wrong NAME for
  them** ("these are daemon-side infrastructure faults"). DeepSeek reached
  independently the same objection §12.2 already records, which is
  corroboration worth having. Its proposed home, `contract-violation`, is
  DECLINED: that class means specifically *the two sides disagree about the
  channel* — an uncatalogued value, a self-contradicting pair, an unparseable
  line — and a cgroupfs `EPERM` is a well-formed verdict the daemon reports
  correctly. Both classes are terminal with identical supervisor behaviour, so
  the only real difference is the diagnostic, and "relay and supervisor are out
  of lockstep" would be actively false. Resolved as §12.2 already did: the
  class doc now names the disposition rather than asserting a diagnosis. Adding
  a seventh class for a naming nuance is the complexity this project's
  architectural-simplicity rule exists to refuse.
- **P2 — row 2 can hide a PERMANENT `ENOENT`/`EACCES` behind a retriable
  class, and retry forever.** Real, and worth a ticket. But it is *pre-existing
  behaviour*, unchanged by this fix: before AIRA-42 the identical case produced
  "unevaluated" without the "unbounded" token, which the substring classifier
  also mapped to the retriable `WorkerAdmitDenied`. No row's retriability moves
  in this PR, per the same scope line §10 already drew on retry POLICY.
  Recommended follow-on: split `outer-scope-unreadable` by errno the way
  `classifyWorkerAdmitDialFailure` splits dial errors.

**Declined — verified false against source:**

- **P1 — "row 8 (EEXIST) is retry-forever; make it terminal."** DeepSeek
  argued that "invalidating the cached sum does not clear the existing cgroup",
  so the retry collides identically until the deadline. True premise, wrong
  conclusion: the retry does not need the child GONE, it needs it COUNTED.
  `state.invalidate()` forces a rescan, the rescan lifts `maxIndex` past the
  colliding child, and the id advances. **Proved by probe, now kept as a
  permanent test** — `TestWorkerAdmitEEXISTRetryAdvancesTheWorkerIDInstead
  OfRepeating` shows the first attempt colliding at id 1 and the retry
  GRANTING at id 2. The pre-existing test only showed the retry denying on the
  corrected aggregate, which does not distinguish the two readings, so this
  finding did expose a genuine coverage gap even though its conclusion was
  wrong. Net effect: one new test, no behaviour change.
- **P1 — "row 1 strips containment too eagerly; ancestor enforcement is not
  guaranteed."** The kernel claim is wrong: cgroup-v2 `memory.max` IS enforced
  hierarchically, so a descendant reading back `max` remains bounded by every
  ancestor's limit — which is precisely the design spec's stated reasoning
  (§3.7). Beyond that, this row's disposition is a spec decision that predates
  this fix, was re-affirmed against a plan-reviewer's objection (§10), and is
  only re-ENCODED here. Changing it is a design change needing owner sign-off,
  not a merge-resolution edit.

Codex/Sol remained unavailable (usage limit), so the integration carries one
adversarial lineage rather than two. The gap stated in §12.7 stands.
