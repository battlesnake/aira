package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// subjectEntryKind is what a tracked entry is, so two trees differing only in
// entry kind cannot share a digest: a symlink a -> b must not collide with a
// regular file a containing "b", and a script that has lost its executable bit
// must not read as the same subject as one that still has it. The set mirrors
// git's own tree model (100644, 100755, 120000), which is what the design docs
// mean by "the commit/tree digest".
type subjectEntryKind byte

const (
	subjectEntryRegular    subjectEntryKind = 'f'
	subjectEntryExecutable subjectEntryKind = 'x'
	subjectEntrySymlink    subjectEntryKind = 'l'
)

// subjectEntry is the one capture type shared by the subject digest and the
// command-gate materialiser, so the bytes that get digested and the bytes that
// get executed can never come from two different reads of the tree.
type subjectEntry struct {
	path    string
	kind    subjectEntryKind
	payload []byte
	perm    os.FileMode
}

func (e subjectEntry) regular() bool {
	return e.kind == subjectEntryRegular || e.kind == subjectEntryExecutable
}

// subjectTreeDigest is the single producer of every gate subject digest.
//
// It covers the whole tracked tree. AIRA-72: the previous implementation
// digested only tracked *.go and .aira/requirements/*.md, because it reused
// trackedTracePaths -- a go/parser input selector written for the traceability
// dimension, where the narrow scope is correct. As a subject-identity selector
// it meant that on any project whose gated logic is not Go, the digest was a
// constant with respect to the code under test, so a stored trusted pass was
// re-served forever (ProofPolicy.MaxAgeSecs defaults to 0, no expiry). The
// design has always specified the subject as the commit/tree digest; this
// restores that.
//
// The scope is deliberately "every tracked file, full stop" rather than an
// allowlist. An allowlist over the subject is the exact mechanism that produced
// AIRA-72: it cannot be kept true as a project grows. Over-invalidation is the
// safe direction -- it yields unevaluated, never a fabricated pass.
//
// Why not git's own `git write-tree` as the identity: it would encode path,
// mode, symlink and gitlink natively and needs no framing of ours. It is
// rejected because it reports the *index*, and the index honours
// assume-unchanged and skip-worktree bits -- under either, a real working-tree
// edit is invisible to git's tree hash. For an honesty digest whose entire job
// is to notice that the subject changed, reading the working tree directly is
// the more faithful witness; adopting git's tree id would reintroduce exactly
// the silent-staleness class this ticket exists to close.
//
// Accepted boundary: a working-tree file never git-added is invisible here,
// because the subject of a gate is the tracked tree and every checker evaluates
// only tracked content -- materializeTrackedSnapshot materialises only tracked
// files, so a command gate never executes untracked content. Pinned by
// TestSubjectDigestIgnoresUntrackedFiles.
func subjectTreeDigest(root string) (string, error) {
	entries, err := captureSubjectEntries(root)
	if err != nil {
		return "", err
	}
	return digestSubjectEntries(entries), nil
}

