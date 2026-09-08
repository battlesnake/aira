---
{"schema":1,"id":"AIRA-169","project":"aira","title":"The agent guide's E_ADMIT_TOO_LARGE sentence calls the pinned population 'a reserve you pinned yourself', which is untrue of aira run, a charged docker --memory limit, and confine-reserve","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","docs"],"hold":false,"relations":[]}
---
AIRA-165 deferral, accepted at the Fable work-review of PR #104 (merged `34ea0b0`) and given its own ticket so it is actionable rather than a paragraph inside another ticket's resolution. Graded P2 because it is the SAME over-claim the AIRA-165 build review BLOCKed at P2 in the daemon's `pinned:client` arm, now surviving only in the generated guide every agent reads (`aira create` accepts no P3).

`internal/core/skill.go` (`renderMarkdownBody`, the cold-start paragraph) teaches:

> When the reserve the daemon resolves is above the ceiling and did NOT come from that escalation -- this command's own measured peak-history estimate, or a reserve you pinned yourself -- the run is refused immediately with `E_ADMIT_TOO_LARGE` ...

Its ACTION (pin at or below the printed `cap_minus_headroom`, or run where the slice is larger) is consistent with the daemon's case-split advice. Its CAUSE clause is not: `pinned=true` reaches the daemon with NO operator flag on three live paths, and the guide attributes all of them to the operator.

- every `aira run` admission: `internal/runner/admission_linux.go` sends `pinned: !req.DaemonEstimateMemory || req.MemoryReservePinned`, and the sole setter of `DaemonEstimateMemory` is confine's launch path (`internal/runner/confine_linux.go`), so `aira run` is ALWAYS `pinned:client`, carrying `run.memory_reserve` or core's own peak-RSS estimate; `aira run` has no `--memory-reserve` flag at all;
- `aira confine -- docker run --memory=X` on an otherwise unpinned job: `runner.ContainerPlan.ResolveReserve` charges the container limit and re-marks the request pinned, and confine writes that back into `request.MemoryReservePinned` before admitting;
- `aira confine-reserve`, the only non-test `MemoryReservePinned: true`.

The fix is one clause: say "a reserve pinned on the client side" (matching `tooLargeRefusalAdvice` in `internal/daemon/admit.go`) instead of "a reserve you pinned yourself", plus a negative in `internal/core/skill_test.go` forbidding "you pinned yourself" in BOTH generated documents so the over-claim cannot return.

Not fixed in PR #104 because that sentence is master's pre-existing text and the PR's `skill.go` edit (AIRA-166) was confined to the paragraph's opening `fallback:` sentences; correcting it there would have been unreviewed drift. See the "Not done, deliberately" section of AIRA-165.

## Resolution (2026-09-08)

Documentation-only, zero behaviour change. One clause in
`internal/core/skill.go`'s `renderMarkdownBody` cold-start paragraph:

- was: `... this command's own measured peak-history estimate, or a reserve you
  pinned yourself ...`
- now: `... this command's own measured peak-history estimate, or a reserve
  pinned on the client side ...`

That is the same correction AIRA-165 made to the daemon's own
`tooLargeRefusalAdvice` in PR #104 (`3af5793`), which now says the reserve was
"PINNED on the client side" and names the possible origins AS POSSIBILITIES
rather than asserting one. The generated guide is the one place that still
attributed the whole `pinned` population to the operator, and it is the place
every agent reads, so it takes the daemon's wording verbatim. The ACTION half of
the sentence (pin at or below the printed `cap_minus_headroom`, or run where the
slice is larger) was already consistent with the daemon and is untouched.

No dispatch table, no admission path, no response contract, and no generated
action changed; the only bytes that moved are inside one prose string and its
test.

### Test

`TestGuideDoesNotBlameTheOperatorForAClientPinnedReserve`
(`internal/core/skill_test.go`) asserts, over BOTH generated documents (the
installed `SKILL.md` and the agent guide — they share `renderMarkdownBody`):

- the RED negative the ticket asks for: the literal `you pinned yourself` must
  not appear, plus three near-miss rephrasings of the same blame (`a reserve you
  pinned`, `you pinned this reserve`, `the reserve you pinned`), so the
  over-claim cannot silently return in a paraphrase;
- the positive: `a reserve pinned on the client side` is present;
- an ANCHORED positive tying that clause to the `E_ADMIT_TOO_LARGE` sentence it
  explains, so the test cannot be satisfied by the phrase appearing anywhere
  else in the document.

Mutation evidence (M4): restoring the old clause in `skill.go` makes the test
FAIL. It is not a vacuous negative.
