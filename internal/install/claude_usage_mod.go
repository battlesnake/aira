package install

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AIRA-284 slice B. `aira install --claude-usage-mod[=off]` installs the fixed,
// reviewable `aira-usage` Claude Code mod into ~/.claude/skills/aira-usage/.
//
// A mod runs inside the harness with the user's privileges, so this is opt-in
// and the install is deliberately paranoid about what it writes and what it
// later removes:
//
//   - the three shipped files are written atomically (a private temp directory
//     beside the target, then rename), and never through a symlink;
//   - a target aira did not write (no marker, or a path the marker does not
//     list) is refused, with NOTHING written;
//   - the marker records the sha256 of every installed file and the absolute
//     path of the aira binary that installed it; `off` removes only marked
//     files, `--status` re-hashes against the marker;
//   - the mod invokes that recorded absolute path, never `aira` from PATH.
//
// A hash check detects drift after the fact; it is not a sandbox.

//go:embed claudemod/.claude-plugin/plugin.json claudemod/hooks/hooks.json claudemod/hooks/register.ts
var claudeModFS embed.FS

const (
	claudeModOn  = "on"
	claudeModOff = "off"

	claudeModDirName    = "aira-usage"
	claudeModMarkerName = ".aira-managed.json"
	// claudeModBinaryToken is the placeholder in register.ts (quotes included)
	// that the install replaces with the recorded binary path as a JS string.
	claudeModBinaryToken = "'@@AIRA_BINARY@@'"
)

// claudeModMarker is the on-disk ownership record.
type claudeModMarker struct {
	Schema int               `json:"schema"`
	Binary string            `json:"binary"`
	Files  map[string]string `json:"files"` // slash-separated relative path -> sha256 hex of the installed bytes
}

// claudeModShippedFiles returns the embedded template, keyed by the path the
// file has inside the installed directory.
func claudeModShippedFiles() (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, rel := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts"} {
		data, err := claudeModFS.ReadFile("claudemod/" + rel)
		if err != nil {
			return nil, fmt.Errorf("embedded claude usage mod is missing %s: %w", rel, err)
		}
		files[rel] = data
	}
	return files, nil
}

// claudeModSubdirs are the two sub-directories the mod's files live in.
var claudeModSubdirs = []string{".claude-plugin", "hooks"}

// plainClaudeModSubdirs reports, per sub-directory, whether it is a real
// directory (not a symlink, not a file). lstat on a path BELOW a symlinked
// sub-directory follows that symlink, so every walk over the shipped files must
// check this first or it will treat the symlink target's files as aira's own.
func plainClaudeModSubdirs(d installDeps, dir string) map[string]bool {
	plain := map[string]bool{}
	for _, sub := range claudeModSubdirs {
		info, err := d.lstat(filepath.Join(dir, sub))
		plain[sub] = err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
	}
	return plain
}

func claudeModSubdirOf(rel string) string {
	sub, _, _ := strings.Cut(rel, "/")
	return sub
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func claudeModDir(d installDeps) (string, error) {
	home, err := installHome(d)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills", claudeModDirName), nil
}

// readClaudeModMarker returns (marker, present, err): present is false when no
// marker file exists; err is set when one exists but cannot be trusted.
func readClaudeModMarker(d installDeps, dir string) (claudeModMarker, bool, error) {
	data, err := d.readFile(filepath.Join(dir, claudeModMarkerName))
	if errors.Is(err, fs.ErrNotExist) {
		return claudeModMarker{}, false, nil
	}
	if err != nil {
		return claudeModMarker{}, true, err
	}
	var marker claudeModMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return claudeModMarker{}, true, fmt.Errorf("marker is not valid JSON: %w", err)
	}
	if marker.Schema != 1 || marker.Files == nil {
		return claudeModMarker{}, true, errors.New("marker has an unknown schema")
	}
	return marker, true, nil
}

