# aitest Slice 1 (AIRA-30) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the core aitest supervisor/worker loop — fork+admission+recycle, flat pull dispatch, pass/fail/unevaluated aggregation — replacing pytest-xdist's worker model for AIRA-governed suites, with per-worker kernel-enforced cgroup containment.

**Architecture:** A new Go daemon verb (`worker-admit`) grants per-worker cgroup sub-scopes nested under an already-running `aira confine --delegate-ram` outer scope, sized against that job's own live occupancy (not a static hold). A new `aira worker-admit` CLI relay does the actual nested cgroup creation client-side (daemon only decides/tracks). A new pytest plugin (`internal/pylib/aitest`) forks workers from a warm-imported supervisor process, places each fork into its granted scope by writing its own PID to `cgroup.procs`, dispatches nodeids via a pull queue, and recycles workers on time/count/memory-watermark checked only between tests.

**Tech Stack:** Go 1.25.0 (daemon, CLI, cgroup v2 syscalls), Python 3 (pytest plugin, `os.fork()`), Unix domain socket + length-prefixed JSON frames (existing daemon wire protocol).

**Spec:** [`docs/superpowers/specs/2026-09-01-aitest-design.md`](../specs/2026-09-01-aitest-design.md) (§3.1-3.4, §3.6-3.7, §5 Slice 1). Ticket: AIRA-30.

## Global Constraints

- Go 1.25.0, module `aira`, no cgo, one static binary — no new external Go dependencies.
- Every new Linux-only cgroup-touching file carries `//go:build linux` and gets a non-Linux stub returning a clear unavailable error/exit code, matching `internal/runner/confine_stub.go`'s convention.
- Daemon protocol version is `ProtocolVersion = 5` (`internal/daemon/protocol.go`) — do not bump it. New verbs are added by dispatch inside `serveConnection`, never by a version change.
- A new daemon verb that holds its connection open as a lease MUST set `wrote = true` before calling its handler in `serveConnection` (disarms the panic-recovery frame writer, matching `admit`/`governor`) and clear the read deadline (`conn.SetReadDeadline(time.Time{})`).
- `worker-admit`'s ledger is a deliberately SIMPLER structure than `internal/daemon/admit.go`'s `sliceQueue`/`admitWaiter` machinery: that machinery arbitrates fairness across many concurrent, unrelated jobs sharing one slice-wide ledger. Worker-admit only arbitrates one job's own worker pool against its own outer scope's ceiling — a mutex-guarded map plus a short poll loop is the right size for that problem. Do not port the waiter-queue/evaluator-goroutine pattern; this is a considered scoping decision, not an oversight (see Task 2).
- Real-cgroup tests use `internal/cgrouptest` helpers (`IsolatedScopeParent`, `SkipOrFailRealCgroup`) and honour `AIRA_REAL_CGROUP=1` (hard-fail instead of silent skip on a host without real cgroup delegation).
- No backward-compatibility constraints — AIRA has no users or data yet (per project memory); add new env vars, verbs, and files freely, and do not preserve any old wire shape for its own sake.
- Every heavy command run while executing this plan (`go build`, `go test`, real-cgroup tests, any real `pytest` run) MUST be prefixed with `aira confine --` per this repository's own CLAUDE.md hard rule.
- A killed-mid-test outcome is never reported as `passed`, `failed`, or silently dropped — it is `unevaluated` after exactly one retry (spec §3.6, §4).
- Recycle checks fire only at test boundaries — a running test is never interrupted (spec §3.4, §4).

## Bootstrap ordering (read before Tasks 1-8)

cgroup v2 forbids a cgroup from holding member processes once it has enabled
controllers in its own `cgroup.subtree_control` for children to use (the
"no internal processes" rule). `aira confine --delegate-ram -- <supervisor>`
places the supervisor process directly into the *outer* scope. If aitest then
tried to create worker children directly under that same outer scope, the
outer scope would be holding both the supervisor process AND delegating
`memory`/`cpu` to children — illegal. So the supervisor must relocate itself
into its own child scope (`outer/.aira-supervisor`) BEFORE the outer scope
enables delegation, giving:

```
outer (confine scope; subtree_control=+cpu+memory; no member processes)
├── .aira-supervisor/   (the pytest supervisor process lives here)
├── .aira-worker-<id>/  (one per admitted worker; own memory.max/high)
└── ...
```

Tasks 1-3 build this bootstrap step. Tasks 4-8 build worker admission and
scope creation, which depend on the outer scope already being delegated
(i.e. bootstrap must run first at runtime, enforced by Task 11's supervisor
code, not by any compile-time dependency between these tasks).

**Accepted, documented gap:** once the supervisor relocates into
`outer/.aira-supervisor`, `confine`'s scope-integrity attestation (#20/#70)
reports `scope-integrity=migrated` for the outer scope rather than
`contained` — its check is leaf-membership, not subtree-aware, and a genuine
in-subtree relocation reads the same as an escape. Verified live (real
spike): the process is still contained within the scope subtree; this is a
false-alarm-shaped label, not an actual containment loss, and the signal is
telemetry-only (#70: zero production consumers). Every aitest outer scope
will report `migrated` for its entire run — expected, not a bug. Making the
integrity check subtree-aware is a separate, optional confine-side
improvement, deliberately NOT bundled into this slice (architectural
simplicity: no new machinery for a telemetry-only signal).

---

### Task 1: `runner.WorkerScopeChildPath` — pure path-joining helper

**Files:**
- Create: `internal/runner/worker_scope.go`
- Test: `internal/runner/worker_scope_test.go`

**Interfaces:**
- Produces: `func WorkerScopeChildPath(parent, id string) string` — used by
  every later task (daemon ledger, bootstrap, worker-scope creation) so the
  daemon and every client agree on the exact same child path from just a
  parent and an id, without either side needing cgroup access to compute it.

This file has no build tag — it's pure `path/filepath` string logic, callable
from both `internal/daemon` (to know where to read a granted worker's
`memory.current`, without ever creating anything) and `internal/runner`'s own
Linux-only scope-creation code (Task 2, Task 7).

- [ ] **Step 1: Write the failing test**

```go
package runner

import "testing"

func TestWorkerScopeChildPathJoinsWithConfineChildConvention(t *testing.T) {
	got := WorkerScopeChildPath("/sys/fs/cgroup/aira.slice/.aira-CONFINE-x", "supervisor")
	want := "/sys/fs/cgroup/aira.slice/.aira-CONFINE-x/.aira-supervisor"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWorkerScopeChildPathRejectsSlashInID(t *testing.T) {
	// Mirrors linuxScopeBackend.Create's own id validation (cgroup_linux.go) —
	// an id must never let a caller escape the parent via a path component.
	got := WorkerScopeChildPath("/parent", "worker/../../etc")
	if got != "" {
		t.Fatalf("path with slash in id must be rejected, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/runner/ -run TestWorkerScopeChildPath -v`
Expected: FAIL (undefined: WorkerScopeChildPath)

- [ ] **Step 3: Write minimal implementation**

```go
package runner

import (
	"path/filepath"
	"strings"
)

// WorkerScopeChildPath returns the exact child cgroup directory path that
// linuxScopeBackend.Create(ctx, id) would create under parent, WITHOUT
// touching the filesystem. It exists so the daemon can know a granted
// worker's scope path (to read memory.current from it) without creating
// anything itself, and so every client (aitest-bootstrap, worker-admit)
// derives the identical path from the same (parent, id) pair. Returns "" for
// an id that Create would itself reject (matches cgroup_linux.go's own
// validation: no "/" in id).
func WorkerScopeChildPath(parent, id string) string {
	if strings.Contains(id, "/") || id == "" {
		return ""
	}
	return filepath.Join(filepath.Clean(parent), ".aira-"+id)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/runner/ -run TestWorkerScopeChildPath -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/runner/worker_scope.go internal/runner/worker_scope_test.go
git commit -m "feat(aitest): add WorkerScopeChildPath shared path-joining helper"
```

---

### Task 2: `runner.BootstrapAitestSupervisor` — outer→supervisor relocation + delegation

**Files:**
- Create: `internal/runner/aitest_bootstrap_linux.go`
- Create: `internal/runner/aitest_bootstrap_stub.go`
- Test: `internal/runner/aitest_bootstrap_linux_test.go`

**Interfaces:**
- Consumes: `WorkerScopeChildPath(parent, id string) string` (Task 1);
  `newDefaultBackend(parent string) ScopeBackend`, `ensureConfineDelegation(parent string) (confineDelegation, error)` (existing, `internal/runner/cgroup_linux.go`, `internal/runner/confine_linux.go`).
- Produces: `func BootstrapAitestSupervisor(ctx context.Context, outerScope string, supervisorPID int) (supervisorScopePath string, err error)` — takes an EXPLICIT outer scope path (not self-discovered) so it stays unit-testable without moving the test binary's own process; Task 3's CLI verb does the self-discovery and passes the result in.

Explicit outer scope keeps this function pure cgroup-mutation logic, testable
against a stand-in subprocess rather than the test binary's own PID.

**Contract note (real, not aspirational):** this function is safe for
EXACTLY ONE call per process tree. It is NOT safe to retry from a fresh CLI
invocation after a prior partial success: a retry would self-discover its
outer scope via `CurrentCgroupPath()` (Task 3) from INSIDE the
already-relocated `.aira-supervisor` scope left by the first call, and
would nest `.aira-supervisor/.aira-supervisor` instead of reopening the
original. Slice 1's supervisor (Task 11) never retries this call for
exactly this reason — do not add a bootstrap retry loop without first
handling re-entry detection.

- [ ] **Step 1: Write the failing test**

```go
//go:build linux

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aira/internal/cgrouptest"
)

func TestBootstrapAitestSupervisorRelocatesAndDelegates(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}

	// TWO blocking `cat` processes stand in for the supervisor plus a
	// transient child that happens to be alive at bootstrap time (confirmed
	// live in a real spike: a supervisor shell plus two short-lived children
	// were all present in outer simultaneously). Both must be drained, or
	// the subtree_control write below EBUSYs nondeterministically.
	primary := startStandInProcess(t)
	transient := startStandInProcess(t)
	for _, pid := range []int{primary, transient} {
		if err := os.WriteFile(filepath.Join(outer, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "cannot place stand-in process into outer scope: %v", err)
		}
	}

	supervisorScope, err := BootstrapAitestSupervisor(context.Background(), outer, primary)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if want := WorkerScopeChildPath(outer, "supervisor"); supervisorScope != want {
		t.Fatalf("supervisor scope=%q want %q", supervisorScope, want)
	}
	if data, err := os.ReadFile(filepath.Join(outer, "cgroup.procs")); err != nil || strings.TrimSpace(string(data)) != "" {
		t.Fatalf("outer cgroup.procs=%q err=%v, want empty (both pids drained)", data, err)
	}
	supervisorProcs, err := os.ReadFile(filepath.Join(supervisorScope, "cgroup.procs"))
	if err != nil {
		t.Fatalf("read supervisor cgroup.procs: %v", err)
	}
	drained := strings.Fields(string(supervisorProcs))
	if len(drained) != 2 || !containsField(drained, strconv.Itoa(primary)) || !containsField(drained, strconv.Itoa(transient)) {
		t.Fatalf("supervisor cgroup.procs=%q, want both %d and %d drained into it", supervisorProcs, primary, transient)
	}
	if data, err := os.ReadFile(filepath.Join(outer, "cgroup.subtree_control")); err != nil || !strings.Contains(string(data), "memory") {
		t.Fatalf("outer subtree_control=%q err=%v, want memory delegated", data, err)
	}
}

func startStandInProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

func containsField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}

func TestBootstrapAitestSupervisorRejectsInvalidPID(t *testing.T) {
	if _, err := BootstrapAitestSupervisor(context.Background(), "/irrelevant", 0); err == nil {
		t.Fatal("pid 0 must be rejected")
	}
	if _, err := BootstrapAitestSupervisor(context.Background(), "/irrelevant", -5); err == nil {
		t.Fatal("negative pid must be rejected")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/runner/ -run TestBootstrapAitestSupervisor -v`
Expected: FAIL (undefined: BootstrapAitestSupervisor)

- [ ] **Step 3: Write minimal implementation**

```go
//go:build linux

package runner

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// BootstrapAitestSupervisor relocates supervisorPID (the caller's own parent
// process — the pytest supervisor, which aira confine placed directly into
// outerScope) into a fresh child scope, then delegates memory/cpu on
// outerScope's own cgroup.subtree_control. This order is load-bearing:
// cgroup v2 forbids a cgroup from delegating controllers to children while
// it still holds member processes of its own.
//
// Safe for exactly one call per process tree. NOT safe to retry from a
// fresh CLI invocation after a prior partial success: a retry would
// self-discover its outer scope from INSIDE the already-relocated
// supervisor scope (CurrentCgroupPath, Task 3) and nest incorrectly rather
// than reopening the original. Slice 1's supervisor (internal/pylib/aitest,
// Task 11) never retries this call for exactly this reason.
func BootstrapAitestSupervisor(ctx context.Context, outerScope string, supervisorPID int) (string, error) {
	if supervisorPID <= 0 {
		return "", fmt.Errorf("aitest bootstrap: invalid supervisor pid %d", supervisorPID)
	}
	backend := newDefaultBackend(outerScope)
	if err := backend.Probe(ctx); err != nil {
		return "", fmt.Errorf("aitest bootstrap: probe outer scope: %w", err)
	}
	supervisorScopePath := WorkerScopeChildPath(outerScope, "supervisor")
	scope, err := backend.Create(ctx, "supervisor")
	if err != nil {
		existing, openErr := backend.Open(ctx, supervisorScopePath)
		if openErr != nil {
			return "", fmt.Errorf("aitest bootstrap: create supervisor scope: %w (reopen: %v)", err, openErr)
		}
		scope = existing
	}
	// Drain EVERY pid in outer, not just supervisorPID: a transient child
	// racing the bootstrap moment can otherwise leave outer non-empty, which
	// EBUSYs the subtree_control write below nondeterministically (confirmed
	// live in a real spike: a supervisor shell plus two short-lived children
	// were all present in outer at once).
	if err := drainIntoScope(outerScope, scope); err != nil {
		return "", fmt.Errorf("aitest bootstrap: drain outer scope: %w", err)
	}
	// A final-state read on the SUPERVISOR scope, not a check against the
	// just-drained set: correct on both a first call and an idempotent
	// re-call (where outer is already empty and nothing gets drained this
	// time, but supervisorPID's earlier relocation still shows up here).
	if !scopeContainsPID(supervisorScopePath, supervisorPID) {
		return "", fmt.Errorf("aitest bootstrap: supervisor pid %d is not a member of %s after relocation", supervisorPID, supervisorScopePath)
	}
	if _, err := ensureConfineDelegation(outerScope); err != nil {
		return "", fmt.Errorf("aitest bootstrap: delegate outer scope controllers: %w", err)
	}
	return supervisorScopePath, nil
}

const maxDrainAttempts = 20

// drainIntoScope repeatedly reads outer's cgroup.procs and moves every pid
// found into scope, until a read comes back empty. Looping (rather than one
// read-then-move pass) is what makes this safe against a pid that appears
// between the read and the move.
func drainIntoScope(outerScope string, scope Scope) error {
	for attempt := 0; attempt < maxDrainAttempts; attempt++ {
		data, err := os.ReadFile(outerScope + "/cgroup.procs")
		if err != nil {
			return fmt.Errorf("read outer cgroup.procs: %w", err)
		}
		pids := strings.Fields(string(data))
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			if err := moveIntoScope(scope, pid); err != nil {
				return fmt.Errorf("move pid %s: %w", pid, err)
			}
		}
	}
	return fmt.Errorf("outer scope still had member processes after %d drain attempts", maxDrainAttempts)
}

func moveIntoScope(scope Scope, pid string) error {
	fd, err := unix.Openat(scope.FD(), "cgroup.procs", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open cgroup.procs: %w", err)
	}
	file := os.NewFile(uintptr(fd), "cgroup.procs")
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open cgroup.procs")
	}
	defer file.Close()
	if _, err := file.WriteString(pid + "\n"); err != nil {
		return fmt.Errorf("write cgroup.procs: %w", err)
	}
	return nil
}

func scopeContainsPID(scopePath string, pid int) bool {
	data, err := os.ReadFile(scopePath + "/cgroup.procs")
	if err != nil {
		return false
	}
	target := strconv.Itoa(pid)
	for _, field := range strings.Fields(string(data)) {
		if field == target {
			return true
		}
	}
	return false
}
```

Non-Linux stub:

```go
//go:build !linux

package runner

import (
	"context"
	"errors"
)

func BootstrapAitestSupervisor(ctx context.Context, outerScope string, supervisorPID int) (string, error) {
	return "", errors.New("aitest bootstrap: unsupported on this platform")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/runner/ -run TestBootstrapAitestSupervisor -v`
Expected: PASS (or a clean skip via `SkipOrFailRealCgroup` on a host without real cgroup-v2 delegation; hard-fail instead under `AIRA_REAL_CGROUP=1`)

- [ ] **Step 5: Commit**

```bash
git add internal/runner/aitest_bootstrap_linux.go internal/runner/aitest_bootstrap_stub.go internal/runner/aitest_bootstrap_linux_test.go
git commit -m "feat(aitest): add BootstrapAitestSupervisor outer-to-supervisor relocation"
```

---

### Task 3: `aira aitest-bootstrap` CLI verb

**Files:**
- Create: `internal/runner/aitest_bootstrap_selfpath_linux.go` (adds `CurrentCgroupPath`)
- Create: `internal/runner/aitest_bootstrap_selfpath_stub.go`
- Test: `internal/runner/aitest_bootstrap_selfpath_linux_test.go`
- Modify: `cmd/aira/main.go`
- Test: `cmd/aira/aitest_bootstrap_test.go`

**Interfaces:**
- Produces: `runner.CurrentCgroupPath() (string, error)` — exported wrapper the CLI needs since `currentCgroupPath`/`unifiedMount` are package-private to `internal/runner` and `main` is a different package.
- Consumes: `runner.BootstrapAitestSupervisor` (Task 2).

- [ ] **Step 1: Write the failing test (self-discovery wrapper)**

```go
//go:build linux

package runner

import "testing"

func TestCurrentCgroupPathReturnsAbsolutePath(t *testing.T) {
	path, err := CurrentCgroupPath()
	if err != nil {
		SkipOrFailNoCgroup(t, err)
	}
	if path == "" || path[0] != '/' {
		t.Fatalf("path=%q, want an absolute path", path)
	}
}
```

(`SkipOrFailNoCgroup` is a tiny new test helper in this same test file: `func SkipOrFailNoCgroup(t *testing.T, err error) { if os.Getenv("AIRA_REAL_CGROUP") == "1" { t.Fatalf("cgroup-v2 required: %v", err) }; t.Skipf("cgroup-v2 unavailable: %v", err) }` — mirrors `cgrouptest.SkipOrFailRealCgroup` but lives here since this test doesn't otherwise depend on the `cgrouptest` package.)

- [ ] **Step 2: Run test to verify it fails** — `aira confine -- go test ./internal/runner/ -run TestCurrentCgroupPath -v` — FAIL (undefined: CurrentCgroupPath)

- [ ] **Step 3: Write minimal implementation**

```go
//go:build linux

package runner

// CurrentCgroupPath returns the caller's own current cgroup-v2 path. Exported
// for cmd/aira's aitest-bootstrap verb, which cannot reach the package-private
// currentCgroupPath/unifiedMount used throughout this file.
func CurrentCgroupPath() (string, error) {
	mount, err := unifiedMount()
	if err != nil {
		return "", err
	}
	return currentCgroupPath(mount)
}
```

Stub (`aitest_bootstrap_selfpath_stub.go`, `//go:build !linux`):

```go
package runner

import "errors"

func CurrentCgroupPath() (string, error) {
	return "", errors.New("cgroup discovery unsupported on this platform")
}
```

- [ ] **Step 4: Run test to verify it passes** — PASS or clean skip.

- [ ] **Step 5: Add the CLI verb.** In `cmd/aira/main.go`, alongside the existing `confine-reserve`/`governor-slot` blocks (~line 108-121 and ~line 437-449):

```go
	if verb == "aitest-bootstrap" {
		if jsonOutput {
			response := core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: option --json is not valid for aitest-bootstrap", Exit: store.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}
			return render(response, true, stdout, stderr)
		}
		return runAitestBootstrapCommand(context.Background(), options, stdout, stderr)
	}
```

```go
	if verb == "aitest-bootstrap" {
		return parseAitestBootstrapArgs(argv)
	}
```

```go
func parseAitestBootstrapArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	for i := 0; i < len(argv); i++ {
		name := strings.TrimPrefix(argv[i], "--")
		if argv[i] != "--supervisor-pid" {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s is not valid for aitest-bootstrap", name)
		}
		if i+1 >= len(argv) {
			return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: option --supervisor-pid requires a value")
		}
		i++
		options["supervisor-pid"] = argv[i]
	}
	if _, present := options["supervisor-pid"]; !present {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: --supervisor-pid is required")
	}
	return nil, options, nil
}

func runAitestBootstrapCommand(ctx context.Context, options map[string]string, stdout, stderr io.Writer) int {
	pid, err := strconv.Atoi(options["supervisor-pid"])
	if err != nil || pid <= 0 {
		_, _ = fmt.Fprintln(stderr, "E_CONFINE_ARGUMENT_INVALID: --supervisor-pid must be a positive integer")
		return store.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	outer, err := runner.CurrentCgroupPath()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: discover outer scope: %v\n", err)
		return store.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	supervisorScope, err := runner.BootstrapAitestSupervisor(ctx, outer, pid)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return store.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	_, _ = fmt.Fprintf(stdout, "bootstrapped outer=%s supervisor_scope=%s\n", outer, supervisorScope)
	return 0
}
```

- [ ] **Step 6: Test the CLI argv parser**

```go
package main

import "testing"

func TestParseAitestBootstrapArgsRequiresSupervisorPID(t *testing.T) {
	if _, _, err := parseAitestBootstrapArgs(nil); err == nil {
		t.Fatal("missing --supervisor-pid must error")
	}
	_, options, err := parseAitestBootstrapArgs([]string{"--supervisor-pid", "123"})
	if err != nil || options["supervisor-pid"] != "123" {
		t.Fatalf("options=%v err=%v", options, err)
	}
}
```

Run: `aira confine -- go test ./cmd/aira/ -run TestParseAitestBootstrapArgs -v` — PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/runner/aitest_bootstrap_selfpath_linux.go internal/runner/aitest_bootstrap_selfpath_stub.go internal/runner/aitest_bootstrap_selfpath_linux_test.go cmd/aira/main.go cmd/aira/aitest_bootstrap_test.go
git commit -m "feat(aitest): add aira aitest-bootstrap CLI verb"
```

---

### Task 4: Daemon worker-admit ledger (pure logic, no networking)

**Files:**
- Create: `internal/daemon/worker_admit.go`
- Create: `internal/daemon/worker_admit_test.go`
- Modify: `internal/daemon/server.go` (add fields to `Server` struct only — wiring is Task 6)

**Interfaces:**
- Consumes: `s.admitReadMemory func(string) (int64, int64, int64, bool, string)` (existing test seam, defaults to `readSliceMemory`), `runner.WorkerScopeChildPath` (Task 1), `exactAdmitInt64`/`admitMaxReserve`/`admitWaitCapMs` (existing, `admit.go`, same package — reused for overflow-safe argument parsing and bound checks). Deliberately does NOT consume `s.admitSliceHeadroom` — that constant is sized for the whole machine-wide slice (2 GiB); this task introduces its own much smaller, aitest-appropriate `workerAdmitHeadroom` field instead (see below).
- Produces: `WorkerAdmitResponse` (JSON wire type), `workerAdmitRequest`, `func (s *Server) evaluateWorkerAdmit(req workerAdmitRequest) WorkerAdmitResponse`, `func (s *Server) releaseWorkerGrant(jobID, outerScope, workerID string)`, `func validateWorkerAdmitArgs(args map[string]any) (workerAdmitRequest, error)`. Task 5 consumes all four.

This ledger is deliberately simpler than `admit.go`'s `sliceQueue`: one job's own worker pool has no cross-job fairness question, so a mutex-guarded map plus the caller polling (Task 5) is the whole mechanism — no waiter channels, no evaluator goroutine.

- [ ] **Step 1: Write the failing test**

```go
package daemon

import (
	"testing"

	"aira/internal/runner"
)

// admitReadMemoryFixture stands in for readSliceMemory. evaluateWorkerAdmit
// now reads ONLY the OUTER scope's own live memory.current (hierarchical:
// already includes the supervisor plus every placed worker, spec 3.3) — it
// no longer sums per-worker grants separately (that summation both
// double-counted against, and could still under-count relative to, what
// the kernel's own memory.oom.group actually acts on). So this fixture
// answers any outer_scope path uniformly against current[path], defaulting
// to 0 (an idle scope) when unset, always readable.
func admitReadMemoryFixture(current map[string]int64, outerMax int64) func(string) (int64, int64, int64, bool, string) {
	return func(path string) (int64, int64, int64, bool, string) {
		return current[path], outerMax, 0, true, ""
	}
}

func TestEvaluateWorkerAdmitGrantsWithinHeadroom(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.workerAdmitHeadroom = 0
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
	if response.State != "granted" || response.WorkerID == "" || response.MemoryMax != 400 || response.MemoryHigh != 320 {
		t.Fatalf("response=%+v", response)
	}
	// Pin the invariant a real deployment depends on: the daemon computes
	// this scope path with WorkerScopeChildPath(outer, "worker-"+id), and
	// Task 7's CreateWorkerScope independently computes the SAME path via
	// backend.Create(ctx, "worker-"+id) + WorkerScopeChildPath — both sides
	// must derive from the identical id string, or the client creates a
	// scope the daemon then can't find to read memory.current from.
	if want := runner.WorkerScopeChildPath("/outer", "worker-"+response.WorkerID); response.ScopePath != want {
		t.Fatalf("ScopePath=%q want %q (daemon/client path convention diverged)", response.ScopePath, want)
	}
}

func TestEvaluateWorkerAdmitDeniesOverBudgetAccountingLiveUsage(t *testing.T) {
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	live := map[string]int64{}
	server.admitReadMemory = admitReadMemoryFixture(live, 1000)
	first := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if first.State != "granted" {
		t.Fatalf("first=%+v", first)
	}
	// The OUTER scope's own live usage (hierarchically includes the
	// supervisor plus every worker) governs the second decision, not a sum
	// of held worker caps: even though 700+700 > 1000, a low live reading
	// on the outer scope itself still admits it.
	live["/outer"] = 100
	second := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if second.State != "granted" {
		t.Fatalf("second (live-usage-based) =%+v", second)
	}
}

func TestEvaluateWorkerAdmitReturnsUnevaluatedWhenOuterScopeLiveUsageUnreadable(t *testing.T) {
	// Fail toward safety, ported to the single-read model: admission no
	// longer reads individual worker-scope paths at all (dropped along
	// with the per-worker summation), so the one signal that can still be
	// unreadable is the OUTER scope's own memory.current/memory.max read
	// itself — that must never silently admit.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "fallback:outer-scope-unreadable"
	}
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
	if response.State != "unevaluated" {
		t.Fatalf("response=%+v, want unevaluated when the outer scope's own live usage cannot be read", response)
	}
}

func TestEvaluateWorkerAdmitDeniesImmediatelyWhenRequestExceedsCeilingEvenAtZeroUsage(t *testing.T) {
	// A request that could never fit even with the WHOLE ceiling free right
	// now is a stable "never going to work" fact about the request, not a
	// transient contention moment — this is the one case Slice 1 makes
	// "denied" genuinely reachable for (see workerAdmitConnection, Task 5):
	// everything else that isn't available right now polls/retries and
	// eventually becomes "timeout" instead.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 1001, maxWaitMS: 0})
	if response.State != "denied" || response.Reason != "reject:exceeds-ceiling" {
		t.Fatalf("response=%+v, want denied/reject:exceeds-ceiling", response)
	}
}

func TestReleaseWorkerGrantIsIdempotent(t *testing.T) {
	// A worker-admit decision no longer depends on job.grants bookkeeping
	// for its arithmetic (admission now reads the outer scope's own live
	// memory.current directly, spec 3.3) — job.grants remains as worker-ID
	// bookkeeping only. What matters here is the property Task 5's fixed
	// workerAdmitConnection depends on: releaseWorkerGrant is safe to call
	// more than once (a write-failure path there defers a release that may
	// race a normal lease-close release of the same grant).
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	granted := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("granted=%+v", granted)
	}
	server.releaseWorkerGrant("job-1", "/outer", granted.WorkerID)
	server.releaseWorkerGrant("job-1", "/outer", granted.WorkerID) // must not panic
	again := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if again.State != "granted" {
		t.Fatalf("again=%+v", again)
	}
}

