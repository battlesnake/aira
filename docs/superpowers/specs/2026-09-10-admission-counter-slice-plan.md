<!-- Ordered implementation slice plan for the admission-counter rebuild.
     Produced by the transform-map workflow (6 subsystem maps + synthesis, advisor-checked), 2026-09-11.
     Source of truth for the slice-by-slice two-loop build on branch admission-counter-planfix. -->

Advisor confirmed the phase structure and gave concrete corrections (move the adoption-term delete before client-reconnect; S1's hidden adoption-switch edit; partial saturated-diagnosis delete; several unassigned sites; mutation-guard specifics; parallelism as disjoint-regions not disjoint-files). Folding all of it into the final checklist.

---

# Admission-counter rebuild — ordered slice plan

Branch `admission-counter-planfix`. Each slice = one two-loop (Opus builds, Fable reviews); each DELETE is its own loop. Heavy Go build/test under `aira confine --`; exact exit code recorded, never green-from-truncation. Line refs are per the maps @ v0.5 (`041a5bc`); where maps 3 and 6 disagree on a range, trust map 3 (it opened the file).

## Decision gates (owner/gate calls, resolve before the noted slice — not code)
- **D1** (before S5): CPU Σleases lives on `Server` (machine-wide; a CPU release must `signal()` **every** live queue) vs assert one-slice-per-box. [map4 RISK-1 — the ONE place "any release re-evaluates the one FIFO" is false today]
- **D2** (at S4): fate of the AIRA-103/106 pressure ceiling (`sliceceiling.go:722-773`) — keep (feeds a smaller ceiling, loop unaffected) or delete. [spec §7]
- **D3** (run the measurement spike ∥ Phase 1; decide before S15/S16): §13 fork — worker connection stays in the Go relay (no Python encoder needed) vs moves into `supervisor.py` (needs a Python ARDR encoder + shared golden fixture). Spike = N Python sockets reconnecting at 2/sec on localhost; pure measurement, no daemon code.
- **D4** (at S4, applied again S15): one daemon-wide **declared-only** policy decision — it retires *both* the `checkedAvailable` physical floor (S4) and the `worker_admit` live-usage/supervisor-RSS guard (S15).
- **D5** (before S14): owner sign-off that `--exclusive` emptiness = `Σleases==0` — this **accepts the orphan gap for exclusivity, not just accounting** (an orphaned RAM holder that lost its lease reads "empty" → `--exclusive` could be granted beside it). Map 2's HIGHEST risk; a note is not a gate.

---

## PHASE 1 — Core data model (signed ledger + CPU). Strictly serial (shared `admitWaiter` / `ledgerCharge` / fit-check / snapshot). **This lands first.**

### S1 — DELETE AIRA-29 dynamic charge
1. **Goal:** retire live-charge; `ledgerCharge()` → `return w.reserve`.
2. **Files:** `admit.go` `refreshWaiterCharge`(1289-1310), `recomputeWaiterCharge`(1208-1233), `chargeColdFloor`(1189-1197), `chargeMargin`(1134-1146), `applyChargeDelta`(1065-1077), `adoptedScopeIsWarm`(1175-1183), charge fields on `admitWaiter`(273-278), consts(59-78), `ledgerCharge()`(394-411) simplify, eval-loop `refreshWaiterCharge` call + `subReserved`(2442-2454, 2475-2481). **Collapse the adoption switch to its `!dynamicReserve` arm — remove the warm-branch at 2570/2582/2588 — but do NOT delete the loop or the `queue.adopted*` writes (that is S12).** `server.go`(90-103,237-240,358-362); `paths.go dynamicReserveFromEnv`(327-344). Keep `pctClamp`, `confineRecordCap` (shared).
3. **Tests:** delete `admit_dynamic_charge_test.go` (+ `_real_cgroup_linux`). **Mutation:** a test asserting a granted waiter's charge == declared reserve must RED if `ledgerCharge()` consults any (now-deleted) tracked field. Verify `oomsteer.go:451/459` + 4 snapshot sites compile/green.
4. **Deps:** none (first).
5. **Parallel:** N.

### S2 — Signed scope-id-keyed ledger (pure refactor, behaviour-preserving)
1. **Goal:** replace the `outstanding int64` scalar with `available = ceiling − Σ` derived over granted waiters keyed by scope-id; all existing tests stay GREEN.
2. **Files:** `admit.go` `sliceQueue.outstanding/outstandingJobs`(770-771), grant add(2801-2802), release subtract(2941-2954), `admitSliceSnapshotFor`(1555-1720 — ~~make `scopeReserves` authoritative~~ **SUPERSEDED, see below**), `sliceProvablyEmpty`(527-529), `admitOutstandingJobs/Reserve/admitCeiling`(1312-1329,1734-1736). Expose the idempotent `SET(scopeID, resource-vector)` primitive at `enqueueAdmitInternal`(2282-2312) — still refuses dup for now; SET wired live in S8.
   - **SUPERSEDED (S2 build, Fable-confirmed):** "make `scopeReserves` authoritative — the sum derives from it" was WRONG and was NOT done. `scopeReserves` covers scope-BACKED leases only; deriving `scopeBytes`/`outstanding` from it would drop every scope-less waiter (`confine-reserve` per-test reservations, `aira run` leases) → under-count → over-admit. The built design keeps `outstanding`/`outstandingJobs` as a derived cache re-summed over ALL granted && accounted waiters by `rederiveLedgerLocked` (the walk over `queue.waiters` IS the scope-id keying, via the duplicate-scope refusal), and keeps `admitSliceSnapshotFor`'s independent walk so `residualBytes()`/`residualJobs()` stay the cross-check tripwire between the cache and the walk. A separate scope-id-keyed *map* is deliberately NOT built: it would be a second copy of state to keep in sync — the double-mutated ledger §2 exists to remove.
3. **Tests:** `admit_small_reservation_contention_test.go:216-223` (outstanding==Σgranted, Σ≤cap−headroom) MUST survive. **Mutation:** drop one granted **waiter** (not a scopeID — the pin's per-test reservations are scope-less, so a scopeID filter never fires) from the derived sum → contention test reds. `admit_release_e2e_test.go` discharge pin stays red if the release **re-derive** (was the `-=`) is dropped.
4. **Deps:** S1.
5. **Parallel:** N.

### S3 — DELETE AIRA-114 aggregate bound
1. **Goal:** remove the oversubscription bound; `Σreserve≤ceiling` restored by the single ceiling (valid only because S1 landed).
2. **Files:** `admit_oversubscription.go`(1-308 whole); `admit.go` cap fields(856-878), eval-loop sites(2609-2614,2417-2422,2803-2812,2828-2842,2681-2685,2743-2746), `oversubscriptionFactorPctDefault`(80-93), `residual*`/cap snapshot(1537-1543,1376-1377); `server.go`(104-112,241,363-367); `paths.go oversubscriptionFactorFromEnv`(346-390); `confine_manage.go` cap fields.
3. **Tests:** `admit_oversubscription_test.go`(+`_real_cgroup`) go RED → remove with the file; drop `AIRA_DAEMON_OVERSUBSCRIPTION_FACTOR` from `serve_env_settings_test.go`. **Mutation:** an admit-fit test must RED if the fit-check still consults a removed `overSubscribed` term.
4. **Deps:** S1, S2.
5. **Parallel:** N.
6. **BUILT (S3) — framing correction (Fable work-review, `e1a35f4`):** the goal's "restored by the single ceiling" is right for the INVARIANT (`Σreserve ≤ ceiling` holds via the single ceiling) but must not be read as "folded into the single ceiling" — the AIRA-114 bound's coverage of two populations is **RETIRED, not folded**, which makes S3 a **LOOSENING** (owner-signed, design §0/§11/§3, NOT a tightening): (a) delegate scopes, whose containment cap (`scopeCeiling` ~48G) far exceeds declared reserve (~1G), so Σcap can pass 2×ceiling while Σreserve fits (three 48G-cap suites on a 64G slice now admit); (b) scan-visible **non-admitted** live scopes (flock-fallback, un-adopted orphans) the aggregate counted at cap-or-`memory.current`. Backstop = per-scope `memory.max` + MemAvailable watchdog. **Sites beyond this list, all built:** `runner/confine_manage.go` cap wire fields + `main.go` render + `cmd/aira` `TestRenderConfineListScopeCapBound` (the `confine --list` cap-bound surface); the saturated-diagnosis "aggregate disjunct" subtest (deleted) + "aggregate unestablished" subtest (retitled); `oomsteer.go`/`serve_env_settings_test.go`/`real_cgroup_scope_helpers_test.go` prose; `paths.go` `math`/`strconv` imports; the now-dead `confineRecordCap` helper. **Pins:** `admit_single_ceiling_test.go` — the single-ceiling exact-fit/one-over pair + `TestDelegateScopesAdmitOnDeclaredReserveNotContainmentCap` (the loosening; mutation-verified). **Kept** (later slices; only decoupled from the deleted test helpers): `TestConfineListNamesAdoptedScopeReserves` + adopted-tracking (AIRA-74/S12).

### S4 — DELETE `checkedAvailable` physical floor + un-clamp to signed (BEHAVIOUR CHANGE) — **RUNNER-UP RISK**
1. **Goal:** `available = ceiling − Σleases`, signed (may go negative); drop the `current`/`reclaimable` physical floor + AIRA-21 reclaimable discount.
2. **Files:** `admit.go` `checkedAvailable`(2885-2902), `readMemory` args + call in `evaluateAdmitQueue`(2617-2618,2724), fit-check gate(2743-2786, drop `overSubscribed`), `noteGrantableLocked`/`Grantable` wire(573-582,1001-1005,3049-3050) — verify signed render, not uint. **`admitConnection` `!ok` path(2132-2142): change from grant-shaped `unevaluated` to fail-closed refuse (Invariant 6), aligning with the eval-loop `!ok` at 2619.** `sliceceiling.go`(722-773) per D2.
3. **Tests:** NEW (i) `available` goes negative when leases exceed ceiling and the next NEW admission waits until a release recovers it; NEW (iv) physical over-use no longer reduces `available` (asserts the accepted behaviour change deliberately). **Mutation:** re-add clamp-at-zero → the negative-available-wait test reds. Guard: `Grantable` renders signed.
4. **Deps:** S2, S3. D4 policy.
5. **Parallel:** N.
6. **SEQUENCING NOTE (from the S3 work-review):** once S3 landed, the scan-visible **NON-ADMITTED** population (flock-fallback `"max"`-capped scopes, un-adopted leaf-drained orphans) binds admission ONLY through `checkedAvailable`'s physical `current` term — which THIS slice deletes. So the client flock-fallback delete (spec §14 / gate P1-B, currently scheduled in S13) should land **WITH or BEFORE S4**, OR the interim over-admit window for that population must be written into S4's build record. Do not let it fall silently between S4 and S13.

### S5 — CPU as 2nd ledger resource; conjunctive fit/wake (§7). **CORE COMPLETE.**
1. **Goal:** lease → `{ram,cpu}` vector; admit/available/release/wake resource-agnostic; CPU ceiling = `2 × runtime.NumCPU()` (integer, no cgroup read, no `cpu.max`).
2. **Files:** `admit.go` `admitWaiter.reserve`→vector(178-181), `ledgerCharge` per-resource(394-411), `sliceQueue.outstanding`→per-resource(766-771), `checkedAvailable` + `cpuAvailable` sibling(2885-2900), fit-check conjunctive(2747), grant increment(2801-2819), release discharge(2941-2965), `afterAdmitRelease`/`signal`/`runEvaluator`(2344-2372,2970-2979); `validateAdmitArgs` add `cpu` + fail-fast `cpu>2×NumCPU`(3199-3232); `admitRequest`(950-978); `AdmitResponse` cpu-cores(941-948); new CPU-ceiling fn. `runner/types.go`(240,379), `confine.go` `DefaultConfineCPUCores=1`(19); `admission_linux.go` frame `cpu` arg(463-468).
3. **Tests:** (1) CPU-only release wakes a CPU-blocked waiter (ideally across two queues, to pin D1/RISK-1); (2) conjunctive both directions (ram-fits/cpu-over blocks, and vice-versa); (3) `cpu>2×NumCPU` → `RequestInvalid`, never enqueues; (5) `N=2×NumCPU+1` one-core workers, last blocks until release. **Mutation:** set ceiling `1×NumCPU` → a test must RED (pins the 2× decision). Assert NO `cpu.max` write anywhere.
4. **Deps:** S2, S4. D1 resolved.
5. **Parallel:** N.

---

## PHASE 2 — independent live-machinery delete

### S6 — DELETE cpuslots flock governor
1. **Goal:** remove AIRA-64 CPU-slots gate; CPU is now governed by the ledger (S5).
2. **Files:** `cpuslots.go`(1-262), `cpuslots_gate.go`(1-290), 4 cpuslots test files; `server.go` cpuSlots* fields+init+reconfig(127-140,220,248-254,406-417); `worker_admit.go` `cpuSlotsDecide`/`acquireCPUSlotsGate` calls(692-706,728-766)+`state.lastGrantAt`; `worker_admit_outcome.go` cpu_slots grade(188,385,390-391,488-490); `worker_admit_client_linux.go`/`_stub` CPUSlots field; `main.go`(2080); `serve_env_settings_test.go` `AIRA_DAEMON_CPU_RESERVE`.
3. **Tests:** grep built binary — `cpuslots`/`cpuSlotsDecide` gone; `cpu_slots=` ABSENT from the worker-admit outcome line; `TestWorkerAdmitOutcomeVocabularyMatchesTheSupervisor` updated+green (Go + `supervisor.py` move together). **Mutation:** a test asserting worker CPU admission routes through the ledger must RED if a residual gate call remains.
4. **Deps:** S5 (CPU-in-ledger MUST precede — else a CPU-ungoverned gap).
5. **Parallel:** **Y with S7** (disjoint regions; both touch `server.go` — cpuSlots fields vs `readInboundFrame` — coordinate the edits).

---

## PHASE 3 — restart/reconnect machinery (the "after the core" frame/dump/reconnect)

### S7 — ARDR frame + encoder + golden fixture + magic-sniff + ProtocolVersion bump
1. **Goal:** freeze the version-frozen re-declare frame + single Go encoder/decoder + 1-byte ack; sniff the 4-byte `ARDR` magic before framing/handshake; bump the protocol.
2. **Files:** `protocol.go` `readFrame`/`writeFrame`(251-298) add ARDR codec + ack + golden bytes; `ProtocolVersion 9→10`(83); `server.go` `readInboundFrame`(911-951) magic-sniff before the proto-mismatch close(754-757); runner mirrors `admission_linux.go`(78-84 `DaemonProtocolVersion` bump lockstep, `writeRunnerAdmitFrame`750-851) + `worker_admit_client_linux.go`. TOTAL parser (every byte-seq parses or hard LOGGED reject). Frame = `magic|frame_len u32|scope_id|ram_bytes u64|cpu_cores u32|parent_scope_id`.
3. **Tests:** frozen invariant `MaxFrameBytes(16MB) < magic(0x41524452)` pinned; every-byte-seq parses-or-hard-rejects; **old-frame→new-parser golden-bytes test asserting the resulting LEDGER CHARGE, not field deserialization** (§4/Inv.8); update `version_test.go`/`protocol_test.go` for 9→10 + keep runner↔daemon version-equality green. **Mutation:** shrink `MaxFrameBytes` above magic OR move the sniff after the proto check → the sniff test reds.
4. **Deps:** S5 (frame carries `cpu_cores`).
5. **Parallel:** **Y with S6.** (Bump makes v0.5 clients loudly refused — fine on-branch; it is the final-cutover gate + atomic PATH-binary/daemon reinstall.)

### S8 — `admitWaiter` new fields + compare-and-release + idempotent SET-by-scope-id
1. **Goal:** add restart/anchor state and make re-declare a re-anchoring SET, not a refusal.
2. **Files:** `admit.go` `admitWaiter`(178-260) ADD `anchor{conn identity + monotone generation}`, `clientPID`, `processStartTick`, `unanchored` (one struct edit, after S1 removed the charge fields); extract the peer-EOF watcher(2165-2171,2264-2267) into a shared anchor+watcher helper reused by `admitConnection` + `workerAdmitConnection`; `releaseAdmitWaiterLocked`(2918-2965) gated compare-and-release (discharge only if `anchor==thisConn`); `enqueueAdmitInternal`(2306-2312) dup-scope refusal → idempotent SET/re-anchor + insert-if-absent (state=granted, accounted); KEEP single-exclusive refusal(2325-2333) + seq-overflow guard(2334). Reuse `unixPeerCredential` (`supervisor_lease.go:30-105`) SO_PEERCRED same-uid — **no cgroup-membership check** (P2-C).
3. **Tests:** **Reconnect-race interleaving with a seam that forces BOTH lock orders** (old-EOF-first must not discharge a lease the re-declare re-anchors; re-declare-first must make the stale EOF a no-op) — not timing-based. `supervisor_lease_test.go:71` (same-uid) reused to gate re-declare. **Mutation:** drop the anchor-generation compare → the stale-EOF-releases-nothing test reds.
4. **Deps:** S2 (SET primitive); struct coordinated with S1.
5. **Parallel:** Y with S6/S7 (admit.go vs cpuslots/protocol; coordinate).

### S9 — ARDR re-declare handler in `serveConnection`
1. **Goal:** route a sniffed ARDR frame to a re-declare branch that SETs+re-anchors the lease, replies the 1-byte ack, holds the connection, and never closes on error.
2. **Files:** `server.go` new handler beside admit/worker-admit dispatch(820-841) — set `wrote=true`, set own read deadline (AIRA-84, cleared at 773), bypass the proto-mismatch close(754-757), keep-connection-on-refuse (not the `defer conn.Close()` at 725). Uses the S8 SET/anchor helper.
3. **Tests:** re-declare accepted during freeze; same-uid re-declare accepted for a holder living OUTSIDE its own scope (confine + aitest); assert no cgroup-membership check. **Mutation:** make the refuse path `return` (→ `defer conn.Close()` drops the lease) → a "refuse-keeps-lease-alive" test reds.
4. **Deps:** S7, S8.
5. **Parallel:** **Y with S10** (`serveConnection` handler vs `Serve` shutdown).

### S10 — Dump-on-shutdown
1. **Goal:** persist the live-lease ledger on graceful shutdown as frozen ARDR records + pid + start-tick + parent_scope_id.
2. **Files:** `server.go` `Serve`(545-573) snapshot all `admitQueues` granted waiters (lock-ordering across per-slice `mu`) **before** `close(stopping)`(573); fsync+rename as the last act; RuntimeDir 0700/0600 perms(375-380). Freshness threshold ≥ `DrainTimeout(10s)+RestartSec(2s)+margin ≈ 30s`.
3. **Tests:** dump captures still-held leases; graceful shutdown closes lease connections only AFTER the dump (§14 P3). **Mutation:** move the snapshot after `close(stopping)` → the "dump captures held leases" test reds.
4. **Deps:** S7 (encoder), S8 (pid/anchor fields).
5. **Parallel:** Y with S9.

### S11 — Reload + kill-probe + consume-once + unanchored-drop + restart-freeze — **★ SINGLE RICHEST-RISK SLICE ★ (spec §4 P1-A "the biggest risk")**
1. **Goal:** a fresh daemon reloads a <30s dump, `kill -0`s each pid (drop dead), seeds survivors UNANCHORED, freezes NEW admissions 2s, and drops any lease still unanchored at end-of-freeze + ~10s grace regardless of pid.
2. **Files:** `server.go` `Serve` after DB-open(425-441) before `net.Listen`(442) — reload+kill-probe+consume-once(rename-on-read); at listen-ready arm `restartFreezeUntil` (**distinct symbol — NOT the AIRA-59 `admitFreeze*` fairness freeze at 812-931**); new unanchored-drop timer goroutine (spawn region 456-543, cancel in shutdown); `admit.go evaluateAdmitQueue` freeze gate on the **NEW-admission grant path only** (re-declares never frozen). **Re-derive `GrantedEstablished` (AIRA-220 honesty bit) from "freeze over AND no unanchored leases" — its scan-adopt source dies in S12; do not hardcode true.**
3. **Tests (this slice carries the merge-gate seed — confine-only form):** freshness boundary (at threshold reloads; at +ε or absent → full quota); NEW admission waits during the 2s freeze while a re-declare is accepted immediately; non-blocking mode returns `unevaluated` during freeze; `GrantedEstablished` reads false during freeze. **Mutations:** (a) disable the unanchored-drop timer → a live-supervisor-pid/dead-worker lease leaks → reds; (b) drop the consume-once rename → a second restart re-seeds → reds; (c) set threshold 10s → a slow drain skips reload → reds.
4. **Deps:** S9, S10, S5.
5. **Parallel:** N.

### S12 — DELETE #74 restart-adoption term (moved to immediately after S11)
1. **Goal:** remove the `queue.adopted` addend; the reload+re-declare of S11 is now the post-restart guard.
2. **Files:** `admit.go` `adopted*` fields(772-794), adoption loop(2515-2607), fit-check(2722 drop `adoptedJobs`; 2724 `addClamp(outstanding,adopted)`→`outstanding`), `admitOutstandingReserve`(1326-1329), snapshot adopted(1581-1597), `delegateRAMAdoptionMargin`(53-57); `confine_manage.go`(200-257) drop adopted/`AdoptedBytes`/`AdoptedJobs`+`totalJobs`; `runner/confine_manage.go` wire(409-410); `main.go`(3456-3457), `tui_top.go`(999); `oomsteer.go:61` comment.
3. **Tests:** `admit_reconstruction_test.go`/`adopt_test.go` removed. **Mutation (the top ordering hazard):** leave the adopted addend in the fit-check → a real restart-under-load test double-counts/over-admits → reds. (Placing this **after** S11 avoids the S11 restart test reding for the wrong reason — a reloaded UNANCHORED lease is not in the adoption `held{}` skip, so the still-live scan would adopt its populated scope and double-count.)
4. **Deps:** S11 (dump/reload+re-declare *exists*).
5. **Parallel:** N.

### S13 — Client reconnect + re-request; DELETE client flock fallback + daemon no-timeout (coordinated multi-delete)
1. **Goal:** on daemon EOF, client reconnects 2/sec and re-requests — never fail-open to an ungoverned launch (AIRA-222 class); daemon has no request timeouts.
2. **Files:** `runner/admission_linux.go` DELETE `admitWithFlock`(251-345), `admitLock`(44-76), `tryAdmissionLock`/`admissionLockDir`(1072-1109), `noteAdmission`(866-870), rebuild `fail()`(421-452) → reconnect+re-request, fallback tail(216-249). `admit.go` DELETE deadline arm(2183-2199), `timeoutAdmitWaiter`(2904-2916), **the saturated arm only(2225-2241) + `E_ADMIT_SATURATED`/`WAIT_TOO_LONG` render + `CodeAdmitWaitTooLong`** — **KEEP the exclusive-unestablished arm(2207-2218): its setter (drain-abort 2655-2670) dies in S14; delete the reader before the setter and `--exclusive` drain wedges silently**; `max_wait` plumbing/consts(24-41). **KEEP `admitExclusiveWaitCeiling*`(95-139)** (flag as a §6 collision for the owner; default keep). CLI `--admit-timeout`(main.go 1059-1063,1382-1386,1521-1523). `confine_linux.go requireAdmissionRefusal` narrows.
3. **Tests:** blocked request never self-expires (only peer-EOF/stopping/grant end it); **the client reconnect must NOT re-declare an `--exclusive` lease** — `admit_exclusive_unwedge_test.go:206` (exclusive=lost, §15 P2-D) stays green. **Mutation:** re-add the flock fallback on daemon EOF → "must reconnect, must NOT launch" reds — **against a REAL daemon restart, not a stub** (a stub accepts any argv — the shipped-`--admit-timeout` class of false-pass).
4. **Deps:** S9 (daemon re-declare handler), S11 (freeze).
5. **Parallel:** N (shares `admit.go` eval-loop region with S12).
6. **Gate exit:** confine-only restart-under-load merge test passes here (see MERGE GATE).

---

## PHASE 4 — shared cgroup-scan teardown (gated on the restart machinery)

### S14 — DELETE the periodic cgroup scan + rewire emptiness to the ledger — **RUNNER-UP RISK**
1. **Goal:** remove the ListConfines periodic scan once all its consumers are off it; derive exclusive/drain emptiness from the ledger.
2. **Files:** **first** rewire `sliceProvablyEmpty`(527-529) + `exclusiveGate.blocks`(763) to `Σleases==0`, THEN delete: periodic scan call(2377-2400), `confineScan` seam(`shim.go`237-254; shim branch 249-253), `admitConfineScan` field/interval(`server.go`164,176,233,259), `adoptedAt`/`adoptedScanFailed`, `scanFailingSince` + drain-abort block(2655-2670, the S13-kept arm's setter), `scopeSeen`/`scopeVanished` writes(2464-2487) → `confine_reaper.go dischargeVanishedStaleLease`(316-333) dead + `Vanished*` wire/renderers. **Keep the AIRA-72 orphan-empty-scope rmdir sweep in `runScopeReaper` (independent hygiene — one grep in `confine_reaper.go` to separate it from `dischargeVanishedStaleLease`; map 6 over-lumps it under DELETE).**
3. **Tests:** re-express `admit_exclusive_test.go` scan-derived cases(280,305,328,408,444,473) as ledger-emptiness; 16 test files inject `admitConfineScan` — update/retire. **Mutation:** make `sliceProvablyEmpty` return true while a live lease exists → the exclusive-not-granted-when-nonempty test reds.
4. **Deps:** S3 (capAggregate gone), S2 (ledger for emptiness), S13 (socket-EOF-only release is the live model). **D5 sign-off.**
5. **Parallel:** N.

---

## PHASE 5 — aitest one-connection-per-lease + reconnect/frame (after the core)

### S15 — worker-admit → blocking queue-lease on the ledger (AIRA-41 reversal) [may sub-split: daemon rebuild / shim unify / client one-conn]
1. **Goal:** the daemon creates the worker sub-scope and returns a lease held on one connection per worker; EOF releases it; workers charge the same signed ledger; no poll loop.
2. **Files:** `worker_admit.go` rebuild `evaluateWorkerAdmit`(435-862) onto the ledger + conjunctive CPU fit; DELETE poll loop(986-1067) + `max_wait` validate(864-937); **AIRA-41 reversal — `workerAdmitConnection` EOF releases the lease under the lock, compare-and-release**(1071-1123); split the scan-as-ledger — keep the slim readdir for the worker-ID re-seed(769-771), delete the `memory.max` committed-sum + `workerCommitted`(108-372); DELETE the live-usage/supervisor-RSS guard(507-546,601-673) per D4. `worker_admit_shim.go`(81-245) shim ledger → the ONE unified signed scope-id-keyed ledger (synthetic scope-id, advisory). `worker_admit_client_linux.go`(16-213) one-conn-per-worker-lease + reconnect/re-declare. `max_wait_ms==0` → non-blocking mode. **`admitSlots`(2063-2086, =1024) is held for a lease's whole lifetime → under N-per-worker connections it becomes a cap on concurrent LIVE leases: release the slot after grant OR size-and-document (required check, don't defer).**
3. **Tests:** INVERT every AIRA-41 test that pins "closed connection frees nothing" → now "EOF releases the lease" (`worker_admit_test.go`, `worker_admit_shim_test.go`, `worker_admit_cli_granted_linux_test.go`); rewrite shim-ledger tests for the signed ledger; EOF-release returns RAM immediately (not at suite end); non-blocking mode returns current available. **Mutation:** re-instate "EOF frees nothing" (the v0.5 AIRA-41 semantics) → the immediate-EOF-release test reds.
4. **Deps:** S5, S6, S9, S11, S12, S13, S14. D3, D4.
5. **Parallel:** N.

### S16 — `supervisor.py` one-connection-per-worker-lease + reconnect
1. **Goal:** the supervisor holds N daemon connections (one per worker), reconnects+re-declares each on daemon EOF, releases on connection close.
2. **Files:** `supervisor.py` `acquire_worker`(796-990) rebuild; `spawn_worker`/`_await_placement_ack`(1226-1393) keep Python placement + ack; `_retire_worker`(1511-1573) close connection = EOF release; `_forget_worker_scope`(1689-1710) rmdir hygiene-only (invert docstring); `run()` reconnect-on-EOF (no fail-open, no `_disable_daemon` on a restart)(2362-2450); drop `_parse_max_wait_seconds`. **If D3 = direct socket: add the Python ARDR encoder + share the S7 golden fixture.**
3. **Tests:** pytest — one-conn-per-worker reconnect on daemon EOF (no fail-open); a daemon-restart EOF is not treated as Unavailable (which would strip containment).
4. **Deps:** S15, D3.
5. **Parallel:** N.

### S17 — DELETE worker-peak relay → retarget to `confine-report`
1. **Goal:** retire the `aira worker-peak` CLI transport; the pool peak sample flows over the KEPT `confine-report`/`ReportPeakSample` verb.
2. **Files:** DELETE `main.go` `parseWorkerPeakArgs`/`runWorkerPeakCommand`+dispatch(1174-1257,298-303,722-723), `scope_dir.go` allowlist(158); `supervisor.py _report_pool_usage`(1643-1687) → confine-report (Kind=pytestWorker), fail-open preserved. KEEP `confine_report.go`(1-107) + `ReportPeakSample`.
3. **Tests:** `_report_pool_usage` still records the pytestWorker peak/oom/budget fail-open via confine-report. **Mutation:** break the confine-report send path → the peak-sample-recorded test reds (proves the estimate feedback is not starved).
4. **Deps:** S16.
5. **Parallel:** N.

---

## PHASE 6 — data/CI (parallelisable late)

### S18 — CI `--dump <file>` + retain data
1. **Goal:** write collected admission/utilisation records as JSONL (atomic write, honest `unevaluated`); no network in the daemon.
2. **Files:** dump writer on the existing store; retain per-admission (declared vs peak, wait, grant/deny/fail-fast) + per-queue (oldest-blocked wait, negative-`available` excursions, per-resource utilisation).
3. **Tests:** JSONL atomic write; `unevaluated` for anything unmeasured; negative-available excursion recorded.
4. **Deps:** S5 (core) + `confine-report` path.
5. **Parallel:** **Y** — buildable from after S5 in a separate worktree.

---

## Single riskiest slice
**S11** (reload + kill-probe + unanchored-drop + restart-freeze). The dump records the **supervisor** pid, which outlives its workers, so `kill -0` alone leaks a retired worker's lease — the drop-after-grace timer is the real safety, `kill -0` only an early-drop optimisation (spec §4 P1-A, self-named "the biggest risk"). Runner-ups: **S4** (physical-floor delete — silent, load-bearing, owner-signed) and **S14** (exclusive-drain emptiness rewire — silent correctness regression, gated by D5). Sharpest ordering hazard: **S12 before S13, after S11** (adopted addend removed too early ⇒ post-restart over-admission).

## Mandatory real restart-under-load merge-gate test (staged)
Real cgroups, under `aira confine --`, exact exit code recorded, never claimed green from truncated output.
- **Confine-only form — gates S13 exit:** dump on graceful shutdown → reload + kill-probe → dead pids dropped → survivors reconnect + re-declare → **no double-count, no over-admit**; plus the reconnect-race interleaving pin (both lock orders, seam-forced) and the old-frame→new-parser golden-bytes charge pin.
- **Full form — gates branch merge (after S16):** the above **with an aitest worker retiring mid-drain across the restart** (exercises the unanchored-lease-drop) and `exclusive=lost` across restart (§15 P2-D).

Draft saved at `/tmp/claude-1000/-home-mark-claude-aira/f2623d20-c3ca-4d22-b003-91a9d2a30768/scratchpad/slice-plan.md`.
