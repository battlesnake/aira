---
{"schema":1,"id":"AIRA-24","project":"aira","title":"Admission saturation-wait UX: queue position/ETA, faster-fail, clearer reject signal","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","dogfood","ux"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-27","to":"AIRA-24"},{"kind":"relates","from":"AIRA-49","to":"AIRA-24"},{"kind":"relates","from":"AIRA-51","to":"AIRA-24"}]}
---
Reported by a dogfooding session (altium via subpipe) 2026-08-31: `aira confine --memory-max 32G --memory-reserve 512M -- make merge-gate` sat in admission-wait 1785s (~30 min) under genuine slice saturation (siblings ~40G/63G), then hard-rejected `E_ADMIT_SATURATED` (exit 1) with ZERO tests run — plus a "is it stuck/running/dead?" ambiguity because the job vanished from `aira confine --list` on reject with no distinct signal.

This is GENUINE reserve over-subscription (the slice was really full of sibling-gate anon), NOT the AIRA-21 cache-inflation false-stall — a distinct problem. The bounded-wait-then-honest-reject is BY DESIGN (#71 made it bounded vs infinite hang; #67 keeps Σreserve ≤ cap-headroom), so waiting itself is correct. The friction is the UX of a long blind wait + a silent hard reject:

1. NO visibility while waiting — the waiter can't see its queue position, the reserves ahead of it, or an ETA. #73 added a slice-reserve summary to `--list` ("why is it waiting"); extend it to surface the WAITER's own position / bytes-ahead / rough ETA (client-side periodic line already exists from #71: "waiting for memory admission (reserve X, waited Ns)" — add position/ahead).
2. 30 min is a long time to discover you won't get in. Consider a configurable / shorter faster-fail default, or an explicit `--admit-timeout`/`--no-wait` so a caller can choose fail-fast over a long hopeful wait.
3. Hard-reject signal is easy to miss — the job vanishes from `--list` with only exit 1 + the E_ code. Make the reject louder/distinct (it already exits 1 with E_ADMIT_SATURATED; the ask is a clearer terminal signal so an agent doesn't mistake reject for "still running").
4. Consider an admitted-with-backpressure mode (admit but throttle) as an alternative to head-of-line blocking for big reservations — heavier design, lower priority.

relates: #71 (admission hang diagnostic), #73 (--list reserve summary), #67 (reserve ledger), AIRA-4 (backfill). Owner-facing UX polish; not a correctness bug.

**CORRECTION (2026-09-01):** the reported reserve was NOT 512M. `--memory-max 32G` UP-CHARGES the admission reserve to 32G (`main.go:796-798`; `--memory-reserve 512M` is discarded) — so the job waited for a **32G** reserve, and a 30-min wait then reject for 32G under genuine saturation is EXPECTED, not anomalous. The UX asks above (visibility / faster-fail / clearer reject) all still stand — they're about the wait experience regardless of reserve size. See AIRA-27's correction for the up-charge semantics.

## RESOLUTION (2026-09-05, backlog remediation plan §4)

Ask **1 (no visibility while waiting)** is what this closure builds. The
periodic progress line now carries the waiter's OWN place in the queue:

```
confine: waiting for memory admission on aira.slice (reserve 32G, waited 45s, queue position 2 of 3 by enqueue order, 8G queued ahead)
```

- The daemon's `confine-list` verb accepts an optional `scope_id` and answers
  `queue_position` / `queued_ahead_bytes` for that scope, derived in the SAME
  locked pass as the existing `queued` total (`internal/daemon/admit.go`,
  `admitSliceSnapshotFor`), so position, queue size and bytes-ahead can never
  describe different instants.
- The blocked launcher asks over a SEPARATE short-lived connection per tick
  (`internal/runner/confine_queue_position_linux.go`). The admission socket is
  the job's lease and the daemon reads its next byte as "the client went
  away", so it is never multiplexed — the deferral note this ticket left in
  `confine_linux.go` said exactly that, and the implementation follows it.
- `aira confine --list` never passes a scope id, so its output is unchanged.

Honest limits, stated rather than papered over:

- **No ETA, by design.** Bytes-ahead is a fact about the queue; an ETA would be
  a prediction the daemon cannot establish (it depends on when other jobs
  finish), and AIRA reports primitives, not judgement.
- **Position is EVALUATION order, not a promise of grant order.** The AIRA-59
  fairness duty cycle's yield phase can admit a later, smaller waiter that fits
  while the head is still too large.
- **An unestablished position prints nothing** — no daemon, an older daemon, or
  a scope that is not queued leaves the line exactly as it was. Zero is an
  absence, never "position 0 of 0".
- Needs the daemon restarted onto this code; the wire shape is unchanged and
  `ProtocolVersion` is NOT bumped, so an old daemon simply answers no position.

Build review (DeepSeek lineage; the Codex/Sol lineage was over its usage limit
and did not run) raised five points. Three were real and are fixed here:

- The daemon-load question was answered by MEASUREMENT, not assertion, and
  then re-answered when a second reviewer pointed out that a per-call figure
  from a nearly-idle slice does not bound a contended one: 1.5–1.7 ms of
  daemon CPU per `confine-list` at 3 live scopes
  (`docs/dev/aira24-probe-cost.sh`), plus a scan slope of ~16 µs per live
  scope, 2.03 ms at 128 (`BenchmarkListConfinesByScopeCount`). Worst case —
  256 queued jobs (the queue's own cap) AND ~128 live scopes at once —
  256/15s × 3.6 ms ≈ 6% of one core; the contended case actually observed is
  under 0.1%. Still clear of AIRA-61's per-poll-scan class, so no cache was
  added.
- "reserved ahead" invited reading the figure as all reserve standing between
  the job and admission; it counts only the QUEUED waiters in front, so the
  line now says "N queued ahead" and names "by enqueue order" explicitly.
- A grant landing while a probe was in flight could print a "still waiting"
  line after admission was granted. The tick now re-checks after the probe and
  skips the line. This narrows the window from the probe's whole duration to
  the nanoseconds between that check and the write; the residual is
  pre-existing (any tick can fire just as a grant lands) and is not claimed to
  be closed. The elapsed-seconds figure is now also read after the probe, so a
  slow daemon no longer makes the printed wait short by up to 2 s.

The other two were verified and rejected: the split-ceiling probe timeout is
test-only (production is a 2 s constant, and the probe honours cancellation),
and non-FIFO grant order is now stated in the line itself.

Ask **2 (faster-fail)** landed earlier as `--admit-timeout` with a refusal at
the shared `runner.AdmitWaitCeiling` (AIRA-58), rather than a silent clamp.

Ask **3 (clearer reject signal)** landed earlier: `E_ADMIT_SATURATED` now
reports "admission rejected after <duration> — slice contended, no memory
admission within the wait (reserve X/ceiling)" instead of a bare code.

Ask **4 (admit-with-backpressure instead of head-of-line blocking)** is
explicitly NOT built and is not implied by this closure. The ticket itself
filed it as "heavier design, lower priority"; it belongs with the
`memory.high`/oomd policy question (AIRA-91 Part B, AIRA-35), which needs
owner sign-off. File a fresh ticket if it is wanted.
