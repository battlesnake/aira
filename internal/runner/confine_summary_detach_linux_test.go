//go:build linux

package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// verifies: AIRA-281 -- a DETACHED job writes its summary line from the real
// supervisor when the job ends: the file (opened up front) is empty while the job
// runs and holds exactly one ran:true line with the job's exit afterwards, even
// though the launcher was group-killed in between.
func TestDetachedConfineWritesTheSummaryLineWhenTheJobEnds(t *testing.T) {
	parent := cgrouptestIsolatedParent(t)
	state := t.TempDir()
	summary := filepath.Join(t.TempDir(), "out.jsonl")
	launcher, handle := startDetachLauncher(t, []string{
		detachStateDirEnv + "=" + state,
		detachLaunchSliceEnv + "=" + parent,
		detachLaunchSummaryEnv + "=" + summary,
		detachLaunchArgvEnv + "=" + strings.Join([]string{"/bin/sh", "-c", "sleep 1.5; exit 5"}, "\x1f"),
	})
	if handle.Error != "" {
		t.Fatalf("launch failed: %s", handle.Error)
	}
	_ = syscall.Kill(-launcher.Process.Pid, syscall.SIGKILL)
	_ = launcher.Wait()
	// The job is still running: the up-front open created the file, and nothing has
	// been written to it.
	data, err := os.ReadFile(summary)
	if err != nil {
		t.Fatalf("the summary file was not opened up front: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("a line was written before the job ended: %q", data)
	}
	deadline := time.Now().Add(testdeadline.Wait(60 * time.Second))
	var record ConfineDetachRecord
	for time.Now().Before(deadline) {
		if records, listErr := ListConfineDetachRecords(state); listErr == nil && len(records) == 1 && records[0].Terminal {
			record = records[0]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !record.Terminal || record.Exit == nil || *record.Exit != 5 {
		t.Fatalf("job did not finish with exit 5: %+v", record)
	}
	lines := readSummaryLines(t, summary)
	if len(lines) != 1 {
		t.Fatalf("%d summary lines, want exactly 1", len(lines))
	}
	_, values := summaryParse(t, lines[0])
	if values["ran"] != true || values["exit"] != json.Number("5") || values["terminated_by"] != "normal" || values["containment"] != "enforced" {
		t.Fatalf("line = %v", values)
	}
	if wall := summaryNumber(t, values, "wall_us"); wall < 1_000_000 {
		t.Fatalf("wall_us = %d, want at least the job's 1.5s sleep", wall)
	}
}

// verifies: AIRA-281 -- a detached launch whose summary path cannot be opened (or
// is relative, which a supervisor must never resolve against its own cwd) is
// refused SYNCHRONOUSLY with E_CONFINE_ARGUMENT_INVALID through the ready pipe,
// and leaves no record of a job that ran. Opening after BeforeAdmit (which would
// report a handle and then fail) or resolving the relative path would red this.
func TestDetachedConfineRefusesABadSummaryPathSynchronously(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"unwritable directory", filepath.Join(t.TempDir(), "no", "such", "dir", "out.jsonl")},
		{"relative path", "relative-out.jsonl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := cgrouptestIsolatedParent(t)
			state := t.TempDir()
			evidence := filepath.Join(t.TempDir(), "the-job-ran")
			_, handle := startDetachLauncher(t, []string{
				detachStateDirEnv + "=" + state,
				detachLaunchSliceEnv + "=" + parent,
				detachLaunchSummaryEnv + "=" + tc.path,
				detachLaunchArgvEnv + "=" + strings.Join([]string{"/bin/sh", "-c", "touch " + evidence}, "\x1f"),
			})
			if !strings.Contains(handle.Error, "E_CONFINE_ARGUMENT_INVALID") || !strings.Contains(handle.Error, "--summary-file") {
				t.Fatalf("launch error = %q, want E_CONFINE_ARGUMENT_INVALID naming --summary-file", handle.Error)
			}
			if handle.ScopeID != "" || handle.SupervisorPID != 0 {
				t.Fatalf("a refused launch reported a handle: %+v", handle)
			}
			time.Sleep(300 * time.Millisecond)
			if _, err := os.Stat(evidence); err == nil {
				t.Fatal("the job ran despite the refused summary path")
			}
			records, err := ListConfineDetachRecords(state)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if !record.Terminal || record.Exit != nil || record.ErrorCode != "E_CONFINE_ARGUMENT_INVALID" {
					t.Fatalf("a refused launch left a record that looks like a job: %+v", record)
				}
			}
		})
	}
}
