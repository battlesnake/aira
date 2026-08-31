package pylib

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestReservationBytesSizingWithoutPytest guards the Slice-1 sizing formula
// (unmarked -> rss+128MiB, no 512MiB default floor; marked -> max(marker, rss+pad))
// WITHOUT requiring a real `pytest` executable, so the headline sizing change is
// still covered where CI has python3 but not pytest (requireRealPytest skips there).
// The plugin's only import-time pytest use is one @pytest.hookimpl decorator, so a
// pass-through stub in sys.modules is enough to import it and call _reservation_bytes
// directly against fake items with a stubbed _read_rss_bytes.
func TestReservationBytesSizingWithoutPytest(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 unavailable: %v", err)
	}
	const script = `
import sys, types, os
# The plugin imports pytest and applies one @pytest.hookimpl decorator at import
# time; a pass-through stub is sufficient to import it pytest-free.
fake = types.ModuleType("pytest")
fake.hookimpl = lambda **k: (lambda f: f)
sys.modules["pytest"] = fake
import aira_xdist_governor as g

PAD = 128 << 20

class Item:
    def __init__(self, marker):
        self._m = marker
    def get_closest_marker(self, name):
        return self._m if name == "aira_mem" else None

class Marker:
    def __init__(self, value):
        self.args = (value,)
        self.kwargs = {}

os.environ["AIRA_TEST_MEM_DEFAULT"] = "512M"

# Unmarked: rss + 128MiB pad — NOT the old 512MiB default floor, NOT rss+512MiB.
g._read_rss_bytes = lambda: 100 << 20
got = g._reservation_bytes(Item(None))
assert got == (100 << 20) + PAD, ("unmarked", got, (100 << 20) + PAD)

# Marked, marker dominates (rss+pad < marker): reserve the absolute peak.
got = g._reservation_bytes(Item(Marker("512M")))
assert got == 512 << 20, ("marked-marker-wins", got)

# Marked, measured dominates (rss+pad > marker).
g._read_rss_bytes = lambda: 500 << 20
got = g._reservation_bytes(Item(Marker("100M")))
assert got == (500 << 20) + PAD, ("marked-measured-wins", got)

# Malformed AIRA_TEST_MEM_DEFAULT -> ungoverned (None), unchanged.
g._read_rss_bytes = lambda: 100 << 20
os.environ["AIRA_TEST_MEM_DEFAULT"] = "not-a-size"
got = g._reservation_bytes(Item(None))
assert got is None, ("malformed-default", got)

print("OK")
`
	cmd := exec.Command(py, "-c", script)
	cmd.Dir = "." // internal/pylib: aira_xdist_governor/ is importable from here
	cmd.Env = append(os.Environ(), "PYTHONPATH=.")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pytest-free sizing check failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("sizing check produced unexpected output: %s", out)
	}
}
