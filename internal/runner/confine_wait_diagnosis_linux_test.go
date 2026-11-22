//go:build linux

package runner

import (
	"strings"
	"testing"
)

// AIRA-370. The wait line's one diagnosis clause, through the real probe
// (queueNoteFor), from the live evidence of 2026-11-22: 27 running, 4 queued,
// 45.5G of a 45.6G ceiling booked with the ceiling cut by system pressure
// (MemAvailable 43.5G), CPU pressure 70%, 16 cores at 2 slots per core.
const (
	diagGranted      = int64(46592) << 20 // 45.5G
	diagCeiling      = int64(46694) << 20 // 45.6G
	diagMemAvailable = int64(44544) << 20 // 43.5G
)

func diagPressure(value float64) *float64 { return &value }

// diagReserve is the live summary as the probe's daemon reports it for one
// queued waiter, before the diagnosis-specific fields.
func diagReserve(position int, blockedBy ...string) *ConfineSliceReserve {
	return &ConfineSliceReserve{
		GrantedEstablished: true, GrantedBytes: diagGranted, CeilingBytes: diagCeiling, Jobs: 27,
		Queued: 4, QueuePosition: position,
		CeilingMode: "enforce", CeilingState: "throttled", CeilingBasis: "system-pressure",
		MemAvailableBytes: diagMemAvailable,
		LaneCapacityBytes: 8 << 30, LaneWait: "full",
		CPUOutstandingCores: 32, CPUCeilingCores: 32, CPUCores: 16,
		CPUPressureSomeAvg10: diagPressure(70),
		BlockedBy:            blockedBy,
	}
}

// verifies: AIRA-370 — the diagnosis is the ONE reason clause and replaces the
// clauses that said the same thing: RAM carries the booked figures (no second
// "granted" clause) and the ceiling cut; CPU names SLOTS and says it is not
// load, and drops the RAM ceiling cut that is not why this job waits; the lane
// clause is gone whenever a diagnosis is present; the box CPU note is context
// on every line. Mutations: append the lane clause anyway (every row's absent
// "small-job lane" goes red); print the ceiling cut on a CPU diagnosis; drop
// "not CPU load"; keep the granted clause on a RAM diagnosis.
func TestConfineWaitDiagnosisIsTheOneReasonClause(t *testing.T) {
	busy := ", box CPU busy: some task waited for a CPU 70% of the last 10 s"
	for _, test := range []struct {
		name    string
		reserve *ConfineSliceReserve
		want    string
	}{
		{"ram, ceiling cut, at position 1", diagReserve(1, ConfineBlockedRAM, ConfineBlockedCeilingCut),
			", 1 of 4 queued, waiting for RAM: 46592M of 46694M booked, ceiling cut to keep system free memory (MemAvailable 44544M)" + busy},
		{"ram without the cut", diagReserve(2, ConfineBlockedRAM),
			", 2 of 4 queued, waiting for RAM: 46592M of 46694M booked" + busy},
		{"cpu slots", diagReserve(2, ConfineBlockedCPU),
			", 2 of 4 queued, waiting for CPU slots: 32 of 32 booked (an admission count, not CPU load), 46592M of 46694M granted" + busy},
		{"ram and cpu", diagReserve(3, ConfineBlockedRAM, ConfineBlockedCeilingCut, ConfineBlockedCPU),
			", 3 of 4 queued, waiting for RAM: 46592M of 46694M booked, ceiling cut to keep system free memory (MemAvailable 44544M); CPU slots: 32 of 32 booked (an admission count, not CPU load)" + busy},
		{"vram", diagReserve(2, ConfineBlockedVRAM),
			", 2 of 4 queued, waiting for GPU memory (VRAM), 46592M of 46694M granted" + busy},
		{"behind the head", diagReserve(2, ConfineBlockedHead),
			", 2 of 4 queued, fits but held behind the head of the queue, 46592M of 46694M granted" + busy},
		{"restart freeze", diagReserve(2, ConfineBlockedRestartFreeze),
			", 2 of 4 queued, new admissions paused while the restarted daemon re-registers running jobs, 46592M of 46694M granted" + busy},
		{"exclusive, holder unknown to this reply", diagReserve(2, ConfineBlockedExclusive),
			", 2 of 4 queued, held for an exclusive job, 46592M of 46694M granted" + busy},
	} {
		t.Run(test.name, func(t *testing.T) {
			note := queueNoteFor(t, 0, test.reserve)
			if note != test.want {
				t.Fatalf("note\n got %q\nwant %q", note, test.want)
			}
			if strings.Contains(note, "small-job lane") || strings.Contains(note, "no room") {
				t.Fatalf("note=%q carries a lane clause beside the diagnosis", note)
			}
		})
	}
}

