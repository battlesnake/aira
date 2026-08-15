//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseMemoryPeakFixtureAndAbsentIsUnevaluated(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "cgroup", "memory.peak"))
	if err != nil {
		t.Fatal(err)
	}
	got := parseMemoryPeak(data)
	if got == nil || *got != 7340032 {
		t.Fatalf("memory.peak=%v, want 7340032", got)
	}
	if got := parseMemoryPeak(nil); got != nil {
		t.Fatalf("absent memory.peak fabricated %v", *got)
	}
}

func TestParseCPUStatFixtureAndMissingKeyIsIndependent(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "cgroup", "cpu.stat"))
	if err != nil {
		t.Fatal(err)
	}
	user, sys := parseCPUStat(data)
	if user == nil || *user != 1200 || sys == nil || *sys != 340 {
		t.Fatalf("cpu.stat user=%v sys=%v", user, sys)
	}
	user, sys = parseCPUStat([]byte("system_usec 9\n"))
	if user != nil || sys == nil || *sys != 9 {
		t.Fatalf("missing user_usec fabricated user=%v sys=%v", user, sys)
	}
}

func TestParseMemoryEventsFixturePositiveOnly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "cgroup", "memory.events"))
	if err != nil {
		t.Fatal(err)
	}
	got := parseMemoryEvents(data)
	if got == nil || *got != 2 {
		t.Fatalf("oom_kill=%v, want 2", got)
	}
	if got := parseMemoryEvents([]byte("oom_kill 0\n")); got == nil || *got != 0 {
		t.Fatalf("zero oom_kill=%v, want a positively read zero", got)
	}
	if got := parseMemoryEvents([]byte("memory 1\n")); got != nil {
		t.Fatalf("missing oom_kill fabricated %v", *got)
	}
}

func TestReadCgroupUsageIsolatesOneFileFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.peak"), []byte("99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("user_usec 7\nsystem_usec 8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "memory.events"), 0o755); err != nil {
		t.Fatal(err)
	}
	usage := readCgroupUsage(dir)
	if usage.PeakRSS == nil || *usage.PeakRSS != 99 || usage.CPUUser == nil || *usage.CPUUser != 7 || usage.CPUSys == nil || *usage.CPUSys != 8 {
		t.Fatalf("unrelated usage was lost: %+v", usage)
	}
	if usage.OOMKill != nil {
		t.Fatalf("read failure fabricated oom evidence: %v", *usage.OOMKill)
	}
}

func TestOOMClassificationPrecedence(t *testing.T) {
	positive := cgroupUsage{OOMKill: int64Ptr(1)}
	if got := classifyOOMKilled(StatusExited, positive, false); got != StatusOOMKilled {
		t.Fatalf("positive OOM status=%q", got)
	}
	if got := classifyOOMKilled(StatusKilled, positive, true); got != StatusKilled {
		t.Fatalf("explicit kill was rewritten as %q", got)
	}
	if got := classifyOOMKilled(StatusExited, cgroupUsage{OOMKill: int64Ptr(0)}, false); got != StatusExited {
		t.Fatalf("zero OOM status=%q", got)
	}
	if got := classifyOOMKilled(StatusExited, cgroupUsage{}, false); got != StatusExited {
		t.Fatalf("unread OOM status=%q", got)
	}
}

func TestMemoryEventsReadFailureDoesNotFabricateOOMKilled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.peak"), []byte("11"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("user_usec 1\nsystem_usec 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "memory.events"), 0o755); err != nil {
		t.Fatal(err)
	}
	usage := readCgroupUsage(dir)
	if got := classifyOOMKilled(StatusKilled, usage, true); got != StatusKilled {
		t.Fatalf("signal/kill base status changed to %q", got)
	}
	if usage.OOMKill != nil {
		t.Fatalf("unread memory.events became positive OOM evidence: %v", *usage.OOMKill)
	}
}

func TestKillLostPathRetainsUsageBeforeScopeBecomesUnavailable(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"memory.peak":   "111",
		"cpu.stat":      "user_usec 4\nsystem_usec 5\n",
		"memory.events": "oom_kill 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &usageFixtureBackend{scope: &usageFixtureScope{path: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, CgroupScope: dir}
	appendRunEvent(t, r, "starting", run)
	_, err = r.Kill(context.Background(), "RUN-1", false)
	if err == nil {
		t.Fatal("incomplete kill was reported as successful")
	}
	current, err := r.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusLost || current.PeakRSS == nil || *current.PeakRSS != 111 || current.CPUUser == nil || *current.CPUUser != 4 || current.CPUSys == nil || *current.CPUSys != 5 {
		t.Fatalf("kill->lost usage=%+v", current)
	}
}

type usageFixtureScope struct {
	path string
}

func (s *usageFixtureScope) Reference() string       { return s.path }
func (s *usageFixtureScope) FD() int                 { return -1 }
func (s *usageFixtureScope) Members() ([]int, error) { return nil, nil }
func (s *usageFixtureScope) Empty() (bool, error)    { return true, nil }
func (s *usageFixtureScope) Terminate([]int) error   { return nil }
func (s *usageFixtureScope) Kill() error             { return nil }
func (s *usageFixtureScope) Remove() error           { return nil }

type usageFixtureBackend struct{ scope *usageFixtureScope }

func (b *usageFixtureBackend) Probe(context.Context) error                   { return nil }
func (b *usageFixtureBackend) Create(context.Context, string) (Scope, error) { return b.scope, nil }
func (b *usageFixtureBackend) Open(context.Context, string) (Scope, error)   { return b.scope, nil }

func int64Ptr(value int64) *int64 { return &value }
