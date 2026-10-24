//go:build linux

package runner

import (
	"strings"
	"testing"
)

// verifies: AIRA-311 -- the admission-wait line names the job, stays short, and
// offers the push-to-front command only when the daemon holds the job queued and
// the scope id yields a supervisor PID to select it by.
func TestFormatConfineWaitLine(t *testing.T) {
	const queued = ", 21 of 21 queued, 44974M of 51648M granted"
	request := ConfineRequest{Name: "gate-x", ScopeID: "CONFINE-gate-x-4242-abc@session-a"}

	t.Run("named-and-queued", func(t *testing.T) {
		got := formatConfineWaitLine("waiting for memory admission", "aira.slice", request, 6144<<20, true, 15, "", queued, true)
		want := `confine: waiting for memory admission on aira.slice: "gate-x" reserve 6144M, waited 15s` + queued +
			". Push to front: aira confine --front 4242 --steal\n"
		if got != want {
			t.Fatalf("line = %q\nwant   %q", got, want)
		}
		if len(got) > 200 {
			t.Fatalf("line is %d bytes; the point was to be short: %q", len(got), got)
		}
	})
	t.Run("unnamed-job-keeps-the-default-name-and-selects-by-pid", func(t *testing.T) {
		// "job" is every unnamed launch's name, so it cannot select anything; the
		// PID in the command is what makes the push unambiguous.
		got := formatConfineWaitLine("waiting for memory admission", "aira.slice",
			ConfineRequest{ScopeID: "CONFINE-job-77-abc"}, 1<<30, false, 30, "", queued, true)
		for _, want := range []string{`"job"`, "requested ~1024M (unpinned; the daemon sizes the real grant)", "--front 77 --steal"} {
			if !strings.Contains(got, want) {
				t.Fatalf("line = %q, want %q", got, want)
			}
		}
	})
	t.Run("not-queued-prints-no-command", func(t *testing.T) {
		got := formatConfineWaitLine("waiting for memory admission", "aira.slice", request, 6144<<20, true, 15, "", "", false)
		if strings.Contains(got, "--front") {
			t.Fatalf("line = %q, a job the daemon does not hold queued cannot be pushed", got)
		}
	})
	t.Run("unparseable-scope-id-prints-no-command", func(t *testing.T) {
		got := formatConfineWaitLine("waiting for memory admission", "aira.slice",
			ConfineRequest{Name: "gate-x", ScopeID: "garbage"}, 6144<<20, true, 15, "", queued, true)
		if strings.Contains(got, "--front") || !strings.Contains(got, `"gate-x"`) {
			t.Fatalf("line = %q, want the name but no command built from a PID that was never established", got)
		}
	})
	t.Run("non-default-slice-names-the-slice-in-the-command", func(t *testing.T) {
		got := formatConfineWaitLine("waiting for memory admission", "custom.slice", request, 6144<<20, true, 15, "", queued, true)
		if !strings.Contains(got, "--front 4242 --steal --slice custom.slice") {
			t.Fatalf("line = %q, a push without --slice queries the default queue", got)
		}
		if def := formatConfineWaitLine("waiting for memory admission", DefaultConfineSlice, request, 6144<<20, true, 15, "", queued, true); strings.Contains(def, "--slice") {
			t.Fatalf("line = %q, the default slice needs no flag", def)
		}
	})
	t.Run("shim-verb-and-vram", func(t *testing.T) {
		got := formatConfineWaitLine("waiting for advisory memory admission", "ci.slice", request, 1<<30, true, 5, ", declared VRAM 2048M", "", false)
		if !strings.HasPrefix(got, "confine: waiting for advisory memory admission on ci.slice:") || !strings.Contains(got, "declared VRAM 2048M") {
			t.Fatalf("line = %q", got)
		}
	})
}
