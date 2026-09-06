---
{"schema":1,"id":"AIRA-130","project":"aira","title":"install --ci=shim: floor the meminfo-MemTotal fallback budget at 4G like the declared/cgroup-derived sources (AIRA-121 F4 residual)","status":"in-review","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","ci","confine","install"],"hold":false,"relations":[]}
---
Found by the AIRA-121 round-4 build review (PR #72, merged as 8f134ee). Accepted there as a documented, non-blocking coverage gap rather than forcing a fifth round; filed so the gap is not silent.

## The gap

AIRA-121 round 4 (finding F4) floors the ci-shim ledger budget at minimumCeilingGiB (4G) for the DECLARED (--memory-max) and CGROUP-DERIVED (own memory.max) sources in internal/install/mode.go resolveShimBudget: the daemon's admission headroom is 2GiB base + 64MiB/job and checkedAvailable answers 0 for every job whenever maximum <= headroom, so a sub-floor budget installs cleanly and then wedges every job forever at E_ADMIT_TOO_LARGE cap_minus_headroom=0.

The third source, the /proc/meminfo MemTotal fallback (ShimBudgetSourceMemTotal), was deliberately left un-floored ("the whole host's memory is essentially never below 4GiB"). That is the same failure shape one source over, and it is inconsistent with the real --ci path, which floors ITS host-wide source (MemAvailable, resolveCIMemoryMax) at the same 4G.

Trigger: no --memory-max, an unbounded or unreadable container memory.max (e.g. `docker run` with no --memory), and a host with MemTotal <= ~2.06GiB (e2-small / t3.small class). Not an ordinary CI box, which is why it was not made blocking; the resulting state is exactly F4's.

## Executable reproduction (run during the review, not committed)

1. internal/install: fake install with /proc/meminfo `MemTotal: 2097152 kB`, /proc/self/cgroup `0::/`, <cgroupRoot>/memory.max `max`; runInstall(installOpts{ciValue:"shim", stage:build}) returns err=nil and records shim_budget_bytes=2147483648 shim_budget_source=meminfo-memtotal.
2. internal/daemon: NewServer + SetConfineShimModeForTest(2<<30, ShimBudgetSourceMemTotal, "") + SetShimMeminfoForTest(2GiB total, 1.5GiB available); readShimMemory -> current=536870912 maximum=2147483648; checkedAvailable(current, maximum, 0, 0, admitSliceHeadroom(1)=2214592512) == 0. Every job answers E_ADMIT_TOO_LARGE cap_minus_headroom=0.

## Fix

Apply the same `bytes < minimumCeilingGiB<<30` refusal to the MemTotal branch of resolveShimBudget (E_INSTALL_UNAVAILABLE, naming the MemTotal value and the floor, pointing at --memory-max), plus a regression test in internal/install/ci_shim_mode_test.go proven to FAIL against the current code, in the style of TestShimInstallRefusesACgroupDerivedBudgetBelowTheFloor. Keep the existing "nothing readable" refusal as-is. Also fold a one-line correction into plan §4.2 / residuals of docs/superpowers/plans/2026-09-06-aira121-ci-shim-mode-plan.md.

## Resolution (in-review — PR pending, branch `aira130-shim-memtotal-floor`)

The MemTotal branch of `resolveShimBudget` (`internal/install/mode.go`) now
applies the SAME `bytes < minimumCeilingGiB<<30` refusal the declared and
cgroup-derived branches got in AIRA-121 round 4, with the same
`E_INSTALL_UNAVAILABLE` shape, naming the offending MemTotal value
(`formatCeilingBytes`) and the 4G floor, and pointing at `--memory-max`. The
"nothing readable" refusal below it is unchanged, and the at-floor case (exactly
4GiB) is still accepted from all three sources — the refusal is strictly below
the floor.

Regression tests in `internal/install/ci_shim_mode_test.go`, in the style of
`TestShimInstallRefusesACgroupDerivedBudgetBelowTheFloor`:

- `TestShimInstallRefusesAMemTotalDerivedBudgetBelowTheFloor` — the ticket's
  reproduction exactly: `MemTotal: 2097152 kB`, `memory.max` = `max`, no
  `--memory-max`. Asserts the `E_INSTALL_UNAVAILABLE` refusal, that the message
  names the value, the floor and `--memory-max`, that no daemon is spawned, and
  that NO install-mode record is left behind for a later `--stage=start` to pick
  up. **Non-porosity proven**: with the fix temporarily reverted this test fails
  with `err=<nil>` (and it is the only test that fails).
- `TestShimInstallAcceptsABudgetExactlyAtTheFloor/meminfo-memtotal` — a 4GiB
  host MemTotal is still accepted, still with
  `shim_budget_source=meminfo-memtotal`, guarding the boundary against an
  off-by-one `<=`.

Plan-doc correction folded in
(`docs/superpowers/plans/2026-09-06-aira121-ci-shim-mode-plan.md`): §4.2 gains an
`[AIRA-130 correction]` note saying all three sources are floored and why the
round-4 reasoning was wrong, and residual 8 records the F4 residual as closed
here.

Verification from the worktree, exact exit codes:

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0 (all
  packages ok)
