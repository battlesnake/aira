//go:build linux

package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aira/internal/cgrouptest"
)

// AIRA-102 real-podman end-to-end.
//
// Everything else about the podman half is argv construction, which a unit test
// can prove. What CANNOT be proven without a real runtime is the claim the whole
// feature rests on: that `--cgroups=split` actually lands the container INSIDE
// this confine job's own cgroup scope, and that the injected `--memory` actually
// becomes that container's own memory.max. Both were measured by hand while
// planning; this makes them a standing guarantee.
//
// ISOLATION: podman only, never docker. Every container is `--rm`, short-lived,
// uniquely named, and created by this test; nothing here enumerates, inspects or
// touches containers the test did not create. The machine this was developed on
// runs 30+ unrelated docker containers and many concurrent confine jobs.
//
// verifies: AIRA-102

func requirePodmanAlpine(t *testing.T) string {
	t.Helper()
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Skipf("podman not installed: %v", err)
	}
	// `podman images` touches only the local image store, never a container.
	output, err := exec.Command(podman, "images", "--format", "{{.Repository}}").CombinedOutput()
	if err != nil {
		t.Skipf("podman image store unavailable: %v", err)
	}
	if !strings.Contains(string(output), "alpine") {
		t.Skip("podman has no local alpine image; refusing to pull one in a test")
	}
	return podman
}

// realPodmanConfineDeps keeps every production dep except the slice location and
// admission: the scope, delegation, cap read and launch are all REAL, because
// they are what the test is about. Admission is stubbed so a test never charges
// the live shared ledger.
func realPodmanConfineDeps(parent string) confineDeps {
	return confineDeps{
		resolveSlicePath: func(string) (string, bool, string) { return parent, true, "" },
		admit: func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
			return admissionResult{state: "immediate"}, nil
		},
	}
}

func TestRealPodmanSplitNestsContainerInsideThisJobsScope(t *testing.T) {
	podman := requirePodmanAlpine(t)
	parent := cgrouptest.IsolatedScopeParent(t)

	const containerCap = int64(512 << 20)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	// `--cgroupns=host` makes the container report its REAL host cgroup path
	// rather than the namespaced "0::/". It is deliberately NOT one of the
	// placement flags AIRA treats as a caller cgroup choice, so the injection
	// must still happen -- which this test also proves.
	request := ConfineRequest{
		Slice: "irrelevant",
		Name:  "a102-real",
		// With --cgroupns=host the container sees the real host cgroup tree, so it
		// reports BOTH facts itself: its own host cgroup path, and the memory.max
		// the kernel actually applied to it, read back through that path. Doing
		// both from inside means neither assertion can race podman's reaping of
		// the payload cgroup after --rm, which would otherwise degrade the
		// memory assertion into a silent skip.
		Argv: []string{podman, "run", "--rm", "--cgroupns=host", "alpine",
			"sh", "-c", `p=$(sed -e "s|^0::||" /proc/self/cgroup); echo "$p"; cat /sys/fs/cgroup$p/memory.max`},
		ScopeMemoryMax: containerCap,
		Stdout:         stdout,
		Stderr:         stderr,
		Stdin:          strings.NewReader(""),
	}
	result, err := confineWithDeps(context.Background(), request, realPodmanConfineDeps(parent))
	if err != nil {
		t.Fatalf("confine failed: %v\nstderr: %s", err, stderr.String())
	}
	if result.Exit != 0 {
		t.Fatalf("podman exited %d\nstdout: %s\nstderr: %s", result.Exit, stdout.String(), stderr.String())
	}

	// The facet must say AIRA injected split -- if it does not, the assertions
	// below could be satisfied by a container the TEST placed rather than one
	// AIRA placed, which is exactly the porous shape the plan review flagged.
	if result.Status.Container != ContainerPlacementPodmanSplitInjected {
		t.Fatalf("container facet = %q, want %q (AIRA did not inject the placement flag)",
			result.Status.Container, ContainerPlacementPodmanSplitInjected)
	}
	wantMemoryFacet := "injected=" + strconv.FormatInt(containerCap, 10)
	if result.Status.ContainerMemory != wantMemoryFacet {
		t.Fatalf("container-memory facet = %q, want %q", result.Status.ContainerMemory, wantMemoryFacet)
	}

	lines := strings.Fields(stdout.String())
	if len(lines) < 2 {
		t.Fatalf("container reported too little; stdout %q stderr %q", stdout.String(), stderr.String())
	}

	// 1. Placement: the container must be a DESCENDANT of this job's own scope.
	//    This is the entire claim of the podman half of AIRA-102.
	containerCgroup := lines[0]
	absolute := filepath.Join("/sys/fs/cgroup", containerCgroup)
	if !strings.HasPrefix(absolute, parent+string(filepath.Separator)) {
		t.Fatalf("container landed at %q, which is NOT inside this job's scope parent %q", absolute, parent)
	}
	if !strings.Contains(containerCgroup, ".aira-CONFINE-") {
		t.Fatalf("container cgroup %q is not inside a confine scope", containerCgroup)
	}
	if !strings.Contains(containerCgroup, "libpod-payload-") {
		t.Fatalf("container cgroup %q does not look like a split payload cgroup", containerCgroup)
	}

	// 2. Memory: the injected --memory really became THAT container's own cap.
	//    Read from inside the container, so this cannot race podman reaping the
	//    payload cgroup and cannot degrade into a skipped assertion.
	got, parseErr := strconv.ParseInt(lines[len(lines)-1], 10, 64)
	if parseErr != nil {
		t.Fatalf("container memory.max %q unparsable: %v", lines[len(lines)-1], parseErr)
	}
	if got != floorMemoryPage(containerCap) {
		t.Fatalf("container memory.max = %d, want the injected %d", got, floorMemoryPage(containerCap))
	}
}

