---
{"schema":1,"id":"AIRA-21","project":"aira","title":"Confine admission charges reclaimable page cache (memory.current) → spurious stalls under heavy-I/O","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","daemon","dogfood"],"hold":false,"relations":[]}
---
Reported live by a dogfooding session ("speed") 2026-08-31: a LIGHT `aira confine -- make t ...` (default 4G reserve) waited 135s+ for memory admission and never got it, despite ~30G of reservation headroom and MemAvailable ~41G, with a single large 32G-reserve job present.

ROOT CAUSE (confirmed by code + live cgroup read, NOT a Slice-3 regression):
The admission gate in evaluateAdmitQueue (internal/daemon/admit.go:708-738) computes
  charge = max(current, outstanding+adopted); available = (maximum - headroom) - charge
where `current` and `maximum` come from readSliceMemory (admit.go:990-995) = the cgroup's
`memory.current` and `memory.max`. Crucially **memory.current INCLUDES reclaimable page cache**
(cgroup-v2 `file` bucket). Live read of aira.slice during triage: memory.current 29.1G =
anon 18.5G + file(cache) 10.4G + kernel 0.3G. Under a heavy-I/O job (a merge-gate/test suite
reading+writing GBs), the `file` bucket balloons and pushes memory.current toward the ceiling.

At speed's stall the 4G reject implies charge > ~57.9G ⇒ memory.current > ~57.9G, yet
MemAvailable was 41G (only ~37G machine-truly-used) — i.e. most of that ~58G was reclaimable
cache the kernel would evict on demand. Admission counts it as occupied → FALSE-FULL → new small
job wrongly queued indefinitely (bounded only by its maxWait → E_ADMIT_SATURATED). headroom is
NOT the culprit (admitSliceHeadroom = base + jobs*perJob ≈ 2G at the observed ceiling).

DIRECTION (needs the two-loop; must NOT re-open the aggregate-OOM window #67 closed):
Discount reclaimable cache from the admission charge. Options to weigh:
 (a) charge against anon+unevictable (memory.stat `anon` + `unevictable`) instead of raw
     memory.current, since reclaimable file cache is evicted under pressure before OOM;
 (b) charge against max(memory.current - reclaimable_file, outstanding) with a safety floor;
 (c) reconcile the slice view with machine MemAvailable (take the more-permissive when cache-dominated).
Keep the safe direction honest: the current behaviour OVER-rejects (never OOMs), so this is a
usability/efficiency bug, not a safety bug — the fix must preserve "never over-admit into real OOM".
Add a discriminating test: high memory.current dominated by `file` cache + abundant MemAvailable
must admit; high memory.current dominated by `anon` must still gate.

Evidence path: internal/daemon/admit.go:708-754 (evaluateAdmitQueue + checkedAvailable),
admit.go:990-995 (readSliceMemory reads memory.current/max). Repro: one big heavy-I/O confine job
present near the slice cap + submit a small confine → stalls. Safe workaround today: run unconfined,
or wait for the big job to finish (cache reclaims). relates: AIRA-11 (MemAvailable-gating), #67 (reserve ledger).
