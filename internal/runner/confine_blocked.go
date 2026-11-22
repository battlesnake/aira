package runner

import (
	"math"
	"strconv"
	"strings"
)

// AIRA-370. The admission wait diagnosis vocabulary: which gate held a queued
// job in the daemon's last admission pass. The daemon writes the tokens
// (ConfineRecord.BlockedBy, ConfineSliceReserve.BlockedBy) in this precedence
// order, and at most one of the first three ever appears, alone:
//
//	restart-freeze  the restarted daemon admits nothing new until survivors re-declare
//	exclusive       an exclusive job holds, or is draining, the slice
//	head            the job fits, but the head of the queue holds it back
//
// Otherwise one or more of the resources that did not fit, in this order:
//
//	ram             the RAM booking ledger (or the slice's real free memory)
//	ceiling-cut     qualifies ram: the RAM ceiling is cut below memory.max this pass
//	cpu             the CPU SLOT ledger: declared cores against slots-per-core x
//	                cores. A booking count, NOT machine CPU load.
//	vram            the GPU memory ledger or card
//
// A renderer names only tokens it knows: an unknown one names nothing.
const (
	ConfineBlockedRestartFreeze = "restart-freeze"
	ConfineBlockedExclusive     = "exclusive"
	ConfineBlockedHead          = "head"
	ConfineBlockedRAM           = "ram"
	ConfineBlockedCeilingCut    = "ceiling-cut"
	ConfineBlockedCPU           = "cpu"
	ConfineBlockedVRAM          = "vram"
)

// ConfineBoxCPUBusyPercent is the CPU pressure (PSI `some avg10`) at and above
// which the wait line and `aira top` say the machine's CPUs are busy. Context
// only: it is never an admission input.
const ConfineBoxCPUBusyPercent = 40

// ConfineBoxCPUBusy is the established, high CPU pressure, or false.
func ConfineBoxCPUBusy(reserve *ConfineSliceReserve) (float64, bool) {
	if reserve == nil || reserve.CPUPressureSomeAvg10 == nil {
		return 0, false
	}
	value := *reserve.CPUPressureSomeAvg10
	return value, value >= ConfineBoxCPUBusyPercent
}

// ParsePressureAvg10 reads `avg10` and `total` from the named line ("some" or
// "full") of a /proc/pressure/* file. Every malformed shape is a refusal with a
// reason, never a zero.
func ParsePressureAvg10(data []byte, kind string) (float64, uint64, bool, string) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != kind {
			continue
		}
		values := make(map[string]string, len(fields)-1)
		for _, field := range fields[1:] {
			key, value, found := strings.Cut(field, "=")
			if !found || key == "" || value == "" {
				return 0, 0, false, "parse-error"
			}
			values[key] = value
		}
		avg, avgErr := strconv.ParseFloat(values["avg10"], 64)
		total, totalErr := strconv.ParseUint(values["total"], 10, 64)
		if avgErr != nil || totalErr != nil || avg < 0 || math.IsNaN(avg) || math.IsInf(avg, 0) {
			return 0, 0, false, "parse-error"
		}
		return avg, total, true, ""
	}
	return 0, 0, false, kind + "-line-absent"
}

// parseCPUPressureSome is the CPU frame's reading: the `some` line's avg10, a
// percentage, so anything above 100 is refused as unparseable.
func parseCPUPressureSome(data []byte) (float64, bool) {
	avg, _, ok, _ := ParsePressureAvg10(data, "some")
	if !ok || avg > 100 {
		return 0, false
	}
	return avg, true
}
