package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aira/internal/runner"
)

// verifies: AIRA-315. The PEAK column wired to the real scope reader: two
// fake scope directories (one with memory.peak, one without) read by
// runner.ListConfines and rendered through the real `confine --list` face.
func TestConfineListPeakColumnWiresTheRealScopeReader(t *testing.T) {
	t.Parallel()
	slice := t.TempDir()
	stamp := time.Now().Add(-time.Second).UnixNano()
	writeScope := func(name string, pid int, peak string) {
		path := filepath.Join(slice, ".aira-CONFINE-"+name+"-"+strconv.Itoa(pid)+"-"+strconv.FormatInt(stamp, 36))
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"cgroup.procs": "", "cgroup.events": "populated 0\n", "memory.current": "4096\n", "memory.max": "8192\n", "cgroup.kill": "",
		}
		if peak != "" {
			files["memory.peak"] = peak
		}
		for file, data := range files {
			if err := os.WriteFile(filepath.Join(path, file), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeScope("peaked", 4301, "6144\n")
	writeScope("unpeaked", 4302, "")
	result, err := runner.ListConfines(context.Background(), slice, nil)
	if err != nil || len(result.Scopes) != 2 {
		t.Fatalf("ListConfines=%+v err=%v", result, err)
	}
	output := renderConfineList(t, result.Scopes)
	if got := confineListCell(t, output, "peaked", "PEAK"); got != "6144" {
		t.Fatalf("memory.peak 6144 rendered %q:\n%s", got, output)
	}
	if got := confineListCell(t, output, "unpeaked", "PEAK"); got != "unevaluated" {
		t.Fatalf("an absent memory.peak rendered %q, want unevaluated:\n%s", got, output)
	}
}
