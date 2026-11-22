# aitest consecutive-chunk batching (AIRA-372): design

Status: approved (plan gate round 3, APPROVE-WITH-NITS, nits folded in at build, section 14); built
on branch `aira-372-aitest-batching`, default off. Ticket `.aira/tickets/AIRA-372.md`; deferrals in
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
| 4.4 pre-test watermark | **Keep, reduced** | Reuse `_should_recycle` unchanged, called before each member of a multi-member chunk (section 4.3). No `statm` (section 6). |
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
   requeues it as one chunk (5.3). Nothing unstarted travels on the result line, whose format (rpartition on the last
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
calls keep working. The loop always passes `nextitem=` as a keyword, so the one-argument fake
`lambda item:` in `test_pool_summary.py:99` is changed to `lambda item, nextitem=None:` in the
same change.

For a chunk `[m_1 .. m_n]`:

```
for i, member in enumerate(members):
    retiring = n >= 2 and _should_recycle(scope_path, started_at, completed_count + 1)
                                            # decided BEFORE the member runs; never skips it
    nextitem = None if (retiring or i == n - 1) else items[members[i + 1]]
    outcome, events = run_one(items[member], nextitem=nextitem)
    completed_count += 1
    _record_memory_sample(scope_path, member, completed_count)
    if not retiring and i == n - 1:
        retiring = _should_recycle(scope_path, started_at, completed_count)   # today's post-item check
    write events + result line (+ wait token only for i == 0) (+ suffix if retiring); flush
    if retiring: return
```

A single nodeid line (n == 1) runs only today's post-item check, so cap 1 is today's loop exactly.

- **Every member of a multi-member chunk is pre-checked, member 1 included.** A pre-check never
  skips a member: a trip only makes that member the worker's last, run with `nextitem=None`. So a
  worker runs at least one item per chunk and cannot make zero progress.
- **The tripping member always runs, with `nextitem=None`**, so every teardown happens inside a real
  protocol call with real reports. No out-of-protocol teardown exists. A worker never exits
  (recycle, `__stop__`) with shared fixtures alive and never breaks a `nextitem` promise, so the
  `SetupState.setup` assertion "previous item was not torn down properly" (`runner.py:514`) cannot
  fire.
- **The count bound is exact**: with `AIRA_AITEST_WORKER_MAX_TESTS=M >= 1` a worker runs at most M
  tests, whatever count a chunk starts at (the pre-check on the member that would be test M trips
  with `completed_count + 1 = M`). `M <= 0` is accepted by `_env_int` today and makes
  `completed_count >= M` always true, so a worker runs one test; under this change the same holds
  (member 1's pre-check trips), and the chunk cap treats it as `M = 1` (section 7). For the time
  and memory bounds the pre-member check reads what today's post-item check on the previous member
  would, and the difference is the trade in section 6: the tripping member runs once more.

### 4.4 Supervisor state and dispatch

- Worker state gains `rest` (a deque, empty by default), `chunk_id` (the dispatched chunk id, for
  the recycle requeue) and `chunk_pos` (for the mid-chunk crash line, 5.4). Invariant: `rest`
  non-empty implies `in_flight` is not None.
- Dispatch (`_dispatch_to_idle_workers`, both the fit branch and the fallback `next_nodeid` branch):
  if the popped entry is a chunk id, set `in_flight = members[0]`, charge `attempts[members[0]]`,
  `rest = deque(members[1:])`, `chunk_id`, `chunk_pos = 1`, write the chunk line. Otherwise
  today's path (`chunk_pos = 1`, `rest` empty).
- Accepted result (the `in_flight = None` site, `:3240`): if not recycling and `rest` is non-empty,
  `in_flight = rest.popleft()`, charge its attempt, `chunk_pos += 1` (`pending_events` is already
  reset right after the replay, `:3234`); else `in_flight = None` and `chunk_pos = 0`. This reset is
  the one mechanism that keeps an idle worker's death from reading as a mid-chunk loss (5.4).
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
at least one. A deterministic crasher spends only its own two attempts. Singles are for the crash
path only: a crash is rare and may be caused by what the chunk accumulated.

### 5.3 Recycle mid-chunk

On a result carrying `__recycle_next__` with `rest` non-empty: put `rest` back at the queue head as
ONE entry, attempts unchanged (none charged), then retire and replace (`:3249-3252`). With two or
more members the entry reuses the chunk id (`self._chunks[chunk_id] = list(rest)`; its need, cpu
and time entries are unchanged and the id has left the queue, so nothing else refers to it); with
one member it is that bare nodeid. A count recycle is routine, not rare (every 200 tests by
default, `worker.py:8`, about 344 per 68,802-test run), and variable chunk lengths put it mid-chunk
most of the time, so requeuing the rest as singles would rerun up to cap-1 tests per recycle
without batching (derived, not measured).

### 5.4 Mid-chunk crash line and teardown attribution

- The death reason string is NOT extended. It reaches a user only when `requeue_once` refuses
  (`supervisor.py:3462-3466`, attempts >= 2), and a chunk member in flight always has attempts 1
  (charged at promotion; requeues are singles or an uncharged rest), so a chunk-position clause
  there would never print. Instead, when `chunk_pos > 1` (which implies `in_flight` is set, because
  `chunk_pos` is reset to 0 wherever `in_flight` is cleared, 4.4), `_handle_worker_exit` writes one
  stderr line before retirement, whatever the retry does: "aira aitest: worker <pid> was lost while
  running item k of a chunk (<nodeid>); k-1 earlier items had run with shared fixtures alive;
  retrying it alone". "Lost", not "died": one caller of `_handle_worker_exit` is a live worker whose
  report batch failed validation (`:3226-3231`). It does not embed `_describe_worker_death`'s
  text, which asserts the retry already failed; the `:3462` unevaluated reason stays byte-identical
  to today. Without the line, a worker killed by memory accumulated across the chunk is retried
  alone, usually passes, and the cause stays invisible. The pool summary counts the same crashes.
- A shared fixture's teardown error is reported on the item whose teardown phase ran it: the last
  user before a scope change, or the chunk's last member. That is serial pytest's attribution for
  the same order. After a crash or recycle cut, the item that ends the run can differ between runs;
  documented, rare, and closer to plain pytest than today's per-test teardown.

### 5.5 `-x`, `--maxfail`, `shouldstop`

In a worker, its copy-on-write session sets `shouldfail` or `shouldstop` after its own failures;
`runtestprotocol` then forces `nextitem=None` (`runner.py:140-141`). The rest of that worker's life
runs unbatched: sound (each member sets up fresh), a performance cost only. No aitest code and
no aitest test; documented in the README.

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
  watermark (64%, `worker.py:23`, owned by AIRA-32), now also read before each member of a
  multi-member chunk, and the memory.max kill backstop with `memory.swap.max=0`. Trade: when the watermark trips before member
  k, member k still runs (with `nextitem=None`), once, above the watermark, where today the worker
  would have retired after k-1. The cap and the retry-once rule remain the backstop. The reservation
  is unchanged (no retained-fixture term; trigger in section 2).
- **CI ledger-only (no cgroup).** No parity claim. Today there is no watermark and no kill backstop
  (`worker.py:278-291`); after this change a worker's steady memory can also include retained fixtures,
  and the container OOM killer is the only backstop, which may pick the supervisor and lose the run.
  Accepted because the default is off, the gain is measured there, and the A/B measures, at cap 1
  against cap c, each worker's private memory (`/proc/self/smaps_rollup` `Private_Clean +
  Private_Dirty`, sampled after every test under the measure dir, section 8) and the container's
  peak (`/sys/fs/cgroup/memory.peak` read by the A/B harness after the run; reported unevaluated
  if absent). `memory.peak` is cumulative over the cgroup's life, so each measured arm runs in a
  fresh container or cgroup, or the harness resets the peak first. Not `ru_maxrss`: it misses
  copy-on-write growth. M (`~/tmp/aira-372-fix/cow_probe.py`, `cow.log`): a forked child touching
  about 80 MiB of inherited pages went from 712 to 82,036 KiB private while `ru_maxrss` stayed at
  90,228 KiB. Retained fixtures touch inherited objects (refcounts alone write the pages).
  Deferred (AIRA-373): a ledger-only watermark from the same `Private_*` sum against the worker's
  lease. Trigger: a CI private-memory peak near the lease or a container OOM.
- **Never `/proc/self/statm`.** A forked child's statm resident counts the supervisor's
  copy-on-write image and misses copy-on-write growth. M (`~/tmp/aitest-batching-review/
  statm_fork.py`, `statm2.log`): child at fork statm 326 MiB with `Private_Dirty` 0; after writing
  about 100 MiB, statm still 326 MiB, `Private_Dirty` 106 MiB. With a multi-GB fastest-ee
  supervisor every worker would read above its 512 MiB lease before its first test.

## 7. Configuration

`AIRA_AITEST_BATCH_MAX_TESTS`, read once in `collect()`:
- unset or `1`: off; queue and wire byte-identical to today;
- an integer `c >= 2`: the chunk cap, clamped to `max(1, AIRA_AITEST_WORKER_MAX_TESTS)` (default
  200, parsed with the worker's own `_env_int`), since a longer chunk is always cut by the count
  recycle; a worker limit of 0 or below runs one test per worker today and so turns batching off;
- non-integer or `< 1`: one warning (`_warn_invalid_env` pattern), then 1.

When `c > 1` the supervisor prints one line at run start: "aitest: batching: N tests in K chunks,
cap c (AIRA_AITEST_BATCH_MAX_TESTS)". No flag, no ini key.

## 8. Measurement (opt-in, `AIRA_AITEST_MEASURE_DIR`, fail-silent like `_record_memory_sample`)

- **Setup runs per scope node**: the worker registers a `pytest_fixture_setup` hookwrapper (public on
  pytest 7, 8, 9; it fires only on a cache miss, `fixtures.py:1077-1110`) that appends
  `scope, argname, request.node.nodeid, seconds` to `fixture-setup-<pid>.tsv`. Each row is a real
  cache miss, so pytest's own cache key and invalidation decide what is counted. Only when the
  measure dir is set; a normal run registers nothing.
- **Private memory per worker**: `_record_memory_sample` appends a fourth column to the existing
  `worker-<pid>.tsv` row, `Private_Clean + Private_Dirty` from `/proc/self/smaps_rollup`
  (`unevaluated` if unreadable); the first three columns are unchanged. The two readers that
  assumed three columns change in the same commit: `pytest_aitest_measurement_e2e_test.go` (now
  requires four fields and a real private-memory reading) and `test_measurement_report.py`.
- **Chunk lengths**: the cut histogram (from `collect()`) and the run lengths actually executed
  (members run before a crash, recycle or end), in `pool-report-<pid>.json`.
- **Drain length**: wall time from the queue first becoming empty to the last worker's retirement.
- **Peak per worker**: existing `memory.peak` (desktop) and the per-pid maximum of the fourth
  column, taken by the A/B harness from the TSVs (no supervisor change); `ru_maxrss` stays in the
  Gantt but is not used to judge memory (section 6).
- **Crashes and OOMs**: count of worker crashes, of mid-chunk crashes (5.4), of OOM group kills,
  and of unstarted members requeued.

## 9. Rollout and A/B

**Slice 0 (speed session's fastest-ee tracer, R; engine run B after #3234, 27,468
test-seconds).** Fixture self-time: module 4,556 s (16.6%, 8,010 setups), function 186 s, session
11 s, class 4 s (session negligible, as section 11 assumes). First-order prediction (consecutive
batches in file order, one module setup per (file, batch) with a user; no parameter identity or
marker cuts): cap 5 -> 8.9%, 10 -> 12.1%, 20 -> 13.7%, 50 -> 14.3%, 100 -> 14.5%
(`~/tmp/egt-scripts/batch_predict.py`, speed's host).

**Pessimistic marker bound (plan review round 2, R: `~/tmp/aira-372-r2/pess.log`).** fastest-ee's
one chunk-cutting marker source is the learned `aira_time` weights (above 10 s only; 5,996 tests
in 108 files). Excluding the parametrised `module:result` in tools/ohw/test_score_ohw.py, the
first-order saving is 2,734 s (10.0%) at cap 10, 2,989 s (10.9%) at 20, 3,162 s (11.5%) at 50;
charging every weighted test two extra chunk boundaries leaves 1,997 s (7.3%), 2,187 s (8.0%),
2,311 s (8.4%), and cap 5 gives 6.0%. All clear the 5% bar (about 1,370 s). Not bounded:
`aira_mem`, `aira_cpu`, `aira_failfast` cuts, and final order against file order. The weights cut
chunks in exactly the slow-test modules, and shift once batching lowers per-test seconds.

**Recorded deviation from the round-1 rule**, which required a simulated walk of pytest's fixture
cache before building. The walk is deleted: it re-implements that cache and kept getting it wrong
(the key is `request.param`, not `param_index`, `fixtures.py:1119-1120`; dependency invalidation,
`:1058-1065`). The build proceeds on the first-order result plus the pessimistic bound; the A/B's
instrumented run measures real cache misses (section 8) instead of modelling them. **Stop rule,
after measurement:** if the measured saving at the best cap is under 5% of engine test-seconds,
the default is not flipped and a collapse memo is written. Candidate caps: 20 and 50.

**A/B (after the build, same commit, default still off).**
1. aira's own suite, cap 1 against caps 10, 25, 50: a no-regression check (little gain expected;
   assumption, not measured).
2. fastest-ee engine gate on GCP (`make gate SHA=<sha> SUITES=engine DIR=<new dir>`, run by the speed
   session), cap 1 against the best cap, at least two runs per arm.
3. Timing is uninstrumented: the timed runs set no `AIRA_AITEST_MEASURE_DIR`, since the fixture
   recorder writes one row per cache miss and cap 1 has far more misses. Setup counts, chunk
   histogram, drain, private memory and crash counts come from one further measured run per arm;
   its wall time is reported next to the timed runs', so the instrumentation overhead is visible
   as the difference and never credited to batching.
4. Report per arm: wall time, test-seconds, setup runs per fixture, chunk-length histogram, drain
   length, peak private memory per worker and container peak, OOM and crash counts (including mid-chunk), unstarted requeues, and every
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
- **W3** `AIRA_AITEST_WORKER_MAX_TESTS=2`, a chunk of 4 whose module fixture's finalizer raises:
  exactly 2 result lines, the second with `__recycle_next__`, outcome `error` and the fixture's
  teardown report among its events (the teardown ran inside member 2's protocol), member 1
  `passed`, members 3-4 not run. Mutants: post-item check (member 2 runs with `nextitem=m3`,
  finalizer not run); member 2 run with `nextitem=m3` plus an out-of-protocol
  `teardown_exact(None)` at exit (finalizer runs, but member 2 reads `passed` with no teardown
  report); `completed_count` without `+1` (3 lines); tripping member skipped (1 line).
- **W3b** reused worker, `AIRA_AITEST_WORKER_MAX_TESTS=2`: one single line, then a chunk of 3:
  exactly 2 result lines in total, the second (chunk member 1) with `__recycle_next__`. Mutant:
  member 1 not pre-checked (3 results under a limit of 2).
- **W4** the memory watermark (fake `memory.current` above 64%) in two cases: crossing after member
  1, so member 2 runs with `nextitem=None` and recycles (2 result lines); and already above at the
  chunk's start, so member 1 runs with `nextitem=None` and recycles (1 result line, member 1 ran).
  Mutants: member 1 not pre-checked (2 lines in the second case); a trip that skips the member
  instead of running it (0 lines).
- **W5** a teardown error in a module fixture lands on the last sharer only, not on earlier
  members. Mutant: per-member `nextitem=None`.
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
- **S5** chunk `[a,b,c,d]`, recycle suffix on b: the queue head is ONE entry, the chunk id now
  mapping to `[c, d]`, with c and d at 0 attempts; worker retired, replacement requested. A recycle
  on c of `[a,b,c,d]` requeues the bare nodeid d. Mutant: rest requeued as singles.
- **S6** crash while b (item 2) is in flight, with b's retry then passing: exactly one stderr line
  ("was lost while running item 2 of a chunk") naming worker and b's nodeid, written before the retry, not containing
  "the one retry"; the pool summary's mid-chunk crash count is 1. No line and no count for a crash
  on item 1, nor for an idle worker dying after its chunk completed. Mutants: the clause attached
  to the death reason only (no line, since the retry passes); `_describe_worker_death` text
  embedded; `chunk_pos` not reset when `in_flight` is cleared (the idle death prints).
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
- **E4** (crash mid-chunk, one worker) a chunk of 5 whose member 3 dies on its first run only
  (marker file) at a test-only seam: `worker._fault_after_events(nodeid)`, a module attribute that
  is `None` in production (the payload stays one write). When set, the loop writes and flushes the
  member's event lines, calls it once per member with that member's nodeid (in both the single-line
  and the chunk path), and only then writes the result line; the test project's
  conftest sets it before fork to `os._exit` for m3's first run. So m3's event bytes reach the
  supervisor's `pending_events` and no result confirms them. Asserts: every nodeid has exactly one
  result and all pass; per-nodeid run counts m1, m2, m4, m5 once and m3 twice; attempts
  `{m1:1, m2:1, m3:2, m4:1, m5:1}`; JUnit has one testcase per nodeid with m3's reports from its
  retry only; the S6 line appears. Fake-worker tests skip event validation (`supervisor.py:3266`),
  so this is the test of replay across a crash. Mutants: staged events replayed on arrival or on
  crash rather than on result (m3 reported twice); promotion off by one (m3's attempts 1, or m4
  run twice); rest dropped; m1/m2 events replayed twice.
- **E5** (recycle mid-chunk, one worker) `AIRA_AITEST_WORKER_MAX_TESTS=4`, cap 3, six tests in one
  module, so chunks `[1,2,3]` and `[4,5,6]`: 6 passes, each run once, no worker runs more than 4
  tests (per-pid run counts), module setup count 3 (chunk A, test 4 alone as the tripping member,
  `[5,6]` requeued as one chunk). Mutants: rest requeued as singles (4 setups); member 1 not
  pre-checked (a worker runs 5).

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
| OOM from fixtures accumulated across a chunk hidden by a passing retry | A stderr line at every mid-chunk crash, written before the retry, and a pool count (5.4; tests S6, E4). |
| Watermark trip runs one more member above 64% (desktop) | memory.max kill backstop and retry-once unchanged; A/B OOM count. |
| CI per-worker memory rises with no backstop | Stated trade, no parity claim (section 6); A/B private memory and container peak; smaps_rollup watermark deferred with a trigger. |
| Drain grows up to cap x slowest test | Cap is the knob; drain measured; tail split deferred (AIRA-373). |
| Chunks are ranked by the shared `aira_time` rank, not their total | Same as a single test of that rank, so long chunks may land late; measured by the drain; summed rank deferred. |
| A third-party plugin reads `nextitem` | Real items only, never a sentinel; pytest's own contract. |
| Saving smaller than predicted (marker cuts, parametrised fixtures) | Pessimistic bound still 6-8% (9); the A/B measures real cache misses and the stop rule keeps the default off below 5%. |
| Crash mid-chunk loses batching for the rest | Bounded at cap-1 single reruns per crash; counted. A recycle (routine, about every 200 tests) keeps it: the rest is requeued as one chunk (5.3). |

## 13. Deferrals (all in AIRA-373, held; the owner clears the hold)

| Deferred | Trigger to build |
|---|---|
| Cost model and per-repo history (4.2) | A/B shows chunking cheap-fixture modules costs wall time |
| Predicted-duration chunks, LPT by chunk seconds or summed rank, tail split (4.3) | A/B drain grows beyond the run-to-run spread |
| Retained-fixture reservation term (4.4) | Peak per worker rises into OOM kills or the cap |
| CI smaps_rollup `Private_*` watermark (4.4) | CI private-memory peak near the lease, or a container OOM |
| `aira_affinity`, `xdist_group`, `aira_isolated` (4.6) | A batching-only failure or a cross-module cache that cannot be fixed in the project |
| `pytest_aitest_prefork` hookspec (4.6) | Its own measurement shows shared-page gain |
| Session fixtures kept across chunks (speed §8 q2) | Per-chunk session setup still material after slice 1 |
| Fallback-mode exclusion (5.7) | An OOM in fallback mode attributed to a chunk |

Out of scope for good: the private sharing key (4.1) and any reordering of the project's order.

## 14. Review log

Plan review round 1 (plan reviews and the plan gate: REVISE). Every finding was checked against the code
at `798af794` before editing.

| Finding | Disposition |
|---|---|
| Count recycle can exceed `WORKER_MAX_TESTS` (chunk starting at M-1 runs M+1); `<= 0` undefined | **Applied** (4.3, 7; W3b). Confirmed from the pseudo-code and `worker.py:276`. Took the gate's uniform rule (pre-check every member of an n >= 2 chunk) over "count only on member 1": a pre-check never skips a member, so member 1 needs no exemption for time or memory either. `<= 0` defined as 1. |
| CI A/B via `ru_maxrss` misses copy-on-write growth | **Applied** (6, 8, 9). Re-measured here (M, section 6). Private memory per worker plus container peak. |
| Death-reason clause never reaches a user; passing retry loses chunk diagnosis | **Applied** (5.4; S6, E4). Confirmed: `supervisor.py:3462` string used only at `:3464-3466` when `requeue_once` refuses, and a chunk member in flight has attempts 1. Clause, `chunk_len` and the old S6 deleted; one stderr line per mid-chunk crash before retry. |
| Slice 0 "setups at cap c" undefined; recorder lacks definition/param identity; timing mixed with tracing | **Applied** (8, 9). Simulation over final order, real cut tuples and an alive-set walk; recorder adds `baseid`, `param_index`; timed runs uninstrumented, overhead reported as the timed-vs-measured difference. |
| Tests that cannot fail: one-arg fake vs keyword call, W6, fake workers skip event validation | **Applied** (4.3, 10). Confirmed `test_pool_summary.py:99` (`lambda item:`) and `supervisor.py:3266` (`config is None` returns `[]`). Fake updated in the same change; W3 carries a failing finalizer; W6 re-scoped as a pytest-contract pin; E4, E5 added. |
| Recycle requeues the rest as singles, losing part of the win routinely | **Applied** (5.3, 12; S5, E5). Confirmed `_DEFAULT_MAX_TESTS = 200` (`worker.py:8`). Rest requeued as one chunk reusing the chunk id; singles only after a crash. |
| No real-fork crash or recycle test | **Applied** (E4, E5). |
| Drain tail from static chunks without tail split | **No change** (approve with nits): already accepted in 3 and deferred to AIRA-373 with a trigger. |
| CI ledger-only has no memory backstop | **No change** beyond the measurement fix above: the trade is stated in 6 and the watermark deferred. |

Plan review round 2 (REVISE; blocker: the crash-line text). Checked against `798af794` and pytest 9.0.3.

| Finding | Disposition |
|---|---|
| Premise "provisionally met" without the walk; the walk mis-modelled pytest's cache (`param_index` for `request.param`, no dependency invalidation) | **Closed by deleting** the walk and the recorder's `baseid` / `param_index` (8, 9). Confirmed `fixtures.py:1119-1120`, `:1058-1065`. Build on first-order plus the reviewer's pessimistic bound (`pess.log` re-read), a recorded deviation; the A/B measures cache misses; 5% stop rule after measurement. |
| Crash line embeds `_describe_worker_death`, which says the retry already failed | **Closed by deleting** the embedding (5.4; S6). Confirmed `supervisor.py:3505-3507`, `:3530-3533`; the `:3462` reason is unchanged. |
| Idle death after a chunk prints with nodeid `None` | **Applied** (4.4, 5.4; S6). Confirmed `:3240` leaves `chunk_pos`. Gate on `in_flight`; reset `chunk_pos`. |
| E4 cannot reach partial-event discard | **Applied** (E4). Confirmed one payload write (`worker.py:600-625`); test-only seam after the event bytes. |
| Container `memory.peak` boundary | **Applied** (6). |
| W6 pins only pytest | **Closed by deleting** W6. |
| Private-memory output under-specified | **Applied** (8): fourth TSV column (`worker.py:332-337`), harness takes the per-pid maximum. |

Plan gate round 3 (APPROVE-WITH-NITS, `~/tmp/aitest-batching/round3-verdict.json`). The four P3 nits,
folded into the build commit:

| Nit | Disposition |
|---|---|
| Fourth TSV column breaks the Go e2e reader (`len(fields) == 3`) and `test_measurement_report.py`'s whole-line match | **Applied** (8): both readers updated in the build commit. |
| S6's third mutant could not fail while two mechanisms covered the idle death | **Applied** (4.4, 5.4, S6): one mechanism, the `chunk_pos` reset where `in_flight` clears; the condition is `chunk_pos > 1`; mutant renamed "`chunk_pos` not reset when `in_flight` is cleared". |
| E4 seam signature unstated | **Applied** (E4): `worker._fault_after_events(nodeid)`, once per member after the event flush, both paths. |
| "died" is wrong for a live worker whose report batch failed validation | **Applied** (5.4, S6): "was lost while running item k of a chunk". |

Build note: E4 runs in-process with real forked workers and asserts on the reports replayed into the
supervisor's own `pytest_runtest_logreport` hook (the hook junitxml listens on), not on a JUnit file,
because it also asserts the supervisor's `attempts` map.
