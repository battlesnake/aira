---
{"schema":1,"id":"AIRA-206","project":"aira","title":"confine's status trailer has no line-break guard, so on an OOM kill it glues onto the job's partial last line and an anchored ^confine: parse misses it","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","rant-triage"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-206","to":"AIRA-214"}]}
---
> Filed from the 2026-09-09 global rant triage (35 rants, adversarially reviewed).
> Evidence below survived an independent refutation pass; claims that did not are
> recorded as dropped in the triage record and deliberately absent here.

confine's status trailer has no line-break guard, so on an OOM kill it glues onto the job's partial last line and an anchored `^confine:` parse misses it
**kind** bug · **severity** P2 · **closes** RANT-30

**SYMPTOM.** A confined job whose output does not end in a newline gets the trailer concatenated onto its own last line — `partial-line-then-oomconfine: slice=... terminated-by=oom peak-rss=64M ...` — so an anchored `(?m)^confine:` parse sees no `terminated-by=` and reads an OOM-killed job as one that produced trustworthy results. This is the **normal** shape for the kills the facet exists to explain: a block-buffered child SIGKILLed by `memory.oom.group` leaves a partial last block. It is already recorded in the tree as an observation and never filed: `.aira/tickets/AIRA-91.md` (~:473) describes the real oomd artifact and its `groupkill` probe as "both truncating mid-line with the trailer glued on with no newline". It matters because the trailer is all a **foreground** caller has: `--json` is refused for non-management confine (`cmd/aira/main.go:213`), `record.json` is written only by the detach supervisor (`internal/runner/confine_detach_linux.go:313/349`), and the wait status gives 137 for any SIGKILL — which is precisely why `terminated-by=` exists. Not a duplicate: AIRA-91 fixed the trailer's *content*; this is its *position*.

**ROOT CAUSE.** `confineLockedWriter` (`internal/runner/confine_linux.go:173-181`) takes a mutex and forwards bytes verbatim; it tracks no column state, and nothing in `internal/runner` does. That writer is `diagnostics` (`:742-746`) **and** the child's stderr (`:1173`, `cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, diagnostics`). Since it is never an `*os.File`, os/exec builds a pipe plus a copy goroutine that `cmd.Wait()` joins, so the trailer at `:1457` (`fmt.Fprintln(diagnostics, FormatConfineStatus(...))`) is written after the child's stderr is fully drained — the glue is deterministic, not racy. `FormatConfineStatus` returns a bare `confine: slice=...`. Same defect, strictly more exposed (they fire *while* the child is writing), on the late-signal line `:863`, the first-signal line `:882`, and the deadline advisory `:1322`. The ci-shim twin repeats it at `internal/runner/confine_shim_linux.go:488`. The two OOM advisories at `:1458` and `~:1471` sit at column 0 only *derivatively*, inheriting the newline the trailer's own `Fprintln` supplied — a consumer's fallback onto them is inheriting, not guaranteed.

**PROPOSED FIX.** Prepend an unconditional `\n` to the trailer and to the three mid-run diagnostics — `fmt.Fprintf(diagnostics, "\n%s\n", ...)` at `:1457`, `confine_shim_linux.go:488`, `:863`, `:882`, `:1322`. One line each, no new state, costs one blank line. **Deliberately not the `lastByteWasNewline` bool the rant proposes:** the child's *stdout* is never wrapped (`stdin, stdout := request.Stdin, request.Stdout` at `:1085-1090`, wired raw at `:1173`), so under an ordinary `aira confine -- pytest > log 2>&1` — the shape of AIRA-91's captured artifact — a partial stdout block reaches the log while the locked writer still believes it is at column 0. Column state cannot be made complete without the writer owning a stream it deliberately does not touch.

**HOW TO TEST.** (1) Using the existing helper `confineTrailer` (`internal/runner/confine_termination_linux_test.go:311`), run `/bin/sh -c 'printf partial-line >&2'` and assert the trailer *begins a line* — anchored, not `strings.Contains`; every current trailer assertion is a whole-buffer `Contains`, which is exactly why no test catches this. (2) The discriminating case: pass the **same** `*os.File` (under `~/tmp`, never `/tmp`) as both `Stdout` and `Stderr` with a child writing a partial line to stdout only; fails against current code *and* against a `lastByteWasNewline`-only patch. Do not substitute a shared `*bytes.Buffer` — a non-file Stdout takes os/exec's pipe path and is not the shape being modelled. (3) The shim twin. (4) The mid-run diagnostics with a signalled or deadline-fired supervisor.

**Severity note.** P2, not blocker: nothing malfunctions, the content is correct, exit 137 still reaches the caller, and `confine.go:911` documents the line as "the single operator-facing honesty projection" with `--json` deliberately refused. If the owner rules the foreground trailer **is** a supported scrape surface, this becomes P1 and should also settle whether a foreground `record.json`/`--json` is owed.

---

## Amendment — owner-elevated (2026-09-09, via speed session; origin RANT-39)

**Elevated to immediate priority as the window-closer for the aitest spurious-red exposure.** Mark
has directed removing fastest-ee's `FASTEST_NO_AITEST=1` pins on hosted/services/pipeline (and
retiring xdist entirely) NOW, accepting a temporary spurious-red window. With the pins off, an
aitest worker systemd-oomd-killed (137) under sustained PSI is exactly the shape this bug
misclassifies: the glued trailer makes an anchored `^confine:` parse miss `terminated-by=`, so a
clean KILL reads as an ordinary gate FAIL (a spurious RED) instead of a clean KILLED/unevaluated
that requeues. Closing this makes the residual exposure HONEST while the deeper fix (AIRA-178
live actuator) is planned. Building this next.

## Build review record — DONE (2026-09-09)

Implemented as the ticket's proposed fix: an unconditional leading `\n` at the trailer
(`confine_linux.go`), the ci-shim twin (`confine_shim_linux.go`), and the mid-run diagnostics
(late-signal, first-signal, deadline advisory) — NOT a `lastByteWasNewline` bool, because the
child's stdout is wired raw and is never seen by the `confineLockedWriter` (the discriminating
shared-`*os.File` test proves this).

TDD: two anchored `(?m)^confine:` tests (partial stderr; the discriminating shared-`*os.File`
stdout case), both RED before the fix, GREEN after; full `internal/runner` suite green.

Adversarial build-review (Fable): SHIP, no P0-P2. Three P3 fold-ins ALL taken — the ci-shim
signal lines `:373/:379` (Fable showed the ci-shim is the owner-elevated Batch path and is *more*
exposed there, so the earlier "advisory-only, lower value" scope call was corrected), the
exclusivity-lost mid-run warning, a single-`Write` atomicity comment, and a new shim-twin test.
Verified no false-fail: every diagnostics consumer uses `strings.Contains`; the post-trailer
advisories still render on their own lines; `confine_never_ran`'s HasPrefix is a different emit
site. Accepted coverage gap: the mid-run/shim signal lines are fixed but tested by inspection
(they need mid-run signal injection); the parsed load-bearing surface — the trailer, real and
shim — is tested both ways.
