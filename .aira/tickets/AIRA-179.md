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

## Not designed here (superseded by the decision below)

Whether this should be a `--global`/`--shared` flag that redirects a rant's
target project (to aira's own project, if adopted on the machine, or to a
configured "shared tooling" project id), a mirror/forward that files in
BOTH the local and a shared project, or something else entirely, is a real
design question -- including how a project that has never adopted/registered
the shared target would even resolve it, and what identifies "this rant is
about the tool, not about my own project's work" (a tag? a separate verb?).
Filed to record the idea and its evidence; not committed to a shape.

## Decision (2026-09-09): an explicit rant target selector, not a shared-project concept

**Shape.** `aira rant [--project <id>|--prefix <PREFIX>] <text>`, and the same
selector on every rant sub-verb (`capture`/`ls`/`get`/`review`/`redact`). It
names the project the rant operation applies to. There is no `--shared`, no
"shared tooling project" concept, no config key, no environment variable, and
no auto-discovery. A rant about aira filed from a downstream project is
`aira rant --prefix AIRA "..."`.

**Why this and not a configured shared target.** The two open questions the
ticket named both dissolve rather than needing new machinery:

- *How does a project resolve the shared target?* It does not have to. A
  project's owned ID prefix is already machine-wide unique
  (`prefix_ownership(prefix TEXT PRIMARY KEY, ...)`,
  `internal/store/store.go:785-789`), human-typable, and already resolvable
  against the one machine-wide `state.db` by
  `DB.ResolveProject(ctx, projectSelector, prefix)`
  (`internal/store/lifecycle.go:45-89`). The consuming project registers
  nothing; the target only has to be adopted on this machine. `--project`
  and `--prefix` reuse `eject`'s existing vocabulary and resolver verbatim
  (`internal/core/core.go:714-718`), including its refusal of both at once.
- *What identifies "this is about the tool, not my work"?* The target project
  itself. Not a tag (unenforceable, unsearchable across projects), not a
  second verb (a whole parallel surface for one argument's worth of meaning).

A configured default target was rejected on two counts. It adds a config
surface that must be repeated in every consuming project's `.aira/config`
(there is no machine-level aira config file today, and inventing one is a
new precedence-and-validation surface for a P3 convenience). More decisively,
it cannot extend to the case the ticket actually cares about most -- a
directory that never adopted aira has no `.aira/config` to hold the setting.
An explicit selector carries the target *in the request*, so it extends to
that case later by dropping the caller-scope requirement, whereas a
configured default is a dead end there.

Mirroring/dual-filing was rejected: two rant IDs, two event rows, two review
threads and a redaction that must reach both, for a provenance question that
one origin field answers. A tag-only convention was rejected: it moves no
data and does nothing for an unadopted caller. Read-side cross-project
aggregation was rejected as the primary shape: it needs a cross-project read
path *and* depends on writes that may never have happened.

**Mechanics (all seams already exist).** The daemon already reconstructs a
full store scope for an arbitrary registered project from disk --
`app.Discover(root)` -> `ScopeFromProject` -> `storeForScope`
(`internal/daemon/discovery.go:46-63`, `internal/daemon/scope.go:15`,
`internal/daemon/server.go:1072`) -- and `ProjectRegistration` returns the
target's active worktree roots (`internal/store/lifecycle.go:118`). So the
change is: resolve the selector, then build the core over the target
project's store at the single existing seam `coreForScope`
(`internal/daemon/server.go:1044`). Numbering, event sequencing, the
common-dir journal and the FTS index then all land in the target project
because the store *is* that project's store. Nothing in `rant.go` needs a
cross-project concept.

**Two honesty consequences that must be built with it, not after.**

1. `crossCheckGitContext` (`internal/store/rant.go:617-640`) compares the
   caller-observed repo root / worktree path / worktree id against the
   *store's own* scope and downgrades any mismatch to status `mismatch`.
   Writing naively into the target's store would therefore record a
   deliberate cross-project file as a provenance anomaly -- a fabricated
   alarm, which is the honesty rule violated in the other direction. The
   cross-check must instead be evaluated against the **caller's**
   daemon-validated scope (`storeForScope` has already checked that scope
   against `CanonicalScopeIdentity`, `internal/daemon/server.go:1085-1090`),
   so a redirected rant records the originating repo truthfully.
2. Git context can legitimately be `unevaluated`, so it is not a reliable
   origin marker on its own. Record an explicit `origin_project_id` on the
   rant, set daemon-side from the verified caller scope and empty for an
   ordinary local rant. A foreign rant must never be indistinguishable from
   a local one. (Schema change is free -- see the no-compat rule.)

**Typed `--ref` args validate against the target project.** That is the
natural consequence of the store swap, not a special case: a local
`TICKET-12` simply does not exist in the target and hits the existing
refusal, while a shared rant may legitimately cite the target's own tickets
(e.g. `AIRA-179`). No new rule.

**Refusals.** Entirely inherited: `E_NOT_ADOPTED` (no such project / no
project owns that prefix), `E_SELECTOR_AMBIGUOUS` (both selectors, or an
ambiguous ID prefix), `E_NO_PROJECT`. All already carry exit mappings
(`internal/codes/codes.go:62-63`). Nothing guesses a target; an unresolvable
one refuses by name.

**Accepted non-boundary (considered, not overlooked).** Any adopted project
can file, review or redact rants in any other registered project. This is
not a new exposure: `state.db` is one machine-wide file already fully
readable and writable by the same user, and `aira eject --prefix X` already
reaches another project destructively from anywhere. Adding an allowlist
would be exactly the per-feature machinery the simplicity rule forbids. The
mitigation is attribution, not authorisation -- `origin_project_id` above
makes every foreign write traceable to the project it came from.

**Deferred, with the trigger that would revive each.**

- A configured default target (`--shared` with no argument): only if typing
  `--prefix AIRA` proves to be real friction in dogfooding.
- Ranting from a never-adopted directory: today the client fails with
  `E_CONFIG_MISSING: no .aira/config was found` before a request is even
  built (`internal/app/project.go:515-528`), so a `confine`-only consumer
  still cannot rant. Closing it means serving a rant that carries an explicit
  target *without* a caller scope, recording origin as absent rather than
  faked. Separate ticket if it bites.
- Cross-project read aggregation (`rant ls` over several projects), mirroring,
  and surfacing origin in the TUI.

**Implementation note.** The daemon is the only production dispatch path; the
in-process dispatcher is a test substrate
(`cmd/aira/dispatcher.go:658-660`), so the parity tests that compare the two
need the selector honoured on both sides or an explicit, written exclusion --
not a silent divergence.
