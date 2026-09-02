# aitest

A pytest plugin that replaces `pytest-xdist` for AIRA-governed suites: a
fork+admission worker pool with per-worker kernel-enforced cgroup memory
containment, in place of `pytest-xdist`'s execnet-spawned, ungoverned
workers.

Activate with `--aitest-workers=N` (a fixed worker count) or
`--aitest-workers=auto` (up to the host's CPU count). This is a NEW, explicit
flag rather than a reinterpretation of `-n` — a project with `pytest-xdist`
installed for unrelated reasons must not have its flag silently hijacked.

`aitest` is a from-scratch replacement for `aira_xdist_governor` (this
package's sibling under `internal/pylib/`), which is retired once `aitest`
reaches feature parity — see
[`docs/superpowers/specs/2026-09-01-aitest-design.md`](../../../docs/superpowers/specs/2026-09-01-aitest-design.md)
for the full design, staging, and the governor's retirement plan (§3.8).
