# aitest consecutive-chunk batching (AIRA-372): design

Status: proposed, 2026-11-22. Not built. Ticket `.aira/tickets/AIRA-372.md`; deferrals in
`.aira/tickets/AIRA-373.md` (held). Source: the fastest-ee "speed" session's proposal (RANT-156,
`~/tmp/aitest-batching/2026-11-22-aitest-fixture-batching-design.md`, cited as "speed §N") and two
independent challenge reviews of it (`~/tmp/aitest-batching/challengers.json`: "minimum" and
"adversarial"). Code references are at `798af794`, `internal/pylib/aitest/`. Class: crash-retry and
attempts accounting change, so the correctness-critical path applies (Opus builds, Fable reviews,
two-loop on the build). **M** marks a measurement, **R** a figure reported by another session and
not re-derived here.

## 1. Problem and evidence

`worker.run_one` calls `item.ihook.pytest_runtest_protocol(item=item, nextitem=None)`
(`worker.py:542`). `nextitem=None` is pytest's "last item of the session" signal, so
`SetupState.teardown_exact(None)` (`_pytest/runner.py:538`, 9.0.3) tears down every collector on the
stack after every test, and with them every session, package, module and class fixture. Each test
pays for every shared fixture it uses, every time. The `run_one` docstring and the README
(`README.md:138`) record this as an accepted Slice 1 limitation.

Evidence (R, fastest-ee engine suite, 68,802 tests, GCP gate runners, speed §2):
- one session fixture cost 42% of test time; fixed at source in fastest-ee, so it is not a target;
- after that fix, module-scoped fixture self-time is 4,556 s of 27,468 test-seconds (17%). That is
  the target, and an upper bound on the saving.

The gate runners use ci-shim ledger-only admission (no cgroup, no memory.max, no watermark). Any
design that switches itself off there gains nothing where the evidence was measured.

## 2. From-scratch minimum and decision

If only this problem existed: give one worker a run of consecutive items in the final collection
order and pass pytest the real next item within the run. pytest's own `SetupState` then keeps alive
exactly what serial pytest would keep alive for that range, tears down at scope changes, and handles
parametrised higher-scope fixtures through `FixtureDef.cache_key`. aitest needs no knowledge of the
fixture graph.

**Decision: slice 1 = consecutive chunks**, cut once in `Supervisor.collect()`, behind one env var,
default off. About 100 lines of product code plus tests.

