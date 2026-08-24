package install

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestComputeMemoryLimitsPrecedenceAndValidation(t *testing.T) {
	tests := []struct {
		name, maximum, high, installed string
		memKB                          int64
		wantMax, wantHigh              string
		wantErr                        bool
	}{
		{name: "flag wins", maximum: "48G", installed: "MemoryMax=40G\n", memKB: 96 * gibPerKiB, wantMax: "48G", wantHigh: "46G"},
		{name: "installed wins", installed: "MemoryMax=40G\n", memKB: 96 * gibPerKiB, wantMax: "40G", wantHigh: "38G"},
		{name: "two thirds RAM", memKB: 96 * gibPerKiB, wantMax: "64G", wantHigh: "62G"},
		{name: "high override", maximum: "48G", high: "32G", wantMax: "48G", wantHigh: "32G"},
		{name: "bad maximum syntax", maximum: "4GiB", wantErr: true},
		{name: "maximum below floor", maximum: "3G", wantErr: true},
		{name: "bad high syntax", maximum: "8G", high: "7g", wantErr: true},
		{name: "high above maximum", maximum: "8G", high: "9G", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			maximum, high, err := computeMemoryLimits(test.maximum, test.high, test.installed, test.memKB)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v, wantErr=%v", err, test.wantErr)
			}
			if maximum != test.wantMax || high != test.wantHigh {
				t.Fatalf("limits=(%q,%q), want (%q,%q)", maximum, high, test.wantMax, test.wantHigh)
			}
		})
	}
}

func TestRenderUnitsSubstitutesAndMarksFirstLine(t *testing.T) {
	slice, anchor, err := renderUnits("test.slice", "test-anchor.service", "/opt/aira", "16G", "14G", true)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"test.slice": slice, "test-anchor.service": anchor} {
		if !strings.HasPrefix(content, "# aira-managed: "+name+"\n") {
			t.Fatalf("%s marker missing from first line: %q", name, content)
		}
		if strings.Contains(content, "@MEM") || strings.Contains(content, "@AIRABIN@") {
			t.Fatalf("%s contains an unsubstituted placeholder: %q", name, content)
		}
	}
	for _, want := range []string{"MemoryMax=16G", "MemoryHigh=14G", "MemoryAccounting=yes", "# aira-overcommit-accepted: yes"} {
		if !strings.Contains(slice, want) {
			t.Fatalf("slice lacks %q: %q", want, slice)
		}
	}
	for _, want := range []string{"Slice=test.slice", "MemoryAccounting=yes", "ExecStart=/opt/aira __slice-anchor", "Restart=always"} {
		if !strings.Contains(anchor, want) {
			t.Fatalf("anchor lacks %q: %q", want, anchor)
		}
	}
}

func TestInstallDryRunWritesNothingAndRunsNoSystemctl(t *testing.T) {
	d, state := newFakeInstall(t)
	d.run = func([]string, []byte) ([]byte, error) { t.Fatal("dry-run invoked a command"); return nil, nil }
	var output bytes.Buffer
	d.logf = func(format string, args ...any) { fmt.Fprintf(&output, format+"\n", args...) }
	err := runInstall(d, installOpts{memoryMax: "16G", dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if state.writes != 0 {
		t.Fatalf("dry-run writes=%d", state.writes)
	}
	if _, err := os.Stat(filepath.Join(state.home, ".config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created config tree: %v", err)
	}
	if got := output.String(); !strings.Contains(got, "# aira-managed: aira.slice") || !strings.Contains(got, "planned: systemctl --user daemon-reload") {
		t.Fatalf("dry-run output=%q", got)
	}
}

func TestInstallReloadFailureConvergesOnNextRun(t *testing.T) {
	d, state := newFakeInstall(t)
	state.failReloads = 1
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
		t.Fatalf("first install error=%v", err)
	}
	slicePath := filepath.Join(state.unitDir(), "aira.slice")
	first, err := os.ReadFile(slicePath)
	if err != nil || !strings.Contains(string(first), "MemoryMax=16G") {
		t.Fatalf("first run did not publish unit: %v %q", err, first)
	}
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if state.reloads != 2 {
		t.Fatalf("daemon-reloads=%d, want 2", state.reloads)
	}
	if state.liveMax != 16<<30 {
		t.Fatalf("live memory.max=%d, want %d", state.liveMax, int64(16<<30))
	}
}

func TestInstallIdempotentAndChangedCap(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	writes := state.writes
	state.logs = nil
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if state.writes != writes {
		t.Fatalf("idempotent run rewrote files: before=%d after=%d", writes, state.writes)
	}
	if !strings.Contains(strings.Join(state.logs, "\n"), "up to date") {
		t.Fatalf("idempotent logs=%q", state.logs)
	}
	if err := runInstall(d, installOpts{memoryMax: "20G"}); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(filepath.Join(state.unitDir(), "aira.slice"))
	if !strings.Contains(string(content), "MemoryMax=20G") || state.liveMax != 20<<30 {
		t.Fatalf("changed cap did not converge: live=%d unit=%q", state.liveMax, content)
	}
	for _, argv := range state.commands {
		if strings.Contains(strings.Join(argv, " "), "set-property") {
			t.Fatalf("install invoked set-property: %q", argv)
		}
	}
}

func TestInstallRefusesForeignAndUnsafeTargets(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "markerless", setup: func(t *testing.T, path string) { mustWrite(t, path, "[Slice]\nMemoryMax=8G\n") }},
		{name: "symlink", setup: func(t *testing.T, path string) {
			target := filepath.Join(filepath.Dir(path), "foreign")
			mustWrite(t, target, "foreign")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "non-regular", setup: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, state := newFakeInstall(t)
			if err := os.MkdirAll(state.unitDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(state.unitDir(), "aira.slice")
			test.setup(t, path)
			before, _ := os.ReadFile(path)
			err := runInstall(d, installOpts{memoryMax: "16G"})
			if err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
				t.Fatalf("error=%v", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatalf("unsafe target changed: before=%q after=%q", before, after)
			}
			if state.reloads != 0 {
				t.Fatalf("unsafe target caused reload")
			}
		})
	}
}

