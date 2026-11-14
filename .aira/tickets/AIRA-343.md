---
{"schema":1,"id":"AIRA-343","project":"aira","title":"aitest: dispatch cost must not grow with ready-queue length: index the ready queue by (reservation, cpu) class, O(log n) dispatch, identical pick order","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.42","labels":[],"hold":false,"relations":[]}
---
Requested by the fastest.ee session ('speed'), 2026-11-14. Client-only change (internal/pylib is embedded and loaded via AIRA_AITEST_LIB): a client install delivers it, no daemon restart. Correctness-critical (two-loop): yes, it is the scheduler's pick.

## Problem
`Supervisor._largest_fitting` (internal/pylib/aitest/supervisor.py) scans the WHOLE ready queue on every dispatch and then does an O(n) `self.queue.remove(best)`. Re-measured on this box (Python 3.12.3, speed's ~/heap/release-speed/critical-path/bench_dispatch.py against master 0ea1582e): 23.2 ms per dispatch at a 65,164-test queue, 11.2 ms at 32,000, 1.9 ms at 8,000 (speed reported 28.7 ms on 3.14.4). That caps the pool at ~43 dispatches/s and costs ~0.44 us x N^2/2 of single-core supervisor time per run, while 56k of fastest.ee's engine-leg tests take under 1 s.

## Spec
Index the ready queue by (reservation_need, cpu_need) class. Each class keeps a heap keyed by (-aira_time, seq) with lazy deletion; seq preserves the list order exactly (appends count up, head inserts from requeue_once count down), so FIFO is the old list position. A pick looks only at classes that FIT both dimensions and compares their tops under the old key (aira_time DESC, need DESC, earliest-queued). Cost O(fitting classes + log n), not O(n). The pick order must be EXACTLY the old one, including the 2-D fit rule (AIRA-261), the requeue-after-crash head insert, pop's attempts increment, failfast, the oversize/bootstrap path and the unconfined next_nodeid FIFO path.

## Acceptance
- under 0.1 ms per dispatch at a 65k-test queue, measured by a committed, executable benchmark comparing old and new on the same synthetic queue (realistic mix of classes and aira_time marks);
- a differential test: many random queues, budgets and mutation sequences (peek, pop, next_nodeid, remove, requeue, terminal drain, queue and map reassignment); the new picker returns exactly the old picker's test and leaves the same queue and attempts. The old algorithm is kept as a test-only oracle;
- the equivalence tests are mutation-verified (tiebreak, class fit rule, lazy deletion, FIFO).

## Challenge
`_pool_covers_the_queue` returns False before sorting whenever the queue is longer than the idle-worker count, so it only sorts a queue no longer than the pool; it needs no index (measure to confirm). `_smallest_ready` stays O(n): it runs only on an empty pool (bootstrap), not per dispatch.

## Outcome (branch aira-aitest-dispatch)
Built as specified (`_ReadyQueue` in supervisor.py; `Supervisor.queue` is now a property holding it). Measured by `AIRA_AITEST_BENCH=1 pytest -s internal/pylib/aitest/test_ready_queue.py -k bench`, same synthetic queue for both pickers (20 (need, cpu) classes, ~1% aira_time marks): Python 3.12.3, 65,164 queued: old 25-28 ms/dispatch, new 0.005-0.006 ms (index build 73-79 ms once); Python 3.14.4: old 33.1 ms, new 0.0045 ms (build 33 ms). Full 65,164-test drain: 0.3-0.4 s of pick time in total. `_pool_covers_the_queue` at a 45k queue with 64 idle workers: 0.004-0.007 ms/call, so it is left unchanged. The differential test (`test_ready_queue.py`, ~77k compared operations, old picker kept as the oracle) passes on 3.12 and 3.14; 10 mutations (tiebreak, fit rule, >=, lazy deletion, liveness-by-nodeid, head-insert seq, reindex-on-map-change, need key, aira_time key, remove-first-occurrence) all turn it red. Deferred: `_smallest_ready` stays O(n) (empty-pool bootstrap only); in-place mutation of the need/cpu/time maps is not detected (nothing does it).

## Benchmark correction (finish pass)
The first benchmark timed 100-400 old picks against 20,000 new picks, and at the 8,000 queue about 12,000 of the 20,000 new calls found the queue empty and returned nothing, which understated the per-dispatch cost. It now times the SAME number of successful dispatches for both pickers on the same queue and budgets (200 at 65,164; 400 at 32,000; 1,000 at 8,000), asserts every timed pick returns a test, and asserts both pickers hand out the same tests. Re-measured, Python 3.12.3, two runs: at 65,164 queued, old 26.8 / 32.2 ms per dispatch, new 0.0100 / 0.0092 ms (acceptance < 0.1 ms: met); 32,000: old 12.2 / 13.4 ms, new 0.0092 / 0.0175 ms; 8,000: old 2.16 / 2.03 ms, new 0.0057 / 0.0053 ms. Index build at 65,164: 110 / 76 ms once. Full 65,164 drain: 0.45 / 0.39 s of pick time. `_pool_covers_the_queue` (64 idle workers): 0.0045 / 0.0044 ms per call.
