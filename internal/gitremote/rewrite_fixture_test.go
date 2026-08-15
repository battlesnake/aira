package gitremote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// verifies: our one-pass resolver matches installed git for effective scopes,
// longest prefix, equal-length ordering, case sensitivity, push precedence,
// conditional includes, and chained non-iteration.
func TestRewriteResolverMatchesInstalledGitFixtures(t *testing.T) {
	tests := []struct {
		name, raw, expected string
		push                bool
		configure           func(*testing.T, string)
	}{
		{name: "longest and chained", raw: "gh:team/repo", expected: "ssh://git@github.com/team/repo", configure: func(t *testing.T, root string) {
			gitConfig(t, root, "--add", "url.ssh://git@github.com/.insteadOf", "gh:")
			gitConfig(t, root, "--add", "url.ssh://git@github.com/team/.insteadOf", "gh:team/")
			gitConfig(t, root, "--add", "url.https://should-not-chain/.insteadOf", "ssh://git@github.com/team/")
		}},
		{name: "case sensitive", raw: "GH:team/repo", expected: "GH:team/repo", configure: func(t *testing.T, root string) {
			gitConfig(t, root, "--add", "url.ssh://git@github.com/.insteadOf", "gh:")
		}},
		{name: "push precedence", raw: "gh:team/repo", expected: "ssh://push@github.com/team/repo", push: true, configure: func(t *testing.T, root string) {
			gitConfig(t, root, "--add", "url.ssh://fetch@github.com/.insteadOf", "gh:")
			gitConfig(t, root, "--add", "url.ssh://push@github.com/.pushInsteadOf", "gh:")
		}},
		{name: "equal prefix tie", raw: "tie:repo", configure: func(t *testing.T, root string) {
			gitConfig(t, root, "--add", "url.ssh://first/.insteadOf", "tie:")
			gitConfig(t, root, "--add", "url.ssh://second/.insteadOf", "tie:")
		}},
		{name: "include", raw: "included:repo", expected: "ssh://included/repo", configure: func(t *testing.T, root string) {
			include := filepath.Join(root, "included.config")
			if err := os.WriteFile(include, []byte("[url \"ssh://included/\"]\n\tinsteadOf = included:\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitConfig(t, root, "include.path", include)
		}},
		{name: "conditional include", raw: "conditional:repo", expected: "ssh://conditional/repo", configure: func(t *testing.T, root string) {
			include := filepath.Join(root, "included.config")
			conditional := filepath.Join(root, "conditional.config")
			if err := os.WriteFile(include, []byte("[url \"ssh://included/\"]\n\tinsteadOf = included:\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(conditional, []byte("[url \"ssh://conditional/\"]\n\tinsteadOf = conditional:\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitConfig(t, root, "include.path", include)
			gitConfig(t, root, "includeIf.gitdir:"+filepath.ToSlash(root)+"/.path", conditional)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			runGit(t, root, "init", "-q")
			gitConfig(t, root, "remote.origin.url", tc.raw)
			tc.configure(t, root)
			output := runGitOutput(t, root, "config", "--null", "--get-regexp", `^url\..*\.(insteadof|pushinsteadof)$`)
			rules := parseRules(output)
			got := applyRewrite(tc.raw, rules, tc.push)
			args := []string{"remote", "get-url"}
			if tc.push {
				args = append(args, "--push")
			}
			args = append(args, "origin")
			want := strings.TrimSpace(runGitOutput(t, root, args...))
			if got != want {
				t.Fatalf("resolver=%q git=%q rules=%+v", got, want, rules)
			}
			if tc.expected != "" && got != tc.expected {
				t.Fatalf("fixture did not exercise intended branch: got=%q expected=%q", got, tc.expected)
			}
		})
	}
}

func gitConfig(t *testing.T, root string, args ...string) {
	t.Helper()
	runGit(t, root, append([]string{"config"}, args...)...)
}
func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
func runGitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(output)
}