| Speed's part | Verdict | Live justification, or the trigger to revisit |
|---|---|---|
| 4.1 sharing key from `item._fixtureinfo.name2fixturedefs` | **Cut** | pytest's `teardown_exact` already decides what stays alive. A key only matters if aitest reorders items to make sharers adjacent, which speed §4.3 itself forbids. It is private API (shape differs across pytest 7.4.4, 8.4.2, 9.0.3, reader-verified, R) and blind to `getfixturevalue` and collector-level setup. Revisit only together with reordering (never in AIRA-373 without a new measurement). |
| 4.2 measured cost model and per-repo history | **Cut, deferred** (AIRA-373) | No history exists to read: Python has no daemon read verb, the confine history is one row per run, and every CI container starts empty (`internal/runner/confine_shim_linux.go:513-531`). Its only job, "batch only when the saving exceeds 0.5 s", protects scheduling granularity, which the count cap already bounds. Trigger: the A/B shows chunking cheap-fixture modules costs wall time. |
| 4.3 consecutive runs, real `nextitem`, `None` for the last | **Keep** | This is the mechanism. |
| 4.3 predicted-duration bounds, LPT by batch seconds, tail re-split | **Defer** (AIRA-373) | No seconds data (`aira_time` is a unitless rank, `supervisor.py:361-365`). The count cap bounds the tail at cap x the slowest test. Trigger: the A/B drain grows. Cheapest later option: use this run's own `report.duration`, already passing through `_replay_event`. |
| 4.4 reservation = baseline + retained fixture RSS + max transient | **Cut** | No retained-RSS data, and a worker's memory.max is fixed at spawn (no resize path). Keep today's per-class reservation and measure peak per worker in the A/B. Trigger: peak rise that causes OOM kills or reaches the cap. |
| 4.4 pre-test watermark | **Keep, reduced** | Reuse `_should_recycle` unchanged, called before each non-first member (section 4). No `statm` (section 6). |
| 4.4 "tear down, report the rest unstarted" | **Cut as written** | Needs an out-of-protocol `teardown_exact(None)`, whose errors produce no report and land in `_exit_child(70)` (`supervisor.py:2115`). Replaced by running the tripping member with `nextitem=None`. |
| 4.5 per-item acks, crash requeues the unstarted rest | **Keep** | Section 5. Requires moving the attempts charge from pop to promotion. |
| 4.5 teardown errors on the item whose protocol ran them | **Keep** | Free from pytest; matches serial pytest (section 5.4). |
| 4.6 `aira_affinity`, `xdist_group` | **Cut, deferred** (AIRA-373) | Module-level and `lru_cache` state already lives for the worker's life (up to 200 tests). Affinity helps only by reordering non-adjacent items. Trigger: a suite shows a large cross-module shared cache with the A/B in hand. |
| 4.6 `aira_isolated` | **Cut, deferred** (AIRA-373) | Tests patching process-wide state are equally exposed today (one process runs up to 200 tests). Trigger: a batching-only failure that cannot be fixed in the project. |
| 4.6 `pytest_aitest_prefork` + `gc.freeze()` | **Defer** (AIRA-373, own item) | `gc.freeze()` already runs before every fork (`supervisor.py:3687-3688`); the hook is unrelated to fixture teardown; speed says "measure before recommending". |
| 4.7 `--aitest-batching` flag, ini, five tunables | **Collapse** | One env var, `AIRA_AITEST_BATCH_MAX_TESTS` (section 7); 1 means off and is byte-identical to today. The other tunables have no data source. |

## 3. Resolved design choice: static chunks, not a one-item look-ahead

Two shapes deliver the mechanism: **(a)** static chunks, where the supervisor dispatches a chunk in
one line, holds the unstarted `rest` itself and promotes the next member on each accepted result;
**(b)** a one-item committed look-ahead, where each dispatch line is `[nodeid, next]` and the
supervisor moves `next` out of the queue as a promise. **Choose (a).** From the code:

1. **One in flight stays one in flight.** Dispatch skips any worker with `in_flight` set
   (`supervisor.py:2362`) and a result must name `in_flight` (`:3207`). Under (a) `in_flight` is
   always exactly one nodeid and the invariant "`rest` non-empty implies `in_flight` set" makes both
   checks, event staging (`:3191`, reset at `:3234`), the stop condition (`:3795`) and
   `_pool_covers_the_queue` (`:3650`) correct unchanged. Under (b) every one of those sites must
   also honour a second slot, the promise: a promised worker may be sent only that item, never
   `__stop__`, and the worker must refuse a non-matching line.
