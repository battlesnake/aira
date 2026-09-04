#!/usr/bin/env bash
# AIRA-68 mutation testing (Tier A). Each mutation is applied to a THROWAWAY copy
# of the tree; the named test must FAIL against it. A test that still passes
# proves nothing about the defect it claims to cover.
set -uo pipefail
SRC=$(git rev-parse --show-toplevel)
WORK=${1:?usage: docs/dev/aira68-mutation-check.sh <scratch-dir> (use ~/tmp, never /tmp)}
mkdir -p "$WORK" || exit 99

run_mutation() {
  local name="$1" test_pattern="$2" pkg="$3" file="$4" from="$5" to="$6"
  local dir="$WORK/$name"
  rm -rf "$dir"
  cp -a "$SRC" "$dir" 2>/dev/null
  rm -rf "$dir/.git"
  python3 - "$dir/$file" "$from" "$to" <<'PY'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path).read()
if old not in s:
    print("MUTATION-NOT-APPLIED", path, file=sys.stderr)
    sys.exit(3)
open(path, "w").write(s.replace(old, new, 1))
PY
  if [ $? -ne 0 ]; then echo "$name: MUTATION-NOT-APPLIED (source drifted) -> INVALID"; return; fi
  local out
  out=$(cd "$dir" && go test "$pkg" -run "$test_pattern" -count=1 2>&1)
  local code=$?
  if printf '%s' "$out" | grep -q 'build failed\|cannot find\|undefined:\|syntax error'; then
    # A mutation that does not COMPILE proves nothing about the test: the
    # non-zero exit came from the toolchain, not from the assertion.
    echo "$name: MUTATION DID NOT COMPILE -> INVALID, not evidence"
  elif [ $code -eq 0 ]; then
    echo "$name: TEST STILL PASSED (exit 0) -> POROUS TEST"
  else
    echo "$name: test failed as required (exit $code) -> mutation caught"
  fi
  printf '%s\n' "$out" | grep -E '^(--- FAIL|    [a-z_]+\.go:|FAIL|ok )' | head -6 | sed 's/^/    /'
  rm -rf "$dir"
}

# M1: drop the ledger discharge entirely (the alleged leak, injected).
run_mutation m1-no-discharge 'TestAdmitLedgerReturnsToZero' ./internal/daemon \
  internal/daemon/admit.go \
  'queue.outstanding -= waiter.reserve
		queue.outstandingJobs--' \
  '_ = waiter.reserve'

# M2: byte-only discharge loss (job counter intact). Targeted at the END-TO-END
# ledger test, not at the residual unit test: the residual test builds its
# desynchronised fixture by hand and never calls releaseAdmitWaiter at all, so
# mutating the discharge could not affect it either way. Pointing a mutation at a
# test the mutated code cannot reach produces a "porous" verdict that says
# nothing about the test.
run_mutation m2-byte-only-loss 'TestAdmitLedgerReturnsToZero' ./internal/daemon \
  internal/daemon/admit.go \
  'queue.outstanding -= waiter.reserve' \
  '_ = waiter.reserve'

# M2b: report only the job residual (v1's design), which is blind to a byte-only
# desynchronisation. THIS is the mutation the residual unit test exists to catch.
run_mutation m2b-no-byte-residual 'TestAdmitSnapshotReportsByteResidualIndependentlyOfJobs' ./internal/daemon \
  internal/daemon/admit.go \
  'return snapshot.outstanding - (snapshot.scopeBytes + snapshot.reservationBytes)' \
  'return 0'

# M3: release only on a clean io.EOF, not on any read error (an abruptly killed client leaks).
run_mutation m3-eof-only 'TestAdmitLedgerReleasesWhenAClientDiesWithoutClosingCleanly' ./internal/daemon \
  internal/daemon/admit.go \
  '_, _ = conn.Read(one[:])
		cancelPeer()' \
  'if _, err := conn.Read(one[:]); err == nil || err.Error() == "EOF" {
			cancelPeer()
		}'

