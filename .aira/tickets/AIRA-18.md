---
{"schema":1,"id":"AIRA-18","project":"aira","title":"Governor connections churn every ~30s — uncleared connect-time write deadline (Slice 2 defect)","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["daemon","scheduler","slice2"],"hold":false,"relations":[]}
---
Discovered by the watched enforce flip (RANT-13). governorConnection's reply writes inherit the connect-time 30s WRITE deadline and are never refreshed, so every governor connection is force-closed ~30s after it opens (on its next reply write), and the relay reconnects (same worker UUID) → the daemon logs a fresh "enforce activated" for that worker every ~30s. Net effect: the intended stable active-set + cooperative park/activate has barely been working since Slice 2 deployed, in BOTH observe and enforce (observe masked it — it caps at acquire and re-caps on each reconnect; only the enforce activation log made the churn visible).

Root cause (code-grounded, master 530c5e4):
- server.go:504 `conn.SetDeadline(time.Now().Add(30*time.Second))` sets BOTH read+write deadlines to bound the initial inbound-frame read.
- The governor branch server.go:565-573 clears only the READ deadline (:572 `SetReadDeadline(time.Time{})`) before calling governorConnection; the WRITE deadline is left at connect+30s.
- governorConnection (governor.go:354) writes every reply via writeFrame (governor.go:356/389/395/410/418/429) with NO per-write deadline → after 30s these writes fail deadline-exceeded → connection closed → churn. The ~30s churn interval matches the 30s deadline exactly.

Severity: benign (suites progress; at any instant the fill still respects active<target; no OOM/panic) but it undermines the whole scheduler mechanism and BLOCKS validating the enforce flip + building Slice 3 (RAM-aware ordering needs a working park/activate path).

Fix: governorConnection must refresh a bounded write deadline before each writeFrame (mirror the watch handler's per-write SetWriteDeadline pattern, server.go:500/527/608), so replies never inherit the stale connect-time deadline. Investigate the admit long-poll path (server.go:561 also clears only the read deadline) for the same latent issue — a slow >30s admission grant-write could hit it.

Discriminating test (the porous-test gap: Slice 2 tests never held a governor connection >30s): a governor connection whose write deadline is set in the PAST, then the daemon writes a reactivation/checkpoint reply — assert the write SUCCEEDS with the fix and FAILS (revert-check) against the unfixed handler.
