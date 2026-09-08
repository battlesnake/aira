---
{"schema":1,"id":"AIRA-194","project":"aira","title":"A zero-reserve pass-through confine mode -- group-kill + cgroup grouping for an orchestrator whose children all self-confine, contributing nothing to the admission ledger","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-194","to":"AIRA-195"}]}
---

Peer proposal (fastest-ee-4b, 2026-09-09), designing per-sub-job `aira
confine` wrapping for `make merge-gate`'s many sub-jobs (pytest legs via
`--delegate-ram`, static gates like semgrep/grimp/jscpd via a plain
reservation). Given [[AIRA-187]] (nested confine competes with its own
parent's reservation, no inheritance), their question: is there, or
should there be, a mode for an orchestrator whose children ALL
self-confine -- a pass-through parent that gets the cgroup (so a kill of
the orchestrator group-kills every child) but contributes ZERO to the
slice admission ledger, since its children already account for
themselves individually?

## Verified from source -- evaluated on its own merits, not just filed as-asked

This is a real, well-motivated request, not speculative: `make merge-gate`
today, wrapped once in `aira confine --memory-reserve R`, reserves R for
the WHOLE gate even though NF-37's `scripts/mg_leg.sh` already re-confines
each leg individually -- the outer reservation is dead weight once every
leg self-accounts, and (per [[AIRA-187]]) it also creates exactly the
starve-your-own-child race that ticket tracks.

**It is not a new kind of risk, it extends an already-shipped trade-off
class.** `--delegate-ram` already ships the same shape of honesty
caveat (`internal/core/skill.go:318`): "a delegate-ram scope's memory.max
is a GENEROUS CEILING, not its reserve... under heavy multi-suite load a
delegate suite can grow past what admission accounts for, over-commit the
slice." A zero-reserve pass-through parent is the same trade taken one
step further -- not a smaller reserve than needed, but none at all --
which is architecturally consistent with, not a departure from, what this
project already ships and documents plainly rather than hides.

**The real correctness risk, stated precisely so it is designed against
rather than assumed away:** the safety of a zero-reserve parent depends
entirely on an INVARIANT AIRA cannot verify -- that every byte of memory
its own cgroup ever accounts for was separately reserved by a
self-confining child, never run directly in the parent's own process
tree. AIRA has no cheap way to enforce or detect a violation of this
(cgroup accounting is hierarchical and does not distinguish "memory used
by a child that reserved for itself" from "memory used directly by the
parent"). This is the same class of unsound-guard risk Astra's review
flagged elsewhere tonight (AIRA-188's thread): "absence of validation
today is not a promise about tomorrow." A pass-through parent must ship
with the SAME kind of explicit, honest caveat `--delegate-ram` already
carries -- not as an airtight guarantee, but as a documented, bounded
trust extension the caller opts into knowingly.

## Not designed here

Exact flag shape (a new `--pass-through`/`--group-only` flag on `aira
confine`, or a natural extension of `--delegate-ram`'s own existing
model), whether/how to warn or refuse if the wrapped command's own direct
children are observed running unconfined work (cheaply detectable via the
same `AIRA_CONFINE_SCOPE_ID` nesting-token mechanism [[AIRA-187]] already
uses, though only after the fact, not preventively), and how this
interacts with group-kill semantics precisely (does killing the
pass-through parent's supervisor `cgroup.kill` the whole subtree
including grandchildren in their own self-confined sibling scopes, or
only its own direct process tree -- these are DIFFERENT cgroups per
[[AIRA-187]]'s own finding that a nested confine creates a SIBLING scope,
not a child cgroup, so a naive group-kill of the pass-through parent's
OWN scope would NOT reach its self-confined children at all, which may
defeat the entire point of wanting the grouping in the first place) --
left for whoever picks this up. This last point is the sharpest open
question and should be resolved before any implementation: if
self-confined children truly become sibling scopes rather than
descendants, "the orchestrator's cgroup group-kills its children" may not
be achievable at all without new machinery beyond a zero-reserve flag.
Architecturally significant enough to need its own plan and two-loop gate
before building, matching how [[AIRA-178]] was handled tonight (evidence
+ sketch, not built).
