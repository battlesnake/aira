---
{"schema":1,"id":"AIRA-166","project":"aira","title":"The agent guide says a first run is capped at estimate:p90-prior even when no p90 exists","status":"done","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","docs"],"hold":false,"relations":[]}
---

AIRA-153 deferral **G7**. Pre-existing, unrelated to that ticket's change, and
named explicitly so the AIRA-153 guide edit is not mistaken for having addressed
it.

`internal/core/skill.go` teaches:

> A FIRST run of a command AIRA has never seen is capped at a machine-wide prior
> (`reserve-basis=estimate:p90-prior`) ...

That is true only when a machine-wide p90 EXISTS. `resolveAdmitReserve` consults
`cachedAdmitPeakP90` and, when it is unavailable or unusable — a fresh box, a
fresh `state.db`, fewer than three recorded peaks — falls through to one of the
four post-block fallbacks, where the true basis is `fallback:no-history` (or
`fallback:no-signature` / `:history-unavailable` / `:insufficient-samples`).

So an agent reading the guide and then seeing `fallback:no-history` on a genuine
first run has no way to tell whether it is looking at the documented cold start
or at something wrong.

Small, self-contained wording fix; not urgent. It should also say what the
fallback number IS (the unpinned default, or its ceiling-fitted form on a small
slice), since AIRA-153 made the fitted form the ordinary basis to see there.

## Resolution (2026-09-08)

Wording only, in the one generated clause. **No admission decision changed**:
`resolveAdmitReserve` is untouched, and every basis named below is one it
already produced before this edit.

### The corrected claim, verified against the code rather than paraphrased

The generated cold-start clause (`internal/core/skill.go`, which renders both
`SKILL.md` and the agent guide) now teaches BOTH bases and the condition that
separates them:

- `reserve-basis=estimate:p90-prior` — "the one to expect on a box with
  history", and the guide now states the condition it silently assumed: the
  machine-wide p90 exists only once some signature in this box's one shared
  `state.db` has **three or more recorded peaks**. That is
  `store.ConfinePeakP90`'s own `GROUP BY signature HAVING COUNT(peak_rss)>=3`
  over rows with `peak_rss>0`, read from the source, not from the ticket's
  paraphrase.
- a `fallback:` basis when it does not — a fresh box, a fresh `state.db`,
  nothing yet measured three times. The four are named with what each means:
  `fallback:no-history` (the genuine first run of a signature nothing is
  recorded against), `fallback:no-signature` (the launch sent none),
  `fallback:history-unavailable` (the history read itself could not be
  established) and `fallback:insufficient-samples` (some observations exist,
  fewer than three usable; it sometimes carries the estimator's own
  `:n=<samples>`, which is the spelling the paragraph's own `,oom-on-record`
  example already showed).

The direction asserted is the one that is TRUE in every branch: **no usable p90
⇒ a `fallback:` basis.** All four post-block fallbacks sit AFTER the
`cachedAdmitPeakP90` block, so reaching one means the p90 was absent or
unusable. The converse is not claimed, because a `fallback:` basis can also be
returned from inside the signature block (with `,oom-on-record`) while a p90
exists — which the very next sentences of the same paragraph already explain.

The clause then says what the number IS, which the ticket asked for: AIRA's
unpinned default (4 GiB), or the `,ceiling-fitted` form of it on a slice too
small to grant the whole default — "which on a small slice is the ordinary thing
to see", since AIRA-153 made the fitted form ordinary there.

And the sentence carries the point explicitly: a `fallback:` basis on a first
run **is NOT a sign that something is wrong** — it is the same documented cold
start, and it says only which term produced the number.

### Tests

`TestSkillTeachesBothColdStartBases` (`internal/core/skill_test.go`) asserts all
four fallback names, the p90's existence condition, the "not a sign that
something is wrong" clause, the 4 GiB number and the `,ceiling-fitted` pointer —
in BOTH generated documents. Its RED direction is a dedicated negative: the
exact old sentence `is capped at a machine-wide prior
(\`reserve-basis=estimate:p90-prior\`)` must NOT reappear, so a revert to the
unconditional claim fails rather than silently returning an agent to the state
where a fallback basis looks like a defect.

Verified RED against `origin/master`'s `skill.go` (with the new test in place):
eight missing clauses in both documents plus the negative firing on both. The
existing `TestSkillTeachesTheOOMVerdictAndTheColdStartSelfHeal` keeps its
`estimate:p90-prior` assertion, with its rationale updated to say the basis is
the one to expect WHEN a machine-wide p90 exists.

Shipped in the same branch and gate as AIRA-165; the gate table is recorded on
that ticket (`build`, `vet`, `go fmt`, and `AIRA_REAL_CGROUP=1 go test ./...`
all exit 0 on commit `f983788`).

## Fable work-review record (2026-09-08)

MERGE verdict. Shipped with AIRA-165 in PR #104, merged `34ea0b0`.

Re-reviewed after the AIRA-165 fix round (`3af5793`, the reworded
`pinned:client` arm). This ticket's own change was untouched by that round:
a word-diff of the PR's `internal/core/skill.go` edit against master confirms
it is confined to the opening `fallback:` sentences of the cold-start
paragraph, and the AIRA-165 review verified the paragraph's other claims
against the source. `internal/core` `ok` under
`AIRA_REAL_CGROUP=1 go test -count=1` on the head SHA in an independent
detached worktree; CI green on `3af5793` (`build + vet + gofmt`, `test`,
`race`); PR `MERGEABLE`/`CLEAN` with no file overlap against the one commit
master gained since the branch point.

Merged via `gh pr merge 104 --repo battlesnake/aira --merge` from
`/home/mark/claude/aira` on `master`; confirmed via `git fetch` +
`git log --oneline -1 origin/master` = `34ea0b0`.