2. **The pull model and `_ReadyQueue` stay as they are.** A chunk is one queue entry whose members
   share (need, cpu, time), so the class index, the 2-D fit filter, LPT and FIFO tiebreak, and the
   `_OldPicker` differential oracle (`test_ready_queue.py`) need no change. Under (b), FIFO pull with
   N workers interleaves them on adjacent items (worker A takes 1 promising 2, worker B takes 3
   promising 4; A's successor 3 is gone), so runs collapse to length 2. Longer runs need a new pick
   rule ("midpoint of the longest queued run"), a new index inside `_ReadyQueue`, and a pick order
   the oracle no longer matches.
3. **Attempts move at one site.** Today `attempts` is charged at pop (`:1291-1292`, `:1485`) and
   `requeue_once` refuses at 2 (`:1491`). Under (a) a chunk id's pop charges nothing; a member is
   charged when it becomes `in_flight` (dispatch of member 1, promotion of the rest). Single-item
   pops keep today's charge. (b) needs the same move for the promised item plus a promised-but-
   uncharged state.
4. **The recycle suffix needs no new field.** The worker still appends `__recycle_next__` to its
   last result line (`worker.py:620`); the supervisor already holds the unstarted `rest` and
   requeues it. Nothing unstarted travels on the result line, whose format (rpartition on the last
   space, `:3197-3206`) stays untouched.
5. **No supervisor round trip between members.** Under (a) the worker runs members back to back. Under
   (b) a worker holding live fixtures blocks for a supervisor line after every item, and the
   supervisor's single thread can be in a 5 s bounded admit read (`_ADMIT_READ_GRACE_SECONDS`).
6. **Deterministic boundaries.** Chunks are cut once from the final order, so the same commit and
   order give the same predecessors for every test, and an order-dependence failure reproduces.
   Under (b) boundaries depend on timing, so such a failure is flaky.

The cost of (a), accepted: the drain can grow by up to cap x the slowest test (a dispatched chunk
cannot be split), and members 2..n carry no dispatch-wait measurement. Both are measured in the A/B
(section 9); tail splitting is deferred to AIRA-373 with that trigger.

## 4. Specification

### 4.1 Wire format (exact)

- Single-item unit (every unit at cap 1): unchanged, `<nodeid>\n`.
- Multi-item chunk (n >= 2): `"\x01" + json.dumps([nodeid_1, ..., nodeid_n]) + "\n"`, sent once.
  JSON because nodeids contain spaces (`test_supervisor.py:1535`); `json.dumps`' default
  `ensure_ascii` escapes any newline. The `\x01` prefix is safe for the reason `_EVENT_LINE_PREFIX`
  is (`worker.py:48-60`: pytest ascii-escapes parametrised ids, the rest of a nodeid is a path); it
  is a new named constant, `_CHUNK_LINE_PREFIX`, because it travels the other direction.
- `__stop__\n`: unchanged.
- Results: one line per member, exactly today's format and order (all event lines, then
  `<nodeid> <outcome>`). Member 1 carries `__dispatch_wait_us__=<N>`; members 2..n carry no token
  (read as unmeasured by `_split_dispatch_wait`, `:124-140`). `__recycle_next__` is appended to the
  member after which the worker retires.
- A malformed chunk line (bad JSON, unknown nodeid) raises in the child and takes the existing
  `except BaseException: _exit_child(70)` path (`:2115`, `:2271`), that is, the crash path.

### 4.2 Chunk cut (in `collect()`, over `session.items` in final order)

`collect()` receives `session.items` after every `pytest_collection_modifyitems` hook
(`__init__.py:273-274`). Let `cap` be the resolved `AIRA_AITEST_BATCH_MAX_TESTS` (section 7). Each
item has a tuple `k(item) = (reservation_need, cpu_need, time_cost, item in _failfast)` and a module
key `m(item) = item.path` (public `Node.path`, never `None`).

1. A **segment** is a maximal run of consecutive items with equal `k` and equal `m`.
2. Walk segments in order, packing them greedily into the current chunk while all three hold: the
   segment's `k` equals the chunk's `k`; the chunk plus the whole segment stays at or under `cap`;
   the items are not fail-fast. Otherwise close the chunk and start a new one with the segment.
3. A segment longer than `cap` is split into pieces of `cap`; the last (shorter) piece may absorb
   following segments under rule 2.
4. **An `@aira_failfast` item is always a chunk of one.** It is never continued from (it runs with
   `nextitem=None` and sets up fresh), so it never inherits a teardown error of a fixture another
   test set up, and its failure keeps today's attribution (`_record_result`, `:3404-3409`).
5. Chunks never reorder items. Within a chunk, order is collection order.
6. A chunk of one is its nodeid, on today's wire. A chunk of n >= 2 gets the id
   `"\x01chunk:%d" % index` (never a nodeid, never printed) in `self._chunks: id -> [nodeids]`, and
   entries in `reservation_need`, `cpu_need` and `time_cost` equal to its members' shared values,
   so `_pick_class`, `_smallest_ready`, `_pool_covers_the_queue` and `_try_grow_one` work unchanged.