func TestInstallRefusesForeignAnchorBeforePublishingSlice(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := os.MkdirAll(state.unitDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(state.unitDir(), "aira-slice-keepalive.service"), "[Service]\nExecStart=/foreign\n")
	err := runInstall(d, installOpts{memoryMax: "16G"})
	if err == nil || !strings.Contains(err.Error(), "marker-less") {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(state.unitDir(), "aira.slice")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("slice was published before foreign anchor refusal: %v", statErr)
	}
}

func TestAtomicPublishUnlinksTemporaryFileOnFailure(t *testing.T) {
	d, state := newFakeInstall(t)
	d.writeFD = func(int, []byte) error { return errors.New("disk full") }
	err := runInstall(d, installOpts{memoryMax: "16G"})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error=%v", err)
	}
	entries, readErr := os.ReadDir(state.unitDir())
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary unit leaked: %s", entry.Name())
		}
	}
}

func TestInstallOvercommitGateWritesNothingAndRecordsAcceptance(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := os.MkdirAll(state.unitDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(state.unitDir(), "whale.slice"), "[Slice]\nMemoryMax=64G\n")
	state.writes = 0
	err := runInstall(d, installOpts{memoryMax: "16G"})
	if err == nil || !strings.Contains(err.Error(), CodeOvercommit) {
		t.Fatalf("error=%v", err)
	}
	if state.writes != 0 || state.reloads != 0 {
		t.Fatalf("refused install mutated: writes=%d reloads=%d", state.writes, state.reloads)
	}
	if err := runInstall(d, installOpts{memoryMax: "16G", allowOvercommit: true}); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(filepath.Join(state.unitDir(), "aira.slice"))
	if !strings.Contains(string(content), "# aira-overcommit-accepted: yes") {
		t.Fatalf("acceptance not recorded: %q", content)
	}
	// A subsequent idempotent run may rely on the recorded acknowledgement.
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatalf("recorded acceptance not honoured: %v", err)
	}
}

func TestStatusReportsUnrecordedCappedCoexistenceHonestly(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(state.unitDir(), "whale.slice"), "[Slice]\nMemoryMax=64G\n")
	// Simulate reverse-order coexistence: the AIRA unit predates whale and has no opt-in marker.
	path := filepath.Join(state.unitDir(), "aira.slice")
	content, _ := os.ReadFile(path)
	content = bytes.ReplaceAll(content, []byte("# aira-overcommit-accepted: yes\n"), nil)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(state.logs, "\n")
	if !strings.Contains(got, "coexisting capped slices present") || !strings.Contains(got, "overcommit opt-in not recorded") {
		t.Fatalf("status=%q", got)
	}
}

func TestEnableMemoryDelegation(t *testing.T) {
	files := map[string][]byte{
		"/cg/cgroup.controllers":     []byte("memory pids\n"),
		"/cg/cgroup.subtree_control": nil,
	}
	writes := map[string][]byte{}
	d := installDeps{
		readFile: func(path string) ([]byte, error) {
			value, ok := files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			if value == nil && path == "/cg/cgroup.subtree_control" && len(writes[path]) > 0 {
				return []byte("memory\n"), nil
			}
			return value, nil
		},
		writeFile: func(path string, data []byte, _ os.FileMode) error {
			writes[path] = append([]byte(nil), data...)
			return nil
		},
	}
	if err := enableMemoryDelegation(d, "/cg"); err != nil {
		t.Fatal(err)
	}
	if string(writes["/cg/cgroup.subtree_control"]) != "+memory\n" {
		t.Fatalf("delegation write=%q", writes["/cg/cgroup.subtree_control"])
	}
	files["/cg/cgroup.controllers"] = []byte("pids\n")
	if err := enableMemoryDelegation(d, "/cg"); err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("missing controller error=%v", err)
	}
}

