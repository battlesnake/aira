---
{"schema":1,"id":"AIRA-193","project":"aira","title":"Admission-wait line renders live ledger totals as raw byte counts, defeating the at-a-glance comparison the line exists for","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","ux"],"hold":false,"relations":[]}
---
Filed from the AIRA-181/AIRA-186 build (see the ACCEPTED GAP comment on
`confineHeldNote`, internal/runner/confine_queue_position_linux.go).

AIRA-181 added the running set's held reserve to the admission-wait progress
line so a caller at queue position 1 can tell "waiting on running jobs" from
"nothing is blocking me". It works, but the figures are rendered by
`FormatConfineBytes` (internal/runner/confine.go:880), which picks whichever of
T/G/M/K divides the value EXACTLY and otherwise falls back to raw bytes. A
granted total is a sum of live ledger charges and is essentially never round in
any unit, so the clause routinely prints an eleven-digit integer.

## Verified live

Against the running daemon on a contended slice, 2026-09-09:

    confine: waiting for memory admission on aira.slice (reserve 12G, waited 30s,
    slice ceiling reduced ..., queue position 4 of 6 by enqueue order,
    5769372876 queued ahead, 54194584616 already granted across 5 admitted jobs
    / 52608M slice ceiling)

Two raw byte counts beside one rounded ceiling. The CATEGORY error AIRA-181 was
filed for is fixed -- the blocker is now named, where the line previously said
only "0B queued ahead" -- but comparing 54194584616 against 52608M at a glance
is exactly what this line exists to make easy, and it does not.

The pre-existing AIRA-24 "queued ahead" figure has the same problem and always
has; AIRA-181 did not introduce it, only made it more visible by putting a
second (larger, always-unround) figure beside it.

## The remedy already exists

The owner made this exact call for `aira top` at 4ea6b86 ("show quantities in M
rather than K or bytes"): one fixed unit, rounded to the nearest MiB
(`topFormatMegabytes`, cmd/aira/tui_top.go:147), whose own comment names the
same root cause -- "live cgroup/RSS readings are essentially never round in any
unit, so that formatter mostly prints bytes here".

## Why it was not done in the AIRA-181/186 PR

Applying it to this line means also moving AIRA-24's existing "queued ahead"
figure, because two unit systems in one sentence would read worse than either.
That is a decision about what unit the admission-wait line speaks -- a change to
an existing surface other sessions read -- not part of the reporting AIRA-181
and AIRA-186 asked for. Filed rather than smuggled into that PR.

## Not designed here

Whether the shared rounding rule should be promoted next to `FormatConfineBytes`
in internal/runner so the CLI and TUI faces have one implementation, and whether
MiB or a one-decimal GiB (which is what both AIRA-181's and AIRA-186's peer
reporters reached for when writing their own suggested wordings -- "52G / cap
64G", "granted 61.2G/61.6G", "your grant 35.7G") reads better on a one-line
diagnostic, are left for whoever picks this up. `FormatConfineBytes` itself must
NOT be changed: it also renders the confine trailer's `cap=`, `peak-rss=` and
`effective=` facets, where the exact-divisor behaviour is correct.
