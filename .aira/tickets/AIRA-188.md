---
{"schema":1,"id":"AIRA-188","project":"aira","title":"aira init defaults an unset --prefixes to the hardcoded \"AIRA\" prefix, silently claiming a machine-wide-exclusive namespace another project already owns","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["init","ownership"],"hold":false,"relations":[]}
---

Peer report (devproc, 2026-09-08), verified from source. `aira init` in a
fastest-ee worktree failed with `E_PREFIX_OWNERSHIP_CONFLICT: AIRA owned by
project ... at /home/mark/tmp/aira-wt-AIRA-185` — bare `aira init` with no
`--prefixes` flag.

## Verified from source

`internal/app/project.go:452-454`: when `--prefixes` is unset, the default
is hardcoded to `[]string{"AIRA"}`, with no auto-detection from the project
slug, repo directory name, or anything else. `prefix_ownership` is a
genuinely machine-wide table keyed purely on the prefix string
(`internal/store/store.go:1949-1965`, `ON CONFLICT(prefix) DO NOTHING`) —
this is deliberate design (prefixes are meant to be globally unambiguous
across every project sharing this machine's coordination layer, the same
property that makes "AIRA-185" unambiguous without a project qualifier),
not a bug in the ownership model itself. The bug is that the *default*,
when the caller supplies no explicit choice, silently claims a real,
globally-exclusive namespace ("AIRA") that this repo's own dogfooding
project legitimately owns — so bare `aira init` succeeds exactly once per
machine, in this repo, and fails with a confusing ownership-conflict error
everywhere else.

## Recommendation: fail closed, don't guess (verified against this
project's own established pattern, not decided from principle alone)

Three shapes were proposed by the reporter: (i) derive a default from the
project slug/directory name, (ii) refuse bare `init` and name `--prefixes`
in the error, (iii) scope ownership per-project so `AIRA` isn't globally
exclusive.

(iii) is wrong by construction — it would defeat the entire point of
machine-wide prefix ownership, which exists specifically so an ID like
"AIRA-185" is unambiguous without a project qualifier; scoping it away
reintroduces the ambiguity the model was built to prevent.

(i) is tempting but doesn't actually fix the underlying problem: an
auto-derived guess (directory name, project slug) can collide too — many
repos share generic directory names — and it would still be an implicit,
silent choice the caller didn't make. It also doesn't match this
project's demonstrated pattern elsewhere: `aira check`'s
`unevaluated`-with-reason, admission's honest `E_ADMIT_SATURATED` rather
than a fabricated result, "ambiguous selectors are refused" (this
project's own CLAUDE.md, "AIRA is primitives, not judgement") — the
consistent house style is to refuse and name the missing input rather
than guess on the caller's behalf in a genuinely ambiguous, collision-prone
situation. **(ii) is recommended**: bare `aira init` with no `--prefixes`
should fail with a message naming `--prefixes` explicitly, rather than
silently attempting to claim `AIRA`. This is a real behaviour change
(breaks any existing script relying on the current bare-init default), but
the current default is actively harmful — a silent global-namespace grab
that only happens to work in exactly one repository on the machine — so
there is no compatibility burden worth preserving here consistent with
this project's stated pre-1.0 stance of prioritising correctness over
compatibility with a behaviour nobody should have been depending on.

## Follow-up (devproc, 2026-09-08) — the conflict error's own wording actively recommended the destructive remedy, fixed directly

Sharper than the default-behaviour question above: the actual
`E_PREFIX_OWNERSHIP_CONFLICT` text (`internal/store/lifecycle.go:175`,
`internal/store/store.go:1960`, verified — both identical) was `"%s owned
by project %s at %s; run aira eject --project %s"` — the *only* suggested
remedy was ejecting the colliding project, which for any caller other
than aira's own dogfood project means deregistering someone else's live
project. Combined with the (now-fixed) hidden `--prefixes` arg, the error
steered a caller toward the destructive wrong action while never
mentioning the safe one. Independent of whichever shape the default-prefix
fix above ends up taking, this was fixable immediately as a pure wording
change with no behaviour change: both sites now read `"...; pass a
different --prefixes, or if that project is yours to retire, aira eject
--project %s"` — the safe remedy first, `eject` explicitly scoped to
"yours to retire" rather than presented as the default move. Fixed
directly (`internal/store/lifecycle.go`, `internal/store/store.go`,
`go build ./internal/store/...` green) — same "purely trivial ...
mechanical" class as the Usage-string fix above.

## Follow-up (devproc, relaying an adversarial review from a second model, 2026-09-08) — the fail-closed fix needs two doors, not one

Sharpens the (ii) recommendation above rather than replacing it. A project
that never allocates IDs through aira at all (fastest-ee: its own
allocator, `scripts/next_id.sh`, has zero coupling to aira; it only wants
`aira spend` telemetry) has no `--prefixes` to name — refusing bare `init`
and asking for one forces a meaningless placeholder token (observed:
`aira init --project fastest-ee --prefixes FEESPEND`, a prefix that will
never be allocated). Verified from source: `ComputeEvent.TicketID` is
already an opaque, project-scoped external reference never resolved
against this project's own ticket table (`internal/store/compute.go:91`,
just trimmed; every read/write scoped by `project_id=?`) — so a
telemetry-only project genuinely has no dependency on owning any prefix
at all. **Amended recommendation: (ii) should offer two doors** — refuse
bare `init` naming `--prefixes` as today, but also accept an explicit
opt-out (e.g. `--no-tickets` / `--telemetry-only`) that registers the
project with zero prefixes, skipping prefix ownership entirely. This is a
real shape decision for `init`'s argument surface, left for whoever
builds AIRA-188 to settle alongside the refusal-message wording below —
not built here.

## Not designed here

Exact refusal message wording, whether `AIRA` itself should keep a
special-cased default *only* when running inside this specific repository
(detectable via the repo's own known project id/slug, matching how this
project already treats its own dogfooding specially in a few other
places) versus removing the hardcoded default entirely and requiring
every caller, aira's own repo included, to pass `--prefixes` explicitly,
and the exact shape of the `--no-tickets`/telemetry-only opt-out above —
left for whoever gates this. [[AIRA-190]]

## Adjacent, already fixed

The same investigation found `init`'s CLI usage string didn't reflect its
real `--project`/`--prefixes` args (`internal/core/core.go:708`,
`Usage: "init"` despite declared `Args`), which caused `aira help`/`aira
init --help` to mislead callers into believing init takes no arguments —
confirmed via a full census of all 46 dispatch-table verbs that this was
the only genuine mismatch (`help` and `check` are correctly bare, having
no real args), so a narrow point fix rather than a sweep. Already fixed
directly (`internal/core/core.go:708`, `go build`/`go test
./internal/core/...` both green) as this is exactly the "purely trivial
... mechanical" class this project's lighter path allows.
