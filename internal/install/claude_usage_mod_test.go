package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// AIRA-284 slice B. `aira install --claude-usage-mod[=off]`.

func modDir(state *fakeInstallState) string {
	return filepath.Join(state.home, ".claude", "skills", "aira-usage")
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readMarker(t *testing.T, state *fakeInstallState) claudeModMarker {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(modDir(state), claudeModMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var marker claudeModMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatal(err)
	}
	return marker
}

// listTree returns every path under dir (relative, sorted), dirs with a slash.
func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if entry.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out
}

func installMod(t *testing.T, d installDeps, value string) error {
	t.Helper()
	return installClaudeUsageMod(d, installOpts{claudeUsageMod: value})
}

// verifies: AIRA-284 §3.6 — the flag is bare (install) or =off (remove); any
// other value is E_INSTALL_ARGUMENT_INVALID.
func TestParseClaudeUsageModFlag(t *testing.T) {
	for args, want := range map[string]string{"--claude-usage-mod": claudeModOn, "--claude-usage-mod=off": claudeModOff} {
		opts, err := parseInstallArgs([]string{args})
		if err != nil || opts.claudeUsageMod != want {
			t.Fatalf("%s: mod=%q err=%v, want %q", args, opts.claudeUsageMod, err, want)
		}
	}
	for _, bad := range []string{"--claude-usage-mod=on", "--claude-usage-mod=", "--claude-usage-mod=yes", "--claude-usage-mod=OFF"} {
		if _, err := parseInstallArgs([]string{bad}); err == nil || !strings.Contains(err.Error(), CodeArgumentInvalid) {
			t.Fatalf("%s: err=%v, want %s", bad, err, CodeArgumentInvalid)
		}
	}
	if opts, err := parseInstallArgs(nil); err != nil || opts.claudeUsageMod != "" {
		t.Fatalf("no flag: mod=%q err=%v", opts.claudeUsageMod, err)
	}
}

// verifies: AIRA-284 §3.6 — like AIRA-283's flag, refused where it cannot take
// effect (--status mutates nothing; --stage=start installs nothing) and
// accepted where an install runs.
func TestClaudeUsageModFlagIsRefusedWhereItCannotTakeEffect(t *testing.T) {
	for _, args := range [][]string{
		{"--status", "--claude-usage-mod"},
		{"--status", "--claude-usage-mod=off"},
		{"--stage=start", "--claude-usage-mod"},
		{"--ci=shim", "--stage=start", "--claude-usage-mod=off"},
	} {
		if _, err := parseInstallArgs(args); err == nil || !strings.Contains(err.Error(), CodeArgumentInvalid) {
			t.Fatalf("%q: err=%v, want %s", args, err, CodeArgumentInvalid)
		}
	}
	for _, args := range [][]string{
		{"--claude-usage-mod"},
		{"--memory-max=16G", "--claude-usage-mod"},
		{"--stage=build", "--claude-usage-mod"},
		{"--ci=shim", "--stage=build", "--claude-usage-mod"},
		{"--ci=shim", "--claude-usage-mod=off"},
		{"--dry-run", "--claude-usage-mod"},
	} {
		if _, err := parseInstallArgs(args); err != nil {
			t.Fatalf("%q refused: %v", args, err)
		}
	}
}