7. At `cap == 1` the queue is exactly `[item.nodeid for item in items]`, today's value.

### 4.3 Worker loop (`run_worker_loop`)

`run_one(item, nextitem=None)`: `nextitem` is a keyword with a default, so existing one-argument
calls and the monkeypatched `lambda item:` in `test_pool_summary.py:99` keep working.

For a chunk `[m_1 .. m_n]`:

```
for i, member in enumerate(members):
    retiring = False
    if i > 0 and _should_recycle(scope_path, started_at, completed_count + 1):
        retiring = True                     # decided BEFORE the member runs
    nextitem = None if (retiring or i == n - 1) else items[members[i + 1]]
    outcome, events = run_one(items[member], nextitem=nextitem)
    completed_count += 1
    _record_memory_sample(scope_path, member, completed_count)
    if not retiring and i == n - 1:
        retiring = _should_recycle(scope_path, started_at, completed_count)   # today's post-item check
    write events + result line (+ wait token only for i == 0) (+ suffix if retiring); flush
    if retiring: return
```

A single nodeid line runs as a chunk of one, which reduces to today's loop exactly.

- **A worker always runs at least one item per chunk**: member 1 is never pre-checked.
- **The tripping member always runs, with `nextitem=None`**, so every teardown happens inside a real
  protocol call with real reports. No out-of-protocol teardown exists. A worker never exits
  (recycle, `__stop__`) with shared fixtures alive and never breaks a `nextitem` promise, so the
  `SetupState.setup` assertion "previous item was not torn down properly" (`runner.py:514`) cannot
  fire.
- **`completed_count + 1` before a member** makes the count bound exact: with
  `AIRA_AITEST_WORKER_MAX_TESTS=M` a worker still runs exactly M tests. For the time and memory
  bounds the pre-member check reads the same values today's post-item check on the previous member
  would, and the difference is the trade in section 6: the tripping member runs once more.

### 4.4 Supervisor state and dispatch

- Worker state gains `rest` (a deque, empty by default) and `chunk_pos`, `chunk_len` (for the death
  reason). Invariant: `rest` non-empty implies `in_flight` is not None.
- Dispatch (`_dispatch_to_idle_workers`, both the fit branch and the fallback `next_nodeid` branch):
  if the popped entry is a chunk id, set `in_flight = members[0]`, charge `attempts[members[0]]`,
  `rest = deque(members[1:])`, `chunk_pos, chunk_len = 1, n`, write the chunk line. Otherwise
  today's path.
- Accepted result (the `in_flight = None` site, `:3240`): if not recycling and `rest` is non-empty,
  `in_flight = rest.popleft()`, charge its attempt, `chunk_pos += 1`, `pending_events = []`; else
  `in_flight = None`.
- `_largest_fitting(pop=True)` and `next_nodeid()` charge attempts only when the popped entry is a
  nodeid, never a chunk id.

### 4.5 Where chunk ids must be expanded

Every site that turns a queue entry into a result or a reason, found by reading every `self.queue`
use (`grep -n "self\.queue"`):
- `_fail_queue_terminal` (`:1402-1405`): mark every member unevaluated with the reason.
- `_bootstrap_from_empty_pool` exceeds-ceiling (`:2996-3016`): remove the chunk, mark every member;
  the annotated/unannotated wording uses `members[0]` (uniform need implies uniform annotation).
- `_abort_pool` (`:3422-3438`): besides the in-flight member, each `rest` member gets "not started:
  the fail-fast abort (tripped by X) stopped the pool".
- Nothing else reads entries as nodeids: `requeue_once` only inserts nodeids, and
  `_synthesize_unevaluated_reports` iterates `items_by_nodeid`.

## 5. Failure semantics

### 5.1 Attempts: charged on promotion, not pop

A nodeid is charged exactly when it becomes `in_flight`. Unstarted `rest` members are never
charged, so a crash that ends a chunk costs them nothing. A crash in the gap after member k's
result is charged to member k+1, correctly: the worker starts k+1 immediately, and k's teardown
with `nextitem=k+1` ran inside k's protocol, so any later crash is inside k+1's protocol.