// installClaudeUsageMod applies --claude-usage-mod / --claude-usage-mod=off. An
// install without the flag does nothing here (an ordinary reinstall preserves an
// installed mod, edited or not).
func installClaudeUsageMod(d installDeps, opts installOpts) error {
	if opts.claudeUsageMod == "" {
		return nil
	}
	d = fillInstallDeps(d)
	dir, err := claudeModDir(d)
	if err != nil {
		return err
	}
	if opts.claudeUsageMod == claudeModOff {
		return removeClaudeUsageMod(d, dir, opts.dryRun)
	}
	return writeClaudeUsageMod(d, dir, opts.dryRun)
}

func writeClaudeUsageMod(d installDeps, dir string, dryRun bool) error {
	executable, err := d.executable()
	if err != nil {
		return unavailable(fmt.Errorf("resolve running executable for the claude usage mod: %w", err))
	}
	if executable, err = d.abs(executable); err != nil {
		return unavailable(fmt.Errorf("make the claude usage mod's aira path absolute: %w", err))
	}
	if !filepath.IsAbs(executable) {
		return unavailable(fmt.Errorf("the claude usage mod must invoke aira by an absolute path, got %q", executable))
	}
	shipped, err := claudeModShippedFiles()
	if err != nil {
		return unavailable(err)
	}
	literal, err := json.Marshal(executable)
	if err != nil {
		return unavailable(err)
	}
	if bytes.Count(shipped["hooks/register.ts"], []byte(claudeModBinaryToken)) != 1 {
		return unavailable(errors.New("embedded claude usage mod: the binary token must appear exactly once in register.ts"))
	}
	shipped["hooks/register.ts"] = bytes.Replace(shipped["hooks/register.ts"], []byte(claudeModBinaryToken), literal, 1)
	marker := claudeModMarker{Schema: 1, Binary: executable, Files: map[string]string{}}
	for rel, data := range shipped {
		marker.Files[rel] = sha256Hex(data)
	}
	if dryRun {
		d.logf("planned: install the aira-usage Claude Code mod into %s (aira binary %s); an install without --claude-usage-mod never creates it", dir, executable)
		return nil
	}
	previous, err := checkClaudeModTargets(d, dir, shipped)
	if err != nil {
		return err
	}
	for _, sub := range claudeModSubdirs {
		if err := d.mkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return unavailable(fmt.Errorf("create %s: %w", filepath.Join(dir, sub), err))
		}
	}
	// The marker goes first: a crash after it leaves a directory aira can prove
	// is its own (status reports it modified; the next install repairs it),
	// whereas files without a marker would look foreign forever.
	markerBytes, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return unavailable(err)
	}
	if err := atomicWriteFile(d, filepath.Join(dir, claudeModMarkerName), append(markerBytes, '\n')); err != nil {
		return unavailable(err)
	}
	for _, rel := range sortedKeys(shipped) {
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if previous != nil && previous.Files[rel] != "" && previous.Files[rel] != marker.Files[rel] {
			d.logf("claude usage mod: replacing %s (its bytes changed since the last install)", rel)
		}
		if err := atomicWriteFile(d, target, shipped[rel]); err != nil {
			return unavailable(err)
		}
	}
	d.logf("installed: Claude Code mod aira-usage in %s (aira binary %s); it reports counters and ids only, and loads in the next Claude Code session", dir, executable)
	return nil
}

