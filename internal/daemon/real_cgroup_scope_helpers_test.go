//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readChargeCgroupInt reads a single-integer cgroup file, reporting ok=false
// when the file cannot be read or does not parse.
func readChargeCgroupInt(dir, file string) (int64, bool) {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
