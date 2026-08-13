//go:build linux

package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupUsage keeps OOM evidence private to the terminal classifier. The
// counter is nullable because an unreadable or unsupported memory.events file
// is unevaluated, not evidence of zero OOM kills.
type cgroupUsage struct {
	PeakRSS *int64
	CPUUser *int64
	CPUSys  *int64
	OOMKill *int64
}

func readCgroupUsage(scopePath string) cgroupUsage {
	var usage cgroupUsage
	if scopePath == "" {
		return usage
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.peak")); err == nil {
		usage.PeakRSS = parseMemoryPeak(data)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "cpu.stat")); err == nil {
		usage.CPUUser, usage.CPUSys = parseCPUStat(data)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.events")); err == nil {
		usage.OOMKill = parseMemoryEvents(data)
	}
	return usage
}

func parseMemoryPeak(data []byte) *int64 {
	fields := strings.Fields(string(data))
	if len(fields) != 1 {
		return nil
	}
	return parseNonNegativeInt(fields[0])
}

func parseCPUStat(data []byte) (user, system *int64) {
	values := parseCgroupKeyValues(data)
	return values["user_usec"], values["system_usec"]
}

func parseMemoryEvents(data []byte) *int64 {
	return parseCgroupKeyValues(data)["oom_kill"]
}

func parseCgroupKeyValues(data []byte) map[string]*int64 {
	values := make(map[string]*int64)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if value := parseNonNegativeInt(fields[1]); value != nil {
			values[fields[0]] = value
		}
	}
	return values
}

func parseNonNegativeInt(value string) *int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return nil
	}
	return &parsed
}

func classifyOOMKilled(status Status, usage cgroupUsage, explicitKill bool) Status {
	if usage.OOMKill != nil && *usage.OOMKill > 0 && !(status == StatusKilled && explicitKill) {
		return StatusOOMKilled
	}
	return status
}
