---
{"schema":1,"id":"AIRA-179","project":"aira","title":"aira rant has no way to target shared/cross-project tooling friction -- every rant is scoped to whichever project it was filed from","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["dogfood","rant"],"hold":false,"relations":[]}
---

Owner observation (2026-09-08): "Perhaps `aira rant` needs a flag to do
'global rants' about shared tools, in addition to the rants about friction
within projects."

## Verified from source

Rants are entirely project-scoped, with no exception: every table
(`rants`, `rant_tags`, `rant_context_refs`, `rant_git_context`,
`rant_reviews`) keys on `project_id`
(`internal/store/rant.go:50-113,130-211`), and `AddRant`'s numbering,
sequencing and event stamping all resolve against `s.projectID` -- the
project the CWD currently resolves to. There is no aggregation, mirroring,
or cross-project rant concept anywhere in the codebase.

## The gap this creates

Friction about a SHARED tool (aira itself, or any cross-cutting machine-level
concern) reported from a DOWNSTREAM consuming project lands only in that
project's own `.aira` state -- invisible to anyone auditing the shared
tool's own friction unless they manually trawl every project that happens
to depend on it. Tonight's own dogfooding is a degenerate case where this
doesn't bite (this session works IN the aira repo itself, so its own rants
about aira already land in the right place) -- but any OTHER project's
session hitting an aira-tooling paper cut has no way to route that
complaint anywhere aira's own maintainers would see it.

## Not designed here

Whether this should be a `--global`/`--shared` flag that redirects a rant's
target project (to aira's own project, if adopted on the machine, or to a
configured "shared tooling" project id), a mirror/forward that files in
BOTH the local and a shared project, or something else entirely, is a real
design question -- including how a project that has never adopted/registered
the shared target would even resolve it, and what identifies "this rant is
about the tool, not about my own project's work" (a tag? a separate verb?).
Filed to record the idea and its evidence; not committed to a shape.