# M4: classify every connection-held grant as scope-backed (the split stops being a split).
run_mutation m4-no-scopeless-class 'TestAdmitSnapshotSeparatesTheThreeLedgerPopulations' ./internal/daemon \
  internal/daemon/admit.go \
  'if waiter.scopeID == "" {
			snapshot.reservationJobs++' \
  'if false {
			snapshot.reservationJobs++'

# M5: restore the pre-AIRA-68 behaviour — the vanished proof never fires, so a
# lease whose scope is already gone falls through to ReapScopeIfEmpty, gets
# ENOENT, and is skipped on every pass forever.
# (Mutating the `if vanished` branch to `if false` instead would leave `vanished`
# unused and fail to COMPILE, which is not evidence about any test.)
run_mutation m5-restore-skip 'TestReleaseStaleGrantedLeasesPassReclaimsALeaseWhoseSeenScopeIsNowGone' ./internal/daemon \
  internal/daemon/confine_reaper.go \
  'if s.dischargeVanishedStaleLease(candidate, grace) {' \
  'if false && s.dischargeVanishedStaleLease(candidate, grace) {'

# M6: reclaim on plain ABSENCE (drop the scopeSeen requirement) — the unsafe
# design. Targeted at the PRODUCER test: the reaper tests set scopeSeen/
# scopeVanished directly on the waiter, so a mutation inside evaluateAdmitQueue
# cannot reach them.
run_mutation m6-absence-not-transition 'TestEvaluateAdmitQueueNeverMarksANeverSeenScopeVanished' ./internal/daemon \
  internal/daemon/admit.go \
  'if waiter.scopeSeen {
					waiter.scopeVanished = true
				}' \
  'waiter.scopeVanished = true'

# M7: drop the locked re-validation — the ABA, both halves.
# (Mutating the whole condition to `if false` leaves `found` unused and fails to
# compile; inverting it to a conjunction keeps every variable referenced while
# still letting a released, replaced waiter through to the destructive reap.)
run_mutation m7-no-revalidation 'TestReleaseStaleGrantedLeasesPassDoesNotReleaseOrReapAReplacementWithTheSameScopeID' ./internal/daemon \
  internal/daemon/confine_reaper.go \
  'return staleLeaseActionableLocked(s.admitNowTime(), queue, waiter, candidate.scopeID, grace)' \
  'return true'

# M8: reclaim a stale lease with NO vanished proof at all — the age signal alone,
# which is a reclaim POLICY masquerading as a death proof.
run_mutation m8-reclaim-without-proof 'TestReleaseStaleGrantedLeasesPassLeavesANeverSeenScopeAlone' ./internal/daemon \
  internal/daemon/confine_reaper.go \
  '!waiter.scopeVanished || queue.adoptedScanFailed {' \
  'queue.adoptedScanFailed {'

# M9: drop the scope-less skip in the candidate selector (D3's pinned gap).
run_mutation m9-select-scopeless 'TestStaleGrantedLeasesNeverSelectsAScopelessReservation' ./internal/daemon \
  internal/daemon/confine_reaper.go \
  'waiter.scopeID == "" || waiter.grantedAt.IsZero()' \
  'waiter.grantedAt.IsZero()'

# M10: mark a currently-OBSERVED scope vanished (the bit stops meaning absence).
# Producer-side, for the same reason as M6.
run_mutation m10-vanish-on-sighting 'TestEvaluateAdmitQueueRecordsTheSeenThenGoneTransition' ./internal/daemon \
  internal/daemon/admit.go \
  'waiter.scopeSeen, waiter.scopeVanished = true, false' \
  'waiter.scopeSeen, waiter.scopeVanished = true, true'

# M10b: latch scopeVanished — never clear it when the scope is seen again, so one
# transient scan gap makes a live job a permanent reclaim candidate.
run_mutation m10b-latch-vanished 'TestEvaluateAdmitQueueClearsVanishedWhenTheScopeIsObservedAgain' ./internal/daemon \
  internal/daemon/admit.go \
  'waiter.scopeSeen, waiter.scopeVanished = true, false' \
  'waiter.scopeSeen = true'