### 5.2 Crash mid-chunk (`_handle_worker_exit`, `:3440`)

1. Insert each `rest` member at the queue head as a single-item unit, in reverse order, attempts
   unchanged.
2. Then today's handling of `in_flight` (`requeue_once` or unevaluated, fail-fast trip), so the
   head reads `[in_flight, rest...]` in collection order.
3. Retire and replace as today.

Unstarted items are never lost and never spend an attempt; the batching benefit for them is lost
(they rerun as singles), which also bounds thrash: each requeue is one item and each worker runs
at least one. A deterministic crasher spends only its own two attempts.

### 5.3 Recycle mid-chunk

On a result carrying `__recycle_next__` with `rest` non-empty: requeue `rest` as in 5.2 step 1, then
retire and replace (`:3249-3252`). No attempt is spent.

### 5.4 Death reason and teardown attribution

- `_describe_worker_death` appends, when `chunk_len > 1`: "while running it as item k of n in a
  chunk (k-1 earlier items had run in this worker with shared fixtures still alive)". Without it a
  worker killed by memory accumulated across the chunk is retried alone, usually passes, and the
  cause stays invisible. The pool summary counts crashes with `chunk_pos > 1`.
- A shared fixture's teardown error is reported on the item whose teardown phase ran it: the last
  user before a scope change, or the chunk's last member. That is serial pytest's attribution for
  the same order. After a crash or recycle cut, the item that ends the run can differ between runs;
  documented, rare, and closer to plain pytest than today's per-test teardown.

### 5.5 `-x`, `--maxfail`, `shouldstop`

In a worker, its copy-on-write session sets `shouldfail` or `shouldstop` after its own failures;
`runtestprotocol` then forces `nextitem=None` (`runner.py:140-141`). The rest of that worker's life
runs unbatched: sound (each member sets up fresh), a performance cost only. No aitest code;
documented in the README and pinned by test W6.

### 5.6 Ordering plugins

Chunks are cut after every `modifyitems` hook and never reorder, so pytest-randomly, trylast
shuffles and pytest's own `reorder_items` are honoured. A run of consecutive items with real
`nextitem` reproduces serial pytest's setup count for that range in any order. An order that
interleaves modules yields short segments and little saving, never a wrong result. (pytest-randomly
is reported to shuffle within module buckets, so module contiguity survives; not verified here.)

### 5.7 Fallback and ledger-only workers: batching stays on, justified

- **ci-shim ledger-only**: this is where the evidence was measured. Switching batching off there
  makes slice 1 worthless. The memory trade is stated in section 6, with no parity claim.
- **Daemon-down fallback** (`reservation None`, `:2287-2295`): it is already ungoverned (no cap, no
  watermark) and capped at `AIRA_AITEST_MAX_WORKERS_FALLBACK` (default 1). A chunked fallback
  worker holds what plain serial pytest holds for the same range, which is what the suite would do
  without aitest. Keeping it avoids a split-at-dispatch special case. Trigger to revisit: an OOM in
  fallback mode attributed to a chunk.

## 6. Memory statement

What changes: within a chunk a worker keeps higher-scope fixtures that members set up (including
ones the current member does not use) until pytest tears them down at a scope change or chunk end.
Today each test holds only its own closure, rebuilt. Peak per worker can therefore rise by at most
one chunk's retained module, class, package and session fixtures, the same set serial pytest holds
for that range.

- **Desktop (enforced, cgroup sub-scope).** Unchanged instruments: the `memory.current / memory.max`
  watermark (64%, `worker.py:23`, owned by AIRA-32), now read before each non-first member, and the
  memory.max kill backstop with `memory.swap.max=0`. Trade: when the watermark trips before member
  k, member k still runs (with `nextitem=None`), once, above the watermark, where today the worker
  would have retired after k-1. The cap and the retry-once rule remain the backstop. The reservation
  is unchanged (no retained-fixture term; trigger in section 2).
