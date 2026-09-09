---
{"schema":1,"id":"AIRA-224","project":"aira","title":"ci-shim: let worker-admit use the CONTAINER's own cgroup memory.max as the outer bound, so aitest is governed in CI","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","ci-shim","containers"],"hold":false,"relations":[]}
---
From the `deploy` session, fastest-ee. This is the one change that would make aira genuinely useful in a containerised CI lane rather than nearly useful.

WHERE IT STANDS TODAY, measured in an arm64 container with v0.4:

  * `confine --delegate-ram` in ci-shim DOES export the aitest coordinates —
    AIRA_AITEST_LIB / _WORKER_ADMIT_CMD / _BOOTSTRAP_CMD / _MAX_WORKERS_FALLBACK=16. So aitest is
    selected and runs.
  * But your guidance says worker-admit "refuses to grant anything unless that outer scope has a
    finite memory.max", and ci-shim creates no scope at all (`scope-memory.max=not-requested`,
    `containment=advisory(ci-shim,no-cgroup,no-kill-backstop)`).
  * So aitest falls back to "its own reduced worker pool (n_workers <= min(requested, NumCPU)) with
    no PER-WORKER cgroup placement and a visible warning".

Net effect in CI: parallelism WITHOUT per-worker containment — the one combination that risks the
over-contention and OOM the tool exists to prevent.

THE OPENING. aira already reads the container cgroup when one exists. Measured, same container run
with `docker run --memory=4g`:

    cgroup memory.max seen in container: 4294967296
    cgroup writable? yes
    shim ledger budget: 4.00GiB (4294967296 bytes) from the containers own cgroup memory.max

So in ci-shim you ALREADY prefer the container cgroup over /proc/meminfo for the LEDGER BUDGET. The
container cgroup is a real, finite, kernel-enforced memory.max — which is exactly the precondition
worker-admit is asking for. It just is not aira-created.

THE ASK: when running in ci-shim inside a container whose own cgroup has a finite memory.max, treat
that as the outer bound for worker-admit, and place worker sub-scopes under it (the cgroup is
writable, as measured above). That would give real per-worker containment in CI without needing a
systemd user manager, which is the thing a batch container cannot have.

IF THAT IS NOT SOUND, the useful answer is a documented statement of it, because the current
behaviour is easy to mistake for working: the coordinates are exported, aitest engages, workers
spawn, and nothing says the containment you asked for is absent unless you catch one warning.

WHY WE CARE, for prioritisation: fastest-ee gates every branch on a dedicated n4a-standard-16 Batch
VM. A full serial gate is 34-76 minutes; leg-sum is 19-30 minutes against 15-55 minutes of
non-test work, so parallelism is worth about 21%. We will take that only if it is governed — an
ungoverned parallel gate that OOMs is worse than a slow honest one. Right now we can have parallel
or governed, not both.

Related: AIRA-222 (a mis-provisioned host runs ungoverned and exits 0).

─────────────────────────────────────────────────────────────────────
CORRECTION (deploy session, same night) — a claim above is WRONG, and the corrected finding is
STRONGER, so please read this before actioning.

RETRACTED: "cgroup writable? yes" and "the cgroup is writable, as measured above". That came from
`test -w /sys/fs/cgroup` run as root, which passes the access() permission check even when the MOUNT
is read-only — root bypasses the DAC bit, and the read-only-ness is only enforced at write() time.
A real write proves it: in default Docker, `mkdir /sys/fs/cgroup/x` fails "Read-only file system",
and so does writing cgroup.subtree_control. So the container cgroup is NOT writable by default. aira
READING memory.max for the ledger budget works (a read); the "place worker sub-scopes under it" ask
does not, because there is nowhere to create them.

CORRECTED, AND BETTER: real, kernel-enforced, systemd-FREE per-worker containment IS achievable in a
container — the burden is shared with the CALLER, who must launch it with a writable delegated
cgroup. Measured tonight, cgroup v2:
  * default `docker run`                         -> /sys/fs/cgroup read-only, no scopes possible
  * `--cgroupns=private` alone                   -> still read-only
  * `--cgroupns=private` + a read-write bind mount of /sys/fs/cgroup (the container-namespaced root,
    so it exposes only the container own subtree, not the host) -> WRITABLE. Created a child scope,
    wrote memory.max=128M and memory.swap.max=0, moved a process in, and the kernel OOM-KILLED a
    400M allocation inside it. Enforcement real. (swap.max=0 is load-bearing: with the container
    default swap, memory.max alone is a soft threshold the job pages past — which aira already knows,
    it sets memory.swap.max=0 on every scope.)
  * podman `--cgroups=split` is the other route and your own skill text already references it.

SO THE CORRECTED ASK, which supersedes "use the container cgroup you are ignoring": ci-mode should
trigger REAL scopes on the presence of a WRITABLE DELEGATED cgroup, not on the presence of systemd —
those are different things, and a container can supply the former without the latter. Probe for a
writable, memory-delegable subtree; use real scopes when present (full worker-admit path, no systemd
needed); fall back to advisory shim only when there is genuinely no writable cgroup, and SAY which,
naming the one launch-flag change that would upgrade it. Today advisory is silent that a governed
mode was one docker flag away.

The corollary for us: this is not aira fixing itself in isolation — our CI must launch the gate
container with a delegated writable cgroup for even a smarter aira to have somewhere to work. That
half is ours.
