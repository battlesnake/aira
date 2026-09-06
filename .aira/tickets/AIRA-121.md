---
{"schema":1,"id":"AIRA-121","project":"aira","title":"aira install/confine: ci-shim mode for systemd/cgroup-unavailable containers (GCP Batch, similar CI/batch containers)","status":"planned","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","ci","confine","install"],"hold":false,"relations":[]}
---
Requested directly by the owner, 2026-09-06, as a follow-up question on AIRA-120.

## The problem AIRA-120 does not cover

AIRA-120 assumes a systemd + delegated-cgroup-v2-available host. A
containerised batch execution shape (GCP Batch container runnables, and
similarly AWS Batch/Fargate, Cloud Build, a Kubernetes Job, a container-mode
CI runner) typically has NO systemd running inside the container at all --
not even for the one-time `aira install` step. Verified against current
source, 2026-09-06:

- `aira install` hard-requires `systemctl --user daemon-reload` /
  `enable --now` to create `aira.slice` and register `aira-daemon.service`
  (internal/install/install.go). No systemd user manager -> install fails
  outright, before any confine/run ever gets a chance to work.
- Per-job `aira confine` itself does NOT re-invoke systemd at request time
  (no systemd-run/StartTransientUnit anywhere in internal/runner) -- it writes
  cgroupfs directly (mkdir a scope dir under the already-delegated aira.slice
  subtree, write memory.max, cgroup.procs, etc). So the ONLY hard systemd
  dependency is the one-time slice+daemon-service setup at install, not every
  confine call -- but that one dependency is still a full, current blocker in
  a systemd-less container.
- The daemon binary has no systemd dependency of its own: `aira daemon serve`
  (cmd/aira/daemon_command.go) is a plain subcommand; systemd is only how
  `aira install` currently chooses to supervise it.
- `aitest`'s pytest worker pool (internal/pylib/aitest) already has a real,
  tested 'daemon-down fallback mode' (Task 16 / AIRA-37 residue --
  test_daemon_down_fallback_completes_suite_with_one_warning_no_admit_subprocess
  in test_supervisor.py): when the daemon is unavailable it runs the whole
  suite via a bare os.fork() worker pool with ONE honest warning, no cgroup
  placement attempted, and time/test-count-based worker recycling still
  governing (RAM-watermark recycling is the only piece that needs a granted
  cgroup scope and is skipped). This already substantially satisfies 'aitest
  concurrency governing' for the systemd-less case -- it does not need a
  redesign, only confirming/keeping it wired up under the new mode below.

## What to build

`aira install --ci=shim` (or auto-detected when `--ci` is given and the
daemon-target probe below fails -- decide which at build time, but whichever
is chosen, ALWAYS report explicitly which mode actually got installed; never
let two boxes running nominally 'the same' install command end up silently
different):

1. A capability probe at install time: can we actually create/own a delegated
   cgroup v2 subtree and talk to a systemd user manager? If not (or if
   `--ci=shim` is explicit), skip ALL systemd unit creation (no aira.slice
   unit, no aira-daemon.service unit) and instead start `aira daemon serve`
   directly as a plain background process, appropriate for that container's
   own lifetime (no persistence expected or required across container
   restarts -- each fresh Batch/CI container is expected to install+run once).
2. In this mode, `aira confine --`/`aira run` runs the job as a normal
   subprocess with NO cgroup scope creation attempted at all (skip it
   entirely up front -- do not attempt and fail, which would be noisy and
   pointless in an environment already known not to support it).
3. Containment in this mode is ADVISORY-ONLY: an in-daemon RAM-budget ledger,
   reusing the EXISTING per-signature reservation-estimate machinery from
   AIRA-67 (peak-RSS history-based estimator) purely for ADMISSION GATING
   (queue a new job if the ledger says the container's own declared/probed
   total RAM is already fully booked by admitted jobs) -- with NO real
   per-process kill backstop, since there is no cgroup to kill. This is a
   genuinely weaker guarantee than the real slice and MUST be reported
   honestly everywhere containment is claimed (`confine --list`/`--status`,
   any 'scope-integrity'-style field) -- e.g. 'containment: advisory
   (ci-shim, no cgroup)' rather than anything implying a hard bound. This
   project's own existing idiom for exactly this class of tradeoff is
   aitest's 'marking unevaluated rather than running unconfined silently' --
   follow the same honesty discipline here, do not invent a new one.
4. Read the container's own total RAM budget for the ledger the same way
   AIRA-120's --ci reads free RAM (MemAvailable at install time; if the
   container's cgroup memory.max IS readable even though we cannot write our
   own nested cgroup, prefer that as the more accurate 'what am I actually
   allowed' figure over host-wide MemAvailable -- decide which source is more
   correct for a container context during build and document the choice).
5. aitest's daemon-down fallback needs NO changes to keep working under this
   mode (it already tolerates no daemon); confirm this with a real end-to-end
   test rather than assuming it. A reasonable, separate enhancement (not
   required for this ticket to close, note as a follow-up if not done here):
   let aitest's worker-count 'auto' sizing consult the new shim RAM-budget
   ledger when a daemon IS present in shim mode, rather than staying
   deliberately CPU-only-sized in that case.

## Deliberately out of scope here

Do not try to make the real cgroup slice mechanism work inside a container via
elevated privileges, --cgroupns=host, or similar -- that is fragile and
provider-specific (Docker/containerd cgroup v2 delegation behaviour is
inconsistent across GCP Batch/AWS Batch/Fargate/k8s), and is exactly the kind
of per-provider special-casing this project's architectural-simplicity rule
warns against. The shim's job is to degrade honestly, not to chase real
containment into every possible container runtime.

## Tests

Real end-to-end coverage: (a) install with the systemd/cgroup probe forced to
fail resolves to shim mode, starts a plain-process daemon, and confine/run
jobs execute successfully with no cgroup scope created; (b) the RAM-budget
ledger actually gates admission (a second job queues when the first has
'used' the whole budget per the estimator, and is admitted once the first
completes); (c) confine --list/--status report the advisory-only containment
state honestly in shim mode, distinguishably from the real-slice case; (d) a
real aitest suite run completes successfully in shim mode end-to-end.