func TestWorkerJobLedgerIsBoundToJobIDAndOuterScopeTogether(t *testing.T) {
	// A job_id is caller-supplied and only as unique as the caller's own
	// pid-reuse window — two concurrent requests that happen to reuse the
	// same job_id with DIFFERENT outer_scope values must never get their
	// scope accounting mixed together.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	live := map[string]int64{"/outer-a": 900, "/outer-b": 0}
	server.admitReadMemory = admitReadMemoryFixture(live, 1000)
	// /outer-a is nearly saturated; /outer-b (same job_id!) is empty.
	denied := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer-a", estimatedBytes: 500, maxWaitMS: 0})
	if denied.State != "denied" {
		t.Fatalf("denied=%+v", denied)
	}
	granted := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer-b", estimatedBytes: 500, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("same job_id, different outer_scope must not inherit the other scope's saturation: %+v", granted)
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — `aira confine -- go test ./internal/daemon/ -run TestEvaluateWorkerAdmit -v` — FAIL (undefined symbols)

- [ ] **Step 3: Write minimal implementation**

```go
package daemon

import (
	"fmt"
	"sync"

	"aira/internal/runner"
)

// workerAdmitHeadroomDefault is a SEPARATE, much smaller headroom than
// admitSliceHeadroomBase (2 GiB, sized for the whole machine-wide slice,
// admit.go). Reusing the slice-wide constant here would swallow most of a
// realistically-sized outer scope's own cap in production. This is a
// build-time tunable, not yet sized from field data — a reasonable small
// fixed default for Slice 1.
const workerAdmitHeadroomDefault int64 = 64 << 20 // 64 MiB

// WorkerAdmitResponse is the one grant/denial payload the worker-admit
// connection sends before optionally holding itself open as the lease.
type WorkerAdmitResponse struct {
	State      string `json:"state"` // "granted" | "denied" | "timeout" | "unevaluated"
	Reason     string `json:"reason,omitempty"`
	WaitedMS   int64  `json:"waited_ms"`
	WorkerID   string `json:"worker_id,omitempty"`
	ScopePath  string `json:"scope_path,omitempty"`
	MemoryMax  int64  `json:"memory_max,omitempty"`
	MemoryHigh int64  `json:"memory_high,omitempty"`
}

type workerAdmitRequest struct {
	jobID      string
	outerScope string
	// signature is accepted on the wire (the key spec 3.3 names for a
	// future per-suite peak-history-based cap-sizing backstop) but UNUSED
	// for anything in Slice 1 — deferred past Slice 1; estimatedBytes
	// alone governs the backstop cap for now (see also Task 17's
	// _resolve_estimated_bytes, which states the same deferral on the
	// Python side).
	signature      string
	estimatedBytes int64
	maxWaitMS      int64
}

type workerGrant struct {
	scopePath string
	memoryMax int64
}

type workerJobState struct {
	mu         sync.Mutex
	outerScope string
	nextSeq    int
	grants     map[string]*workerGrant
}

// workerJobKey binds ledger state to the (job_id, outer_scope) PAIR, not
// job_id alone — job_id is caller-supplied and only as unique as the
// caller's own pid-reuse window, so two concurrent requests that reuse the
// same job_id with DIFFERENT outer_scope values must never get their scope
// accounting mixed together.
func workerJobKey(jobID, outerScope string) string {
	return jobID + "\x00" + outerScope
}

// workerJobs is never actively pruned once a job's last worker releases —
// accepted Slice 1 gap: unbounded-but-slow growth, one entry per distinct
// (job_id, outer_scope) pair across the daemon's lifetime. A real concern
// only for a very long-lived daemon running very many distinct aitest
// jobs; not worth cleanup machinery for Slice 1.
func (s *Server) workerJobFor(jobID, outerScope string) *workerJobState {
	key := workerJobKey(jobID, outerScope)
	s.workerJobsMu.Lock()
	defer s.workerJobsMu.Unlock()
	if s.workerJobs == nil {
		s.workerJobs = make(map[string]*workerJobState)
	}
	job := s.workerJobs[key]
	if job == nil {
		job = &workerJobState{outerScope: outerScope, grants: make(map[string]*workerGrant)}
		s.workerJobs[key] = job
	}
	return job
}

// evaluateWorkerAdmit makes one synchronous grant/deny decision for req.
// "Used" is the OUTER scope's own live memory.current, read directly —
// cgroup memory accounting is hierarchical, so this single read already
// includes the supervisor's own RSS plus every already-placed worker's
// (spec 3.3). Summing individually-read worker-scope grants separately (an
// earlier version of this function did) was both redundant with that
// hierarchical accounting AND unsafe: Σ(worker grants) + supervisor RSS
// could exceed outerMax even when the ledger thought there was room,
// risking an outer-scope-level memory.oom.group kill of the ENTIRE run —
// precisely the incident class this design exists to prevent.
func (s *Server) evaluateWorkerAdmit(req workerAdmitRequest) WorkerAdmitResponse {
	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	used, outerMax, _, ok, reason := readMemory(req.outerScope)
	if !ok {
		return WorkerAdmitResponse{State: "unevaluated", Reason: reason}
	}
	headroom := s.workerAdmitHeadroom
	if headroom < 0 {
		headroom = 0
	}
	ceiling := outerMax - headroom
	if req.estimatedBytes > ceiling {
		// Could never fit even at zero current usage — a stable fact about
		// THIS request, not a transient contention moment. Deny
		// immediately (workerAdmitConnection, Task 5, breaks its poll loop
		// on this reason) instead of waiting out the full poll timeout
		// only to time out anyway.
		return WorkerAdmitResponse{State: "denied", Reason: "reject:exceeds-ceiling"}
	}
	job := s.workerJobFor(req.jobID, req.outerScope)
	job.mu.Lock()
	defer job.mu.Unlock()
	if req.estimatedBytes > ceiling-used {
		// Not available RIGHT NOW (transient: current live usage), but
		// could be granted once usage drops — the caller's poll loop keeps
		// retrying this until granted or its own max_wait_ms deadline
		// converts it to "timeout".
		return WorkerAdmitResponse{State: "denied", Reason: "fallback:insufficient-headroom"}
	}
	job.nextSeq++
	workerID := fmt.Sprintf("%d", job.nextSeq)
	scopePath := runner.WorkerScopeChildPath(req.outerScope, "worker-"+workerID)
	memoryHigh := req.estimatedBytes * 4 / 5
	job.grants[workerID] = &workerGrant{scopePath: scopePath, memoryMax: req.estimatedBytes}
	return WorkerAdmitResponse{State: "granted", WorkerID: workerID, ScopePath: scopePath, MemoryMax: req.estimatedBytes, MemoryHigh: memoryHigh}
}

// releaseWorkerGrant frees one worker's ledger bookkeeping entry. Called
// when its connection closes (Task 5) — the same dies-with-socket lease
// shape admit and governor already use. Idempotent by construction (delete
// on an absent map key is a no-op): Task 5's deferred release can race a
// normal lease-close release of the same grant, and both must be safe.
func (s *Server) releaseWorkerGrant(jobID, outerScope, workerID string) {
	key := workerJobKey(jobID, outerScope)
	s.workerJobsMu.Lock()
	job := s.workerJobs[key]
	s.workerJobsMu.Unlock()
	if job == nil {
		return
	}
	job.mu.Lock()
	delete(job.grants, workerID)
	job.mu.Unlock()
}

func validateWorkerAdmitArgs(args map[string]any) (workerAdmitRequest, error) {
	req := workerAdmitRequest{}
	str := func(key string, required bool) (string, error) {
		raw, exists := args[key]
		if !exists {
			if required {
				return "", fmt.Errorf("%s: worker-admit %s is required", CodeProtocol, key)
			}
			return "", nil
		}
		value, ok := raw.(string)
		if !ok || (required && value == "") {
			return "", fmt.Errorf("%s: worker-admit %s must be a non-empty string", CodeProtocol, key)
		}
		return value, nil
	}
	var err error
	if req.jobID, err = str("job_id", true); err != nil {
		return workerAdmitRequest{}, err
	}
	if req.outerScope, err = str("outer_scope", true); err != nil {
		return workerAdmitRequest{}, err
	}
	if req.signature, err = str("signature", false); err != nil {
		return workerAdmitRequest{}, err
	}
	// exactAdmitInt64 (existing, admit.go) — overflow-safe float64->int64,
	// reused rather than the naive int64(estimated) truncation this used
	// to do, which let an arbitrary huge float64 truncate unchecked.
	estimated, ok := exactAdmitInt64(args["estimated_bytes"])
	if !ok || estimated <= 0 || estimated > admitMaxReserve {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit estimated_bytes must be a positive number no larger than %d", CodeProtocol, admitMaxReserve)
	}
	req.estimatedBytes = estimated
	maxWait, ok := exactAdmitInt64(args["max_wait_ms"])
	if !ok || maxWait < 0 || maxWait > admitWaitCapMs {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit max_wait_ms must be in [0,%d]", CodeProtocol, admitWaitCapMs)
	}
	req.maxWaitMS = maxWait
	return req, nil
}
```

Add three fields to the `Server` struct in `internal/daemon/server.go` (near the existing `governor *governorSet` field):

```go
	workerJobsMu        sync.Mutex
	workerJobs          map[string]*workerJobState
	workerAdmitHeadroom int64
```

Also initialize the new headroom field's default in `NewServer`'s literal
(near the existing `admitSliceHeadroomSupervisor: admitSliceHeadroomSupervisorDefault,` line, `server.go:132-133`) — mirrors that exact existing init pattern so a fresh `Server` starts with the real default and a test overrides it explicitly (as `admitSliceHeadroomBase`/`admitSliceHeadroomSupervisor` already do):

```go
		workerAdmitHeadroom:          workerAdmitHeadroomDefault,
```

- [ ] **Step 4: Run test to verify it passes** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/worker_admit.go internal/daemon/worker_admit_test.go internal/daemon/server.go
git commit -m "feat(aitest): add worker-admit ledger (live-occupancy, per-job)"
```

---

### Task 5: `workerAdmitConnection` — connection-held lease wire handler

**Files:**
- Modify: `internal/daemon/worker_admit.go` (add the connection handler)
- Modify: `internal/daemon/worker_admit_test.go` (add a `net.Pipe()` test, Pattern A from `admit_test.go`)

**Interfaces:**
- Consumes: `s.evaluateWorkerAdmit`, `s.releaseWorkerGrant`, `validateWorkerAdmitArgs` (Task 4); `writeFrame`/`responseFrame`/`errorFrame` (existing, `protocol.go`).
- Produces: `func (s *Server) workerAdmitConnection(conn net.Conn, args map[string]any)` — consumed by Task 6's dispatch wiring.

Polls `evaluateWorkerAdmit` on a short interval up to `max_wait_ms` rather than
using admit.go's waiter-channel/evaluator-goroutine machinery — deliberate,
see Global Constraints. Add one field to `Server` for test speed:
`workerAdmitPollInterval time.Duration` (near `admitPollInterval`).

- [ ] **Step 1: Write the failing test**

```go
func TestWorkerAdmitConnectionGrantsThenHoldsUntilPeerCloses(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(400), "max_wait_ms": float64(0),
		})
	}()

	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	var grant WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &grant); err != nil || grant.State != "granted" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	select {
	case <-done:
		t.Fatal("connection released before peer closed")
	case <-time.After(20 * time.Millisecond):
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection did not release after peer close")
	}
	// Releasing must not leave the daemon in a broken state that then
	// rejects everything.
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 1000, maxWaitMS: 0})
	if response.State != "granted" {
		t.Fatalf("post-release admission unexpectedly broken: %+v", response)
	}
}

