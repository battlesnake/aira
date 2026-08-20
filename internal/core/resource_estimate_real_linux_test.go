//go:build linux

package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

func TestRealCgroupPeakRSSHistoryDrivesEstimatedAdmission(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "python3 is unavailable for peak-RSS estimate fixture")
	}
	parent := memoryEstimateParent(t, true)
	const headroom = int64(64 << 20)
	r, err := runner.New(runner.Config{
		CommonDir: t.TempDir(), CgroupParent: parent, MemorySlice: parent, MemoryReserve: headroom,
		Grace: time.Second, TermGrace: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real memory-enabled cgroup unavailable: %v", err)
	}
	c := NewWithRunner(nil, r).WithMemoryEstimate(true)
	args := map[string]any{"argv": []string{"python3", "-c", "x=bytearray(8*1024*1024); x[-1]=1"}}
	for i := 0; i < minSamples; i++ {
		record := runRealEstimateCommand(t, c, args)
		if record.PeakRSS == nil || *record.PeakRSS <= 0 || record.AdmissionReserve == nil || *record.AdmissionReserve != headroom || !strings.HasPrefix(record.AdmissionReserveBasis, "fallback:") {
			t.Fatalf("history run %d record=%+v", i+1, record)
		}
	}
	estimated := runRealEstimateCommand(t, c, args)
	if estimated.Admission != "immediate" || estimated.AdmissionReserve == nil || *estimated.AdmissionReserve <= 0 || *estimated.AdmissionReserve > maxEstimateReserve || !strings.HasPrefix(estimated.AdmissionReserveBasis, "estimate:") {
		t.Fatalf("estimated record=%+v", estimated)
	}
}

func TestRealCgroupWithoutMemoryDelegationFallsBackCaptureUnavailable(t *testing.T) {
	parent := memoryEstimateParent(t, false)
	const headroom = int64(8 << 20)
	r, err := runner.New(runner.Config{
		CommonDir: t.TempDir(), CgroupParent: parent, MemorySlice: parent, MemoryReserve: headroom,
		Grace: time.Second, TermGrace: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real non-memory-delegating cgroup unavailable: %v", err)
	}
	c := NewWithRunner(nil, r).WithMemoryEstimate(true)
	args := map[string]any{"argv": []string{"/bin/true"}}
	first := runRealEstimateCommand(t, c, args)
	if first.PeakRSS != nil || first.AdmissionReserveBasis != "fallback:no-history" {
		t.Fatalf("first non-delegated record=%+v", first)
	}
	second := runRealEstimateCommand(t, c, args)
	if second.PeakRSS != nil || second.AdmissionReserve == nil || *second.AdmissionReserve != headroom || second.AdmissionReserveBasis != "fallback:capture-unavailable" {
		t.Fatalf("second non-delegated record=%+v", second)
	}
}

func memoryEstimateParent(t *testing.T, delegateMemory bool) string {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "memory.max"), []byte("1073741824"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory.max is not writable: %v", err)
	}
	if !delegateMemory {
		return parent
	}
	controllers, err := os.ReadFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil || !containsField(string(controllers), "memory") {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller is unavailable under %s: %v", parent, err)
	}
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot delegate memory controller under %s: %v", parent, err)
	}
	return parent
}

func containsField(value, want string) bool {
	for _, field := range strings.Fields(value) {
		if field == want {
			return true
		}
	}
	return false
}

func runRealEstimateCommand(t *testing.T, c *Core, args map[string]any) runner.RunRecord {
	t.Helper()
	response := c.Do(context.Background(), Request{Verb: "run", Args: args})
	if !response.OK {
		t.Fatalf("run response=%+v", response)
	}
	data, ok := response.Data.(runResponseData)
	if !ok {
		t.Fatalf("run response data type=%T", response.Data)
	}
	return data.RunRecord
}