// checkClaudeModTargets refuses, before anything is written, every layout aira
// cannot prove is its own. It returns the previous marker when there is one.
func checkClaudeModTargets(d installDeps, dir string, shipped map[string][]byte) (*claudeModMarker, error) {
	info, err := d.lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable(fmt.Errorf("inspect %s: %w", dir, err))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, unavailable(fmt.Errorf("%s is a symlink; refusing to install the claude usage mod through it", dir))
	}
	if !info.IsDir() {
		return nil, unavailable(fmt.Errorf("%s exists and is not a directory", dir))
	}
	marker, present, markerErr := readClaudeModMarker(d, dir)
	var previous *claudeModMarker
	switch {
	case markerErr != nil:
		return nil, unavailable(fmt.Errorf("%s has a marker aira cannot trust (%v); remove the directory by hand to reinstall", dir, markerErr))
	case present:
		previous = &marker
	default:
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return nil, unavailable(fmt.Errorf("inspect %s: %w", dir, readErr))
		}
		if len(entries) != 0 {
			return nil, unavailable(fmt.Errorf("%s exists, is not empty and has no aira marker; refusing to overwrite a directory aira did not write", dir))
		}
	}
	for _, sub := range claudeModSubdirs {
		subInfo, subErr := d.lstat(filepath.Join(dir, sub))
		if errors.Is(subErr, fs.ErrNotExist) {
			continue
		}
		if subErr != nil {
			return nil, unavailable(fmt.Errorf("inspect %s: %w", filepath.Join(dir, sub), subErr))
		}
		if subInfo.Mode()&os.ModeSymlink != 0 || !subInfo.IsDir() {
			return nil, unavailable(fmt.Errorf("%s is not a plain directory; refusing to install the claude usage mod through it", filepath.Join(dir, sub)))
		}
	}
	for rel := range shipped {
		target := filepath.Join(dir, filepath.FromSlash(rel))
		fileInfo, fileErr := d.lstat(target)
		if errors.Is(fileErr, fs.ErrNotExist) {
			continue
		}
		if fileErr != nil {
			return nil, unavailable(fmt.Errorf("inspect %s: %w", target, fileErr))
		}
		if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
			return nil, unavailable(fmt.Errorf("%s is not a regular file (a symlink?); refusing to write through it", target))
		}
		if previous == nil || previous.Files[rel] == "" {
			return nil, unavailable(fmt.Errorf("%s exists and aira did not write it (the marker does not list it); refusing to overwrite", target))
		}
	}
	return previous, nil
}

// atomicWriteFile writes data to path through a private temp directory beside
// it and a rename, so a reader (or a failure) never sees a half-written file
// and a pre-existing target is replaced, never followed or truncated in place.
func atomicWriteFile(d installDeps, path string, data []byte) error {
	tmpDir, err := d.mkdirTemp(filepath.Dir(path), ".aira-tmp-")
	if err != nil {
		return fmt.Errorf("create a temp directory beside %s: %w", path, err)
	}
	defer os.RemoveAll(tmpDir)
	tmp := filepath.Join(tmpDir, "file")
	if err := d.writeFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := d.rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func removeClaudeUsageMod(d installDeps, dir string, dryRun bool) error {
	info, err := d.lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		d.logf("claude usage mod: not installed; nothing to remove")
		return nil
	}
	if err != nil {
		return unavailable(fmt.Errorf("inspect %s: %w", dir, err))
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		d.logf("claude usage mod: %s is not a plain directory aira wrote; left alone", dir)
		return nil
	}
	marker, present, markerErr := readClaudeModMarker(d, dir)
	if markerErr != nil || !present {
		d.logf("claude usage mod: %s has no aira marker I can trust; left alone (nothing removed)", dir)
		return nil
	}
	shipped, err := claudeModShippedFiles()
	if err != nil {
		return unavailable(err)
	}
	if dryRun {
		d.logf("planned: remove the aira-usage Claude Code mod's marked files from %s", dir)
		return nil
	}
	// Only a path that is BOTH in the shipped set and in the marker is ever
	// removed, so a doctored marker cannot aim a removal elsewhere.
	plain := plainClaudeModSubdirs(d, dir)
	for _, rel := range sortedKeys(shipped) {
		if marker.Files[rel] == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if sub := claudeModSubdirOf(rel); !plain[sub] {
			if _, subErr := d.lstat(filepath.Join(dir, sub)); !errors.Is(subErr, fs.ErrNotExist) {
				d.logf("claude usage mod: left %s (%s is not a plain directory aira wrote; not following it)", target, filepath.Join(dir, sub))
			}
			continue
		}
		fileInfo, fileErr := d.lstat(target)
		if errors.Is(fileErr, fs.ErrNotExist) {
			continue
		}
		if fileErr != nil || fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
			d.logf("claude usage mod: left %s (no longer a regular file aira wrote)", target)
			continue
		}
		if err := d.remove(target); err != nil {
			return unavailable(fmt.Errorf("remove %s: %w", target, err))
		}
	}
	if err := d.remove(filepath.Join(dir, claudeModMarkerName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return unavailable(fmt.Errorf("remove the marker: %w", err))
	}
	// Empty directories go (os.Remove refuses a non-empty one); whatever is left
	// was not written by aira and is reported.
	for _, sub := range claudeModSubdirs {
		if plain[sub] {
			_ = d.remove(filepath.Join(dir, sub))
		}
	}
	_ = d.remove(dir)
	var left []string
	if _, statErr := d.lstat(dir); statErr == nil {
		_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr == nil && !entry.IsDir() {
				rel, _ := filepath.Rel(dir, path)
				left = append(left, rel)
			}
			return nil
		})
	}
	if len(left) > 0 {
		sort.Strings(left)
		d.logf("claude usage mod: removed the files aira wrote; left in place (not written by aira): %s", strings.Join(left, ", "))
	} else {
		d.logf("claude usage mod: removed %s", dir)
	}
	return nil
}