// verifies: AIRA-370 — an exclusive diagnosis with the holder in the same reply
// is said once, by the existing exclusive clause.
func TestConfineWaitDiagnosisExclusiveIsSaidOnce(t *testing.T) {
	reserve := diagReserve(2, ConfineBlockedExclusive)
	reserve.Exclusive = &ConfineExclusiveState{State: "held", Name: "bench", Owner: "s1"}
	reserve.CPUPressureSomeAvg10 = nil
	want := `, 2 of 4 queued, 46592M of 46694M granted, slice held exclusively by "bench" (s1)`
	if note := queueNoteFor(t, 0, reserve); note != want {
		t.Fatalf("note\n got %q\nwant %q", note, want)
	}
}

// verifies: AIRA-370 — the exclusive clause is the exclusive DIAGNOSIS's
// wording, not an extra clause: when another gate wins the precedence (the
// restart freeze over a drain, a resource for the drain head on a provably
// empty slice) the line names that gate alone. An older daemon (no diagnosis)
// keeps today's exclusive clause. Mutation: append the exclusive clause
// whatever the diagnosis (the freeze and ram rows go red); drop it from the
// fallback (the older-daemon row goes red).
func TestConfineWaitDiagnosisNamesExclusivityOnlyWhenItIsTheGate(t *testing.T) {
	for _, test := range []struct {
		name    string
		blocked []string
		want    string
	}{
		{"restart freeze over a drain", []string{ConfineBlockedRestartFreeze},
			", 2 of 4 queued, new admissions paused while the restarted daemon re-registers running jobs, 46592M of 46694M granted"},
		{"ram for the drain head", []string{ConfineBlockedRAM},
			", 2 of 4 queued, waiting for RAM: 46592M of 46694M booked"},
		{"exclusive", []string{ConfineBlockedExclusive},
			`, 2 of 4 queued, 46592M of 46694M granted, slice draining for exclusive job "bench" (s1)`},
		{"older daemon", nil,
			`, 2 of 4 queued, small-job lane full, 46592M of 46694M granted, ceiling cut to keep system free memory (MemAvailable 44544M), slice draining for exclusive job "bench" (s1)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reserve := diagReserve(2, test.blocked...)
			reserve.Exclusive = &ConfineExclusiveState{State: "draining", Name: "bench", Owner: "s1"}
			reserve.CPUPressureSomeAvg10 = nil
			if note := queueNoteFor(t, 0, reserve); note != test.want {
				t.Fatalf("note\n got %q\nwant %q", note, test.want)
			}
		})
	}
}

// verifies: AIRA-370 — figures the daemon did not establish are never
// fabricated: an unestablished ledger or an absent slot ceiling names the
// resource without numbers (no "0M", no "0 of 0").
func TestConfineWaitDiagnosisWithholdsUnestablishedFigures(t *testing.T) {
	reserve := diagReserve(1, ConfineBlockedRAM, ConfineBlockedCPU)
	reserve.GrantedEstablished = false
	reserve.CPUPressureSomeAvg10 = nil
	if note := queueNoteFor(t, 0, reserve); note != ", 1 of 4 queued, waiting for RAM; CPU slots (an admission count, not CPU load)" {
		t.Fatalf("note=%q", note)
	}
	reserve = diagReserve(1, ConfineBlockedCPU)
	reserve.CPUCeilingCores = 0
	reserve.CPUPressureSomeAvg10 = nil
	if note := queueNoteFor(t, 0, reserve); !strings.Contains(note, "waiting for CPU slots (an admission count, not CPU load)") {
		t.Fatalf("note=%q", note)
	}
}

// verifies: AIRA-370 skew — a daemon that sends no diagnosis (older), or only
// tokens this binary does not know (newer), gets today's clauses unchanged.
func TestConfineWaitDiagnosisAbsentKeepsTodaysLine(t *testing.T) {
	today := ", 2 of 4 queued, small-job lane full, 46592M of 46694M granted, ceiling cut to keep system free memory (MemAvailable 44544M)"
	for _, blocked := range [][]string{nil, {"some-future-gate"}} {
		reserve := diagReserve(2, blocked...)
		reserve.CPUPressureSomeAvg10 = nil
		if note := queueNoteFor(t, 0, reserve); note != today {
			t.Fatalf("blocked=%v note\n got %q\nwant %q", blocked, note, today)
		}
	}
	reserve := diagReserve(2)
	reserve.LaneWait = "no-room" // only an older daemon sends it
	reserve.CPUPressureSomeAvg10 = nil
	if note := queueNoteFor(t, 0, reserve); !strings.Contains(note, ", no room or CPU slot in the slice") {
		t.Fatalf("old-daemon no-room lost: %q", note)
	}
}

// verifies: AIRA-370 — the box CPU note appears only for an established
// pressure at or above ConfineBoxCPUBusyPercent: absent, 0 and 39.9 print
// nothing, 40 prints. Mutation: > instead of >= (the 40 row goes red).
func TestConfineWaitBoxCPUNoteThreshold(t *testing.T) {
	for _, test := range []struct {
		pressure *float64
		want     string
	}{
		{nil, ""},
		{diagPressure(0), ""},
		{diagPressure(39.9), ""},
		{diagPressure(40), ", box CPU busy: some task waited for a CPU 40% of the last 10 s"},
	} {
		reserve := diagReserve(2, ConfineBlockedRAM)
		reserve.CPUPressureSomeAvg10 = test.pressure
		note := queueNoteFor(t, 0, reserve)
		got := ""
		if index := strings.Index(note, ", box CPU busy"); index >= 0 {
			got = note[index:]
		}
		if got != test.want {
			t.Fatalf("pressure %v: clause %q, want %q (note %q)", test.pressure, got, test.want, note)
		}
	}
}

// verifies: AIRA-370 — the CPU pressure parser reads the `some` avg10 and
// refuses every malformed or out-of-range shape rather than returning 0.
func TestParseCPUPressureSome(t *testing.T) {
	for _, test := range []struct {
		data string
		want float64
		ok   bool
	}{
		{"some avg10=70.12 avg60=55.00 avg300=40.00 total=123456\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", 70.12, true},
		{"some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", 0, true},
		{"full avg10=1.00 avg60=0.00 avg300=0.00 total=1\n", 0, false},
		{"some avg10=100.01 avg60=0 avg300=0 total=1\n", 0, false},
		{"some avg10=-1 avg60=0 avg300=0 total=1\n", 0, false},
		{"some avg10=NaN avg60=0 avg300=0 total=1\n", 0, false},
		{"some avg10 avg60=0 total=1\n", 0, false},
		{"", 0, false},
	} {
		got, ok := parseCPUPressureSome([]byte(test.data))
		if ok != test.ok || got != test.want {
			t.Fatalf("parse(%q)=%v,%v want %v,%v", test.data, got, ok, test.want, test.ok)
		}
	}
}

// verifies: AIRA-370 honesty — the RAM fit charges the larger of the bookings
// and the slice's real use (less reclaimable), so when real use is the larger
// the clause states it rather than bookings that show room. Below the bookings
// (or unread) it is the booked form. Mutation: always print the booked form.
func TestConfineWaitRAMDiagnosisNamesRealUseAboveTheBookings(t *testing.T) {
	reserve := diagReserve(1, ConfineBlockedRAM)
	reserve.GrantedBytes = 20000 << 20
	reserve.SliceCurrentBytes, reserve.SliceReclaimableBytes = 46000<<20, 500<<20
	reserve.CPUPressureSomeAvg10 = nil
	want := ", 1 of 4 queued, waiting for RAM: 45500M in use of 46694M, above the 20000M booked"
	if note := queueNoteFor(t, 0, reserve); note != want {
		t.Fatalf("note\n got %q\nwant %q", note, want)
	}
	reserve.SliceCurrentBytes = 10000 << 20
	if note := queueNoteFor(t, 0, reserve); note != ", 1 of 4 queued, waiting for RAM: 20000M of 46694M booked" {
		t.Fatalf("note=%q", note)
	}
}
