---
{"schema":1,"id":"AIRA-70","project":"aira","title":"A confined job's SIGKILL is unattributable — three distinct kill paths produce byte-identical output","status":"planned","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["confine","daemon","dogfood","honesty"],"hold":false,"relations":[]}
---
## Finding (from the AIRA-67 investigation)

When a confined job dies with exit 137 (SIGKILL) and its supervisor survives to print a trailer, that trailer currently cannot distinguish between at least three structurally different causes:

1. **SIGINT/SIGTERM delivered to the confine supervisor itself** — the signal handler calls `scope.Kill()` (writes `cgroup.kill`) and then forwards the signal. No log line anywhere records that this happened or who/what sent the signal.
2. **An external `aira confine --kill` from another session** — `scope.Kill()` via `confine_manage_linux.go:489`; the daemon's dispatch for this (`confine_manage.go:141-152`) has no `log.Printf` at all.
3. **A slice-level OOM with no scope cap set** — the kernel actually did the killing, but the confine trailer's own OOM-advisory only fires when a scope cap was set (see the companion finding below), so this path is ALSO silent in the one case it would be most surprising to leave unexplained.

All three produce the same observable shape: supervisor survives, prints a normal-looking trailer, process exits 137. A caller — human or agent — has no way to tell which of these happened, or who/what was responsible, from anything AIRA records.

## Why this matters

This is precisely the class of problem this project's own honesty contract exists to prevent elsewhere (`unevaluated` never silently reads as `pass`, canaries must prove they fired, etc.) — a job's death is currently unattributable, indistinguishable from a false-negative-OOM or a legitimate external kill, no matter how carefully someone investigates after the fact. AIRA-67's own multi-hour investigation of one real incident ran into exactly this wall: everything upstream and downstream of the kill could be checked and ruled out, but the kill itself left no trace anywhere, by design (or rather, by omission).

## Suggested direction

Record teardown provenance on the confine trailer — a field like `terminated-by=signal:SIGTERM|external-cgroup-kill|oom|normal` — and add the log lines that are currently missing on the paths that can cause it: one in the confine supervisor's own signal handler (recording that an external signal arrived, and forwarding it, before or alongside the existing `cleanupConfineScope`/`scope.Kill()` behavior), and one in the daemon's `confine-kill` dispatch (`confine_manage.go:141-152`) recording the killer's identity (whatever ownership/actor information is already available to that RPC) and target every time it actually kills something. Companion fix: `internal/runner/confine_linux.go:817`'s OOM-advisory gate currently returns empty and stays silent precisely when no scope cap was set, which is the case where an unexplained 137 is most likely to be mistaken for something else — it should still report an OOM signal it can actually detect (e.g. via `usage.OOMKill`) regardless of whether a cap happened to be configured.

This is correctness-adjacent daemon/supervisor work (touches the signal-handling and kill-dispatch paths directly) — treat it with the same rigor as other daemon changes tonight (external review, TDD), not a quick logging patch.