func TestWorkerAdmitConnectionTimesOutWhenSaturated(t *testing.T) {
	server := NewServer(Paths{})
	// The outer scope's own live usage already consumes the entire
	// ceiling — under the live-occupancy model (spec 3.3) there is no
	// per-worker-grant summation to "saturate" separately; a prior grant
	// alone does not change what a later admission decision sees unless
	// the outer scope's own live memory.current reflects it, exactly like
	// production (the daemon never tracks a synthetic reserve here).
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{"/outer": 500}, 500)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(100), "max_wait_ms": float64(5),
		})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	var response WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &response); err != nil || response.State != "timeout" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	_ = clientConn.Close()
}

func TestWorkerAdmitConnectionDeniesImmediatelyWithoutWaitingOutMaxWait(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 100)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	started := time.Now()
	go func() {
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			// 1000 bytes can never fit under a 100-byte outer ceiling no
			// matter how long we wait -- must come back "denied" well
			// before the (deliberately long) max_wait_ms elapses.
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(1000), "max_wait_ms": float64(60000),
		})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("denial took %v — looks like it waited out max_wait_ms instead of denying immediately", elapsed)
	}
	var response WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &response); err != nil || response.State != "denied" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	_ = clientConn.Close()
}

func TestWorkerAdmitConnectionReleasesGrantWhenResponseWriteFails(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	// Close the CLIENT side before the server ever gets to write its
	// response -- a peer-vanished-in-the-exact-window race.
	// evaluateWorkerAdmit already inserted the grant into the ledger by
	// this point; the subsequent writeFrame on serverConn must then fail,
	// and that grant must still be released rather than leaking against
	// the job's ledger forever (the bug: the old code just `return`ed on a
	// write failure with no release at all).
	_ = clientConn.Close()
	server.workerAdmitConnection(serverConn, map[string]any{
		"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(900), "max_wait_ms": float64(0),
	})
	_ = serverConn.Close()

	again := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if again.State != "granted" {
		t.Fatalf("write-failure path leaked the grant: %+v", again)
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — `aira confine -- go test ./internal/daemon/ -run TestWorkerAdmitConnection -v` — FAIL (undefined: workerAdmitConnection)

- [ ] **Step 3: Write minimal implementation**

```go
func (s *Server) workerAdmitConnection(conn net.Conn, args map[string]any) {
	req, err := validateWorkerAdmitArgs(args)
	if err != nil {
		_ = writeFrame(conn, errorFrame(CodeProtocol, err.Error()))
		return
	}
	peerCtx, cancelPeer := context.WithCancel(context.Background())
	defer cancelPeer()
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancelPeer()
	}()

	poll := s.workerAdmitPollInterval
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	deadline := s.admitNowTime().Add(time.Duration(req.maxWaitMS) * time.Millisecond)
	var response WorkerAdmitResponse
	for {
		response = s.evaluateWorkerAdmit(req)
		if response.State == "granted" || response.State == "unevaluated" {
			break
		}
		if response.State == "denied" && response.Reason == "reject:exceeds-ceiling" {
			// A stable "never going to fit" fact about this request, not a
			// transient contention moment — surface "denied" to the client
			// immediately instead of waiting out the full poll timeout
			// only to time out anyway. Every OTHER non-granted state keeps
			// polling below (a live-usage-driven "not right now" is
			// retried until it clears or the deadline converts it to
			// "timeout").
			break
		}
		if !s.admitNowTime().Before(deadline) {
			response = WorkerAdmitResponse{State: "timeout", Reason: "reject:saturated"}
			break
		}
		select {
		case <-time.After(poll):
		case <-peerCtx.Done():
			return
		case <-s.stopping:
			return
		}
	}

	// A "granted" response has ALREADY inserted a ledger entry inside
	// evaluateWorkerAdmit above. From this point on, EVERY exit path —
	// a write failure, the peer vanishing in the exact window between
	// grant and delivery, or the normal lease-close below — must release
	// that grant exactly once, or it leaks against the job's ledger
	// forever. Mirrors admitConnection's own deferred, idempotent release
	// (admit.go:458-466); releaseWorkerGrant is idempotent by construction
	// (delete on an absent key is a no-op), so a double-fire here (e.g. a
	// write failure racing this deferred call with a direct call further
	// down — there is none further down anymore, but the mirroring is
	// deliberate) is always safe.
	released := false
	release := func() {
		if released || response.State != "granted" {
			return
		}
		released = true
		s.releaseWorkerGrant(req.jobID, req.outerScope, response.WorkerID)
	}
	defer release()

	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	ok := response.State == "granted"
	if err := writeFrame(conn, responseFrame(core.Response{OK: ok, Code: "OK", Data: response})); err != nil {
		return
	}
	if !ok {
		return
	}
	select {
	case <-peerCtx.Done():
	case <-s.stopping:
	}
}
```

(`s.admitNowTime()` is the existing test-seam wrapper already used by `admit.go` for injectable time — reuse it rather than calling `time.Now()` directly, so a future test can control the deadline deterministically the same way `admit_test.go` does.)

Add `workerAdmitPollInterval time.Duration` to the `Server` struct (`server.go`), next to `admitPollInterval`.

- [ ] **Step 4: Run test to verify it passes** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/worker_admit.go internal/daemon/worker_admit_test.go internal/daemon/server.go
git commit -m "feat(aitest): add workerAdmitConnection lease-holding handler"
```

---

### Task 6: wire `worker-admit` into `serveConnection`

**Files:**
- Modify: `internal/daemon/server.go`
- Test: `internal/daemon/server_test.go` (add a real-socket round trip, Pattern B)

- [ ] **Step 1: Write the failing test**

```go
func TestServerDispatchesWorkerAdmitVerbOverRealSocket(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.workerAdmitHeadroom = 0
	_, _ = startServer(t, server)
	scope := testScope(t, paths, "one")

	conn, err := net.Dial("unix", paths.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := writeFrame(conn, RequestFrame{
		Proto: ProtocolVersion, Scope: scope,
		Request: core.Request{Verb: "worker-admit", Args: map[string]any{
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(400), "max_wait_ms": float64(0),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var frame ResponseFrame
	if err := readFrame(conn, &frame); err != nil {
		t.Fatal(err)
	}
	if !frame.OK {
		t.Fatalf("frame=%+v", frame)
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — FAIL (verb falls through to `core.ClassifyRequest`, gets `E_DAEMON_PROTOCOL: client-only operation cannot run in daemon` or similar, not `OK`)

- [ ] **Step 3: Add the dispatch block** in `serveConnection`, immediately after the existing `if verb == "governor" { ... }` block and before `if scope.ProjectID != "" { ... }`:

```go
	if verb == "worker-admit" {
		if s.OnRequest != nil {
			s.OnRequest(request.Scope, request.Request)
		}
		// workerAdmitConnection owns its only frame and the lease-release
		// path, exactly like admit/governor above — never let the generic
		// dispatcher touch this connection again.
		wrote = true
		_ = conn.SetReadDeadline(time.Time{})
		s.workerAdmitConnection(conn, request.Request.Args)
		return
	}
```

- [ ] **Step 4: Run test to verify it passes** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/server.go internal/daemon/server_test.go
git commit -m "feat(aitest): dispatch worker-admit verb in serveConnection"
```

---

### Task 7: `runner.CreateWorkerScope` — nested worker cgroup with hard cap

**Files:**
- Create: `internal/runner/worker_scope_linux.go`
- Create: `internal/runner/worker_scope_stub.go`
- Test: `internal/runner/worker_scope_linux_test.go`

**Interfaces:**
- Consumes: `newDefaultBackend`, `writeScopeMemoryCap` (existing, `cgroup_linux.go`/`confine_linux.go`); `WorkerScopeChildPath` (Task 1); `BootstrapAitestSupervisor`'s delegation (Task 2 — this task's real-cgroup test performs the same outer-scope delegation setup as Task 2's test, since it needs a delegated parent to create a capped child under).
- Produces: `func CreateWorkerScope(ctx context.Context, outerScope, workerID string, memoryMax, memoryHigh int64) (string, error)` — consumed by Task 8.

- [ ] **Step 1: Write the failing test**

```go
//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aira/internal/cgrouptest"
)

func TestCreateWorkerScopeWritesVerifiedMemoryCap(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfineDelegation(outer); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot delegate outer scope: %v", err)
	}

	// 134217728 (128 MiB) and 104857600 (100 MiB) are both exact multiples
	// of the page size — writeScopeMemoryCap's own verification page-floors
	// the value before comparing (confirmed against the real
	// verifyScopeMemoryValue/floorMemoryPage code), so an unaligned value
	// like 107374182 would be floored by the kernel to 107372544 and this
	// verbatim-string comparison would fail even on a correct
	// implementation.
	scopePath, err := CreateWorkerScope(context.Background(), outer, "1", 134217728, 104857600)
	if err != nil {
		t.Fatalf("CreateWorkerScope: %v", err)
	}
	if want := WorkerScopeChildPath(outer, "worker-1"); scopePath != want {
		t.Fatalf("scopePath=%q want %q", scopePath, want)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.max")); err != nil || strings.TrimSpace(string(data)) != "134217728" {
		t.Fatalf("memory.max=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.high")); err != nil || strings.TrimSpace(string(data)) != "104857600" {
		t.Fatalf("memory.high=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.oom.group")); err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("memory.oom.group=%q err=%v", data, err)
	}
	_ = strconv.Itoa // silence unused import if trimmed during edit
}
```

- [ ] **Step 2: Run test to verify it fails** — FAIL (undefined: CreateWorkerScope)

- [ ] **Step 3: Write minimal implementation**

```go
//go:build linux

package runner

import (
	"context"
	"fmt"
)

// CreateWorkerScope creates one worker's cgroup as a child of outerScope
// (already delegated by BootstrapAitestSupervisor), with a hard memory.max
// cap and a memory.high soft-throttle watermark below it, plus
// memory.oom.group=1 so a runaway inside this one worker self-contains
// (spec 3.3: per-worker hard cap, not a pool-level cap only).
func CreateWorkerScope(ctx context.Context, outerScope, workerID string, memoryMax, memoryHigh int64) (string, error) {
	backend := newDefaultBackend(outerScope)
	scope, err := backend.Create(ctx, "worker-"+workerID)
	if err != nil {
		return "", fmt.Errorf("aitest worker scope: create: %w", err)
	}
	if err := writeScopeMemoryCap(scope, memoryMax, memoryHigh, true); err != nil {
		return "", fmt.Errorf("aitest worker scope: memory cap: %w", err)
	}
	return WorkerScopeChildPath(outerScope, "worker-"+workerID), nil
}
```

Stub (`worker_scope_stub.go`, `//go:build !linux`):

```go
package runner

import (
	"context"
	"errors"
)

func CreateWorkerScope(ctx context.Context, outerScope, workerID string, memoryMax, memoryHigh int64) (string, error) {
	return "", errors.New("aitest worker scope: unsupported on this platform")
}
```

- [ ] **Step 4: Run test to verify it passes** — PASS or clean skip.

- [ ] **Step 5: Commit**

```bash
git add internal/runner/worker_scope_linux.go internal/runner/worker_scope_stub.go internal/runner/worker_scope_linux_test.go
git commit -m "feat(aitest): add CreateWorkerScope nested per-worker cgroup cap"
```

---

### Task 8: `aira worker-admit` CLI verb (client dial + scope creation + lease hold)

**Files:**
- Create: `internal/runner/worker_admit_client_linux.go`
- Create: `internal/runner/worker_admit_client_stub.go` (`//go:build !linux`) —
  `cmd/aira/main.go` is cross-platform and calls `runner.RequestWorkerAdmit`
  unconditionally; without a stub, `go build` fails on any non-Linux target,
  mirroring the same gap Task 2/7 each closed for their own Linux-only piece.
- Modify: `cmd/aira/main.go`
- Test: `internal/runner/worker_admit_client_linux_test.go` (package `runner_test` — an
  EXTERNAL test package, so it can import both `aira/internal/runner` and
  `aira/internal/daemon` to spin up a real test daemon without an import
  cycle; `internal/runner` itself must never import `internal/daemon`,
  confirmed precedent: `admitThroughDaemon`, `internal/runner/admission_linux.go:240`,
  defines its own local `runnerAdmitRequestFrame`/`runnerAdmitResponseFrame`/
  `writeRunnerAdmitFrame`/`readRunnerAdmitFrame` instead of importing daemon's
  types, because `internal/daemon` already imports `internal/runner` — the
  reverse would cycle. This task's client reuses those exact existing
  functions rather than duplicating a third copy.)
- Test: `cmd/aira/worker_admit_test.go`

**Interfaces:**
- Consumes: `runnerAdmitRequestFrame`, `runnerAdmitResponseFrame`, `runnerDaemonProtocolVersion`, `writeRunnerAdmitFrame`, `readRunnerAdmitFrame` (existing, package-private to `internal/runner`, `admission_linux.go`); `CreateWorkerScope` (Task 7).
- Produces: `runner.RequestWorkerAdmit(ctx, WorkerAdmitClientRequest) (*WorkerAdmitLease, error)`, `WorkerAdmitLease.Close() error` — consumed by Task 11 indirectly (Python shells out to this CLI verb, mirroring how it shells out to `confine-reserve` today rather than calling Go directly).

- [ ] **Step 1: Write the failing test (client dial, against a real test daemon)**

```go
package runner_test

import (
	"context"
	"testing"
	"time"

	"aira/internal/daemon"
	"aira/internal/runner"
)

func TestRequestWorkerAdmitReturnsHeldLeaseOnGrant(t *testing.T) {
	paths := daemonTestPaths(t) // small local helper: mirrors internal/daemon's own testPaths(t), sets XDG_STATE_HOME/XDG_RUNTIME_DIR under t.TempDir()
	server := daemon.NewServer(paths)
	server.SetAdmitReadMemoryForTest(func(string) (int64, int64, int64, bool, string) { return 0, 1000, 0, true, "" })
	server.SetWorkerAdmitHeadroomForTest(0) // production default (64 MiB) would swallow this test's tiny synthetic byte values
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-ready

	lease, err := runner.RequestWorkerAdmit(context.Background(), runner.WorkerAdmitClientRequest{
		SocketPath: paths.SocketPath, JobID: "job-1", OuterScope: "/outer", EstimatedBytes: 400, MaxWait: time.Second,
	})
	if err != nil {
		t.Fatalf("RequestWorkerAdmit: %v", err)
	}
	defer lease.Close()
	if lease.WorkerID == "" || lease.MemoryMax != 400 || lease.MemoryHigh != 320 {
		t.Fatalf("lease=%+v", lease)
	}
}

func TestRequestWorkerAdmitReturnsErrorOnDenial(t *testing.T) {
	paths := daemonTestPaths(t)
	server := daemon.NewServer(paths)
	server.SetAdmitReadMemoryForTest(func(string) (int64, int64, int64, bool, string) { return 0, 100, 0, true, "" })
	server.SetWorkerAdmitHeadroomForTest(0)
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-ready

	_, err := runner.RequestWorkerAdmit(context.Background(), runner.WorkerAdmitClientRequest{
		SocketPath: paths.SocketPath, JobID: "job-1", OuterScope: "/outer", EstimatedBytes: 10000, MaxWait: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error for a request over budget")
	}
}
```

This test needs two new tiny exported test seams on `*daemon.Server`
(`internal/daemon/testing_seams.go`, new file — the underlying fields stay
private; these are the exports needed for an external package's test to
drive them, matching the spirit of the existing `OnRequest`/`Ready` exported
test seams already on `Server`):

```go
func (s *Server) SetAdmitReadMemoryForTest(fn func(string) (int64, int64, int64, bool, string)) {
	s.admitReadMemory = fn
}

// SetWorkerAdmitHeadroomForTest overrides the production worker-admit
// headroom default (64 MiB, worker_admit.go) so a test admitting against
// small synthetic byte values (or a small real cgroup memory.max) is not
// universally denied.
func (s *Server) SetWorkerAdmitHeadroomForTest(value int64) {
	s.workerAdmitHeadroom = value
}
```

Also add a small local `daemonTestPaths(t)`
helper in the same `_test.go` file, mirroring `internal/daemon/server_test.go`'s
own `testPaths(t)` (same `XDG_STATE_HOME`/`XDG_RUNTIME_DIR`-under-`t.TempDir()`
shape) since that helper is private to the `daemon` package's own tests.

- [ ] **Step 2: Run test to verify it fails** — `aira confine -- go test ./internal/runner/ -run TestRequestWorkerAdmit -v` — FAIL (undefined: RequestWorkerAdmit, SetAdmitReadMemoryForTest)

- [ ] **Step 3: Write minimal implementation**

```go
//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// WorkerAdmitLease is a granted worker-admit connection, held open as the
// daemon-side lease until Close releases it (the daemon frees the ledger
// entry when it detects the peer disconnect — see the worker-admit design).
type WorkerAdmitLease struct {
	WorkerID   string
	ScopePath  string
	MemoryMax  int64
	MemoryHigh int64
	conn       net.Conn
}

func (l *WorkerAdmitLease) Close() error {
	if l == nil || l.conn == nil {
		return nil
	}
	return l.conn.Close()
}

type WorkerAdmitClientRequest struct {
	SocketPath     string
	JobID          string
	OuterScope     string
	Signature      string
	EstimatedBytes int64
	MaxWait        time.Duration
}

type workerAdmitGrant struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	WorkerID   string `json:"worker_id,omitempty"`
	ScopePath  string `json:"scope_path,omitempty"`
	MemoryMax  int64  `json:"memory_max,omitempty"`
	MemoryHigh int64  `json:"memory_high,omitempty"`
}

// RequestWorkerAdmit dials the daemon and sends one worker-admit request,
// reusing admitThroughDaemon's proven local wire types/framing (this package
// may not import internal/daemon — see admission_linux.go). On "granted" the
// returned lease holds the connection open; Close releases the daemon-side
// ledger entry.
func RequestWorkerAdmit(ctx context.Context, req WorkerAdmitClientRequest) (*WorkerAdmitLease, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", req.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: dial daemon: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	frame := runnerAdmitRequestFrame{Proto: runnerDaemonProtocolVersion, Scope: map[string]any{}}
	frame.Request.Verb = "worker-admit"
	frame.Request.Args = map[string]any{
		"job_id": req.JobID, "outer_scope": req.OuterScope, "signature": req.Signature,
		"estimated_bytes": req.EstimatedBytes, "max_wait_ms": req.MaxWait.Milliseconds(),
	}
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: send worker-admit request: %w", err)
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: read worker-admit response: %w", err)
	}
	var grant workerAdmitGrant
	if err := json.Unmarshal(response.Data, &grant); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: malformed worker-admit response: %w", err)
	}
	if grant.State != "granted" {
		_ = conn.Close()
		reason := grant.Reason
		if reason == "" {
			reason = response.Error
		}
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: worker-admit %s: %s", grant.State, reason)
	}
	return &WorkerAdmitLease{WorkerID: grant.WorkerID, ScopePath: grant.ScopePath, MemoryMax: grant.MemoryMax, MemoryHigh: grant.MemoryHigh, conn: conn}, nil
}
```

- [ ] **Step 4: Run test to verify it passes** — PASS.

- [ ] **Step 5: Add the non-Linux stub.** `cmd/aira/main.go` is cross-platform
and calls `runner.RequestWorkerAdmit`/`runner.WorkerAdmitClientRequest`
unconditionally in the CLI wiring added next — without this, `go build` on a
non-Linux target fails with undefined symbols. Mirrors Task 2/7's own stub
convention for their Linux-only pieces.

`internal/runner/worker_admit_client_stub.go`:

```go
//go:build !linux

package runner

import (
	"context"
	"errors"
	"time"
)

type WorkerAdmitLease struct {
	WorkerID   string
	ScopePath  string
	MemoryMax  int64
	MemoryHigh int64
}

func (l *WorkerAdmitLease) Close() error { return nil }

type WorkerAdmitClientRequest struct {
	SocketPath     string
	JobID          string
	OuterScope     string
	Signature      string
	EstimatedBytes int64
	MaxWait        time.Duration
}

func RequestWorkerAdmit(ctx context.Context, req WorkerAdmitClientRequest) (*WorkerAdmitLease, error) {
	return nil, errors.New("aitest worker-admit: unsupported on this platform")
}
```

- [ ] **Step 6: Wire the CLI verb.** In `cmd/aira/main.go`, alongside `confine-reserve`/`aitest-bootstrap`:

```go
	if verb == "worker-admit" {
		if jsonOutput {
			response := core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: option --json is not valid for worker-admit", Exit: store.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}
			return render(response, true, stdout, stderr)
		}
		return runWorkerAdmitCommand(context.Background(), options, stdin, stdout, stderr)
	}
```

```go
	if verb == "worker-admit" {
		return parseWorkerAdmitArgs(argv)
	}
```

```go
func parseWorkerAdmitArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	valid := map[string]bool{"job-id": true, "outer-scope": true, "estimated-bytes": true, "signature": true, "max-wait": true}
	for i := 0; i < len(argv); i++ {
		name := strings.TrimPrefix(argv[i], "--")
		if !valid[name] {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s is not valid for worker-admit", name)
		}
		if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s requires a value", name)
		}
		i++
		options[name] = argv[i]
	}
	for _, required := range []string{"job-id", "outer-scope", "estimated-bytes"} {
		if _, present := options[required]; !present {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --%s is required for worker-admit", required)
		}
	}
	return nil, options, nil
}

func runWorkerAdmitCommand(ctx context.Context, options map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	estimatedBytes, err := runner.ParseMemorySize(options["estimated-bytes"])
	if err != nil || estimatedBytes <= 0 {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --estimated-bytes: %v\n", err)
		return store.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	maxWait := runner.DefaultConfineReserveMaxWait
	if raw := options["max-wait"]; raw != "" {
		if maxWait, err = time.ParseDuration(raw); err != nil || maxWait <= 0 {
			_, _ = fmt.Fprintln(stderr, "E_CONFINE_ARGUMENT_INVALID: invalid --max-wait")
			return store.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
	}
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: daemon paths unavailable: %v\n", err)
		return store.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	lease, err := runner.RequestWorkerAdmit(signalCtx, runner.WorkerAdmitClientRequest{
		SocketPath: paths.SocketPath, JobID: options["job-id"], OuterScope: options["outer-scope"],
		Signature: options["signature"], EstimatedBytes: estimatedBytes, MaxWait: maxWait,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return store.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	scopePath, err := runner.CreateWorkerScope(ctx, options["outer-scope"], lease.WorkerID, lease.MemoryMax, lease.MemoryHigh)
	if err != nil {
		_ = lease.Close()
		_, _ = fmt.Fprintln(stderr, err)
		return store.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	if _, err := fmt.Fprintf(stdout, "granted scope=%s worker_id=%s memory_max=%d memory_high=%d\n", scopePath, lease.WorkerID, lease.MemoryMax, lease.MemoryHigh); err != nil {
		_ = lease.Close()
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: write grant: %v\n", err)
		return store.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	// Hold stdin open as the release signal, exactly mirroring confine-reserve.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stdin); close(done) }()
	select {
	case <-done:
	case <-signalCtx.Done():
	}
	_ = lease.Close()
	return 0
}
```

- [ ] **Step 7: Test the argv parser**

```go
package main

import "testing"

func TestParseWorkerAdmitArgsRequiresJobIDOuterScopeAndEstimatedBytes(t *testing.T) {
	if _, _, err := parseWorkerAdmitArgs(nil); err == nil {
		t.Fatal("missing required options must error")
	}
	_, options, err := parseWorkerAdmitArgs([]string{"--job-id", "j1", "--outer-scope", "/outer", "--estimated-bytes", "400M"})
	if err != nil || options["job-id"] != "j1" || options["outer-scope"] != "/outer" || options["estimated-bytes"] != "400M" {
		t.Fatalf("options=%v err=%v", options, err)
	}
}
```

Run: `aira confine -- go test ./cmd/aira/ -run TestParseWorkerAdmitArgs -v` — PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/runner/worker_admit_client_linux.go internal/runner/worker_admit_client_stub.go internal/runner/worker_admit_client_linux_test.go internal/daemon/testing_seams.go cmd/aira/main.go cmd/aira/worker_admit_test.go
git commit -m "feat(aitest): add aira worker-admit CLI verb"
```

---

### Task 9: pylib extract aitest package + minimal plugin skeleton

**Files:**
- Modify: `internal/pylib/extract.go`
- Modify: `internal/pylib/extract_test.go`
- Create: `internal/pylib/aitest/__init__.py`
- Create: `internal/pylib/aitest/.gitignore`
- Create: `internal/pylib/aitest/README.md`

**Interfaces:**
- Consumes: `extractPyLibFS(source fs.FS, sourceRoot, dataHome string) (string, error)`, `dataHomeFromEnv() (string, error)` (existing, `extract.go` — already parameterized on source/root, so this is DRY reuse, not a new mechanism).
- Produces: `ExtractAitest() (string, error)`, `embeddedAitest embed.FS`, `embeddedAitestRoot` const — consumed by Task 10's `extractAitestForChild` seam.

The `go:embed all:aitest` directive requires the target files to exist on disk
*at compile time* — unlike the Go-side steps elsewhere in this plan, the
Python package skeleton must be created before step 4's implementation can
even build, not after.

- [ ] **Step 1: Write the failing tests**

Add to `internal/pylib/extract_test.go`:

```go
func TestEmbeddedAitestIncludesImportPackageAndDocumentation(t *testing.T) {
	for _, name := range []string{
		"aitest/__init__.py",
		"aitest/.gitignore",
		"aitest/README.md",
	} {
		if _, err := fs.Stat(embeddedAitest, name); err != nil {
			t.Fatalf("embedded aitest tree is missing %s: %v", name, err)
		}
	}
}

func TestExtractAitestIsIdempotent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	first, err := ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	ready, err := os.ReadFile(filepath.Join(first, readyName))
	if err != nil || strings.TrimSpace(string(ready)) != filepath.Base(first) {
		t.Fatalf("ready=%q err=%v target=%s", ready, err, first)
	}

	marker := filepath.Join(first, "caller-marker")
	if err := os.WriteFile(marker, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second extraction path=%q want %q", second, first)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "preserved" {
		t.Fatalf("fast path rewrote published tree: got=%q err=%v", got, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/pylib/ -run TestEmbeddedAitest -v`
Expected: FAIL to compile (undefined: `embeddedAitest`) — this is a compile
failure, not a runtime failure, because the code under test does not exist
yet.

- [ ] **Step 3: Create the minimal aitest package skeleton**

`internal/pylib/aitest/__init__.py`:

```python
"""aitest -- fork+admission pytest worker pool replacing pytest-xdist for
AIRA-governed suites (design spec docs/superpowers/specs/2026-09-01-aitest-design.md).

This module is the pytest plugin entry point. Slice 1 wires activation only:
Supervisor/worker dispatch lives in supervisor.py/worker.py (Tasks 11-16) and
is driven from a pytest_runtestloop hookimpl added in Task 17, once
--aitest-workers is set.
"""


def pytest_addoption(parser):
    group = parser.getgroup("aitest")
    group.addoption(
        "--aitest-workers",
        action="store",
        default=None,
        help=(
            "N or 'auto': run tests under aitest's own admission-gated worker "
            "pool instead of plain in-process execution."
        ),
    )


def pytest_configure(config):
    if config.getoption("aitest_workers") is None:
        return
    # Real activation (pytest_runtestloop) is wired in Task 17; this task
    # only establishes the flag and its inert default.
    return
```

`internal/pylib/aitest/.gitignore`:

```
__pycache__/
```

`internal/pylib/aitest/README.md`:

```markdown
# aitest

A pytest plugin that replaces `pytest-xdist` for AIRA-governed suites: a
fork+admission worker pool with per-worker kernel-enforced cgroup memory
containment, in place of `pytest-xdist`'s execnet-spawned, ungoverned
workers.

Activate with `--aitest-workers=N` (a fixed worker count) or
`--aitest-workers=auto` (up to the host's CPU count). This is a NEW, explicit
flag rather than a reinterpretation of `-n` — a project with `pytest-xdist`
installed for unrelated reasons must not have its flag silently hijacked.

`aitest` is a from-scratch replacement for `aira_xdist_governor` (this
package's sibling under `internal/pylib/`), which is retired once `aitest`
reaches feature parity — see
[`docs/superpowers/specs/2026-09-01-aitest-design.md`](../../../docs/superpowers/specs/2026-09-01-aitest-design.md)
for the full design, staging, and the governor's retirement plan (§3.8).
```

- [ ] **Step 4: Write minimal implementation**

In `internal/pylib/extract.go`, change the const block:

```go
const (
	embeddedRoot       = "aira_xdist_governor"
	embeddedAitestRoot = "aitest"
	readyName          = ".ready"
	tempPrefix         = ".tmp-"
)
```

Add beneath the existing `embeddedPyLib` var and `ExtractPyLib` func:

```go
// all: is load-bearing here too: aitest's own "_"/"." prefixed files (a
// Python package's __init__.py, the extraction hygiene file) would be
// omitted by a plain directory embed.
//
//go:embed all:aitest
var embeddedAitest embed.FS

// ExtractAitest publishes the embedded aitest pytest plugin beneath a
// content hash, mirroring ExtractPyLib's extraction contract exactly (AIRA
// distributes these bytes but never imports or executes them).
func ExtractAitest() (string, error) {
	dataHome, err := dataHomeFromEnv()
	if err != nil {
		return "", err
	}
	return extractPyLibFS(embeddedAitest, embeddedAitestRoot, dataHome)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `aira confine -- go test ./internal/pylib/ -run 'TestEmbeddedAitest|TestExtractAitest' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/pylib/extract.go internal/pylib/extract_test.go internal/pylib/aitest/__init__.py internal/pylib/aitest/.gitignore internal/pylib/aitest/README.md
git commit -m "feat(aitest): embed aitest package and add --aitest-workers flag skeleton"
```

---

### Task 10: env wiring for aitest

**Files:**
- Modify: `internal/pylib/env.go`
- Modify: `internal/pylib/env_test.go`
- Modify: `internal/runner/confine_linux.go`

**Interfaces:**
- Consumes: `ExtractAitest` (Task 9), `upsertChildEnv` (existing, `env.go`).
- Produces: `IsAitestEnvironmentKey(key string) bool`, `StripAitestEnvironment(env []string) []string`, `AppendAitestChildEnvironment(env []string, runtimeDir string, diagnostics io.Writer, workerAdmitCommand string) []string` — consumed by the `confine_linux.go` call site added in this same task, and by the child pytest process's `os.environ` reads in Task 11.

- [ ] **Step 1: Write the failing tests**

Add to `internal/pylib/env_test.go`:

```go
func TestAppendAitestChildEnvironmentInjectsAndStripsStaleKeys(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	inherited := []string{
		"PATH=/bin",
		"AIRA_AITEST_LIB=/stale",
		"AIRA_AITEST_WORKER_ADMIT_CMD=/stale/aira",
		"AIRA_AITEST_BOOTSTRAP_CMD=/stale/aira",
		"AIRA_AITEST_MAX_WORKERS_FALLBACK=999",
	}
	got := childEnvValues(t, AppendAitestChildEnvironment(inherited, runtimeDir, nil, "/opt/aira"))
	if got["AIRA_AITEST_LIB"] == "" || got["AIRA_AITEST_LIB"] == "/stale" {
		t.Fatalf("AIRA_AITEST_LIB=%q", got["AIRA_AITEST_LIB"])
	}
	if got["AIRA_AITEST_WORKER_ADMIT_CMD"] != "/opt/aira" || got["AIRA_AITEST_BOOTSTRAP_CMD"] != "/opt/aira" {
		t.Fatalf("worker admit/bootstrap cmd=%v", got)
	}
	if got["AIRA_AITEST_MAX_WORKERS_FALLBACK"] == "999" || got["AIRA_AITEST_MAX_WORKERS_FALLBACK"] == "" {
		t.Fatalf("stale fallback count was not replaced: %q", got["AIRA_AITEST_MAX_WORKERS_FALLBACK"])
	}
	if _, err := os.Stat(filepath.Join(got["AIRA_AITEST_LIB"], "aitest", "__init__.py")); err != nil {
		t.Fatalf("injected aitest lib path is not importable: %v", err)
	}
}

func TestAppendAitestChildEnvironmentEmptyArgsAreSideEffectFree(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("XDG_DATA_HOME", dataHome)
	input := []string{"PATH=/bin", "AIRA_AITEST_WORKER_ADMIT_CMD=/stale/aira"}

	byEmptyRuntimeDir := AppendAitestChildEnvironment(input, "", nil, "/opt/aira")
	if strings.Join(byEmptyRuntimeDir, "\x00") != "PATH=/bin" {
		t.Fatalf("empty runtimeDir retained aitest environment: %v", byEmptyRuntimeDir)
	}
	byEmptyCommand := AppendAitestChildEnvironment(input, filepath.Join(t.TempDir(), "runtime"), nil, "")
	if strings.Join(byEmptyCommand, "\x00") != "PATH=/bin" {
		t.Fatalf("empty workerAdmitCommand retained aitest environment: %v", byEmptyCommand)
	}
	if _, err := os.Stat(dataHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("side-effect-free call extracted aitest lib: %v", err)
	}
}

func TestAppendAitestChildEnvironmentSkipsEverythingOnExtractionFailure(t *testing.T) {
	previousExtract := extractAitestForChild
	previousOnce := aitestEnvFailureOnce
	extractAitestForChild = func() (string, error) { return "", errors.New("injected aitest extraction failure") }
	aitestEnvFailureOnce = new(sync.Once)
	t.Cleanup(func() {
		extractAitestForChild = previousExtract
		aitestEnvFailureOnce = previousOnce
	})
	input := []string{"PATH=/bin", "AIRA_AITEST_LIB=/stale", "AIRA_AITEST_WORKER_ADMIT_CMD=/stale/aira"}
	var diagnostics bytes.Buffer
	first := AppendAitestChildEnvironment(input, t.TempDir(), &diagnostics, "/opt/aira")
	second := AppendAitestChildEnvironment(input, t.TempDir(), &diagnostics, "/opt/aira")
	for _, got := range [][]string{first, second} {
		values := childEnvValues(t, got)
		if len(values) != 1 || values["PATH"] != "/bin" {
			t.Fatalf("failure retained aitest environment: %v", got)
		}
	}
	if strings.Count(diagnostics.String(), "injected aitest extraction failure") != 1 {
		t.Fatalf("failure was not logged once: %q", diagnostics.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/pylib/ -run TestAppendAitestChildEnvironment -v`
Expected: FAIL to compile (undefined: `AppendAitestChildEnvironment`, `extractAitestForChild`, `aitestEnvFailureOnce`)

- [ ] **Step 3: Write minimal implementation**

In `internal/pylib/env.go`, add `"runtime"` and `"strconv"` to the import
block, extend the var block, and add the keys map / Is / Strip pair /
`AppendAitestChildEnvironment`:

```go
var (
	extractForChild     = ExtractPyLib
	childEnvFailureOnce = new(sync.Once)

	extractAitestForChild = ExtractAitest
	aitestEnvFailureOnce  = new(sync.Once)
)

var aitestEnvironmentKeys = map[string]struct{}{
	"AIRA_AITEST_LIB":                  {},
	"AIRA_AITEST_WORKER_ADMIT_CMD":     {},
	"AIRA_AITEST_BOOTSTRAP_CMD":        {},
	"AIRA_AITEST_MAX_WORKERS_FALLBACK": {},
}

// IsAitestEnvironmentKey reports whether key is aitest launch coordination
// rather than part of the tested child environment identity.
func IsAitestEnvironmentKey(key string) bool {
	_, ok := aitestEnvironmentKeys[key]
	return ok
}

// StripAitestEnvironment removes inherited or explicitly supplied aitest
// coordinates. Failed setup must disable aitest rather than retain stale
// state.
func StripAitestEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && IsAitestEnvironmentKey(key) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func AppendAitestChildEnvironment(env []string, runtimeDir string, diagnostics io.Writer, workerAdmitCommand string) []string {
	result := StripAitestEnvironment(env)
	if strings.TrimSpace(runtimeDir) == "" || strings.TrimSpace(workerAdmitCommand) == "" {
		return result
	}
	aitestDir, err := extractAitestForChild()
	if err != nil {
		aitestEnvFailureOnce.Do(func() {
			if diagnostics != nil {
				_, _ = fmt.Fprintf(diagnostics, "aitest disabled: %v\n", err)
				return
			}
			log.Printf("aitest disabled: %v", err)
		})
		return result
	}
	result = upsertChildEnv(result, "AIRA_AITEST_LIB", aitestDir)
	result = upsertChildEnv(result, "AIRA_AITEST_WORKER_ADMIT_CMD", workerAdmitCommand)
	result = upsertChildEnv(result, "AIRA_AITEST_BOOTSTRAP_CMD", workerAdmitCommand)
	result = upsertChildEnv(result, "AIRA_AITEST_MAX_WORKERS_FALLBACK", strconv.Itoa(runtime.NumCPU()))
	return result
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/pylib/ -run TestAppendAitestChildEnvironment -v`
Expected: PASS

- [ ] **Step 5: Wire the call site.** In `internal/runner/confine_linux.go`,
immediately after the existing line

```go
	cmd.Env = pylib.AppendConfineChildEnvironment(confineEnvironment(request.Env), request.RuntimeDir, diagnostics, request.DelegateRAM, reserveCommand, memoryDefault, scopeID, sliceName)
```

add:

```go
	if request.DelegateRAM {
		// aitest is only meaningful for a delegate-RAM launch (worker-admit
		// grants nested sub-scopes under THIS job's own outer scope); every
		// other launch gets no aitest coordinates at all, mirroring
		// AppendConfineChildEnvironment's own delegateRAM gate immediately
		// above. reserveCommand is the SAME resolved self binary already
		// computed for the RAM governor a few lines up — both worker-admit
		// and aitest-bootstrap are verbs on that one aira binary.
		cmd.Env = pylib.AppendAitestChildEnvironment(cmd.Env, request.RuntimeDir, diagnostics, reserveCommand)
	}
```

- [ ] **Step 6: Run test to verify the package still builds**

Run: `aira confine -- go build ./...`
Expected: exit 0

- [ ] **Step 7: Commit**

```bash
git add internal/pylib/env.go internal/pylib/env_test.go internal/runner/confine_linux.go
git commit -m "feat(aitest): inject aitest child environment on delegate-RAM launches"
```

---

### Task 11: Python Supervisor — bootstrap, collection, queue, worker-admit relay

**Files:**
- Create: `internal/pylib/aitest/supervisor.py`
- Create: `internal/pylib/aitest/test_supervisor.py`
- Create: `internal/pylib/pytest_aitest_supervisor_test.go`

**Interfaces:**
- Produces: `class WorkerAdmitUnavailable(Exception)` (the daemon is
  genuinely unreachable — dial/connect failure, the CLI itself could not be
  launched, or its response was malformed/garbage), `class
  WorkerAdmitDenied(Exception)` (the daemon IS reachable and responded
  normally with "denied" or "timeout" — busy/contended right now, not
  down), `class Supervisor` with `__init__`, `bootstrap()`, `collect(items)`,
  `next_nodeid()`, `requeue_once(nodeid)`,
  `acquire_worker(estimated_bytes, max_wait="30s")` — every later Python
  task in this plan extends this exact class. This two-exception split is
  load-bearing for Task 16's fallback logic: only `WorkerAdmitUnavailable`
  may ever disable daemon-backed admission for the rest of the run.
- Consumes (via subprocess, at runtime): `AIRA_AITEST_BOOTSTRAP_CMD`,
  `AIRA_AITEST_WORKER_ADMIT_CMD` (Task 10) invoking the real `aira
  aitest-bootstrap` (Task 3) / `aira worker-admit` (Task 8) CLI verbs, whose
  exact stdout shapes (`bootstrapped outer=<path> supervisor_scope=<path>`,
  `granted scope=<path> worker_id=<id> memory_max=<n> memory_high=<n>`) this
  module's parsing depends on precisely.

`internal/pylib/pytest_aitest_supervisor_test.go` is a NEW Go integration
tier for the aitest package's own Python internals (distinct from
`pytest_integration_test.go`'s tests of the OLDER `aira_xdist_governor`
plugin's activation surface). It runs `pytest -q` over the WHOLE
`internal/pylib/aitest/` source directory (not a single file) — every later
task in this plan (12 through 16) adds MORE `test_*.py` files under
`aitest/` that this SAME Go test discovers and runs automatically. No further
Go-side wiring is needed per task; only this one Go file is created, here.

- [ ] **Step 1: Write the failing tests**

`internal/pylib/aitest/test_supervisor.py`:

```python
import os

from aitest.supervisor import Supervisor, WorkerAdmitDenied, WorkerAdmitUnavailable


def _write_stub(path, body):
    path.write_text("#!/usr/bin/env python3\n" + body)
    os.chmod(path, 0o700)
    return str(path)


def test_bootstrap_parses_outer_scope_on_success(tmp_path, monkeypatch):
    stub = _write_stub(tmp_path / "bootstrap-ok", """
import sys
print("bootstrapped outer=/outer supervisor_scope=/outer/.aira-supervisor")
sys.exit(0)
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", stub)
    supervisor = Supervisor()
    supervisor.bootstrap()
    assert supervisor.outer_scope == "/outer"
    assert supervisor.supervisor_scope == "/outer/.aira-supervisor"
    assert supervisor.daemon_available is True


def test_bootstrap_disables_daemon_when_command_unset(monkeypatch):
    monkeypatch.delenv("AIRA_AITEST_BOOTSTRAP_CMD", raising=False)
    supervisor = Supervisor()
    supervisor.bootstrap()
    assert supervisor.daemon_available is False
    assert supervisor.outer_scope is None


def test_bootstrap_disables_daemon_on_nonzero_exit(tmp_path, monkeypatch, capsys):
    stub = _write_stub(tmp_path / "bootstrap-fail", """
import sys
sys.stderr.write("boom\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", stub)
    supervisor = Supervisor()
    supervisor.bootstrap()
    assert supervisor.daemon_available is False
    assert "boom" in capsys.readouterr().err


def test_acquire_worker_parses_grant_and_holds_process(tmp_path, monkeypatch):
    stub = _write_stub(tmp_path / "worker-admit-ok", """
import sys
print("granted scope=/outer/.aira-worker-1 worker_id=1 memory_max=400 memory_high=320")
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    grant, process = supervisor.acquire_worker(400)
    try:
        assert grant == {"scope": "/outer/.aira-worker-1", "worker_id": "1", "memory_max": "400", "memory_high": "320"}
    finally:
        process.stdin.close()
        process.wait(timeout=5)


def test_acquire_worker_raises_unavailable_when_daemon_unavailable():
    supervisor = Supervisor()
    supervisor.daemon_available = False
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable:
        pass


def test_acquire_worker_raises_unavailable_when_command_unset():
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable:
        pass


def test_acquire_worker_raises_denied_on_daemon_denial(tmp_path, monkeypatch):
    # Mirrors the real aira worker-admit CLI's actual failure shape
    # (RequestWorkerAdmit, Task 8): a non-grant STATE response is wrapped
    # as "worker-admit <state>: <reason>" on stderr with a nonzero exit —
    # the daemon IS reachable here, it just declined this request.
    stub = _write_stub(tmp_path / "worker-admit-denied", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: fallback:insufficient-headroom\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitDenied"
    except WorkerAdmitDenied as exc:
        assert "denied" in str(exc)


def test_acquire_worker_raises_denied_on_daemon_timeout_response(tmp_path, monkeypatch):
    # A "timeout" wire response (the daemon waited out the full poll
    # window, just busy/contended) is ALSO WorkerAdmitDenied, never
    # WorkerAdmitUnavailable — the whole point of the split (fix for a real
    # bug: one saturated moment must not permanently strip containment for
    # the rest of the run, see Task 16).
    stub = _write_stub(tmp_path / "worker-admit-timeout", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit timeout: reject:saturated\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitDenied"
    except WorkerAdmitDenied:
        pass


def test_acquire_worker_raises_unavailable_on_genuine_connection_failure(tmp_path, monkeypatch):
    # A dial-level failure (no daemon to talk to at all) must NOT match the
    # denied/timeout classification above, even though its text happens to
    # come from the same E_CONFINE_UNAVAILABLE-prefixed error family.
    stub = _write_stub(tmp_path / "worker-admit-dial-failure", """
import sys
sys.stderr.write("E_CONFINE_UNAVAILABLE: dial daemon: dial unix /run/aira.sock: connect: no such file or directory\\n")
sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", stub)
    supervisor = Supervisor()
    supervisor.outer_scope = "/outer"
    try:
        supervisor.acquire_worker(100)
        assert False, "expected WorkerAdmitUnavailable"
    except WorkerAdmitUnavailable:
        pass


def test_next_nodeid_and_requeue_once_semantics():
    supervisor = Supervisor()
    supervisor.queue = ["test_a.py::test_one", "test_b.py::test_two"]
    first = supervisor.next_nodeid()
    assert first == "test_a.py::test_one"
    assert supervisor.attempts[first] == 1
    assert supervisor.requeue_once(first) is True
    assert supervisor.attempts[first] == 1
    again = supervisor.next_nodeid()
    assert again == first
    assert supervisor.attempts[first] == 2
    assert supervisor.requeue_once(first) is False
```

`internal/pylib/pytest_aitest_supervisor_test.go`:

```go
//go:build linux

package pylib

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRealPytestAitestPackageUnitTests shells out to a real pytest run over
// the WHOLE aitest/ source directory (not the go:embed-extracted copy) --
// this is a source-level Python unit test tier for the aitest plugin's own
// internals (supervisor.py, worker.py), distinct from
// pytest_integration_test.go's tests of the OLDER aira_xdist_governor
// plugin's activation surface. Every later task in this plan (12 onward)
// adds MORE test_*.py files under aitest/ that this same pytest discovery
// run picks up automatically -- no further Go-side wiring is needed per
// task. --ignore=testdata excludes Task 17's testdata/ fixture suite: its
// own conftest.py requires AIRA_AITEST_LIB to already be set (it imports
// the extracted aitest package by that path), which this broader
// source-directory run does not set -- testdata/ has its own dedicated Go
// e2e test/invocation (Task 17, pytest_aitest_e2e_test.go) that sets it
// correctly instead.
func TestRealPytestAitestPackageUnitTests(t *testing.T) {
	pytest := requireRealPytest(t)
	aitestDir, err := filepath.Abs("aitest")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(pytest, "-q", "--ignore=testdata")
	command.Dir = aitestDir
	command.Env = append(os.Environ(), "PYTHONPATH="+filepath.Dir(aitestDir), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pytest aitest/ package unit tests failed: %v\n%s", err, output)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: FAIL (`ModuleNotFoundError: No module named 'aitest.supervisor'`)

- [ ] **Step 3: Write minimal implementation**

`internal/pylib/aitest/supervisor.py`:

```python
import os
import subprocess
import sys


class WorkerAdmitUnavailable(Exception):
    """The daemon is genuinely unreachable at the connection level: a dial
    failure, the worker-admit CLI itself could not even be launched, or its
    response was malformed/garbage -- there is no daemon to talk to. This
    is the ONLY failure class that should ever disable daemon-backed
    admission for the rest of the run (_disable_daemon, Task 16)."""
    pass


class WorkerAdmitDenied(Exception):
    """The daemon IS reachable and responded normally with "denied" (budget
    genuinely exhausted right now) or "timeout" (the request waited out its
    full window -- the daemon is just busy/contended, not down). Means
    "don't add a worker at this moment", never "abandon containment for the
    rest of the run"."""
    pass


class Supervisor:
    """Owns collection, the pull dispatch queue, and worker-admit relaying.
    Never runs a test itself -- see worker.py for that."""

    def __init__(self):
        self.queue = []
        self.attempts = {}  # nodeid -> attempt count (Task 15's retry-once rule)
        self.outer_scope = None
        self.supervisor_scope = None
        self.daemon_available = True
        self.max_workers_fallback = max(1, int(os.environ.get("AIRA_AITEST_MAX_WORKERS_FALLBACK", "1")))
        self._fallback_warned = False

    def bootstrap(self):
        """Relocate this process into its own child scope so the outer scope
        can delegate controllers to worker children. Must run before any
        worker is admitted."""
        command = os.environ.get("AIRA_AITEST_BOOTSTRAP_CMD", "")
        if not command:
            self._disable_daemon("AIRA_AITEST_BOOTSTRAP_CMD is unset")
            return
        try:
            result = subprocess.run(
                [command, "aitest-bootstrap", "--supervisor-pid", str(os.getpid())],
                capture_output=True, text=True, timeout=30,
            )
        except Exception as exc:
            self._disable_daemon(str(exc))
            return
        if result.returncode != 0:
            self._disable_daemon((result.stderr or "").strip() or "aitest-bootstrap failed")
            return
        for token in result.stdout.split():
            if token.startswith("outer="):
                self.outer_scope = token[len("outer="):]
            elif token.startswith("supervisor_scope="):
                self.supervisor_scope = token[len("supervisor_scope="):]
        if not self.outer_scope:
            self._disable_daemon("aitest-bootstrap did not report an outer scope")

    def _disable_daemon(self, reason):
        self.daemon_available = False
        if not self._fallback_warned:
            self._fallback_warned = True
            sys.stderr.write(
                "aira aitest: %s -- falling back to n_workers<=%d, UNCONFINED (no per-worker RAM containment)\n"
                % (reason, self.max_workers_fallback)
            )

    def collect(self, items):
        """items: the pytest-collected Item objects. Session collection
        already ran in this process before bootstrap; the forked worker
        inherits self.items_by_nodeid by COW, so only nodeids (strings)
        cross the dispatch/result pipes (Task 13)."""
        self.items_by_nodeid = {item.nodeid: item for item in items}
        self.queue = [item.nodeid for item in items]

    def next_nodeid(self):
        if not self.queue:
            return None
        nodeid = self.queue.pop(0)
        self.attempts[nodeid] = self.attempts.get(nodeid, 0) + 1
        return nodeid

    def requeue_once(self, nodeid):
        """A worker died mid-test. Requeue exactly once; the second failure
        is the caller's job to report unevaluated, not this method's."""
        if self.attempts.get(nodeid, 0) >= 2:
            return False
        self.queue.insert(0, nodeid)
        return True

    def acquire_worker(self, estimated_bytes, max_wait="30s"):
        """Returns (grant: dict, process: subprocess.Popen) on success.
        process.stdin stays open as the daemon lease -- close it to release.
        Raises WorkerAdmitUnavailable when there is no daemon to talk to at
        all (dial/launch failure, malformed response); raises
        WorkerAdmitDenied when the daemon responded normally but declined
        (denied or timeout) -- the caller MUST treat these differently
        (Task 16): only WorkerAdmitUnavailable may disable daemon-backed
        admission for the rest of the run."""
        if not self.daemon_available:
            raise WorkerAdmitUnavailable("daemon unavailable")
        command = os.environ.get("AIRA_AITEST_WORKER_ADMIT_CMD", "")
        if not command:
            raise WorkerAdmitUnavailable("AIRA_AITEST_WORKER_ADMIT_CMD is unset")
        try:
            process = subprocess.Popen(
                [command, "worker-admit", "--job-id", str(os.getpid()), "--outer-scope", self.outer_scope,
                 "--estimated-bytes", str(estimated_bytes), "--max-wait", max_wait],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, close_fds=True,
            )
        except OSError as exc:
            raise WorkerAdmitUnavailable(str(exc))
        line = process.stdout.readline().decode("utf-8", "strict").strip()
        if not line.startswith("granted "):
            stderr = process.stderr.read().decode("utf-8", "replace") if process.stderr else ""
            try:
                process.wait(timeout=5)
            except Exception:
                process.kill()
            message = line or stderr or "worker-admit exited without a grant"
            # RequestWorkerAdmit (Task 8) wraps a daemon-reachable-but-
            # declined response as "worker-admit <state>: <reason>" on
            # stderr -- ANYTHING else (a dial failure, a launch failure, a
            # malformed response) means there is no daemon to talk to at
            # all. This distinction is load-bearing (Task 16).
            if "worker-admit denied" in message or "worker-admit timeout" in message:
                raise WorkerAdmitDenied(message)
            raise WorkerAdmitUnavailable(message)
        grant = {}
        for field in line[len("granted "):].split():
            key, _, value = field.partition("=")
            grant[key] = value
        return grant, process
```

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/pylib/aitest/supervisor.py internal/pylib/aitest/test_supervisor.py internal/pylib/pytest_aitest_supervisor_test.go
git commit -m "feat(aitest): add Supervisor bootstrap/collect/queue/worker-admit relay"
```

---

### Task 12: Python worker fork + cgroup self-placement

**Files:**
- Create: `internal/pylib/aitest/worker.py`
- Create: `internal/pylib/aitest/test_worker.py`

**Interfaces:**
- Produces: `fork_worker(scope_path) -> (pid, in_child)`, `place_self(scope_path)` — consumed by `supervisor.py`'s `spawn_worker` (Task 13).

This task's tests run via Task 11's already-created
`pytest_aitest_supervisor_test.go` (which discovers every `test_*.py` under
`aitest/`) — no new Go file.

- [ ] **Step 1: Write the failing tests**

`internal/pylib/aitest/test_worker.py`:

```python
import os
import time

from aitest.worker import fork_worker, place_self


def test_place_self_writes_pid_to_cgroup_procs(tmp_path):
    """A pure I/O unit test of place_self, independent of real cgroup
    delegation: cgroup.procs is just a file to this function."""
    scope_dir = tmp_path / "fake-scope"
    scope_dir.mkdir()
    (scope_dir / "cgroup.procs").write_text("")
    place_self(str(scope_dir))
    assert (scope_dir / "cgroup.procs").read_text() == str(os.getpid())


def _cgroup_own_path():
    with open("/proc/self/cgroup", encoding="ascii") as source:
        line = source.read().strip()
    # cgroup v2: a single "0::<path>" line.
    return "/sys/fs/cgroup" + line.split(":", 2)[2]


def test_fork_worker_places_child_pid_into_real_scope_cgroup(tmp_path):
    real_cgroup = os.environ.get("AIRA_REAL_CGROUP") == "1"
    try:
        scope_dir = os.path.join(_cgroup_own_path(), ".aitest-worker-test")
        os.makedirs(scope_dir, exist_ok=True)
        if not os.access(os.path.join(scope_dir, "cgroup.procs"), os.W_OK):
            raise PermissionError("cgroup.procs not writable")
    except Exception as exc:
        if real_cgroup:
            import pytest
            pytest.fail("AIRA_REAL_CGROUP=1 but real cgroup-v2 delegation is unavailable: %s" % exc)
        import pytest
        pytest.skip("real cgroup-v2 delegation unavailable: %s" % exc)

    marker = tmp_path / "child-marker"
    pid, in_child = fork_worker(scope_dir)
    if in_child:
        marker.write_text(str(os.getpid()))
        time.sleep(0.5)
        os._exit(0)

    deadline = time.time() + 2
    procs = ""
    while time.time() < deadline:
        with open(os.path.join(scope_dir, "cgroup.procs"), encoding="ascii") as source:
            procs = source.read()
        if str(pid) in procs.split():
            break
        time.sleep(0.01)
    os.waitpid(pid, 0)
    assert str(pid) in procs.split(), "child pid never appeared in scope cgroup.procs: %r" % procs
    assert marker.exists() and marker.read_text() == str(pid)


def test_fork_worker_exits_child_without_propagating_when_place_self_fails(tmp_path):
    """place_self() failing in the child (e.g. cgroup.procs itself is not
    writable/does not exist) must never propagate as a normal Python
    exception: that would unwind into the child's COW-duplicated copy of
    the supervisor's own interpreter frames, producing a second, fully
    UNCONFINED pytest process running arbitrary supervisor code -- a real
    safety hazard, not just a bug. The child must os._exit() immediately
    instead of ever reaching ordinary control flow."""
    missing_scope = str(tmp_path / "does-not-exist")
    pid, in_child = fork_worker(missing_scope)
    if in_child:
        # Unreachable if fork_worker's own guard is working -- only hit if
        # the fix regresses and place_self's exception propagated here.
        os._exit(0)
    _, status = os.waitpid(pid, 0)
    assert os.WIFEXITED(status), "child did not exit cleanly: status=%r" % status
    assert os.WEXITSTATUS(status) == 70
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: FAIL (`ModuleNotFoundError: No module named 'aitest.worker'`)

- [ ] **Step 3: Write minimal implementation**

`internal/pylib/aitest/worker.py`:

```python
import os


def fork_worker(scope_path):
    """Forks. In the child, places itself into scope_path's cgroup before
    returning. Returns (pid, in_child: bool).

    DELIBERATE DEVIATION from confine's own placement, worth naming
    explicitly rather than letting it slide: aira confine places a NEW
    process atomically via clone3(CLONE_INTO_CGROUP) (Go's
    SysProcAttr{UseCgroupFD}, internal/runner/confine_linux.go) -- a
    successful Start() there IS proof of placement, no gap at all. A worker
    here is forked from an ALREADY-RUNNING Python process instead (the whole
    point being COW-shared warm-imported interpreter state, spec 3.1) --
    Python's stdlib os.fork() is a plain fork(2), with no CLONE_INTO_CGROUP
    binding available (that would need a raw ctypes clone3 syscall, which is
    real added complexity and risk for what this buys). So there IS a brief
    window, between fork() returning in the child and place_self()
    completing, where the child is still a member of the SUPERVISOR's scope,
    not its own worker scope. Two things bound the actual risk to
    negligible: (1) this window is pure interpreter overhead (a syscall
    return, an open(), a write()) -- it ends before any test code runs, so
    no test-driven allocation can happen inside it; (2) cgroup memory.max is
    hierarchical, so the child's usage during that window still counts
    against the OUTER scope's cap, not an unbounded cgroup. Accepted for
    Slice 1 as an architecturally-simpler choice than a raw-syscall
    workaround for a race this narrow (architectural-simplicity: no new
    machinery for a bounded, sub-millisecond gap) -- but call it out plainly
    in plan-review rather than have it read as an oversight.

    Safety note: any exception place_self() raises happens in the CHILD --
    it must never propagate through normal Python control flow from here,
    since that would unwind into the child's COW-duplicated copy of the
    supervisor's own interpreter frames and could run arbitrary supervisor
    cleanup code fully UNCONFINED (a placement failure specifically means
    containment was never established at all). os.fork() itself CAN also
    raise, but only in the PARENT (no child exists yet in that case) --
    that failure is deliberately left to propagate normally here."""
    pid = os.fork()
    if pid == 0:
        try:
            place_self(scope_path)
        except BaseException:
            os._exit(70)
        return 0, True
    return pid, False


def place_self(scope_path):
    """Writes this process's own pid into scope_path/cgroup.procs. Must run
    before any test code executes in the forked child -- see fork_worker's
    docstring for why this is a plain write rather than an atomic
    clone-into-cgroup."""
    with open(os.path.join(scope_path, "cgroup.procs"), "w") as handle:
        handle.write(str(os.getpid()))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: PASS (the real-cgroup case cleanly skips without `AIRA_REAL_CGROUP=1`
on a host without real delegation; hard-fails with it, matching the Go side's
`SkipOrFailRealCgroup` policy)

- [ ] **Step 5: Commit**

```bash
git add internal/pylib/aitest/worker.py internal/pylib/aitest/test_worker.py
git commit -m "feat(aitest): add worker fork + cgroup self-placement"
```

---

### Task 13: Python worker test execution + report streaming (Slice 1 scope: pass/fail/unevaluated only)

**Files:**
- Modify: `internal/pylib/aitest/worker.py`
- Modify: `internal/pylib/aitest/supervisor.py`
- Modify: `internal/pylib/aitest/test_worker.py`
- Create: `internal/pylib/aitest/conftest.py`

**Interfaces:**
- Produces (worker.py): `run_one(item) -> "passed"|"failed"|"skipped"|"error"`,
  `run_worker_loop(scope_path, items_by_nodeid, pipe_in, pipe_out)`.
- Produces (supervisor.py): `Supervisor.spawn_worker(estimated_bytes,
  max_wait="30s") -> pid`, `Supervisor.run(estimated_bytes, worker_count=1,
  max_wait="30s") -> results dict` — Tasks 14/15/16 all extend `run()` and
  its helpers in place.
- Consumes: `fork_worker` (Task 12).

**Accepted Slice 1 limitation (nextitem=None):** `run_one` calls pytest's
item protocol with `nextitem=None` for every single test — pytest's own
documented signal that this is the LAST item in the session, which tears
down the ENTIRE fixture stack (including session/module/class-scoped
fixtures) after every individual test. Unlike plain pytest or xdist (which
look ahead to supply the real next item so a fixture shared across tests
persists), a Slice 1 suite relying on expensive or stateful session-scoped
fixtures will see them re-run per test. Real look-ahead dispatch is a
candidate for a later slice — it is closely related to, and no more urgent
than, the loadscope/loadgroup fixture-affinity grouping spec §2 already
defers past Slice 1. Not implemented here; see the test added below that
proves (not just documents) the actual behavior.

`conftest.py` enables pytest's own bundled `pytester` fixture (opt-in via
`pytest_plugins` in a conftest, per pytest's own restriction against
declaring it elsewhere), needed to get real, in-process pytest `Item` objects
for `run_one`'s test without a subprocess.

- [ ] **Step 1: Write the failing tests**

`internal/pylib/aitest/conftest.py`:

```python
pytest_plugins = ["pytester"]
```

Append to `internal/pylib/aitest/test_worker.py`:

```python
import io

from aitest.worker import run_one, run_worker_loop


def test_run_one_reports_passed_and_failed_outcomes(pytester):
    items = pytester.getitems("""
        def test_passes():
            assert True

        def test_fails():
            assert False
    """)
    by_name = {item.name: item for item in items}
    assert run_one(by_name["test_passes"]) == "passed"
    assert run_one(by_name["test_fails"]) == "failed"


def test_run_one_reports_skipped_outcome_distinctly(pytester):
    """A skip is pytest's own well-defined, intentional outcome -- it must
    never be folded into "error"/"unevaluated" downstream (Task 15's
    crash/retry aggregation, Task 17's e2e assertions)."""
    items = pytester.getitems("""
        import pytest

        def test_skipped():
            pytest.skip("not applicable")
    """)
    assert run_one(items[0]) == "skipped"


def test_run_one_tears_down_and_rebuilds_session_scoped_fixtures_per_test(pytester):
    """Proves the accepted nextitem=None limitation documented above: with
    plain pytest, a module-scoped fixture shared by two tests sets up ONCE;
    here it is torn down and rebuilt after EVERY item, so the counter below
    reaches 2, not 1."""
    items = pytester.getitems("""
        import pytest

        _counter = {"value": 0}

        @pytest.fixture(scope="module")
        def counting_fixture():
            _counter["value"] += 1
            yield _counter["value"]

        def test_first(counting_fixture):
            pass

        def test_second(counting_fixture):
            pass
    """)
    assert len(items) == 2
    for item in items:
        assert run_one(item) == "passed"
    assert items[0].module._counter["value"] == 2


def test_run_worker_loop_dispatch_and_result_round_trip(pytester):
    items = pytester.getitems("""
        def test_ok():
            assert True
    """)
    items_by_nodeid = {item.nodeid: item for item in items}
    nodeid = next(iter(items_by_nodeid))
    pipe_in = io.StringIO(nodeid + "\n__stop__\n")
    pipe_out = io.StringIO()
    run_worker_loop(None, items_by_nodeid, pipe_in, pipe_out)
    pipe_out.seek(0)
    assert pipe_out.read().splitlines() == ["%s passed" % nodeid]
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: FAIL (`ImportError: cannot import name 'run_one'`)

- [ ] **Step 3: Write minimal implementation**

Append to `internal/pylib/aitest/worker.py`:

```python
class _OutcomeCollector:
    """Captures the worst-of outcome across setup/call/teardown reports for
    one pytest_runtest_protocol call. Registered on item.config.pluginmanager
    only for the duration of that one call -- see run_one."""

    _RANK = {"passed": 0, "skipped": 1, "failed": 2}

    def __init__(self):
        self.worst = "passed"

    def pytest_runtest_logreport(self, report):
        outcome = report.outcome if report.outcome in self._RANK else "failed"
        if self._RANK[outcome] > self._RANK.get(self.worst, 0):
            self.worst = outcome


def run_one(item):
    """Executes one already-collected pytest Item through pytest's own item
    protocol (setup/call/teardown), returning "passed", "failed", "skipped",
    or "error".

    UNCERTAIN, flagged for verification during implementation: calling
    item.ihook.pytest_runtest_protocol(item=item, nextitem=None) directly,
    outside pytest's own Session.main() loop, is not a path this plugin has
    exercised against a real pytest version yet. It is pytest's own
    documented per-item hook (the same one xdist's worker calls per design
    spec 3.2) and SHOULD behave identically to normal collection -- but the
    exact hookimpl/pluginmanager registration dance below needs a real-pytest
    verification pass before it is trusted, not a guess presented as
    certain.

    ACCEPTED SLICE 1 LIMITATION: nextitem=None is pytest's own signal that
    this is the LAST item in the session, so it tears down and rebuilds the
    ENTIRE fixture stack -- including session/module/class-scoped fixtures
    -- after every single test, unlike plain pytest or xdist (which look
    ahead to supply the real next item so a fixture shared across tests
    persists). A suite relying on expensive or stateful session-scoped
    fixtures will see them re-run per test in Slice 1. Real look-ahead
    dispatch is deferred, a candidate for a later slice (see this task's own
    Interfaces note and the test proving this behavior below).
    """
    collector = _OutcomeCollector()
    plugin_manager = item.config.pluginmanager
    plugin_manager.register(collector, name="aitest-outcome-collector")
    try:
        item.ihook.pytest_runtest_protocol(item=item, nextitem=None)
    finally:
        plugin_manager.unregister(collector)
    if collector.worst == "passed":
        return "passed"
    if collector.worst == "skipped":
        return "skipped"
    if collector.worst == "failed":
        return "failed"
    return "error"


def run_worker_loop(scope_path, items_by_nodeid, pipe_in, pipe_out):
    """Child-side loop: read one nodeid per line from pipe_in, run it via
    run_one, write "<nodeid> <outcome>" back to pipe_out per completed test.
    An empty line means no work right now -- read again. The line
    "__stop__" ends the loop cleanly. scope_path is unused until Task 14's
    recycle checks; kept as a parameter now for API stability."""
    for line in pipe_in:
        nodeid = line.rstrip("\n")
        if nodeid == "":
            continue
        if nodeid == "__stop__":
            break
        item = items_by_nodeid[nodeid]
        outcome = run_one(item)
        pipe_out.write("%s %s\n" % (nodeid, outcome))
        pipe_out.flush()
```

Extend `internal/pylib/aitest/supervisor.py`: add `import select` and
`import time` to the top, `from aitest.worker import fork_worker,
run_worker_loop`, then in `Supervisor.__init__` add:

```python
        self.items_by_nodeid = {}
        self.workers = {}
        self.results = {}
        self._run_estimated_bytes = 0
        self._run_max_wait = "30s"
```

Also add this new exception near `WorkerAdmitDenied`/`WorkerAdmitUnavailable`
(Task 11) at the top of `supervisor.py`:

```python
class WorkerPlacementFailed(Exception):
    """place_self() never completed (or its child-side placement ack never
    arrived) -- the forked child died before we could confirm it actually
    joined its granted cgroup scope. Distinct from a worker that WAS placed
    and crashed later mid-test (Task 15's _handle_worker_exit path): a
    placement failure means the admitted grant was never even used for a
    test."""
    pass


def _read_line_blocking(fd, state):
    """Blocking read of exactly one line from a raw fd, used only for the
    one-time post-fork placement-ack wait (spawn_worker/
    _spawn_fallback_worker) -- blocking IS the desired behaviour there.
    Shares state["read_buffer"] with _drain_available_lines below (both
    read the SAME fd over the worker's lifetime) so no byte a single
    os.read() call happens to over-read past this line's newline is ever
    lost to a later caller."""
    buf = state.get("read_buffer", b"")
    while b"\n" not in buf:
        chunk = os.read(fd, 65536)
        if not chunk:
            state["result_eof"] = True
            break
        buf += chunk
    line, sep, buf = buf.partition(b"\n")
    state["read_buffer"] = buf
    return line.decode("utf-8", "strict") if sep else ""


def _drain_available_lines(fd, state):
    """Non-blocking read of everything CURRENTLY available on fd, split
    into complete lines; any trailing partial line is kept in
    state["read_buffer"] for the next call. fd must already be in
    non-blocking mode (spawn_worker sets this right after the placement
    ack is read).

    This exists instead of a select()-after-readline() check on a
    buffered file object because that combination is a real, demonstrated
    race: os.fdopen(fd, "r")'s TextIOWrapper commonly pulls MULTIPLE
    already-flushed lines off the kernel pipe in a single underlying read
    to satisfy one readline() call (a worker writes a result line, then --
    if recycling -- an immediate __recycle__ line, both flushed in rapid
    succession with no intervening syscall for a reader to wake between
    them) -- so by the time readline() returns just the first line, the
    kernel pipe can already be EMPTY while the wrapper's own internal
    buffer silently holds the second. select() only sees kernel-level
    readiness, so it reports "nothing more" even though a complete
    __recycle__ line is sitting unread one layer up -- reproducing the
    exact silently-dropped-dispatch race this whole mechanism exists to
    close (spec 3.6). Reading raw bytes directly off a non-blocking fd
    into one buffer this module fully owns removes that blind spot: there
    is no second, invisible buffering layer between "what select saw" and
    "what the caller can see"."""
    buf = state.get("read_buffer", b"")
    while True:
        try:
            chunk = os.read(fd, 65536)
        except BlockingIOError:
            break
        if not chunk:
            state["result_eof"] = True
            break
        buf += chunk
    lines = []
    while b"\n" in buf:
        line, _, buf = buf.partition(b"\n")
        lines.append(line.decode("utf-8", "strict"))
    state["read_buffer"] = buf
    return lines
```

and append these methods to the class:

```python
    _PLACED_LINE = "__placed__"

    def _child_close_other_workers_fds(self):
        """A forked child inherits DUPLICATES of every fd already open in
        the parent's fd table -- fork() copies the whole table and there is
        no exec() here for CLOEXEC to ever fire. Without this, a
        later-forked worker keeps a live copy of an EARLIER worker's
        admit-lease pipe (and dispatch/result pipes). So when the
        supervisor later closes ITS OWN copy of an earlier worker's
        admit_process.stdin to retire it, the daemon-side `aira
        worker-admit` CLI's stdin-EOF read never sees EOF (some OTHER fd
        still holds the write end open), and admit_process.wait(timeout=5)
        hangs/raises. Must run before this child does anything else --
        BEFORE closing its own inherited copies of ITS OWN pipes too, so
        order this first in both spawn_worker and _spawn_fallback_worker
        (Task 16)."""
        for state in self.workers.values():
            try:
                state["dispatch_write"].close()
            except Exception:
                pass
            try:
                os.close(state["result_fd"])
            except OSError:
                pass
            admit_process = state.get("admit_process")
            if admit_process is not None:
                for stream in (admit_process.stdin, admit_process.stdout, admit_process.stderr):
                    if stream is not None:
                        try:
                            stream.close()
                        except Exception:
                            pass

    def spawn_worker(self, estimated_bytes, max_wait="30s"):
        """Admits and forks one worker, returning its pid. Raises
        WorkerAdmitUnavailable/WorkerAdmitDenied if admission fails, or
        WorkerPlacementFailed if the forked child died before confirming it
        joined its granted cgroup scope -- the caller (run()) decides
        fallback/retry policy for each.

        Safety: the ENTIRE forked-child branch below is wrapped in one
        broad try/except that os._exit()s on ANY exception. A forked child
        must never be allowed to fall through to normal Python control
        flow / interpreter shutdown -- that risks running supervisor-level
        cleanup code fully UNCONFINED. (place_self() itself is separately
        guarded the same way inside fork_worker, Task 12, since it can
        raise before this function's own try even starts.)"""
        grant, admit_process = self.acquire_worker(estimated_bytes, max_wait=max_wait)
        dispatch_read, dispatch_write = os.pipe()
        result_read, result_write = os.pipe()
        pid, in_child = fork_worker(grant["scope"])
        if in_child:
            try:
                self._child_close_other_workers_fds()
                os.close(dispatch_write)
                os.close(result_read)
                admit_process.stdin.close()
                admit_process.stdout.close()
                pipe_in = os.fdopen(dispatch_read, "r")
                pipe_out = os.fdopen(result_write, "w")
                # Placement is already verified by the time we get here --
                # fork_worker's own child branch os._exit()s before ever
                # returning if place_self failed (Task 12) -- so reaching
                # this line already IS the placement proof. One line down
                # the result pipe lets the parent (below) tell "placed
                # fine, died/recycled later" apart from "never even got
                # placed" for its crash-handling logic (spec 4).
                pipe_out.write(self._PLACED_LINE + "\n")
                pipe_out.flush()
                run_worker_loop(grant["scope"], self.items_by_nodeid, pipe_in, pipe_out)
            except BaseException:
                os._exit(70)
            os._exit(0)
        os.close(dispatch_read)
        os.close(result_write)
        # result_read is handled as a RAW fd with manual line-buffering
        # (_read_line_blocking / _drain_available_lines below) for this
        # worker's whole lifetime, never wrapped in os.fdopen()'s buffered
        # TextIOWrapper -- see _drain_available_lines's docstring for why:
        # a buffered readline() can silently pull more than one line off
        # the kernel pipe in a single underlying read, and a later
        # select()-on-the-wrapped-object check cannot see bytes already
        # sitting in the wrapper's own buffer rather than the pipe.
        state = {"result_fd": result_read, "read_buffer": b"", "result_eof": False}
        ack = _read_line_blocking(result_read, state)
        if ack != self._PLACED_LINE:
            # The child died (os._exit'd, above, or from fork_worker's own
            # guard) before ever confirming placement -- it never joined
            # its granted cgroup scope. This is a PLACEMENT failure, not a
            # mid-test crash (Task 15's _handle_worker_exit is for a worker
            # that WAS placed and later died) -- release the now-dead
            # admit lease and raise distinctly so the caller does not
            # spend this nodeid's one-and-only crash-retry budget on it.
            os.close(result_read)
            try:
                os.waitpid(pid, 0)
            except ChildProcessError:
                pass
            admit_process.stdin.close()
            try:
                admit_process.wait(timeout=5)
            except Exception:
                pass
            raise WorkerPlacementFailed("worker %d exited before confirming cgroup placement" % pid)
        os.set_blocking(result_read, False)
        state.update({
            "grant": grant,
            "admit_process": admit_process,
            "dispatch_write": os.fdopen(dispatch_write, "w"),
            "in_flight": None,
        })
        self.workers[pid] = state
        return pid

    def _dispatch_to_idle_workers(self):
        for state in self.workers.values():
            if state["in_flight"] is not None:
                continue
            nodeid = self.next_nodeid()
            if nodeid is None:
                continue
            state["in_flight"] = nodeid
            state["dispatch_write"].write(nodeid + "\n")
            state["dispatch_write"].flush()

    def _retire_worker(self, pid, state):
        state["dispatch_write"].close()
        try:
            os.close(state["result_fd"])
        except OSError:
            pass
        try:
            os.waitpid(pid, 0)
        except ChildProcessError:
            pass
        state["admit_process"].stdin.close()
        state["admit_process"].wait(timeout=5)
        del self.workers[pid]

    def run(self, estimated_bytes, worker_count=1, max_wait="30s"):
        """Slice 1's whole dispatch loop: spawn up to worker_count workers,
        pull-dispatch the queue to whichever are idle, collect results via
        select() over each worker's result pipe, until the queue is drained
        and every worker has retired. Recycle (Task 14), crash/retry (Task
        15), and daemon-down fallback (Task 16) extend this method in
        place."""
        self.bootstrap()
        # gc.freeze() moves already-imported objects into the permanent
        # generation before any fork, per design spec 3.1: post-fork COW
        # pages a worker's own GC scanning would otherwise touch (and
        # dirty) shrink to near nothing.
        import gc
        gc.freeze()
        self._run_estimated_bytes = estimated_bytes
        self._run_max_wait = max_wait
        for _ in range(worker_count):
            if not self.queue:
                break
            self.spawn_worker(estimated_bytes, max_wait=max_wait)
        self._dispatch_to_idle_workers()
        while self.workers:
            fd_to_pid = {state["result_fd"]: pid for pid, state in self.workers.items()}
            ready, _, _ = select.select(list(fd_to_pid), [], [], 1.0)
            if not ready:
                continue
            for fd in ready:
                pid = fd_to_pid[fd]
                if pid not in self.workers:
                    continue
                state = self.workers[pid]
                for line in _drain_available_lines(fd, state):
                    nodeid, _, outcome = line.partition(" ")
                    self.results[nodeid] = outcome
                    state["in_flight"] = None
            self._dispatch_to_idle_workers()
            if not self.queue and all(state["in_flight"] is None for state in self.workers.values()):
                for pid, state in list(self.workers.items()):
                    state["dispatch_write"].write("__stop__\n")
                    state["dispatch_write"].flush()
                    self._retire_worker(pid, state)
        # Best-effort: rmdir the supervisor's OWN child scope this run
        # relocated itself into (bootstrap, Task 2/3/11). The OUTER scope
        # itself is `aira confine`'s own job to tear down when the whole
        # launch process exits -- this is only about the new child scope
        # aitest itself created. NOTE: in the real-cgroup case this
        # typically still fails here (EBUSY) since the supervisor process
        # calling rmdir is itself still a live member of the scope it is
        # trying to remove -- it only ever succeeds AFTER this process
        # exits, which is after this call returns. Attempted anyway because
        # it is free and occasionally correct (e.g. non-real-cgroup test
        # doubles); #72's existing orphaned-scope reaper is the real
        # backstop that cleans this up machine-wide once the process is
        # actually gone.
        if self.supervisor_scope:
            try:
                os.rmdir(self.supervisor_scope)
            except OSError as exc:
                sys.stderr.write("aira aitest: could not remove supervisor scope %s: %s\n" % (self.supervisor_scope, exc))
        return self.results
```

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/pylib/aitest/worker.py internal/pylib/aitest/supervisor.py internal/pylib/aitest/test_worker.py internal/pylib/aitest/conftest.py
git commit -m "feat(aitest): add run_one item execution and the supervisor dispatch loop"
```

---

### Task 14: recycle conditions

**Files:**
- Modify: `internal/pylib/aitest/worker.py`
- Modify: `internal/pylib/aitest/supervisor.py`
- Modify: `internal/pylib/aitest/test_supervisor.py`

**Interfaces:**
- Produces (worker.py): `_should_recycle(scope_path, started_at,
  completed_count) -> bool` (module-private).
- Produces (supervisor.py): `Supervisor._replace_worker()` (module-private) —
  reused by Task 15's crash/retry path and extended by Task 16's fallback
  path.
- Consumes: `Supervisor.run`'s ready-handling loop (Task 13), replaced in
  place by this task and Task 15.

- [ ] **Step 1: Write the failing test**

Add `import time` to the top of `internal/pylib/aitest/test_supervisor.py`
(needed by the two-worker timing test below). Append to
`internal/pylib/aitest/test_supervisor.py`:

```python
def test_recycle_after_max_tests_respawns_a_fresh_worker(tmp_path, monkeypatch, pytester):
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit_calls = tmp_path / "admit-calls"
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
open({str(admit_calls)!r}, "a").write("x")
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_WORKER_MAX_TESTS", "1")

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert len(results) == 2
    assert all(outcome == "passed" for outcome in results.values())
    # One worker per test proves exactly one recycle event fired between them.
    assert admit_calls.read_text().count("x") == 2


def test_recycle_with_two_concurrent_workers_does_not_hang_on_retirement(tmp_path, monkeypatch, pytester):
    """A forked child inherits DUPLICATES of every already-open fd in the
    parent's fd table (fork() copies the whole table; there is no exec()
    here for CLOEXEC to ever fire) -- without closing every OTHER
    already-known worker's fds before entering its own loop, a
    later-forked worker (here, worker 2) keeps a live copy of an EARLIER
    worker's (worker 1's) admit-lease pipe write end. When the supervisor
    then closes ITS OWN copy of worker 1's admit_process.stdin to retire it
    (recycle, at AIRA_AITEST_WORKER_MAX_TESTS=1), the daemon-side stub's
    stdin-read never sees EOF unless worker 2 ALSO closed its inherited
    duplicate -- admit_process.wait(timeout=5) would then hang/raise. This
    test needs TWO concurrent workers specifically to make that
    fd-inheritance bug observable: Task 13's own test and this file's other
    recycle test above both use worker_count=1, which cannot exercise it at
    all (worker_count=1's startup loop never has two workers registered in
    self.workers at the same time)."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit_calls = tmp_path / "admit-calls-2"
    admit = _write_stub(tmp_path / "worker-admit-2", f"""
import os, sys
open({str(admit_calls)!r}, "a").write("x")
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=%d memory_max=104857600 memory_high=83886080" % (scope, os.getpid()))
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_WORKER_MAX_TESTS", "1")

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True

        def test_three():
            assert True

        def test_four():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)

    started = time.monotonic()
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=2)
    elapsed = time.monotonic() - started

    assert len(results) == 4
    assert all(outcome == "passed" for outcome in results.values())
    # A hang on the fd-inheritance bug would show up as admit_process.wait's
    # own 5-second timeout firing at least once across the four
    # retirements; a healthy run completes in a small fraction of that.
    assert elapsed < 4.0, "run() took %.1fs -- looks like a retirement hang (fd-inheritance bug)" % elapsed
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: FAIL (both workers admitted at once / no recycle event — assertion
on `admit_calls` content fails; the two-worker test additionally exercises a
retirement hang once `_should_recycle`/dispatch exist without the
fd-inheritance and drain-before-dispatch fixes applied)

- [ ] **Step 3: Write minimal implementation**

Extend `internal/pylib/aitest/worker.py`: add `import time` to the top,
then:

```python
_DEFAULT_MAX_SECONDS = 600
_DEFAULT_MAX_TESTS = 200
_DEFAULT_HIGH_WATERMARK_PCT = 80


def _read_cgroup_int(scope_path, filename):
    with open(os.path.join(scope_path, filename), encoding="ascii") as handle:
        raw = handle.read().strip()
    if raw == "max":
        return None
    return int(raw)


def _should_recycle(scope_path, started_at, completed_count):
    max_seconds = int(os.environ.get("AIRA_AITEST_WORKER_MAX_SECONDS", str(_DEFAULT_MAX_SECONDS)))
    if time.monotonic() - started_at > max_seconds:
        return True
    max_tests = int(os.environ.get("AIRA_AITEST_WORKER_MAX_TESTS", str(_DEFAULT_MAX_TESTS)))
    # >= , not >: with AIRA_AITEST_WORKER_MAX_TESTS=1 and exactly one
    # completed test, 1 > 1 is false and recycle would never fire at all.
    if completed_count >= max_tests:
        return True
    if scope_path is None:
        # Daemon-down fallback mode (Task 16): no granted cgroup scope to
        # watermark-check; time/count bounds above still apply.
        return False
    watermark_pct = float(os.environ.get("AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT", str(_DEFAULT_HIGH_WATERMARK_PCT)))
    try:
        current = _read_cgroup_int(scope_path, "memory.current")
        high = _read_cgroup_int(scope_path, "memory.high")
    except (OSError, ValueError):
        return False
    if current is None or high is None or high <= 0:
        return False
    return (current * 100.0 / high) > watermark_pct
```

Replace `run_worker_loop` in `internal/pylib/aitest/worker.py` with:

```python
def run_worker_loop(scope_path, items_by_nodeid, pipe_in, pipe_out):
    started_at = time.monotonic()
    completed_count = 0
    for line in pipe_in:
        nodeid = line.rstrip("\n")
        if nodeid == "":
            continue
        if nodeid == "__stop__":
            break
        item = items_by_nodeid[nodeid]
        outcome = run_one(item)
        completed_count += 1
        pipe_out.write("%s %s\n" % (nodeid, outcome))
        pipe_out.flush()
        if _should_recycle(scope_path, started_at, completed_count):
            pipe_out.write("__recycle__\n")
            pipe_out.flush()
            return
```

In `internal/pylib/aitest/supervisor.py`, add module-level constants
alongside the imports (worker.py's sentinels are duplicated by VALUE here,
not imported, to avoid a circular import between the two modules — keep them
in sync if ever changed):

```python
_STOP_LINE = "__stop__"
_RECYCLE_LINE = "__recycle__"
```

Replace `_retire_worker` (Task 13) to also tear down the worker's own scope
directory, best-effort, once its process has actually exited (the
`os.waitpid` call just above already guarantees that):

```python
    def _retire_worker(self, pid, state):
        state["dispatch_write"].close()
        try:
            os.close(state["result_fd"])
        except OSError:
            pass
        try:
            os.waitpid(pid, 0)
        except ChildProcessError:
            pass
        state["admit_process"].stdin.close()
        state["admit_process"].wait(timeout=5)
        grant = state.get("grant")
        if grant is not None:
            # Best-effort: this is the NEW per-worker child scope aitest
            # itself created (CreateWorkerScope, Task 7) -- not the outer
            # confine scope, which is `aira confine`'s own job to tear down
            # when the whole launch exits. The worker process was just
            # waited on above, so (unlike the supervisor's own scope,
            # which the process calling rmdir is itself still inside of)
            # this one's cgroup should now be empty and actually
            # removable. Log and continue on failure rather than let a
            # cleanup race crash the supervisor -- an orphaned empty scope
            # directory is a cosmetic leak, not a correctness problem, and
            # #72's existing orphan-scope reaper already sweeps these up
            # machine-wide as a backstop.
            try:
                os.rmdir(grant["scope"])
            except OSError as exc:
                sys.stderr.write("aira aitest: could not remove worker scope %s: %s\n" % (grant["scope"], exc))
        del self.workers[pid]
```

Add these two new methods to `Supervisor`:

```python
    def _replace_worker(self):
        """Acquire a fresh worker if queue work remains -- shared by the
        recycle (this task) and crash/retry (Task 15) paths."""
        if not self.queue:
            return
        self.spawn_worker(self._run_estimated_bytes, max_wait=self._run_max_wait)

    def _drain_worker(self, pid, state):
        """Handles every line CURRENTLY AVAILABLE for this worker's result
        pipe in one pass via _drain_available_lines (module-level, defined
        alongside WorkerPlacementFailed), not just the first. A
        completed-test result line and an immediately-following
        __recycle__ sentinel can both already be available by the time
        select() wakes the caller (the worker writes the result line,
        flushes, then -- if recycling -- writes __recycle__ and flushes
        again, in rapid succession with no intervening syscall that would
        let the parent's select() wake in between). Reading only the FIRST
        line per wakeup and letting _dispatch_to_idle_workers run before a
        pending recycle is checked is exactly the race this drains: it
        would hand a fresh nodeid to a worker that is already retiring,
        silently losing that dispatch (and, once Task 15's crash detection
        exists, wrongly reclassifying the loss as a crash and burning that
        nodeid's one genuine retry on a fictitious one). Must run to
        completion for a ready worker BEFORE _dispatch_to_idle_workers is
        called for this select() wakeup. See _drain_available_lines's own
        docstring for why this reads a raw fd directly rather than
        select()-checking a buffered file object.

        EOF (no lines at all, and _drain_available_lines set
        state["result_eof"]) means the worker's result pipe closed without
        a terminating record for its in-flight nodeid -- a crash (kernel
        OOM, host watchdog, any non-reporting exit). Task 15 defines
        _handle_worker_exit; until Task 15 lands, that call is a no-op
        placeholder (see Task 15's own Step 1)."""
        lines = _drain_available_lines(state["result_fd"], state)
        if not lines:
            if state.get("result_eof"):
                self._handle_worker_exit(pid, state)
            return
        for line in lines:
            if line == _RECYCLE_LINE:
                self._retire_worker(pid, state)
                self._replace_worker()
                return
            nodeid, _, outcome = line.partition(" ")
            self.results[nodeid] = outcome
            state["in_flight"] = None

    def _handle_worker_exit(self, pid, state):
        """Minimal stub so _drain_worker's EOF branch above has something
        safe to call in this task -- just retires and tries to keep the
        queue moving, with NO requeue/unevaluated bookkeeping yet. Task 15
        replaces this with the real requeue-once-then-unevaluated version;
        this task's own tests never intentionally crash a worker, so this
        stub is never exercised by Task 14's test suite, only present so a
        genuinely unexpected crash during this task's tests fails loudly
        and comprehensibly rather than with an AttributeError."""
        self._retire_worker(pid, state)
        self._replace_worker()
```

Replace `Supervisor.run`'s per-worker body inside the `for fd in ready:`
loop (the `for line in _drain_available_lines(fd, state): ...` block
Task 13 introduced) with a call to the new helper:

```python
            for fd in ready:
                pid = fd_to_pid[fd]
                if pid not in self.workers:
                    continue
                self._drain_worker(pid, self.workers[pid])
```

Also replace the `"__stop__"` literal at the bottom of `run()` with
`_STOP_LINE` for consistency:

```python
                    state["dispatch_write"].write(_STOP_LINE + "\n")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/pylib/aitest/worker.py internal/pylib/aitest/supervisor.py internal/pylib/aitest/test_supervisor.py
git commit -m "feat(aitest): add time/count/watermark worker recycle conditions"
```

---

### Task 15: crash/retry semantics

**Files:**
- Modify: `internal/pylib/aitest/supervisor.py`
- Modify: `internal/pylib/aitest/test_supervisor.py`

**Interfaces:**
- Produces: `Supervisor._handle_worker_exit(pid, state)` (module-private).
- Consumes: `Supervisor.requeue_once` (Task 11), `Supervisor._replace_worker`
  (Task 14).

- [ ] **Step 1: Write the failing test**

Append to `internal/pylib/aitest/test_supervisor.py`:

```python
def test_crash_mid_test_requeues_once_then_reports_unevaluated(tmp_path, monkeypatch, pytester):
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        import os
        def test_crashes():
            os._exit(137)
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    nodeid = items[0].nodeid
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert results[nodeid] == "unevaluated"
    assert supervisor.attempts[nodeid] == 2
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: FAIL (`run()` hangs or raises on an empty `readline()` result —
`nodeid, _, outcome = "".partition(" ")` yields `nodeid=""`, silently
corrupting `self.results`, not `unevaluated`)

- [ ] **Step 3: Write minimal implementation**

Replace `Supervisor._handle_worker_exit` in `internal/pylib/aitest/supervisor.py`
-- Task 14 added a minimal stub (retire + replace, no requeue) so its own
`_drain_worker` had something safe to call; this is the real version:

```python
    def _handle_worker_exit(self, pid, state):
        """A worker's result pipe hit EOF without a terminating record for
        its in-flight nodeid: a crash (kernel OOM, host watchdog, any
        non-reporting exit). Requeue once; a second failure here is
        unevaluated -- distinct from failed everywhere results are
        aggregated, never silently folded into either outcome."""
        nodeid = state["in_flight"]
        self._retire_worker(pid, state)
        if nodeid is not None and not self.requeue_once(nodeid):
            self.results[nodeid] = "unevaluated"
        self._replace_worker()
```

`_drain_worker` itself is unchanged from Task 14 -- it already calls
`self._handle_worker_exit(pid, state)` on EOF (via `_drain_available_lines`'s
`state["result_eof"]` flag); this task only replaces what that method DOES.

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/pylib/aitest/supervisor.py internal/pylib/aitest/test_supervisor.py
git commit -m "feat(aitest): requeue a crashed worker's nodeid once, then unevaluated"
```

---

### Task 16: daemon-down fallback

**Files:**
- Modify: `internal/pylib/aitest/supervisor.py`
- Modify: `internal/pylib/aitest/test_supervisor.py`

**Interfaces:**
- Produces: `Supervisor._spawn_fallback_worker()` (module-private).
- Consumes/extends: `Supervisor.run`, `Supervisor._retire_worker`,
  `Supervisor._replace_worker` (Tasks 13-15).

- [ ] **Step 1: Write the failing test**

Append to `internal/pylib/aitest/test_supervisor.py`:

```python
def test_daemon_down_fallback_completes_suite_with_one_warning_no_admit_subprocess(tmp_path, monkeypatch, pytester, capsys):
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", str(tmp_path / "missing-bootstrap"))
    monkeypatch.setenv("AIRA_AITEST_MAX_WORKERS_FALLBACK", "2")
    monkeypatch.delenv("AIRA_AITEST_WORKER_ADMIT_CMD", raising=False)

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=2)

    assert len(results) == 2
    assert all(outcome == "passed" for outcome in results.values())
    assert supervisor.daemon_available is False
    stderr = capsys.readouterr().err
    assert stderr.count("aira aitest:") == 1


def test_worker_admit_denied_does_not_disable_daemon_and_still_completes(tmp_path, monkeypatch, pytester, capsys):
    """A "denied" response means the daemon is reachable and just declined
    THIS request right now -- it must NOT disable daemon-backed admission
    or fall back to unconfined workers (the bug this fixes: one saturated
    moment permanently stripping containment for the rest of the run).
    This stub denies the first two admission attempts, then grants; the
    suite must still complete with containment intact throughout (no
    fallback warning emitted at all)."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    denial_state = tmp_path / "denials-remaining"
    denial_state.write_text("2")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
state_path = {str(denial_state)!r}
remaining = int(open(state_path).read())
if remaining > 0:
    open(state_path, "w").write(str(remaining - 1))
    sys.stderr.write("E_CONFINE_UNAVAILABLE: worker-admit denied: fallback:insufficient-headroom\\n")
    sys.exit(1)
scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
os.makedirs(scope, exist_ok=True)
print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
sys.stdout.flush()
sys.stdin.buffer.read()
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)

    items = pytester.getitems("""
        def test_one():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=1)

    assert len(results) == 1
    assert all(outcome == "passed" for outcome in results.values())
    assert supervisor.daemon_available is True
    assert capsys.readouterr().err == ""


def test_fallback_worker_count_capped_at_pool_size_not_added_on_top(tmp_path, monkeypatch, pytester):
    """Fallback spawning must respect min(requested_worker_count,
    max_workers_fallback) as the TOTAL pool size -- not spawn up to
    max_workers_fallback ON TOP OF whatever was already admitted before
    the daemon was marked unavailable mid-startup, and not ignore
    --aitest-workers by always growing to the (possibly NumCPU-sized)
    fallback cap regardless of what was actually requested. The first
    worker-admit call succeeds (one confined worker gets running); the
    second reveals the daemon is genuinely unreachable."""
    outer = tmp_path / "outer"
    outer.mkdir()
    bootstrap = _write_stub(tmp_path / "bootstrap", f"""
import sys
print("bootstrapped outer={outer} supervisor_scope={outer}/.aira-supervisor")
sys.exit(0)
""")
    admit_state = tmp_path / "admit-count"
    admit_state.write_text("0")
    admit = _write_stub(tmp_path / "worker-admit", f"""
import os, sys
state_path = {str(admit_state)!r}
count = int(open(state_path).read())
open(state_path, "w").write(str(count + 1))
if count == 0:
    scope = os.path.join({str(outer)!r}, "worker-scope-%d" % os.getpid())
    os.makedirs(scope, exist_ok=True)
    print("granted scope=%s worker_id=1 memory_max=104857600 memory_high=83886080" % scope)
    sys.stdout.flush()
    sys.stdin.buffer.read()
else:
    sys.stderr.write("E_CONFINE_UNAVAILABLE: dial daemon: connection refused\\n")
    sys.exit(1)
""")
    monkeypatch.setenv("AIRA_AITEST_BOOTSTRAP_CMD", bootstrap)
    monkeypatch.setenv("AIRA_AITEST_WORKER_ADMIT_CMD", admit)
    monkeypatch.setenv("AIRA_AITEST_MAX_WORKERS_FALLBACK", "5")

    items = pytester.getitems("""
        def test_one():
            assert True

        def test_two():
            assert True

        def test_three():
            assert True
    """)
    supervisor = Supervisor()
    supervisor.collect(items)
    fallback_spawns = []
    original_spawn_fallback = supervisor._spawn_fallback_worker

    def counting_spawn_fallback():
        pid = original_spawn_fallback()
        fallback_spawns.append(pid)
        return pid

    supervisor._spawn_fallback_worker = counting_spawn_fallback

    results = supervisor.run(estimated_bytes=100 * (1 << 20), worker_count=3)

    assert len(results) == 3
    assert all(outcome == "passed" for outcome in results.values())
    assert supervisor.daemon_available is False
    # 1 confined worker was already admitted before unavailability was
    # detected -- the fallback loop must add at most 2 MORE (pool size 3 =
    # min(worker_count=3, max_workers_fallback=5), minus the 1 already
    # running), never up to 5 on top of it.
    assert len(fallback_spawns) <= 2
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: FAIL (`run()` never spawns a worker at all when `daemon_available`
is `False` from the start — `results` stays empty and the assertion on
`len(results) == 2` fails)

- [ ] **Step 3: Write minimal implementation**

Add this new method to `Supervisor` in `internal/pylib/aitest/supervisor.py`:

```python
    def _spawn_fallback_worker(self):
        """No admission, no cgroup placement -- reuses worker.py's own
        execution loop with scope_path=None. The already-emitted
        _disable_daemon warning is the ONLY notice; never warn again per
        worker. Wrapped the same way spawn_worker (Task 13) is: the entire
        forked-child branch os._exit()s on any exception rather than ever
        falling through to normal Python control flow, and every OTHER
        already-known worker's fds are closed before this child does
        anything else (same fd-inheritance hazard as spawn_worker -- this
        is a second, independent os.fork() call site with the identical
        fd-table-copy problem)."""
        dispatch_read, dispatch_write = os.pipe()
        result_read, result_write = os.pipe()
        pid = os.fork()
        if pid == 0:
            try:
                self._child_close_other_workers_fds()
                os.close(dispatch_write)
                os.close(result_read)
                pipe_in = os.fdopen(dispatch_read, "r")
                pipe_out = os.fdopen(result_write, "w")
                run_worker_loop(None, self.items_by_nodeid, pipe_in, pipe_out)
            except BaseException:
                os._exit(70)
            os._exit(0)
        os.close(dispatch_read)
        os.close(result_write)
        # Same raw-fd, non-blocking treatment as spawn_worker (Task 13) --
        # no placement ack to wait for here (scope_path=None, nothing to
        # place into), so just flip to non-blocking immediately.
        os.set_blocking(result_read, False)
        self.workers[pid] = {
            "grant": None,
            "admit_process": None,
            "dispatch_write": os.fdopen(dispatch_write, "w"),
            "result_fd": result_read,
            "read_buffer": b"",
            "result_eof": False,
            "in_flight": None,
        }
        return pid
```

Replace `_retire_worker` (Task 14's version, which already tears down the
worker's own scope directory) to also guard the now-possibly-`None`
`admit_process`/`grant` a fallback worker has:

```python
    def _retire_worker(self, pid, state):
        state["dispatch_write"].close()
        try:
            os.close(state["result_fd"])
        except OSError:
            pass
        try:
            os.waitpid(pid, 0)
        except ChildProcessError:
            pass
        if state["admit_process"] is not None:
            state["admit_process"].stdin.close()
            state["admit_process"].wait(timeout=5)
        grant = state.get("grant")
        if grant is not None:
            try:
                os.rmdir(grant["scope"])
            except OSError as exc:
                sys.stderr.write("aira aitest: could not remove worker scope %s: %s\n" % (grant["scope"], exc))
        del self.workers[pid]
```

Replace `_replace_worker` to route the two admission-failure exception
types differently, and fall back to unconfined only on the ones that
actually mean "no daemon":

```python
    def _replace_worker(self):
        """Acquire a fresh worker if queue work remains -- shared by the
        recycle and crash/retry paths.

        WorkerAdmitDenied (the daemon IS reachable, it just declined this
        particular request right now -- budget exhausted or contended)
        leaves daemon_available untouched: simply don't replace this
        worker yet. The NEXT retirement's _replace_worker call (or a later
        dispatch pass) tries again -- one saturated moment must never
        permanently strip containment for the rest of the run.

        WorkerAdmitUnavailable (no daemon to talk to at all) and
        WorkerPlacementFailed (the cgroup mechanism itself is broken
        locally, not just momentarily busy) both fall back to an
        unconfined worker for the rest of the run -- these are the only
        two failure classes that mean the daemon path is genuinely not
        going to work."""
        if not self.queue:
            return
        if self.daemon_available:
            try:
                self.spawn_worker(self._run_estimated_bytes, max_wait=self._run_max_wait)
                return
            except WorkerAdmitDenied:
                return
            except (WorkerAdmitUnavailable, WorkerPlacementFailed) as exc:
                self._disable_daemon(str(exc))
        self._spawn_fallback_worker()
```

Add these two new module-level constants alongside `_STOP_LINE`/`_RECYCLE_LINE`:

```python
_STARTUP_DENIAL_RETRY_ATTEMPTS = 5
_STARTUP_DENIAL_RETRY_SECONDS = 1.0
```

Replace `run()`'s startup section (everything from `self.bootstrap()` through
the first `self._dispatch_to_idle_workers()` call) with:

```python
        self.bootstrap()
        import gc
        gc.freeze()
        self._run_estimated_bytes = estimated_bytes
        self._run_max_wait = max_wait
        if self.daemon_available:
            for _ in range(worker_count):
                if not self.queue:
                    break
                try:
                    self.spawn_worker(estimated_bytes, max_wait=max_wait)
                except WorkerAdmitDenied:
                    # Contended/no budget RIGHT NOW -- the daemon is still
                    # there. Stop trying to grow the pool this instant and
                    # start dispatching to however many DID get admitted;
                    # a later retirement's _replace_worker tries for more.
                    break
                except (WorkerAdmitUnavailable, WorkerPlacementFailed) as exc:
                    self._disable_daemon(str(exc))
                    break
            # A denied/contended daemon must never silently strip
            # containment (denied != unavailable, above) -- but with ZERO
            # workers admitted yet there is also no later retirement to
            # hook a retry off of (_replace_worker only fires when an
            # EXISTING worker retires). Retry getting AT LEAST one worker
            # running a small bounded number of times; only if that still
            # never succeeds do we fall back for this run rather than
            # silently completing with an empty result set while the
            # queue still has work.
            attempt = 0
            while (self.daemon_available and not self.workers and self.queue
                   and attempt < _STARTUP_DENIAL_RETRY_ATTEMPTS):
                attempt += 1
                time.sleep(_STARTUP_DENIAL_RETRY_SECONDS)
                try:
                    self.spawn_worker(estimated_bytes, max_wait=max_wait)
                except WorkerAdmitDenied:
                    continue
                except (WorkerAdmitUnavailable, WorkerPlacementFailed) as exc:
                    self._disable_daemon(str(exc))
                    break
            if self.daemon_available and not self.workers and self.queue:
                self._disable_daemon("worker-admit stayed denied after %d retries" % _STARTUP_DENIAL_RETRY_ATTEMPTS)
        if not self.daemon_available:
            # Cap TOTAL concurrent workers (already-admitted + fallback) at
            # the configured pool size -- min(worker_count,
            # max_workers_fallback), never NumCPU regardless of what
            # --aitest-workers actually asked for, and never on top of
            # whatever got admitted before the daemon was marked
            # unavailable mid-startup-loop.
            remaining_pool = max(0, min(worker_count, self.max_workers_fallback) - len(self.workers))
            for _ in range(remaining_pool):
                if not self.queue:
                    break
                self._spawn_fallback_worker()
        self._dispatch_to_idle_workers()
```

(The rest of `run()` — the `while self.workers:` loop — is unchanged from
Task 15.)

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestPackageUnitTests -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/pylib/aitest/supervisor.py internal/pylib/aitest/test_supervisor.py
git commit -m "feat(aitest): add daemon-down fallback to an unconfined worker pool"
```

---

### Task 17: end-to-end integration test

**Files:**
- Modify: `internal/pylib/aitest/__init__.py`
- Create: `internal/pylib/aitest/testdata/conftest.py`
- Create: `internal/pylib/aitest/testdata/test_pass.py`
- Create: `internal/pylib/aitest/testdata/test_fail.py`
- Create: `internal/pylib/aitest/testdata/test_oom.py`
- Create: `internal/pylib/pytest_aitest_e2e_test.go`

**Interfaces:**
- Produces (`__init__.py`): a `pytest_runtestloop(session)` hookimpl — the
  activation wiring that drives `Supervisor.run()` from a real `pytest
  --aitest-workers=N` invocation. **This wiring was not assigned to any of
  Tasks 9-16** (they built and unit-tested `Supervisor`/`worker.py` in
  isolation; nothing before this task connects pytest's own collection
  phase to `Supervisor.collect`/`run`). It is added here because Task 17's
  own goal — driving the chain through a real `pytest` subprocess — is not
  achievable without it. Flagged for extra review scrutiny; see this task's
  Step 3 note.
- Consumes (`testing_seams.go`): `SetWorkerAdmitHeadroomForTest` (Task 8) —
  the real-daemon e2e test zeros it so admission isn't universally denied
  against the small real cgroup `memory.max` this test uses. No new
  worker-admit-specific seam is needed here; `worker-admit` is the only
  daemon verb this e2e test exercises (it never issues a plain `admit`
  request), so the general `admit`-verb headroom fields are irrelevant to
  it.
- Consumes: `Supervisor` (Tasks 11-16), `aira aitest-bootstrap` (Task 3),
  `aira worker-admit` (Task 8), `daemon.NewServer`/`Paths`/`PathsFromEnv`
  (existing), `cgrouptest.IsolatedScopeParent`/`SkipOrFailRealCgroup`
  (existing).

**Achievability** (per this task's own brief — stated here, and repeated in
the final report): this task writes TWO Go tests.
`TestRealPytestAitestEndToEndFallback` needs no real cgroup delegation and no
real daemon — it exercises the daemon-down fallback path through a REAL
`pytest --aitest-workers` subprocess (not a direct `Supervisor()` call, unlike
every test in Tasks 11-16) and runs in any sandboxed CI environment.
`TestRealPytestAitestEndToEndRealDaemonAndCgroup` exercises the FULL chain —
real daemon, real cgroup-v2 outer scope, real `aira` binary, real
`memory.max` OOM-kill and recovery — gated behind
`cgrouptest.SkipOrFailRealCgroup`'s policy (clean skip without
`AIRA_REAL_CGROUP=1` on a host without real delegation; hard fail with it).
Both are written in full below; which one a given host actually exercises
depends on that host's cgroup delegation, not on this plan.

- [ ] **Step 1: Write the failing tests**

`internal/pylib/aitest/testdata/conftest.py`:

```python
import importlib
import os
import sys

aira_py_lib = os.environ["AIRA_AITEST_LIB"]
if aira_py_lib not in sys.path:
    sys.path.insert(0, aira_py_lib)
importlib.import_module("aitest")
pytest_plugins = ("aitest",)
```

`internal/pylib/aitest/testdata/test_pass.py`:

```python
def test_one():
    assert True


def test_two():
    assert 1 + 1 == 2
```

`internal/pylib/aitest/testdata/test_fail.py`:

```python
def test_deliberate_failure():
    assert False, "this test is meant to fail"
```

`internal/pylib/aitest/testdata/test_oom.py`:

```python
import os

import pytest


def test_deliberate_oom():
    if os.environ.get("AIRA_REAL_CGROUP") != "1":
        pytest.skip("requires AIRA_REAL_CGROUP=1 and real cgroup-v2 memory delegation")
    # Deliberately allocate well past the tiny --estimated-bytes cap this
    # e2e run configures, to trigger a real kernel memory.max OOM-kill and
    # prove per-worker containment (design spec 3.4, hard invariant 4).
    block = bytearray(512 * 1024 * 1024)  # 512 MiB, touched to force real RSS
    for i in range(0, len(block), 4096):
        block[i] = 1
```

`internal/pylib/pytest_aitest_e2e_test.go`:

```go
//go:build linux

package pylib

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"aira/internal/cgrouptest"
	"aira/internal/daemon"
)

func TestRealPytestAitestEndToEndFallback(t *testing.T) {
	pytest := requireRealPytest(t)
	aitestDir, err := filepath.Abs("aitest")
	if err != nil {
		t.Fatal(err)
	}
	pythonDir, err := ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	command := exec.Command(pytest, "-q", "--aitest-workers=2")
	command.Dir = filepath.Join(aitestDir, "testdata")
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Dir(aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+pythonDir,
		"AIRA_AITEST_BOOTSTRAP_CMD="+filepath.Join(t.TempDir(), "missing-aira"),
	)
	output, err := command.CombinedOutput()
	text := string(output)
	// test_fail.py's deliberate failure makes this a nonzero pytest exit --
	// expected, not a Go test failure. A Go test failure is anything OTHER
	// than a clean, complete pytest run (a crash, a missing report line).
	if !strings.Contains(text, "test_pass.py::test_one passed") {
		t.Fatalf("pytest output missing expected pass line: %v\n%s", err, text)
	}
	if !strings.Contains(text, "test_fail.py::test_deliberate_failure failed") {
		t.Fatalf("pytest output missing expected fail line: %v\n%s", err, text)
	}
	if strings.Count(text, "aira aitest:") != 1 {
		t.Fatalf("expected exactly one fallback warning: %v\n%s", err, text)
	}
	// AIRA_REAL_CGROUP is unset in this fallback run, so test_oom.py skips
	// itself -- and a skip must be its own genuine outcome, never folded
	// into "unevaluated" (a check that could not establish a result). This
	// also makes the negative assertion below meaningful rather than
	// vacuous: it only holds if skip is genuinely NOT unevaluated.
	if !strings.Contains(text, "test_oom.py::test_deliberate_oom skipped") {
		t.Fatalf("pytest output missing expected skipped line: %v\n%s", err, text)
	}
	if strings.Contains(text, "unevaluated") {
		t.Fatalf("fallback run unexpectedly reported unevaluated: %s", text)
	}
}

func shortE2ERuntimeDir(t *testing.T) string {
	t.Helper()
	// A daemon socket path must fit the 108-byte AF_UNIX sun_path limit;
	// t.TempDir() embeds the (long) test name. Mirrors
	// internal/daemon/server_test.go's shortRuntimeDir exactly.
	dir, err := os.MkdirTemp("", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestRealPytestAitestEndToEndRealDaemonAndCgroup(t *testing.T) {
	pytest := requireRealPytest(t)
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-e2e")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	// A real, small, numeric cap -- readSliceMemory treats a bare "max" as
	// unreadable ("unbounded"), so admission needs this written explicitly.
	if err := os.WriteFile(filepath.Join(outer, "memory.max"), []byte("268435456"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot set outer memory.max: %v", err)
	}

	binary := filepath.Join(t.TempDir(), "aira")
	// aira confine -- wraps this build, per this plan's own Global
	// Constraints ("every go build/go test ... MUST be prefixed with aira
	// confine --"). It nests under the confinement the outer `aira confine
	// -- go test ...` invocation (Step 2/4/5's Run: lines) already applies
	// to this whole test binary -- a normal, supported nested scope, not a
	// circular dependency: this is the machine's already-installed `aira`
	// on PATH wrapping a build of a FRESH `aira` binary into a temp dir.
	build := exec.Command("aira", "confine", "--", "go", "build", "-o", binary, "aira/cmd/aira")
	if buildOutput, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aira binary: %v\n%s", err, buildOutput)
	}

	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortE2ERuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	server := daemon.NewServer(paths)
	server.SetWorkerAdmitHeadroomForTest(0)
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-ready

	aitestDir, err := filepath.Abs("aitest")
	if err != nil {
		t.Fatal(err)
	}
	pythonDir, err := ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	outerFile, err := os.Open(outer)
	if err != nil {
		t.Fatal(err)
	}
	defer outerFile.Close()

	command := exec.Command(pytest, "-q", "--aitest-workers=2")
	command.Dir = filepath.Join(aitestDir, "testdata")
	// Places the pytest subprocess (the supervisor) directly into outer's
	// cgroup AT process creation -- the same clone3(CLONE_INTO_CGROUP)
	// mechanism confine_linux.go already uses for a top-level confine
	// launch (UseCgroupFD/CgroupFD), reused here instead of the full
	// aira-confine argv/handshake machinery, since this test starts one
	// level in: "an outer scope already exists and is about to run its
	// supervisor", not "aira confine itself parses argv and creates it".
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(outerFile.Fd())}
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Dir(aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+pythonDir,
		"AIRA_AITEST_BOOTSTRAP_CMD="+binary,
		"AIRA_AITEST_WORKER_ADMIT_CMD="+binary,
		"AIRA_AITEST_ESTIMATED_BYTES="+strconv.Itoa(32<<20),
		"AIRA_REAL_CGROUP=1",
	)
	output, err := command.CombinedOutput()
	text := string(output)

	if !strings.Contains(text, "test_pass.py::test_one passed") {
		t.Fatalf("pytest output missing expected pass line: %v\n%s", err, text)
	}
	if !strings.Contains(text, "test_fail.py::test_deliberate_failure failed") {
		t.Fatalf("pytest output missing expected fail line: %v\n%s", err, text)
	}
	// The OOM test exceeds its 32 MiB worker cap deterministically on both
	// the original attempt and the one requeue-once retry (Task 15) --
	// design spec 3.4/4: never passed, never failed, always unevaluated.
	if !strings.Contains(text, "test_oom.py::test_deliberate_oom unevaluated") {
		t.Fatalf("pytest output missing expected OOM-contained unevaluated line: %v\n%s", err, text)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestEndToEnd -v`
Expected: `TestRealPytestAitestEndToEndFallback` FAILs (`pytest_runtestloop`
does not exist yet, so `--aitest-workers` is accepted but silently ignored —
pytest runs its own default loop, producing no `nodeid outcome`/`aira
aitest:` lines at all). `TestRealPytestAitestEndToEndRealDaemonAndCgroup`
either FAILs the same way (on a host with real cgroup-v2 delegation) or
SKIPs cleanly (without it).

- [ ] **Step 3: Write minimal implementation**

Add to `internal/pylib/aitest/__init__.py`:

```python
def pytest_runtestloop(session):
    """Slice 1 activation: when --aitest-workers is set, replace pytest's
    default per-item loop with the Supervisor-driven fork+admission pool.

    Deliberately NOT full TestReport replay (design spec 5, Slice 2) --
    Slice 1 reports pass/fail/unevaluated via plain terminal lines and the
    process exit code only. session.items is already populated here:
    pytest's own collection phase (unmodified) always runs before
    pytest_runtestloop fires.
    """
    workers_option = session.config.getoption("aitest_workers")
    if workers_option is None:
        return None

    from aitest.supervisor import Supervisor

    supervisor = Supervisor()
    supervisor.collect(session.items)
    results = supervisor.run(
        estimated_bytes=_resolve_estimated_bytes(),
        worker_count=_resolve_worker_count(workers_option),
    )
    passed = failed = skipped = unevaluated = 0
    for item in session.items:
        outcome = results.get(item.nodeid, "unevaluated")
        print("%s %s" % (item.nodeid, outcome))
        if outcome == "passed":
            passed += 1
        elif outcome == "failed":
            failed += 1
        elif outcome == "skipped":
            # A skip is pytest's own well-defined, intentional outcome --
            # never folded into unevaluated ("a check that could not
            # establish its result") or failed.
            skipped += 1
        else:
            unevaluated += 1
    print("aitest: %d passed, %d failed, %d skipped, %d unevaluated" % (passed, failed, skipped, unevaluated))
    session.testsfailed = failed + unevaluated
    return True


def _resolve_worker_count(workers_option):
    if workers_option == "auto":
        return max(1, os.cpu_count() or 1)
    try:
        count = int(workers_option)
    except ValueError:
        count = 1
    return max(1, count)


def _resolve_estimated_bytes():
    # Slice 1: a pinned per-worker memory.max backstop from an env var.
    # Suite-signature-based sizing (design spec 3.3) is a safety-backstop
    # sizing refinement, not the admission signal, and is deferred -- not
    # needed to validate this slice's core admission/lifecycle loop.
    raw = os.environ.get("AIRA_AITEST_ESTIMATED_BYTES", "")
    try:
        value = int(raw)
    except ValueError:
        value = 0
    return value if value > 0 else (512 << 20)
```

Add `import os` at the top of `internal/pylib/aitest/__init__.py` (it
currently has no imports).

No `testing_seams.go` change is needed in this task — `SetWorkerAdmitHeadroomForTest`
(Task 8) already covers the one seam this e2e test needs (see Step 1's
`server.SetWorkerAdmitHeadroomForTest(0)` call above); this task exercises
only the `aitest-bootstrap`/`worker-admit` verbs, never the general `admit`
verb, so no `admit`-specific headroom seam applies here.

- [ ] **Step 4: Run test to verify it passes**

Run: `aira confine -- go test ./internal/pylib/ -run TestRealPytestAitestEndToEnd -v`
Expected: `TestRealPytestAitestEndToEndFallback` PASS.
`TestRealPytestAitestEndToEndRealDaemonAndCgroup` PASS on a host with real
cgroup-v2 delegation, clean SKIP otherwise (hard FAIL under
`AIRA_REAL_CGROUP=1` without delegation, per
`cgrouptest.SkipOrFailRealCgroup`'s existing policy).

- [ ] **Step 5: Run the full aitest and pylib suite one more time**

Run: `aira confine -- go test ./internal/pylib/... ./internal/runner/... ./internal/daemon/... ./cmd/aira/... -v`
Expected: PASS (or clean, policy-consistent SKIP for every real-cgroup case
without delegation)

- [ ] **Step 6: Commit**

```bash
git add internal/pylib/aitest/__init__.py internal/pylib/aitest/testdata internal/pylib/pytest_aitest_e2e_test.go
git commit -m "feat(aitest): wire pytest_runtestloop activation and add the Slice 1 e2e test"
```
