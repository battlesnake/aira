---
{"schema":1,"id":"AIRA-222","project":"aira","title":"confine exits 0 with admission=unevaluated, so a mis-provisioned host runs every job ungoverned and looks fine","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["ci","confine","silent-degradation"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-224","to":"AIRA-222"},{"kind":"relates","from":"AIRA-225","to":"AIRA-222"}]}
---
Found putting aira v0.4 into a Docker CI runner image (fastest-ee GCP Batch lane). From the `deploy` session. Every figure measured, in an arm64 container.

THE SHAPE. `aira install --ci=shim` at Docker BUILD time leaves an install record in the image but no slice. A fresh container from that image:

    $ aira confine -- /bin/echo hi
    aira: warning: memory admission unevaluated (slice-not-found); launching without an admission lock
    hi
    confine: slice=ci-shim ... admission=unevaluated ...
    $ echo $?
    0

confine SUCCEEDS having governed nothing. The only signal is one stderr warning, which in a CI log sits among thousands of lines and is never read.

WHY IT IS WORSE THAN ORDINARY MISCONFIGURATION. The obvious way to containerise aira is to install it in the image, and the obvious smoke test is `aira confine -- /bin/true` in that same build layer. That test PASSES, because the slice exists while the layer builds. The failure appears only at runtime and is silent there. We were one review away from shipping an image that ran every merge-gate leg with no admission lock, on every branch, looking green.

WHY THIS IS A DEFECT NOT USER ERROR. Same class as AIRA-220, which you just fixed: `confine --list` reporting a fabricated 0B granted rather than saying it had no ledger. The lesson was that "nothing there" and "nothing measured" must not share an encoding. This is that lesson on the LAUNCH path rather than the REPORT path: "governed, slice was empty" and "not governed at all" currently share an exit code.

SUGGESTED FIX, in preference order:
1. A --require-admission / --fail-closed flag making an unevaluated admission a REFUSAL. Callers that do not care keep todays behaviour; CI and anything unattended opt into loudness.
2. Consider that as the DEFAULT for --ci installs. A --ci install is unattended by definition, so the warning has no reader.
3. `aira install` could refuse or warn loudly when run inside a Docker BUILD. The install is where the mistake is made; confine is only where it surfaces.

OUR WORKAROUND, for reference: install at container START, then probe and refuse the parallel path if the trailer says unevaluated:

    probe=$(aira confine -- /bin/true 2>&1 || true)
    printf %s "$probe" | grep -q admission=unevaluated && fall_back_to_serial

That works, but every consumer must reinvent it, and only after being bitten.

USEFUL CONTEXT: a RUNTIME `install --ci=shim` fixes it entirely (admission=immediate) and additionally reads the ledger budget from the live host rather than freezing the builders. Our image is built on n4a-standard-8 and runs on n4a-standard-16, so a baked budget is wrong in both directions.

## Build review record — DONE (2026-09-09)

Fix #1 (the flag): `--require-admission` (ConfineRequest.RequireAdmission) refuses to launch —
terminal E_CONFINE_UNAVAILABLE — when the job was NOT admitted, on both the real and ci-shim paths,
via a shared `requireAdmissionRefusal`. Opt-in, default-off; ordinary launches are untouched.
Replaces deploy's grep-the-trailer workaround with a real exit code, and is the honest fallback
AIRA-224 will name. Documented in the core dispatch table (help/agent-guide/MCP) and the confine
skill text.

Deferred, explicitly (asks #2/#3): default-on-for-`--ci` and refuse-inside-Docker-BUILD both
interact with AIRA-224's real-vs-advisory-mode distinction and the build-vs-runtime install policy;
filed as follow-ups rather than guessed.

TWO-LOOP RECORD — the adversarial review was load-bearing. The first cut keyed the gate on
`admission == unevaluated` and passed its own green tests. Fable's build-review BLOCKed it with a
MEASURED P1: a flock-fallback `timeout` admission (the real path's "waited the whole budget,
admitted nothing, launching anyway") is an ungoverned launch that the `== unevaluated` key let
through. Fix: key on NOT-admitted (state ∉ {immediate, waited}), which catches `timeout` too while
still allowing a real flock `immediate`/`waited` (so a daemon-restart on a real slice still
launches). Fable also caught that the original commit prose overclaimed a daemon-down real-path
refusal that does not happen (real daemon-down flock-admits), the flag's absence from the dispatch
table, and the untested CLI→request wiring; all fixed. Owner-visible semantic choice recorded:
keyed on "admitted" rather than the stricter "daemon-booked", because flock admission is real
governance and the stricter key would make the flag unusable during a daemon restart — a one-line
change if the owner prefers otherwise.