- **CI ledger-only (no cgroup).** No parity claim. Today there is no watermark and no kill backstop
  (`worker.py:278-291`); after this change a worker's steady RSS can also include retained fixtures,
  and the container OOM killer is the only backstop, which may pick the supervisor and lose the run.
  Accepted because the default is off, the gain is measured there, and the A/B measures per-worker
  `ru_maxrss` (Gantt `peak_rss_bytes`) at cap 1 against cap c. Deferred option (AIRA-373): a
  ledger-only watermark from `/proc/self/smaps_rollup` `Private_Clean + Private_Dirty` against the
  worker's lease. Trigger: a CI `ru_maxrss` rise near the lease or a container OOM.
- **Never `/proc/self/statm`.** A forked child's statm resident counts the supervisor's
  copy-on-write image and does not see copy-on-write growth. M (`~/tmp/aitest-batching-review/
  statm_fork.py`, `statm2.log`, 6.18 WSL kernel): parent 328 MiB resident; child at fork statm 326
  MiB with `Private_Dirty` 0; after writing about 100 MiB, statm still 326 MiB while `Private_Dirty`
  is 106 MiB. With a multi-GB fastest-ee supervisor every worker would read above its 512 MiB lease
  before its first test. (`ru_maxrss` has the same copy-on-write floor, so the CI comparison is a
  delta between arms, never an absolute.)

## 7. Configuration

`AIRA_AITEST_BATCH_MAX_TESTS`, read once in `collect()`:
- unset or `1`: off; queue and wire byte-identical to today;
- an integer `c >= 2`: the chunk cap, clamped to `AIRA_AITEST_WORKER_MAX_TESTS` (default 200), since
  a longer chunk is always cut by the count recycle;
- non-integer or `< 1`: one warning (`_warn_invalid_env` pattern), then 1.

When `c > 1` the supervisor prints one line at run start: "aitest: batching: N tests in K chunks,
cap c (AIRA_AITEST_BATCH_MAX_TESTS)". No flag, no ini key.

## 8. Measurement (opt-in, `AIRA_AITEST_MEASURE_DIR`, fail-silent like `_record_memory_sample`)

- **Setup runs per scope node**: the worker registers a `pytest_fixture_setup` hookwrapper (public on
  pytest 7, 8, 9; it fires only on a cache miss, `fixtures.py:1077-1110`) that appends
  `scope, argname, request.node.nodeid, seconds` to `fixture-setup-<pid>.tsv`. Only when the
  measure dir is set; a normal run registers nothing.
- **Chunk lengths**: the cut histogram (from `collect()`) and the run lengths actually executed
  (members run before a crash, recycle or end), in `pool-report-<pid>.json`.
- **Drain length**: wall time from the queue first becoming empty to the last worker's retirement.
- **Peak per worker**: existing `memory.peak` (desktop) and `ru_maxrss` (everywhere), unchanged.
- **Crashes and OOMs**: count of worker crashes, of crashes with `chunk_pos > 1`, of OOM group kills,
  and of unstarted members requeued.

## 9. Rollout and A/B

**Slice 0 (premise, no aira code; the speed session, from its fastest-ee fixture tracer).** Report
fixture self-time by scope (session, package, module, class) after the 42% fix, the tests-per-module
distribution for modules using the costly module fixtures, and the number of module switches in the
final order. Decision rule, fixed before the query: predicted saving at cap c =
sum over module fixtures of (setups today - setups at cap c) x mean setup seconds. **Build slice 1
only if the best cap <= 50 predicts at least 5% of engine test-seconds** (about 1,370 s of 27,468
R); otherwise stop and write a collapse memo.

**A/B (after the build, same commit, default still off).**
1. aira's own suite, cap 1 against caps 10, 25, 50: a no-regression check (little gain expected;
   assumption, not measured).
2. fastest-ee engine gate on GCP (`make gate SHA=<sha> SUITES=engine DIR=<new dir>`, run by the speed
   session), cap 1 against the best cap, at least two runs per arm.
