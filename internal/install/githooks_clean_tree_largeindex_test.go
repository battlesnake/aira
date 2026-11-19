package install

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// AIRA-359 (Sol round-4 review): aira_ci_pass_clean_tree looked for hidden
// index entries with `git ls-files -v | grep -q -v '^H '`. Under the hooks'
// `set -o pipefail`, grep -q exits at the FIRST non-plain entry, ls-files then
// dies of SIGPIPE on a large index, the pipeline status is 141, the `if` reads
// it as "no hidden entry", and the helper reports a clean tree that is not.
//
// verifies: AIRA-355
func TestCleanTreeRefusesAHiddenEntryOnALargeIndex(t *testing.T) {
	t.Parallel()
	fx := newPushFixture(t)
	// Enough long paths that ls-files writes well past the 64 KiB pipe buffer
	// after the hidden entry (".aira/config" sorts first).
	for i := 0; i < 3000; i++ {
		fx.write(t, fmt.Sprintf("bulk/dir-%04d/file-with-a-fairly-long-name-%04d.txt", i, i), "x\n")
	}
	fx.commitAll(t, "large index")
	fx.git(t, fx.worktree, "update-index", "--assume-unchanged", ".aira/config")
	fx.write(t, ".aira/config", "{\"edited\":true}\n") // hidden from `status`

	script := `set -euo pipefail; . "$1/.githooks/common.sh"; aira_ci_pass_clean_tree "$2"`
	out, err := exec.Command("bash", "-c", script, "bash", fx.primary, fx.worktree).CombinedOutput()
	if err == nil {
		t.Fatalf("a hidden (assume-unchanged) edit on a large index was reported as a clean tree: %q", out)
	}

	// The control: with no hidden entry the same large index is clean and the
	// helper prints HEAD's tree.
	fx.git(t, fx.worktree, "update-index", "--no-assume-unchanged", ".aira/config")
	fx.git(t, fx.worktree, "checkout", "--", ".aira/config")
	out, err = exec.Command("bash", "-c", script, "bash", fx.primary, fx.worktree).CombinedOutput()
	if err != nil {
		t.Fatalf("a plain large index was refused: %v %q", err, out)
	}
	if want := strings.TrimSpace(fx.git(t, fx.worktree, "rev-parse", "HEAD^{tree}")); strings.TrimSpace(string(out)) != want {
		t.Fatalf("clean tree printed %q, want %q", out, want)
	}
}