// reportClaudeUsageModStatus is `aira install --status`'s mod facet: the mod's
// files re-hashed against the marker (ok | modified | absent) and whether the
// recorded aira binary still exists (a moved binary would otherwise look like
// "mod not loaded").
func reportClaudeUsageModStatus(d installDeps) {
	dir, err := claudeModDir(d)
	if err != nil {
		d.logf("claude usage mod: unevaluated (%v)", err)
		return
	}
	info, err := d.lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		d.logf("claude usage mod: absent")
		return
	}
	if err != nil {
		d.logf("claude usage mod: unevaluated (%v)", err)
		return
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		d.logf("claude usage mod: absent (%s is not a plain directory aira wrote)", dir)
		return
	}
	marker, present, markerErr := readClaudeModMarker(d, dir)
	if !present {
		d.logf("claude usage mod: absent (%s exists without an aira marker; not managed)", dir)
		return
	}
	if markerErr != nil {
		d.logf("claude usage mod: modified (the marker in %s is unreadable: %v)", dir, markerErr)
		return
	}
	shipped, err := claudeModShippedFiles()
	if err != nil {
		d.logf("claude usage mod: unevaluated (%v)", err)
		return
	}
	var problems []string
	plain := plainClaudeModSubdirs(d, dir)
	for _, sub := range claudeModSubdirs {
		if _, subErr := d.lstat(filepath.Join(dir, sub)); !plain[sub] && !errors.Is(subErr, fs.ErrNotExist) {
			problems = append(problems, sub+" is not a plain directory")
		}
	}
	for _, rel := range sortedKeys(shipped) {
		if sub := claudeModSubdirOf(rel); !plain[sub] {
			continue
		}
		want := marker.Files[rel]
		target := filepath.Join(dir, filepath.FromSlash(rel))
		fileInfo, fileErr := d.lstat(target)
		switch {
		case want == "":
			problems = append(problems, rel+" is not in the marker")
		case errors.Is(fileErr, fs.ErrNotExist):
			problems = append(problems, rel+" is missing")
		case fileErr != nil:
			problems = append(problems, fmt.Sprintf("%s cannot be inspected (%v)", rel, fileErr))
		case fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular():
			problems = append(problems, rel+" is not a regular file")
		default:
			data, readErr := d.readFile(target)
			if readErr != nil {
				problems = append(problems, fmt.Sprintf("%s cannot be read (%v)", rel, readErr))
			} else if sha256Hex(data) != want {
				problems = append(problems, rel+" changed")
			}
		}
	}
	if len(problems) > 0 {
		d.logf("claude usage mod: modified (%s)", strings.Join(problems, "; "))
	} else {
		d.logf("claude usage mod: ok (%s)", dir)
	}
	if _, statErr := d.stat(marker.Binary); marker.Binary != "" && statErr == nil {
		d.logf("claude usage mod binary: ok (%s)", marker.Binary)
	} else {
		d.logf("claude usage mod binary: absent (%s); re-run `aira install --claude-usage-mod` from the aira you want it to call", marker.Binary)
	}
}