3. Report per arm: wall time, test-seconds, setup runs per fixture, chunk-length histogram, drain
   length, peak per worker, OOM and crash counts (including mid-chunk), unstarted requeues, and every
   failure seen only with batching (each is an isolation bug in that project, which serial pytest
   would also expose, or a reason to revisit `aira_isolated`).

**Flip the default** to the chosen cap only when the engine gate shows a wall-time gain larger than
the run-to-run spread, aira's suite is not slower beyond its spread, OOM and crash counts are not
higher, and every batching-only failure is explained. Otherwise the default stays 1 and the record
says why.

## 10. Tests (each must fail against the named wrong implementation)

Worker (`test_worker.py`, pytester):
- **W1** `run_one(items[0], nextitem=items[1])` then `run_one(items[1])` gives a module counter of 1;
  the fixture's finalizer runs in `items[1]`'s teardown. Mutant: `nextitem` ignored, counter 2.
  Existing `test_worker.py:178` stays as the `nextitem=None` case (counter 2).
- **W2** a chunk line of 3 members in one module: counter 1, three result lines in order, a wait
  token on the first only, the finalizer done before the loop reads its next line. Mutants: last
  member run with a non-None `nextitem` (finalizer not run); token on every member.
- **W3** `AIRA_AITEST_WORKER_MAX_TESTS=2`, a chunk of 4: exactly 2 result lines, the second with
  `__recycle_next__`, the finalizer run inside member 2's protocol, members 3-4 not run. Mutants:
  post-item check (member 2 runs with `nextitem=m3`, finalizer not run at exit); `completed_count`
  without `+1` (3 lines); tripping member skipped (1 line).
- **W4** the memory watermark tripping before member 2 (fake `memory.current` above 64%): member 2
  runs with `nextitem=None` and recycles; member 1 is never pre-checked (a scope already above the
  watermark still runs member 1). Mutant: check before member 1 (zero progress).
- **W5** a teardown error in a module fixture lands on the last sharer only, not on earlier
  members. Mutant: per-member `nextitem=None`.
- **W6** `session.shouldfail` set before a chunk: every member rebuilds, all pass, no SetupState
  assertion. Fails if the loop tears down outside `runtestprotocol`.
- **W7** the `pytest_fixture_setup` recorder writes one row per cache miss only under
  `AIRA_AITEST_MEASURE_DIR` and nothing otherwise.

Supervisor (`test_supervisor.py`, fake workers as in `_fake_worker_state`):
- **S1** cut rules, table-driven: tuple change cuts; module-aware packing; segment > cap split at
  cap; failfast items are singletons; cap clamped to `WORKER_MAX_TESTS`; invalid env warns and
  means 1. Mutants: each rule removed or off by one.
- **S2** cap 1: queue equals the nodeid list and every dispatch line is a bare nodeid. Mutant: JSON
  always.
- **S3** attempts: after dispatching `[a,b,c]`, `{a:1}`; after a's result, `{a:1, b:1}`; c never
  charged. Mutant: charge at pop.
- **S4** crash while b is in flight: queue head `[b, c]`, both bare nodeids, b's attempts 1, c's 0;
  a's result kept; a second crash on b gives b unevaluated and c still runs. Mutants: rest requeued
  as a chunk, charged, or dropped.
- **S5** recycle suffix on b with c unstarted: c requeued, 0 attempts, worker retired, replacement
  requested.
- **S6** OOM-killed scope mid-chunk: the death reason names "item 2 of 3 in a chunk".
- **S7** exceeds-ceiling on a chunk id: every member unevaluated with the ceiling reason; no chunk id
  appears in `results` or in any synthesized report. Same for `_fail_queue_terminal` (**S8**).
- **S9** fail-fast trip while another worker is mid-chunk: its `rest` members carry the abort reason.
- **S10** a chunk containing a parametrised nodeid with a space round-trips. Mutant: space-split wire.
- **S11** `dispatches_measured` counts chunk heads only; the summary reads "measured on k of n".
- `test_ready_queue.py` unchanged and green: at cap 1 no chunk id exists, and the oracle's
  attempts comparison still holds for single-item pops.

