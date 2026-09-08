---
{"schema":1,"id":"AIRA-192","project":"aira","title":"aira top's RAM bar sums scope CAP, not granted reserve -- a delegate-ram scope's ceiling inflates the displayed claim far past what is actually reserved","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["confine","delegate-ram","observability","tui"],"hold":false,"relations":[]}
---

Owner observation (2026-09-09, live): "aira top currently shows 90GB of
reservations on a system with 80GB of RAM. No wonder jobs keep dying and
the mem usage bar in aira top is flooded." Investigated immediately.

## Verified from source -- confirmed real, root cause identified precisely

At the moment of the report, `aira confine --list` (the real admission
ledger) showed `slice reserve: 40086514564 granted / 52928M ceiling
across 8 admitted jobs` -- healthy, nowhere near over-subscribed. But the
SAME listing's own `slice scope caps:` line showed `94990824K across live
scopes` -- ~93 GiB, matching the owner's "90GB" observation almost
exactly. Two different numbers, and `aira top` draws the wrong one.

`topReserveFor` (`cmd/aira/tui_top.go:109-125`) is the RAM bar's per-scope
width function. Its own doc comment states the (now-false) assumption
directly: **"The cap IS the granted reserve since AIRA-67 (the daemon
writes the grant as the scope's memory.max hard sub-cap), so this needs
no second source and invents nothing."** That was true when AIRA-67
shipped. `internal/daemon/admit.go:953-954` documents precisely when it
stopped being true: **"a delegate scope's memory.max is its scope
ceiling, not its pinned framework reserve, and that is the largest cap
population on the machine."** AIRA's own admission ledger (AIRA-114)
already reads `scopeCeiling` correctly and knows not to conflate it with
the pinned reserve for its own aggregate-bound arithmetic -- `aira top`
never got the same correction, and sums every scope's `cap`
indiscriminately, delegate-ram scopes included, into `bar.Claimed`
(`tui_top.go:576-611`, accumulated into `offset` per scope, passed as
`claimed` to `topBarFor`).

**Effect:** a handful of generously-capped `--delegate-ram` scopes (whose
whole design point is a ceiling wider than any single pinned framework
reserve, to give aitest/xdist workers room without individually admission-
gating each one) inflate the bar's `Claimed` total far past what is
actually reserved on the slice, producing exactly the "more reserved than
the machine has RAM" reading the owner saw -- a display artifact, not
evidence of genuine over-admission. This is closely related to, but
sharper than, [[AIRA-191]] (filed minutes earlier from a different
report, about the SAME cap-vs-granted gap causing per-scope attribution
confusion in `--json` output) -- AIRA-191 is about an operator having to
reason it out by hand; this ticket is about the flagship dashboard
rendering the wrong number outright, including feeding
`bar.Overcommitted` (`tui_top.go:678`, `Claimed+Outside > Total`) a
falsely-inflated `Claimed`, which can fire the `OVER-SUBSCRIBED` banner
on a healthy slice.

## Not designed here

Whether the fix reads each delegate-ram scope's actual pinned/charged
reserve from a source `aira top` doesn't currently have access to (the
admission ledger's `scopeCeiling`-vs-pinned-reserve distinction lives
server-side in `internal/daemon/admit.go`, not in anything `confine
--list --json` exposes per-scope today -- this is exactly what
[[AIRA-191]]'s proposed per-scope `reserve_bytes` field would supply),
versus excluding delegate-ram scopes from the summed bar width entirely
with a note ("N delegate-ram scopes not drawn, ceiling not their reserve"
-- matching the existing `uncapped`/`unevaluated` note pattern already in
`tui_top.go:596-603`) as a cheaper interim fix that doesn't require a new
wire field, is left for whoever builds this. Given the severity (an
actively misleading flagship display, not a cosmetic gap), recommend NOT
waiting on AIRA-191's wire-field work if the interim exclusion is
buildable now.
