//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// realConfineScope creates <parent>/.aira-CONFINE-<name>-<pid>-<stamp>, shaped
// so the production parseConfineScopeID accepts it and the production scan
// therefore returns a record for it. The stamp must be canonical lowercase
// base 36 or the scanner omits the scope entirely and the caller would silently
// prove nothing.
//
// Shared by the real-cgroup anti-INERT tests (AIRA-114 oversubscription and the
// oom-steerer). Lives in its own helper file so those tests do not depend on any
// one feature's test file being present.
func realConfineScope(t *testing.T, parent, name string) (scopePath, scopeID string) {
	t.Helper()
	stamp := strconv.FormatInt(time.Now().UnixNano()%(1<<40), 36)
	scopeID = "CONFINE-" + name + "-" + strconv.Itoa(os.Getpid()) + "-" + stamp
	scopePath = filepath.Join(parent, ".aira-"+scopeID)
	if err := os.Mkdir(scopePath, 0o755); err != nil {
		t.Fatal(err)
	}
	return scopePath, scopeID
}

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