# M10c: fail-OPEN on a failed scan — treat the scan error as an empty scan, so
# every live lease is marked vanished the moment cgroupfs hiccups.
run_mutation m10c-vanish-on-scan-failure 'TestEvaluateAdmitQueueWritesNoTransitionBitOnAFailedScan|TestEvaluateAdmitQueueTreatsAnUnevaluatedScanAsAFailure' ./internal/daemon \
  internal/daemon/admit.go \
  'if scanErr != nil {
			if !queue.adoptedScanFailed {' \
  'if false {
			if !queue.adoptedScanFailed {'

# M10d: write transition bits for SCOPE-LESS reservations too, which have no
# cgroup artifact for the scan to observe in either direction.
run_mutation m10d-scopeless-transition 'TestEvaluateAdmitQueueLeavesScopelessReservationsUntouched' ./internal/daemon \
  internal/daemon/admit.go \
  'if waiter == nil || waiter.state != admitGranted || waiter.scopeID == "" {
					continue
				}
				held[waiter.scopeID] = struct{}{}' \
  'if waiter == nil || waiter.state != admitGranted {
					continue
				}
				held[waiter.scopeID] = struct{}{}'

# M11: report a present ledger for an ABSENT queue (the honesty contract).
run_mutation m11-absent-queue-present 'TestAdmitSnapshotAbsentQueueStaysAGenuineIdleZero' ./internal/daemon \
  internal/daemon/admit.go \
  'return admitSnapshot{phase: phase}' \
  'return admitSnapshot{phase: phase, present: true}'

# M12: floor the byte residual through the 0B formatter (hides the negative half).
run_mutation m12-floor-residual 'TestRenderConfineListReserveBreakdown' ./cmd/aira \
  cmd/aira/main.go \
  'LEDGER INCONSISTENCY: jobs %+d, bytes %+d unattributable to any population\n",
				result.SliceReserve.ResidualJobs, result.SliceReserve.ResidualBytes)' \
  'LEDGER INCONSISTENCY: jobs %+d, bytes %s unattributable to any population\n",
				result.SliceReserve.ResidualJobs, formatReserveBytes(result.SliceReserve.ResidualBytes))'

# M13: count vanished leases as a fourth population (the split stops summing).
run_mutation m13-vanished-not-subset 'TestAdmitSnapshotVanishedLeasesRemainInsideTheScopeBackedPopulation' ./internal/daemon \
  internal/daemon/admit.go \
  'snapshot.scopeJobs++
		snapshot.scopeBytes = addClamp(snapshot.scopeBytes, waiter.reserve)
		if waiter.scopeVanished {' \
  'if !waiter.scopeVanished {
			snapshot.scopeJobs++
			snapshot.scopeBytes = addClamp(snapshot.scopeBytes, waiter.reserve)
		}
		if waiter.scopeVanished {'

# M14: act on a stale vanished sighting while the confine scan is FAILING, i.e.
# reclaim on an observation that can no longer be confirmed.
run_mutation m14-reclaim-while-scan-failing 'TestReleaseStaleGrantedLeasesPassWillNotReclaimAVanishedLeaseWhileTheScanIsFailing' ./internal/daemon \
  internal/daemon/confine_reaper.go \
  '!waiter.scopeVanished || queue.adoptedScanFailed {' \
  '!waiter.scopeVanished {'

# M14b: the recovery half — never reclaim on the vanished proof at all, so the
# guard above becomes a permanent wedge rather than a fail-closed pause.
run_mutation m14b-never-recover 'TestReleaseStaleGrantedLeasesPassReclaimsOnceTheScanRecovers' ./internal/daemon \
  internal/daemon/confine_reaper.go \
  '!waiter.scopeVanished || queue.adoptedScanFailed {' \
  'true {'

# M15: claim every discharge transitioned the waiter, so a reclaim a concurrent
# ordinary release actually performed is reported as this pass's own.
run_mutation m15-fabricated-reclaim 'TestReleaseAdmitWaiterLockedReportsOnlyTheCallThatTransitioned' ./internal/daemon \
  internal/daemon/admit.go \
  'if waiter.state == admitReleased {
		return false
	}' \
  'if waiter.state == admitReleased {
		return true
	}'

# M16: drop the post-reap re-validation, restoring the validate-unlock-discharge
# window both plan reviewers found.
run_mutation m16-no-post-reap-revalidation 'TestDischargeReapedStaleLeaseRevalidatesBeforeTouchingTheLedger' ./internal/daemon \
  internal/daemon/confine_reaper.go \
  '	queue.mu.Lock()
	if !staleLeaseActionableLocked(s.admitNowTime(), queue, waiter, candidate.scopeID, grace) {
		queue.mu.Unlock()
		return false
	}
	released := releaseAdmitWaiterLocked(queue, waiter)
	queue.mu.Unlock()
	if released {
		s.afterAdmitRelease(queue)
	}
	return released
}

// staleLeaseStillActionable' \
  '	queue.mu.Lock()
	releaseAdmitWaiterLocked(queue, waiter)
	queue.mu.Unlock()
	s.afterAdmitRelease(queue)
	return true
}

// staleLeaseStillActionable'

echo "MUTATIONS-DONE"