End to end (real fork):
- **E1** a 10-test module with a module fixture counting setups to a file: cap 5 gives 2, cap 1
  gives 10. Mutant: worker ignores `nextitem`.
- **E2** JUnit fidelity (`test_junit_fidelity.py`): a module fixture with a failing teardown; plain
  `pytest` and aitest at cap >= the module size produce identical normalised XML. Mutant: per-test
  teardown (today's cap-1 behaviour gives an error on every test).
- **E3** a trylast `modifyitems` hook interleaving two modules: results correct, setup count equal
  to serial pytest's for that order, dispatch order within each chunk equal to `session.items`.
  Mutant: chunker groups by module (reorders).

Docs: README `Look-ahead dispatch` limitation replaced by the batching note (cap, default off,
`-x` de-batching, attribution); the `run_one` docstring updated.

## 11. Answers to speed §8

1. **History storage**: none in slice 1. Nothing in the design reads one. If AIRA-373 ever needs
   durations, use this run's own `report.duration` online; a stored history would be a repo cache
   file restored on CI, never a daemon protocol change.
2. **Session fixtures kept across chunks**: no. Within a chunk they are shared already, cutting
   session setups from one per test to one per chunk. Keeping them across chunks needs the next
   chunk committed before the current one ends (breaking the pull model) or a private
   `teardown_exact(None)` at exit whose errors produce no report; it would cache a session setup
   error against every later test on that worker; and the measured 42% case is fixed at source.
   Trigger: Slice 0 or the A/B shows per-chunk session setup is still material.
3. **Thresholds**: one, the cap, chosen from the A/B as the smallest cap that captures most of the
   module-fixture saving without lengthening the drain. The watermark stays at 64% (AIRA-32).

## 12. Risks

| Risk | Mitigation |
|---|---|
| Order dependence within a module becomes visible | Default off; deterministic chunks make it reproducible; the A/B lists every batching-only failure; serial pytest exposes the same bugs. |
| OOM from fixtures accumulated across a chunk hidden by a passing retry | Death reason and pool count name the chunk position (5.4). |
| Watermark trip runs one more member above 64% (desktop) | memory.max kill backstop and retry-once unchanged; A/B OOM count. |
| CI per-worker RSS rises with no backstop | Stated trade, no parity claim (section 6); A/B `ru_maxrss`; smaps_rollup option deferred with a trigger. |
| Drain grows up to cap x slowest test | Cap is the knob; drain measured; tail split deferred (AIRA-373). |
| Chunks are ranked by the shared `aira_time` rank, not their total | Same as a single test of that rank, so long chunks may land late; measured by the drain; summed rank deferred. |
| A third-party plugin reads `nextitem` | Real items only, never a sentinel; pytest's own contract. |
| Saving smaller than predicted (many 1-3-test modules) | Slice 0 decides before any build. |
| Crash or recycle mid-chunk loses batching for the rest | Bounded at cap-1 single reruns per event; counted. |

## 13. Deferrals (all in AIRA-373, held; the owner clears the hold)

| Deferred | Trigger to build |
|---|---|
| Cost model and per-repo history (4.2) | A/B shows chunking cheap-fixture modules costs wall time |
| Predicted-duration chunks, LPT by chunk seconds or summed rank, tail split (4.3) | A/B drain grows beyond the run-to-run spread |
| Retained-fixture reservation term (4.4) | Peak per worker rises into OOM kills or the cap |
| CI smaps_rollup `Private_*` watermark (4.4) | CI `ru_maxrss` rises near the lease, or a container OOM |
| `aira_affinity`, `xdist_group`, `aira_isolated` (4.6) | A batching-only failure or a cross-module cache that cannot be fixed in the project |
| `pytest_aitest_prefork` hookspec (4.6) | Its own measurement shows shared-page gain |
| Session fixtures kept across chunks (speed §8 q2) | Per-chunk session setup still material after slice 1 |
| Fallback-mode exclusion (5.7) | An OOM in fallback mode attributed to a chunk |

Out of scope for good: the private sharing key (4.1) and any reordering of the project's order.
