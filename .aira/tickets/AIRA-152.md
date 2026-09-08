---
{"schema":1,"id":"AIRA-152","project":"aira","title":"Candidate: make the OOM escalation a replacement rather than a max with the unpinned default","status":"superseded","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-153","to":"AIRA-152"}]}
---

AIRA-149 deferral **F3**, with the cost analysis so a future session does not
re-derive it.

Making the escalation a REPLACEMENT rather than `max(estimate, 1.5x OOM peak)`
when no usable ordinary estimate exists would fix the sizing half of AIRA-149's
facet 1 exactly and remove most of AIRA-150's reachability.

**Cost.** In AIRA-149's measured example it sizes the next attempt at 84 MiB
instead of 4 GiB for a job that needs ~320 MiB, so the self-heal takes SEVERAL
MORE OOM KILLS to converge instead of one. The current `max()` encodes a
defensible rule — never size the next attempt below what an unpinned client
asked for — and replacing it trades operator-visible OOM kills for slice
efficiency.

It would also require restating AIRA-128's shipped claim ("the very next run
succeeds") as a convergence ladder and retuning its real-cgroup fixture
(`internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go`).

A real design question with a real cost either way; not a by-product of an
honesty fix.

## Owner decision and current standing (2026-09-07)

**Not chosen for this round, and deliberately NOT closed.** The owner was asked
to choose between AIRA-151 (narrow the ceiling clamp to the rows where the
escalation determined the value) and AIRA-152 (replace rather than `max` the
escalation) as the sizing fix for AIRA-150, and chose **AIRA-151**, which has
since shipped. This ticket is therefore no longer a live candidate for that
decision; it is a filed, still-valid design option awaiting its own two-loop if
anyone takes it.

Why it was not chosen, recorded so a future session meets a decision rather than
an open question (AIRA-151 plan §2.2):

- in AIRA-149's measured example it sizes the next attempt at 84 MiB instead of
  4 GiB for a job that needs ~320 MiB, so the self-heal converges over several
  more OOM kills instead of one;
- it requires restating AIRA-128's shipped "the very next run succeeds" claim as
  a convergence ladder and re-tuning its real-cgroup fixture — the invariant
  AIRA-151 was able to hold untouched (AIRA-151 I4);
- the current `max()` encodes a defensible rule: never size the next attempt
  below what an unpinned client asked for.

**AIRA-151 forecloses nothing here.** The two are compatible: AIRA-152, if ever
taken, would shrink the population AIRA-151 acts on by making the resolved value
smaller, not by changing what the clamp does. Anyone reopening it should start
from AIRA-151's shipped behaviour, not from the pre-AIRA-151 code this ticket
was written against.

## Closed as superseded (2026-09-07)

Status flipped `planned` -> `superseded`: the owner's decision above is final
for this backlog round, so this is no longer live open work, though the
technical option and its cost analysis remain valid and are preserved above
for whoever reopens it. Superseded by the AIRA-151 decision, not rejected on
technical merit.

## Still superseded after AIRA-153 (2026-09-08)

AIRA-153 shipped and does not reopen this. Recorded because AIRA-153's rejection
of one variant of its own fix rested on this ticket's rule, and that rule has now
been REFINED rather than removed (AIRA-153 plan §2.3, G10).

The rule this ticket's rejection rested on — *never size the next attempt below
what an unpinned client asked for* — now reads:

> never size the next attempt below what an unpinned client asked for, **bounded
> by what the slice can hold**.

On every slice where the old rule was satisfiable that is the identical rule, and
on slices where it was unsatisfiable (the unpinned default alone exceeding the
admission ceiling) it replaces "refuse the job" with "give it the largest prior
the slice can grant". AIRA-153 also keeps `max(prior, escalation)` a genuine max
of two candidates by fitting the prior BEFORE the comparison, so the escalation
can still raise the reserve to whatever this command's own OOM evidence
justifies — which is exactly the `max()` behaviour this ticket proposed to
remove.

So the cost analysis above is unchanged and still valid, but the "several more
OOM kills to converge" cost is now weighed against a fit that has already removed
the case where the `max()` produced an unusable number. Anyone reopening this
should start from AIRA-153's shipped behaviour.
