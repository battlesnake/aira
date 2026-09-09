---
{"schema":1,"id":"AIRA-221","project":"aira","title":"A daemon restart discards the AIRA-29 peak ratchet: adoption re-derives from live RSS with no memory of a job's demonstrated peak","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","dogfood"],"hold":false,"relations":[]}
---
> Found during the 2026-09-09 deploy of PR #125, alongside AIRA-220. Distinct
> defect, same trigger (a daemon restart), opposite axis: AIRA-220 is a reporting
> lie with sound accounting; this is sound reporting with a real accounting loss.

## SYMPTOM

A daemon restart silently DISCARDS the AIRA-29 peak ratchet for every running
confine job, and the loss is in the over-admission direction and does not heal.

A job that peaked at 30 GB and has since freed down to 5 GB is charged ~30 GB
while its connection is held, and ~5 GB + margin after a restart re-adopts it —
for the whole of its remaining life. The slice is then accounting ~25 GB less
than the job has demonstrated it can occupy, and will admit others into that gap.

## ROOT CAUSE

The two charging paths are asymmetric by construction.

Connection-held (`recomputeWaiterCharge`, `internal/daemon/admit.go:1218-1233`)
maintains a monotone lifetime ratchet:

```go
if tracked > waiter.trackedRatchet {
    waiter.trackedRatchet = tracked
}
charge := waiter.trackedRatchet
```

Adoption (`admit.go:2574-2586`) re-derives from scratch on EVERY scan, with no
ratchet and nothing carried across:

```go
tracked := addClamp(*record.RSSBytes, margin)
if tracked < cap {
    cap = tracked
}
```

A restart moves every live job from the first path to the second permanently —
the waiter and its `trackedRatchet` live only in the connection, which the
restart destroyed. The peak is not recorded anywhere the adoption path reads.

This is not the same as AIRA-220. There the ledger is absent and admission is
still safe, because `checkedAvailable` floors the charge at realised
`memory.current`. Here the ledger is PRESENT and wrong, and the floor does not
help: realised current is exactly the freed-down figure, which is the whole
problem — a job that has released memory can take it back at any moment, which is
precisely what the ratchet exists to remember.

## WHY IT MATTERS MORE NOW

AIRA-203 (open) will make restarts routine: `aira install` does not currently
restart the daemon when only the binary changed, so once that is fixed, every
deploy restarts the daemon and every restart takes this loss on whatever is
running at the time. The two tickets genuinely couple here — not on the
admission-window argument (there is no admission window; see AIRA-220), but on
this.

AIRA-203's own PROPOSED FIX already says to state the drain question and cite
`aira drain` (AIRA-185) and the 2026-09-08 drain-mode plan's Question 4. That
instruction should be treated as load-bearing rather than optional, and THIS
ratchet loss named in it as the concrete reason a deploy should prefer draining
to bouncing a busy daemon.

## PROPOSED FIX

Options, for an owner decision rather than a foregone conclusion:

1. **Persist the ratchet.** Record each job's peak where adoption can read it —
   the scope's own cgroup already holds `memory.peak` on kernels that expose it,
   which needs no new state and survives a restart by construction. Adopt at
   `max(RSS + margin, memory.peak)` clamped to cap. Cheapest and most in keeping
   with "read the truth from the kernel rather than remember it".
2. **Adopt at cap.** Simple, but over-charges every job that never approaches its
   cap, which is the over-reservation AIRA-29 was built to remove. Probably wrong.
3. **Accept and document.** State that a restart resets peak accounting to
   observed RSS, and that a deploy should drain first. Legitimate under the
   simplicity rule, but it should be a recorded decision, not a silent property.

Option 1 should be checked for kernel availability first: `memory.peak` requires
a recent kernel and the codebase already has precedent for a fail-closed
capability probe.

## HOW TO TEST

Capture `confine --list --json`'s `scopes[].reserve_bytes` for a long-running job
that has peaked and since freed, restart the daemon, re-capture, and diff the same
scope id. The reserve dropping to ~RSS is the defect. As a unit test: drive
`recomputeWaiterCharge` to a high ratchet, drop `peakSoFar`, assert the charge
holds; then run the adoption path over an equivalent record and assert it does
NOT fall below the previously demonstrated peak once the fix is in.

## UNEVALUATED

The magnitude was established from source only — the mechanism is certain, but no
instance was measured, because measuring it needs a deliberate restart while a
job with a known peak-then-release profile is running. The command that would
settle it is in HOW TO TEST above.