// verifies: AIRA-284 §3.6 — the root leg of `sudo aira install` forwards the
// flag (value included) to the unprivileged leg that owns the user's HOME, and
// forwards nothing when it was not given.
func TestClaudeUsageModFlagSurvivesTheSudoReexec(t *testing.T) {
	target := installTarget{uid: 1000, gid: 1000, home: "/home/u", username: "u"}
	for opts, want := range map[string]string{claudeModOn: "--claude-usage-mod", claudeModOff: "--claude-usage-mod=off"} {
		got := reexecRequestFor("/opt/aira", target, installOpts{claudeUsageMod: opts}).args
		if !containsArg(got, want) {
			t.Fatalf("mod=%q: reexec args %q lack %q", opts, got, want)
		}
	}
	for _, arg := range reexecRequestFor("/opt/aira", target, installOpts{}).args {
		if strings.Contains(arg, "claude-usage-mod") {
			t.Fatalf("a flag that was not given was forwarded: %q", arg)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// verifies: AIRA-284 §3.1 — the shipped mod: exactly the three files, the
// binary token appears exactly once, and the source is a review-friendly
// counters-and-ids hook (a review aid, not a sandbox).
func TestEmbeddedClaudeUsageModShape(t *testing.T) {
	files, err := claudeModShippedFiles()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != ".claude-plugin/plugin.json,hooks/hooks.json,hooks/register.ts" {
		t.Fatalf("shipped files=%s", got)
	}
	source := string(files["hooks/register.ts"])
	if n := strings.Count(source, claudeModBinaryToken); n != 1 {
		t.Fatalf("the binary token %s appears %d times in register.ts, want exactly 1", claudeModBinaryToken, n)
	}
	for _, forbidden := range []string{
		"$.session.messages", "$.fs", "$.http", "$.settings", "$.session.append", "$.prompt", "$.agent.spawn",
		"$.store", "$.state", "$.mcp", "$.tool", "--at'", "eval(", "Function(",
	} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("register.ts mentions the forbidden surface %q", forbidden)
		}
	}
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "import ") && !strings.HasPrefix(line, "import type ") {
			t.Fatalf("register.ts has a runtime import: %q", line)
		}
	}
	if !strings.Contains(source, "'turn.complete'") || strings.Count(source, "on('") != 1 {
		t.Fatalf("register.ts must hook turn.complete and nothing else")
	}
	var manifest map[string]any
	if err := json.Unmarshal(files[".claude-plugin/plugin.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["name"] != "aira-usage" {
		t.Fatalf("manifest name=%v", manifest["name"])
	}
	if _, has := manifest["userConfig"]; has {
		t.Fatal("the mod must have no userConfig")
	}
	var hooks struct{ Modules []string }
	if err := json.Unmarshal(files["hooks/hooks.json"], &hooks); err != nil || len(hooks.Modules) != 1 || hooks.Modules[0] != "./register.ts" {
		t.Fatalf("hooks.json=%s err=%v", files["hooks/hooks.json"], err)
	}
}

