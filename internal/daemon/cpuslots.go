//go:build linux

package daemon

import (
	"fmt"
	"os"
	"strconv"
)

// desiredCPUSlots is the daemon governor's active-set capacity. Reserve one
// CPU for interactive work by default, but never reduce capacity below one.
func desiredCPUSlots(cpuCount int) (int, error) {
	reserve := 1
	if raw, present := os.LookupEnv("AIRA_DAEMON_CPU_RESERVE"); present && raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_CPU_RESERVE must be a non-negative integer")
		}
		reserve = value
	}
	count := cpuCount - reserve
	if count < 1 {
		count = 1
	}
	return count, nil
}