// digestSubjectEntries frames every field with an explicit length.
//
// The previous framing wrote path, NUL, data, NUL, which is ambiguous: one file
// {"a": "b\x00c\x00d"} and two files {"a": "b", "c": "d"} both serialise to
// a\0b\0c\0d\0. Git paths cannot contain NUL but file content can, so both trees
// are constructible and collided -- a stale pass servable against a genuinely
// different tree, without breaking SHA-256. Length prefixes remove the whole
// ambiguity class rather than patching one case.
func digestSubjectEntries(entries []subjectEntry) string {
	h := sha256.New()
	var length [8]byte
	write := func(data []byte) {
		binary.BigEndian.PutUint64(length[:], uint64(len(data)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(data)
	}
	for _, entry := range entries {
		write([]byte(entry.path))
		_, _ = h.Write([]byte{byte(entry.kind)})
		write(entry.payload)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// captureSubjectEntries reads every tracked path once, in sorted order.
func captureSubjectEntries(root string) ([]subjectEntry, error) {
	paths, err := trackedSnapshotPaths(root)
	if err != nil {
		return nil, err
	}
	entries := make([]subjectEntry, 0, len(paths))
	for _, path := range paths {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, fmt.Errorf("U_GATE_EVIDENCE_UNAVAILABLE: tracked path %s is unavailable: %w", path, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(absolute)
			if readErr != nil {
				return nil, fmt.Errorf("U_GATE_EVIDENCE_UNAVAILABLE: tracked symlink %s is unreadable: %w", path, readErr)
			}
			entries = append(entries, subjectEntry{path: path, kind: subjectEntrySymlink, payload: []byte(target), perm: info.Mode().Perm()})
		case info.Mode().IsRegular():
			data, readErr := os.ReadFile(absolute)
			if readErr != nil {
				return nil, fmt.Errorf("U_GATE_EVIDENCE_UNAVAILABLE: tracked file %s is unreadable: %w", path, readErr)
			}
			kind := subjectEntryRegular
			if info.Mode().Perm()&0o111 != 0 {
				kind = subjectEntryExecutable
			}
			entries = append(entries, subjectEntry{path: path, kind: kind, payload: data, perm: info.Mode().Perm()})
		default:
			// A tracked gitlink (mode 160000) or any other non-regular entry
			// cannot be read faithfully from this root. Ignoring it would make
			// the digest claim to cover a subject it did not read, which is the
			// fabricated-evidence direction, so refuse instead.
			//
			// Known regression, accepted deliberately: a repository with a
			// tracked submodule evaluated before AIRA-72 (gitlinks were simply
			// not .go files, so the old digest skipped them) and now reports
			// U_GATE_EVIDENCE_UNAVAILABLE. That is a false fail, which is the
			// safe direction, and it is loud rather than silent. AIRA-79 tracks
			// digesting the pinned submodule commit instead. Pinned by
			// TestSubjectDigestGitlinkFailsClosed.
			return nil, fmt.Errorf("U_GATE_EVIDENCE_UNAVAILABLE: tracked path %s is neither a regular file nor a symlink", path)
		}
	}
	return entries, nil
}

// capturedSubject is one read of a tracked tree together with the digest of
// exactly those bytes.
//
// It exists so that "the bytes a verdict is bound to" and "the bytes a verdict
// was derived from" cannot be two different things. Before AIRA-80 every
// evaluator took a root path and re-read the tree for itself: the dimension
// lane digested one read (subjectTreeDigest) and evaluated another
// (captureTraceSnapshot), so a verdict could be bound to a digest of a state
// that was never evaluated. Passing the capture instead of the path makes that
// unrepresentable rather than merely avoided -- an evaluator has no root to
// re-read.
//
// root is retained only for the few things that legitimately need the location
// rather than the content: a command's cwd resolution and the traceability
// lane's requirements-directory probe.
type capturedSubject struct {
	root    string
	entries []subjectEntry
	digest  string
}

// captureSubject is the single constructor. It takes the stable (double-read)
// capture for every lane, not just the command lane: since AIRA-80 the
// dimension lane evaluates the captured bytes too, so a torn read there is no
// longer merely a digest that will fail to match -- refusing it is the
// fail-closed direction.
//
// GateCheck deliberately keeps the cheaper single-read subjectTreeDigest: it
// computes a lookup key, not evidence, and a torn read there can only fail to
// find a stored result, never fabricate one.
func captureSubject(root string) (capturedSubject, error) {
	entries, err := stableSubjectEntries(root)
	if err != nil {
		return capturedSubject{}, err
	}
	return capturedSubject{root: root, entries: entries, digest: digestSubjectEntries(entries)}, nil
}

// stableSubjectEntries is captureSubjectEntries with a double-read agreement
// check. The command-gate materialiser copies these bytes into a tree a command
// will execute and the dimension lane parses them, so a torn read would run or
// evaluate content that never existed as a coherent tree state.
func stableSubjectEntries(root string) ([]subjectEntry, error) {
	first, err := captureSubjectEntries(root)
	if err != nil {
		return nil, err
	}
	second, err := captureSubjectEntries(root)
	if err != nil {
		return nil, err
	}
	if !subjectEntriesAgree(first, second) {
		return nil, errors.New("tracked snapshot changed during materialisation")
	}
	return first, nil
}

// subjectEntriesAgree reports whether two reads of a tracked tree describe the
// same state. It is a separate function so the agreement rule the capture's
// fail-closed behaviour rests on can be tested directly: a real temporal tear
// between the two reads is not deterministically drivable from a test without a
// production hook, and the rule is what a tear would have to defeat.
//
// Every field the digest covers is compared, plus perm, which the digest folds
// into the kind byte only for the executable bit.
func subjectEntriesAgree(first, second []subjectEntry) bool {
	if len(first) != len(second) {
		return false
	}
	for i := range first {
		if first[i].path != second[i].path || first[i].kind != second[i].kind || first[i].perm.Perm() != second[i].perm.Perm() || !bytes.Equal(first[i].payload, second[i].payload) {
			return false
		}
	}
	return true
}