// verifies: AIRA-284 §3.6 — install writes the three files with the ABSOLUTE
// path of the running binary substituted for the token (a JS string literal),
// a marker with the sha256 of each installed file, mode 0644, and leaves no
// temp files; the install honours the test HOME.
func TestInstallClaudeUsageModWritesFilesMarkerAndBinaryPath(t *testing.T) {
	d, state := newFakeInstall(t)
	d.executable = func() (string, error) { return "/opt/with space/ai\"ra", nil }
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	dir := modDir(state)
	if got := strings.Join(listTree(t, dir), ","); got != ".aira-managed.json,.claude-plugin/,.claude-plugin/plugin.json,hooks/,hooks/hooks.json,hooks/register.ts" {
		t.Fatalf("tree=%s", got)
	}
	register, err := os.ReadFile(filepath.Join(dir, "hooks", "register.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(register), "@@AIRA_BINARY@@") {
		t.Fatal("the binary token survived the install")
	}
	wantLiteral, _ := json.Marshal("/opt/with space/ai\"ra")
	if !strings.Contains(string(register), "const AIRA_BINARY = "+string(wantLiteral)+"\n") {
		t.Fatalf("register.ts does not bind AIRA_BINARY to %s:\n%s", wantLiteral, register)
	}
	marker := readMarker(t, state)
	if marker.Binary != "/opt/with space/ai\"ra" || len(marker.Files) != 3 {
		t.Fatalf("marker=%+v", marker)
	}
	for rel, want := range marker.Files {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil || sha(data) != want {
			t.Fatalf("marker hash for %s does not match the installed bytes (err=%v)", rel, err)
		}
		info, _ := os.Lstat(filepath.Join(dir, rel))
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("%s mode=%v, want 0644", rel, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(state.home, ".claude", "skills", "aira-usage", ".tmp")); err == nil {
		t.Fatal("temp state left behind")
	}
}

// verifies: AIRA-284 §3.6 — a relative executable path is made absolute before it
// is recorded: a mod must never run `aira` by PATH or by a cwd-relative name.
func TestInstallClaudeUsageModRecordsAnAbsolutePath(t *testing.T) {
	d, state := newFakeInstall(t)
	d.executable = func() (string, error) { return "bin/aira", nil }
	d.abs = func(p string) (string, error) {
		if strings.HasPrefix(p, "/") {
			return p, nil
		}
		return "/srv/" + p, nil
	}
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	if got := readMarker(t, state).Binary; got != "/srv/bin/aira" {
		t.Fatalf("recorded binary=%q", got)
	}
	d.abs = func(p string) (string, error) { return p, nil }
	if err := installMod(t, d, claudeModOn); err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
		t.Fatalf("a still-relative path was accepted: err=%v", err)
	}
}

// verifies: AIRA-284 §3.6 — re-running the install converges: same bytes, same
// marker; a modified marked file is replaced (the marker says the file is ours).
func TestInstallClaudeUsageModReinstallConvergesAndRepairs(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	first := readMarker(t, state)
	register := filepath.Join(modDir(state), "hooks", "register.ts")
	if err := os.WriteFile(register, []byte("// tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	second := readMarker(t, state)
	if second.Binary != first.Binary || second.Files["hooks/register.ts"] != first.Files["hooks/register.ts"] {
		t.Fatalf("reinstall did not restore the shipped bytes: %+v vs %+v", second, first)
	}
	if data, _ := os.ReadFile(register); sha(data) != first.Files["hooks/register.ts"] {
		t.Fatal("the tampered file was not replaced")
	}
}

// verifies: AIRA-284 §3.6 — refusals leave NOTHING written: a symlinked mod
// directory, a symlinked marked file, a foreign file at a target path, and a
// non-empty directory with no marker. A symlink's destination is never written
// through.
func TestInstallClaudeUsageModRefusesSymlinkedAndForeignTargets(t *testing.T) {
	t.Run("symlinked directory", func(t *testing.T) {
		d, state := newFakeInstall(t)
		elsewhere := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(modDir(state)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, modDir(state)); err != nil {
			t.Fatal(err)
		}
		if err := installMod(t, d, claudeModOn); err == nil || !strings.Contains(err.Error(), CodeUnavailable) || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err=%v, want %s naming the symlink", err, CodeUnavailable)
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Fatalf("wrote through a symlinked directory: %v", entries)
		}
	})
	t.Run("symlinked marked file", func(t *testing.T) {
		d, state := newFakeInstall(t)
		if err := installMod(t, d, claudeModOn); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
			t.Fatal(err)
		}
		register := filepath.Join(modDir(state), "hooks", "register.ts")
		if err := os.Remove(register); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, register); err != nil {
			t.Fatal(err)
		}
		if err := installMod(t, d, claudeModOn); err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
			t.Fatalf("err=%v, want %s", err, CodeUnavailable)
		}
		if data, _ := os.ReadFile(victim); string(data) != "precious" {
			t.Fatalf("wrote through a symlinked file: %q", data)
		}
		if info, _ := os.Lstat(register); info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the symlink was replaced instead of refused")
		}
	})
	t.Run("foreign file at a target path, no marker", func(t *testing.T) {
		d, state := newFakeInstall(t)
		if err := os.MkdirAll(filepath.Join(modDir(state), "hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(modDir(state), "hooks", "register.ts")
		if err := os.WriteFile(foreign, []byte("// someone else's\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := installMod(t, d, claudeModOn); err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
			t.Fatalf("err=%v, want %s", err, CodeUnavailable)
		}
		if data, _ := os.ReadFile(foreign); string(data) != "// someone else's\n" {
			t.Fatalf("the foreign file was overwritten: %q", data)
		}
		if _, err := os.Stat(filepath.Join(modDir(state), claudeModMarkerName)); err == nil {
			t.Fatal("a marker was written over a foreign directory")
		}
	})
	t.Run("non-empty directory without a marker", func(t *testing.T) {
		d, state := newFakeInstall(t)
		if err := os.MkdirAll(modDir(state), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(modDir(state), "notes.txt"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := installMod(t, d, claudeModOn); err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
			t.Fatalf("err=%v, want %s", err, CodeUnavailable)
		}
		if got := strings.Join(listTree(t, modDir(state)), ","); got != "notes.txt" {
			t.Fatalf("the directory changed: %s", got)
		}
	})
	t.Run("marked directory but a foreign (unmarked) file at a target path", func(t *testing.T) {
		d, state := newFakeInstall(t)
		if err := installMod(t, d, claudeModOn); err != nil {
			t.Fatal(err)
		}
		// The marker is rewritten to stop claiming hooks.json: that file is now
		// foreign as far as aira can prove.
		marker := readMarker(t, state)
		delete(marker.Files, "hooks/hooks.json")
		data, _ := json.Marshal(marker)
		if err := os.WriteFile(filepath.Join(modDir(state), claudeModMarkerName), data, 0o644); err != nil {
			t.Fatal(err)
		}
		hooks := filepath.Join(modDir(state), "hooks", "hooks.json")
		if err := os.WriteFile(hooks, []byte(`{"modules":["./mine.ts"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := installMod(t, d, claudeModOn); err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
			t.Fatalf("err=%v, want %s", err, CodeUnavailable)
		}
		if got, _ := os.ReadFile(hooks); string(got) != `{"modules":["./mine.ts"]}` {
			t.Fatalf("the foreign file was overwritten: %q", got)
		}
	})
}

// verifies: AIRA-284 §3.6 — writes are atomic: when the rename of one file
// fails, every previously installed file keeps its bytes and no temp files
// remain.
func TestInstallClaudeUsageModIsAtomicPerFile(t *testing.T) {
	d, state := newFakeInstall(t)
	d.executable = func() (string, error) { return "/opt/aira-old", nil }
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, rel := range listTree(t, modDir(state)) {
		if data, err := os.ReadFile(filepath.Join(modDir(state), rel)); err == nil {
			before[rel] = string(data)
		}
	}
	d.executable = func() (string, error) { return "/opt/aira-new", nil }
	realRename := d.rename
	d.rename = func(from, to string) error {
		if strings.HasSuffix(to, filepath.Join("hooks", "register.ts")) {
			return errors.New("injected rename failure")
		}
		return realRename(from, to)
	}
	if err := installMod(t, d, claudeModOn); err == nil {
		t.Fatal("the injected rename failure was swallowed")
	}
	data, err := os.ReadFile(filepath.Join(modDir(state), "hooks", "register.ts"))
	if err != nil || string(data) != before["hooks/register.ts"] {
		t.Fatalf("register.ts is no longer the previously installed file (err=%v)", err)
	}
	for _, rel := range listTree(t, modDir(state)) {
		if strings.Contains(rel, "tmp") {
			t.Fatalf("temp state left behind: %s", rel)
		}
	}
}

// verifies: AIRA-284 §3.6 — `--claude-usage-mod=off` removes ONLY the marked
// files and the marker; an unknown file stays and is reported, and its
// directory with it.
func TestClaudeUsageModOffRemovesOnlyMarkedFiles(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(modDir(state), "hooks", "mine.txt")
	if err := os.WriteFile(stray, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	state.logs = nil
	if err := installMod(t, d, claudeModOff); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(listTree(t, modDir(state)), ","); got != "hooks/,hooks/mine.txt" {
		t.Fatalf("after off the tree is %s, want only the unknown file", got)
	}
	if !strings.Contains(strings.Join(state.logs, "\n"), "mine.txt") {
		t.Fatalf("the left-behind file was not reported:\n%s", strings.Join(state.logs, "\n"))
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	// Idempotent: nothing marked remains, nothing to do, no error.
	if err := installMod(t, d, claudeModOff); err != nil {
		t.Fatalf("off with nothing installed: %v", err)
	}
}

// verifies: AIRA-284 §3.6 — a clean `off` removes the whole directory, and `off`
// over a directory aira did not write (no marker) touches nothing.
func TestClaudeUsageModOffCleanAndForeign(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	if err := installMod(t, d, claudeModOff); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(modDir(state)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a clean off left %s behind (err=%v)", modDir(state), err)
	}
	if err := os.MkdirAll(modDir(state), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir(state), "register.ts"), []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installMod(t, d, claudeModOff); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(listTree(t, modDir(state)), ","); got != "register.ts" {
		t.Fatalf("off touched a foreign directory: %s", got)
	}
}

// verifies: AIRA-284 §3.6 — `off` never removes a marked path that has become a
// symlink (not ours any more), and never follows it.
func TestClaudeUsageModOffDoesNotRemoveASymlinkedMarkedPath(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(modDir(state), "hooks", "hooks.json")
	_ = os.Remove(hooks)
	if err := os.Symlink(victim, hooks); err != nil {
		t.Fatal(err)
	}
	if err := installMod(t, d, claudeModOff); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "precious" {
		t.Fatal("off removed or rewrote through the symlink")
	}
	if info, err := os.Lstat(hooks); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was removed (err=%v)", err)
	}
}

// verifies: AIRA-284 §3.6 — an ordinary reinstall (no flag) preserves an
// installed mod, modified or not; only the flag touches it.
func TestOrdinaryReinstallPreservesTheInstalledMod(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", claudeUsageMod: claudeModOn}); err != nil {
		t.Fatal(err)
	}
	register := filepath.Join(modDir(state), "hooks", "register.ts")
	if err := os.WriteFile(register, []byte("// local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(register); string(data) != "// local edit\n" {
		t.Fatalf("an ordinary reinstall rewrote the mod: %q", data)
	}
	if _, err := os.Stat(filepath.Join(modDir(state), claudeModMarkerName)); err != nil {
		t.Fatalf("an ordinary reinstall removed the marker: %v", err)
	}
}

// verifies: AIRA-284 §3.6 — an install without the flag never creates the mod
// (opt-in), on the real-slice and the ci-shim paths alike.
func TestInstallWithoutTheFlagNeverCreatesTheMod(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(state.home, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an install without --claude-usage-mod touched ~/.claude (err=%v)", err)
	}
}

// verifies: AIRA-284 §3.6 — the flag installs the mod as part of a real install,
// and of a ci-shim build, in the install's own HOME.
func TestFlagInstallsTheModOnRealAndShimPaths(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", claudeUsageMod: claudeModOn}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(modDir(state), "hooks", "register.ts")); err != nil {
		t.Fatalf("real install did not install the mod: %v", err)
	}
	if err := installMod(t, d, claudeModOff); err != nil {
		t.Fatal(err)
	}

	d2, state2 := newFakeInstall(t)
	d2 = shimProbeDeps(t, d2, state2, map[string]bool{"timeout": true})
	d2.readFile = shimProcReader(nil)
	d2.spawnShimDaemon = func(shimDaemonSpec) error { t.Fatal("the build stage started a daemon"); return nil }
	if err := runInstall(d2, installOpts{ciValue: "shim", memoryMax: "8G", stage: installStageBuild, claudeUsageMod: claudeModOn}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(modDir(state2), "hooks", "register.ts")); err != nil {
		t.Fatalf("shim build did not install the mod: %v", err)
	}
}

// verifies: AIRA-284 §3.6 — --dry-run plans and writes nothing.
func TestClaudeUsageModDryRunWritesNothing(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", dryRun: true, claudeUsageMod: claudeModOn}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(state.home, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--dry-run wrote ~/.claude (err=%v)", err)
	}
	if !strings.Contains(strings.Join(state.logs, "\n"), "planned: install the aira-usage Claude Code mod") {
		t.Fatalf("the dry run did not plan the mod:\n%s", strings.Join(state.logs, "\n"))
	}
}

func statusLines(t *testing.T, d installDeps, state *fakeInstallState) string {
	t.Helper()
	state.logs = nil
	reportClaudeUsageModStatus(d)
	return strings.Join(state.logs, "\n")
}

// verifies: AIRA-284 §3.6 — --status re-hashes against the marker and reports
// ok | modified | absent, plus whether the recorded binary still exists.
func TestClaudeUsageModStatus(t *testing.T) {
	d, state := newFakeInstall(t)
	binary := filepath.Join(t.TempDir(), "aira")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.executable = func() (string, error) { return binary, nil }

	if got := statusLines(t, d, state); !strings.Contains(got, "claude usage mod: absent") || strings.Contains(got, "binary:") {
		t.Fatalf("before install:\n%s", got)
	}
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	if got := statusLines(t, d, state); !strings.Contains(got, "claude usage mod: ok") || !strings.Contains(got, "claude usage mod binary: ok") {
		t.Fatalf("after install:\n%s", got)
	}
	register := filepath.Join(modDir(state), "hooks", "register.ts")
	original, _ := os.ReadFile(register)
	if err := os.WriteFile(register, append(original, []byte("// edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := statusLines(t, d, state); !strings.Contains(got, "claude usage mod: modified") || !strings.Contains(got, "hooks/register.ts") {
		t.Fatalf("after a byte edit:\n%s", got)
	}
	if err := os.Remove(register); err != nil {
		t.Fatal(err)
	}
	if got := statusLines(t, d, state); !strings.Contains(got, "claude usage mod: modified") {
		t.Fatalf("after deleting a marked file:\n%s", got)
	}
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	got := statusLines(t, d, state)
	if !strings.Contains(got, "claude usage mod: ok") || !strings.Contains(got, "claude usage mod binary: absent") {
		t.Fatalf("after the recorded binary vanished (a moved aira would look like 'mod not loaded'):\n%s", got)
	}
	if err := os.WriteFile(filepath.Join(modDir(state), claudeModMarkerName), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := statusLines(t, d, state); !strings.Contains(got, "claude usage mod: modified") {
		t.Fatalf("with a corrupt marker:\n%s", got)
	}
}

// verifies: AIRA-284 §3.6 — a directory aira did not write is reported absent,
// never ok, and status is part of runStatus.
func TestClaudeUsageModStatusIsPartOfRunStatus(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", claudeUsageMod: claudeModOn}); err != nil {
		t.Fatal(err)
	}
	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(state.logs, "\n"); !regexp.MustCompile(`claude usage mod: ok`).MatchString(got) {
		t.Fatalf("runStatus lacks the mod line:\n%s", got)
	}
}

// verifies: AIRA-284 §3.6 — a failure on a FRESH install (the first file's
// rename) leaves a directory aira can prove is its own: status says modified,
// and re-running the install repairs it instead of refusing it as foreign.
func TestFailedFreshInstallIsRepairableByRerunning(t *testing.T) {
	d, state := newFakeInstall(t)
	realRename := d.rename
	failing := true
	d.rename = func(from, to string) error {
		if failing && strings.HasSuffix(to, filepath.Join(".claude-plugin", "plugin.json")) {
			return errors.New("injected rename failure")
		}
		return realRename(from, to)
	}
	if err := installMod(t, d, claudeModOn); err == nil {
		t.Fatal("the injected failure was swallowed")
	}
	if got := statusLines(t, d, state); !strings.Contains(got, "claude usage mod: modified") {
		t.Fatalf("a half-installed mod must read modified:\n%s", got)
	}
	failing = false
	if err := installMod(t, d, claudeModOn); err != nil {
		t.Fatalf("re-running did not repair the half-installed directory: %v", err)
	}
	if got := statusLines(t, d, state); !strings.Contains(got, "claude usage mod: ok") {
		t.Fatalf("after the repair:\n%s", got)
	}
}

// verifies: AIRA-284 §3.6 — a directory aira did not write reads `absent`, never
// `ok`, and a marker that lists a path outside the shipped set cannot aim a
// removal at it.
func TestClaudeUsageModForeignDirectoryAndDoctoredMarker(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := os.MkdirAll(modDir(state), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir(state), "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := statusLines(t, d, state); !strings.Contains(got, "claude usage mod: absent") || strings.Contains(got, "mod: ok") {
		t.Fatalf("foreign directory:\n%s", got)
	}
	d2, state2 := newFakeInstall(t)
	if err := installMod(t, d2, claudeModOn); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(state2.home, "outside.txt")
	if err := os.WriteFile(outside, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := readMarker(t, state2)
	marker.Files["../../../outside.txt"] = "00"
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(filepath.Join(modDir(state2), claudeModMarkerName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installMod(t, d2, claudeModOff); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "precious" {
		t.Fatal("a doctored marker aimed `off` at a file outside the mod")
	}
}