// TestRealPodmanSplitJobIsLiveInSubtreePopulated pins the `confine --list`
// honesty fix to a real split job: while it runs, the scope's own cgroup.procs
// is empty (podman moved everything into <scope>/runtime and the payload), so a
// leaf-only reading calls a running job dead.
func TestRealPodmanSplitJobIsLiveInSubtreePopulated(t *testing.T) {
	podman := requirePodmanAlpine(t)
	parent := cgrouptest.IsolatedScopeParent(t)

	observed := make(chan struct {
		leaf    int
		subtree bool
		err     error
	}, 1)

	deps := realPodmanConfineDeps(parent)
	// OnPlaced fires once placement is proven; by the time the container has
	// started, podman has already drained the scope's own cgroup.procs.
	request := ConfineRequest{
		Slice:          "irrelevant",
		Name:           "a102-live",
		Argv:           []string{podman, "run", "--rm", "alpine", "sleep", "2"},
		ScopeMemoryMax: 512 << 20,
		Stdout:         io.Discard,
		Stderr:         io.Discard,
		Stdin:          strings.NewReader(""),
	}
	request.OnPlaced = func(info ConfineLaunchInfo) {
		go func() {
			// Sample once the container has had time to start and podman has
			// performed the split relocation.
			scopePath := ""
			entries, err := os.ReadDir(parent)
			if err == nil {
				for _, entry := range entries {
					if entry.IsDir() && strings.HasPrefix(entry.Name(), ".aira-CONFINE-") {
						scopePath = filepath.Join(parent, entry.Name())
					}
				}
			}
			result := struct {
				leaf    int
				subtree bool
				err     error
			}{}
			if scopePath == "" {
				result.err = os.ErrNotExist
				observed <- result
				return
			}
			for attempt := 0; attempt < 40; attempt++ {
				procs, _ := os.ReadFile(filepath.Join(scopePath, "cgroup.procs"))
				events, _ := os.ReadFile(filepath.Join(scopePath, "cgroup.events"))
				leaf := len(strings.Fields(strings.TrimSpace(string(procs))))
				subtree := strings.Contains(string(events), "populated 1")
				result.leaf, result.subtree = leaf, subtree
				// The interesting instant: drained leaf, live subtree.
				if leaf == 0 && subtree {
					break
				}
			}
			observed <- result
		}()
	}

	if _, err := confineWithDeps(context.Background(), request, deps); err != nil {
		t.Fatalf("confine failed: %v", err)
	}

	select {
	case sample := <-observed:
		if sample.err != nil {
			t.Skipf("could not sample the live scope: %v", sample.err)
		}
		// The binding requirement of the --list fix: while the job is RUNNING,
		// the subtree-aware signal that feeds the LIVE column must say live. The
		// leaf count is allowed to be anything -- that is exactly why it is no
		// longer the column an operator reads.
		if !sample.subtree {
			t.Fatalf("a running split job read subtree-populated=false (leaf=%d); "+
				"the LIVE column would call a running job dead", sample.leaf)
		}
		if sample.leaf == 0 {
			t.Logf("sampled the drained instant: leaf cgroup.procs=0 while subtree is live "+
				"-- exactly the state that made the old POPULATED column lie (leaf=%d)", sample.leaf)
		}
	default:
		t.Skip("no sample taken")
	}
}