type fakeInstallState struct {
	home        string
	uid         int
	writes      int
	reloads     int
	failReloads int
	liveMax     int64
	liveHigh    int64
	logs        []string
	commands    [][]string
	cgroup      map[string][]byte
}

func (s *fakeInstallState) unitDir() string {
	return filepath.Join(s.home, ".config", "systemd", "user")
}

func newFakeInstall(t *testing.T) (installDeps, *fakeInstallState) {
	t.Helper()
	state := &fakeInstallState{home: t.TempDir(), uid: os.Geteuid(), cgroup: map[string][]byte{}}
	userControllers := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/cgroup.controllers", state.uid, state.uid)
	state.cgroup[userControllers] = []byte("memory pids\n")
	state.cgroup["/sys/fs/cgroup/fake/aira.slice/cgroup.controllers"] = []byte("memory pids\n")
	state.cgroup["/sys/fs/cgroup/fake/aira.slice/cgroup.subtree_control"] = []byte("memory\n")
	state.cgroup["/sys/fs/cgroup/fake/anchor/memory.max"] = []byte("max\n")
	d := realInstallDeps()
	realWriteFD := d.writeFD
	d.writeFD = func(fd int, data []byte) error { state.writes++; return realWriteFD(fd, data) }
	d.getenv = func(name string) string {
		if name == "HOME" {
			return state.home
		}
		return ""
	}
	d.geteuid = func() int { return state.uid }
	d.executable = func() (string, error) { return "/opt/aira", nil }
	realWriteFile := d.writeFile
	d.writeFile = func(path string, data []byte, mode os.FileMode) error {
		state.writes++
		if _, ok := state.cgroup[path]; ok || strings.HasPrefix(path, "/sys/fs/cgroup/") {
			state.cgroup[path] = append([]byte(nil), data...)
			if strings.HasSuffix(path, "cgroup.subtree_control") && strings.Contains(string(data), "+memory") {
				state.cgroup[path] = []byte("memory\n")
			}
			return nil
		}
		return realWriteFile(path, data, mode)
	}
	realReadFile := d.readFile
	d.readFile = func(path string) ([]byte, error) {
		if value, ok := state.cgroup[path]; ok {
			return append([]byte(nil), value...), nil
		}
		return realReadFile(path)
	}
	d.run = func(argv []string, _ []byte) ([]byte, error) {
		state.commands = append(state.commands, append([]string(nil), argv...))
		joined := strings.Join(argv, " ")
		switch {
		case joined == "systemctl --user daemon-reload":
			state.reloads++
			if state.failReloads > 0 {
				state.failReloads--
				return nil, errors.New("reload failed")
			}
			content, err := os.ReadFile(filepath.Join(state.unitDir(), "aira.slice"))
			if err == nil {
				maximum, _ := parseInstalledMemoryMax(string(content))
				high, _ := parseInstalledMemoryHigh(string(content))
				state.liveMax, _ = sizeBytes(maximum)
				state.liveHigh, _ = sizeBytes(high)
				state.cgroup["/sys/fs/cgroup/fake/aira.slice/memory.max"] = []byte(fmt.Sprintf("%d\n", state.liveMax))
				state.cgroup["/sys/fs/cgroup/fake/aira.slice/memory.high"] = []byte(fmt.Sprintf("%d\n", state.liveHigh))
			}
			return nil, nil
		case strings.HasPrefix(joined, "systemctl --user enable --now "):
			return nil, nil
		case strings.HasPrefix(joined, "systemctl --user is-active "):
			return []byte("active\n"), nil
		case joined == "systemctl --user show -p ControlGroup --value aira.slice":
			return []byte("/fake/aira.slice\n"), nil
		case joined == "systemctl --user show -p ControlGroup --value aira-slice-keepalive.service":
			return []byte("/fake/anchor\n"), nil
		case joined == "systemctl --user show -p MemoryMax --value aira.slice":
			return []byte(fmt.Sprintf("%d\n", state.liveMax)), nil
		case joined == "systemctl --user show -p MemoryHigh --value aira.slice":
			return []byte(fmt.Sprintf("%d\n", state.liveHigh)), nil
		default:
			return nil, fmt.Errorf("unexpected command: %v", argv)
		}
	}
	d.logf = func(format string, args ...any) { state.logs = append(state.logs, fmt.Sprintf(format, args...)) }
	return d, state
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRealInstallDepsComplete(t *testing.T) {
	v := reflect.ValueOf(realInstallDeps())
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.Func && v.Field(i).IsNil() {
			t.Fatalf("installDeps.%s is nil", v.Type().Field(i).Name)
		}
	}
}
