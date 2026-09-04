package store

import (
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aira/internal/domain"
)

const (
	traceCovers   = "covers"
	traceVerifies = "verifies"
)

type traceEdge struct {
	Kind string
	ID   string
	Path string
	Line int
}

type traceScanResult struct {
	Edges []traceEdge
}

type traceSnapshotFile struct {
	Path        string
	Data        []byte
	Requirement bool
}

type traceSnapshot struct {
	Files []traceSnapshotFile
}

// scanTraceability discovers annotation edges from the tracked-file snapshot.
// Parsing the Go syntax first is intentional: a string containing
// "covers: AR-1" is data, not an annotation.
func scanTraceability(root string) (traceScanResult, error) {
	snapshot, err := captureTraceSnapshot(root, nil)
	if err != nil {
		return traceScanResult{}, err
	}
	return parseTraceabilitySnapshot(root, snapshot)
}

func captureTraceSnapshot(root string, hook func()) (traceSnapshot, error) {
	paths, err := trackedTracePaths(root)
	if err != nil {
		return traceSnapshot{}, err
	}
	first, err := readTraceSnapshotFiles(root, paths)
	if err != nil {
		return traceSnapshot{}, err
	}
	if hook != nil {
		hook()
	}
	secondPaths, err := trackedTracePaths(root)
	if err != nil {
		return traceSnapshot{}, err
	}
	if !sameStrings(paths, secondPaths) {
		return traceSnapshot{}, errors.New("U_TRACE_UNSCANNED: tracked-file set changed during snapshot")
	}
	second, err := readTraceSnapshotFiles(root, secondPaths)
	if err != nil {
		return traceSnapshot{}, err
	}
	if len(first) != len(second) {
		return traceSnapshot{}, errors.New("U_TRACE_UNSCANNED: tracked-file snapshot changed during read")
	}
	for i := range first {
		if first[i].Path != second[i].Path || first[i].Requirement != second[i].Requirement || !bytes.Equal(first[i].Data, second[i].Data) {
			return traceSnapshot{}, fmt.Errorf("U_TRACE_UNSCANNED: tracked file %s changed during snapshot", first[i].Path)
		}
	}
	return traceSnapshot{Files: first}, nil
}

// isTracePath is the traceability parser's input selector: go/parser runs over
// every non-requirement file in the set, so it must stay Go plus the requirement
// registry.
//
// It is one function rather than two because both the check lane
// (trackedTracePaths, which lists the tree itself) and the gate lane
// (traceSnapshotFromSubject, which filters an already-captured subject) select
// with it. A second copy is exactly how a selector drifts, and a drifted
// selector on the gate side is a subject that claims coverage it does not have.
func isTracePath(path string) bool {
	if strings.HasSuffix(path, ".go") && !strings.HasPrefix(path, "vendor/") {
		return true
	}
	return strings.HasPrefix(path, ".aira/requirements/") && strings.HasSuffix(path, ".md")
}

// traceSnapshotFromSubject derives the traceability parser's input from an
// already-captured subject instead of re-reading the tree (AIRA-80). The
// subject's digest is then a digest of the very bytes parsed here.
//
// The non-regular refusal mirrors readTraceSnapshotFiles: a tracked symlink
// named *.go is not parseable source, and parsing its target's path bytes as Go
// would be evidence about a file that was never read.
func traceSnapshotFromSubject(subject capturedSubject) (traceSnapshot, error) {
	// A directory readability probe, not a content read: it cannot tear, and
	// dropping it would silently widen the lane in the one case it catches.
	if err := validateTraceRequirementsDirectory(subject.root); err != nil {
		return traceSnapshot{}, err
	}
	files := make([]traceSnapshotFile, 0, len(subject.entries))
	for _, entry := range subject.entries {
		if !isTracePath(entry.path) {
			continue
		}
		if entry.kind == subjectEntrySymlink {
			return traceSnapshot{}, fmt.Errorf("U_TRACE_UNSCANNED: tracked file %s is not regular", entry.path)
		}
		files = append(files, traceSnapshotFile{Path: entry.path, Data: entry.payload, Requirement: strings.HasPrefix(entry.path, ".aira/requirements/")})
	}
	return traceSnapshot{Files: files}, nil
}

// trackedTracePaths serves the check lane, which walks every registered
// worktree. It is deliberately NOT rebased onto the whole-tree capture: it has
// no persisted digest binding, so a torn read there cannot mint or re-serve a
// stale pass, while widening it would read every byte of every worktree on
// every `aira check` and would import captureSubjectEntries' gitlink refusal
// (AIRA-79) into a lane that does not need it, making check fail closed on any
// repository with a submodule.
func trackedTracePaths(root string) ([]string, error) {
	if err := validateTraceRequirementsDirectory(root); err != nil {
		return nil, err
	}
	out, stderr, err := runGit(root, "ls-files", "-z", "--cached", "--")
	if err != nil {
		return nil, fmt.Errorf("U_TRACE_UNSCANNED: tracked-file snapshot: %w: %s", err, strings.TrimSpace(stderr))
	}
	var paths []string
	for _, raw := range bytes.Split([]byte(out), []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "" {
			continue
		}
		if isTracePath(path) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func validateTraceRequirementsDirectory(root string) error {
	_, err := os.ReadDir(filepath.Join(root, ".aira", "requirements"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("U_TRACE_UNSCANNED: read requirements directory: %w", err)
	}
	return nil
}

func readTraceSnapshotFiles(root string, paths []string) ([]traceSnapshotFile, error) {
	files := make([]traceSnapshotFile, 0, len(paths))
	for _, path := range paths {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, fmt.Errorf("U_TRACE_UNSCANNED: read tracked file %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("U_TRACE_UNSCANNED: tracked file %s is not regular", path)
		}
		data, err := os.ReadFile(absolute)
		if err != nil {
			return nil, fmt.Errorf("U_TRACE_UNSCANNED: read tracked file %s: %w", path, err)
		}
		files = append(files, traceSnapshotFile{Path: path, Data: data, Requirement: strings.HasPrefix(path, ".aira/requirements/")})
	}
	return files, nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func parseTraceabilitySnapshot(root string, snapshot traceSnapshot) (traceScanResult, error) {
	result := traceScanResult{}
	for _, tracked := range snapshot.Files {
		if tracked.Requirement {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(tracked.Path))
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, tracked.Data, parser.ParseComments)
		if err != nil {
			return traceScanResult{}, fmt.Errorf("U_TRACE_UNSCANNED: parse %s: %w", repoPath(root, path), err)
		}
		isTest := strings.HasSuffix(path, "_test.go")
		for _, group := range file.Comments {
			for _, comment := range group.List {
				line := fset.Position(comment.Pos()).Line
				kind, ids, ok, parseErr := parseTraceComment(comment.Text)
				if parseErr != nil {
					return traceScanResult{}, fmt.Errorf("U_TRACE_UNSCANNED: %s:%d: %w", repoPath(root, path), line, parseErr)
				}
				if !ok || (kind == traceCovers && isTest) || (kind == traceVerifies && !isTest) {
					continue
				}
				for _, id := range ids {
					result.Edges = append(result.Edges, traceEdge{Kind: kind, ID: id, Path: repoPath(root, path), Line: line})
				}
			}
		}
	}
	sort.Slice(result.Edges, func(i, j int) bool {
		left, right := result.Edges[i], result.Edges[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.ID < right.ID
	})
	return result, nil
}

func parseTraceComment(raw string) (string, []string, bool, error) {
	text := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(text, "//"):
		text = strings.TrimSpace(strings.TrimPrefix(text, "//"))
	case strings.HasPrefix(text, "/*"):
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "/*"), "*/"))
	}
	for strings.HasPrefix(text, "*") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "*"))
	}
	colon := strings.IndexByte(text, ':')
	if colon < 0 {
		return "", nil, false, nil
	}
	kind := strings.TrimSpace(text[:colon])
	if kind != traceCovers && kind != traceVerifies {
		return "", nil, false, nil
	}
	parts := strings.Split(text[colon+1:], ",")
	if len(parts) == 0 {
		return "", nil, false, fmt.Errorf("%s annotation has no requirement IDs", kind)
	}
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" {
			return "", nil, false, fmt.Errorf("%s annotation has an empty requirement ID", kind)
		}
		if err := domain.ValidateID(id); err != nil {
			return "", nil, false, fmt.Errorf("%s annotation has invalid requirement ID %q", kind, id)
		}
		ids = append(ids, id)
	}
	return kind, ids, true, nil
}

type traceRequirement struct {
	status domain.RequirementStatus
	path   string
}

// malformedNode is deliberately file-shaped rather than ID-shaped. A malformed
// requirement can have no trustworthy ID, but it is still an enumerated node
// for the traceability gauge. IDs preserves the aliases used by check's
// resolver, in discovery order, without collapsing distinct files.
type malformedNode struct {
	Subject string
	IDs     []string
	Message string
}

type traceUnevaluated struct {
	Code    string
	Subject string
	Message string
}

type traceScan struct {
	edges        []traceEdge
	requirements map[string]traceRequirement
	malformed    []malformedNode
	unevaluated  *traceUnevaluated
}

func traceScanFailure(code, subject, message string) traceScan {
	return traceScan{requirements: map[string]traceRequirement{}, unevaluated: &traceUnevaluated{Code: code, Subject: subject, Message: message}}
}

func appendTraceAlias(ids []string, id string) []string {
	if id == "" {
		return ids
	}
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

// scanTraceabilityGraph is the lossless, pure status seam shared by check and
// the traceability gauge. It intentionally retains ordered raw edges and
// malformed files instead of deriving a result from findings.
func (s *Store) scanTraceabilityGraph() (traceScan, error) {
	registry, err := readRegistry(s.registryPath)
	if err != nil {
		return traceScanFailure("U_TRACE_UNSCANNED", "traceability", err.Error()), nil
	}

	var entries []registryEntry
	if len(s.prefixesByKind(kindRequirement)) == 0 {
		_, stderr, gitErr := runGit(s.root, "rev-parse", "--show-toplevel")
		if gitErr != nil && isNotGitRepository(stderr) {
			return traceScanFailure("U_TRACE_EMPTY", "traceability", "requirement registry is empty"), nil
		}
		entries = []registryEntry{{ProjectID: s.projectID, WorktreeID: s.worktreeID, Root: s.root}}
	} else {
		entries, err = discoverWorktrees(s.root, s.projectID, registry)
		if err != nil {
			return traceScanFailure("U_TRACE_UNSCANNED", "traceability", err.Error()), nil
		}
	}

	type capturedWorktree struct {
		entry    registryEntry
		snapshot traceSnapshot
	}
	captured := make([]capturedWorktree, 0, len(entries))
	hook := s.traceabilitySnapshotHook
	s.traceabilitySnapshotHook = nil
	for _, entry := range entries {
		snapshot, snapshotErr := captureTraceSnapshot(entry.Root, hook)
		hook = nil
		if snapshotErr != nil {
			return traceScanFailure("U_TRACE_UNSCANNED", repoPath(s.root, entry.Root), snapshotErr.Error()), nil
		}
		captured = append(captured, capturedWorktree{entry: entry, snapshot: snapshot})
	}

	scan := traceScan{requirements: make(map[string]traceRequirement), malformed: []malformedNode{}}
	for _, worktree := range captured {
		result, parseErr := parseTraceabilitySnapshot(worktree.entry.Root, worktree.snapshot)
		if parseErr != nil {
			scan.unevaluated = &traceUnevaluated{Code: "U_TRACE_UNSCANNED", Subject: repoPath(s.root, worktree.entry.Root), Message: parseErr.Error()}
			return scan, nil
		}
		scan.edges = append(scan.edges, result.Edges...)
		for _, tracked := range worktree.snapshot.Files {
			if !tracked.Requirement {
				continue
			}
			subject := filepath.ToSlash(filepath.Join(repoPath(s.root, worktree.entry.Root), tracked.Path))
			requirement, parseErr := domain.ParseRequirement(tracked.Data)
			filenameID := strings.TrimSuffix(filepath.Base(filepath.FromSlash(tracked.Path)), ".md")
			if parseErr != nil || requirement.ID != filenameID {
				message := "E_REQUIREMENT_INVALID: filename/frontmatter mismatch"
				if parseErr != nil {
					message = parseErr.Error()
				}
				ids := []string{}
				if id, ok := ticketIDFromFilename(tracked.Path); ok {
					ids = appendTraceAlias(ids, id)
				}
				if parseErr == nil {
					ids = appendTraceAlias(ids, requirement.ID)
				}
				scan.malformed = append(scan.malformed, malformedNode{Subject: subject, IDs: ids, Message: message})
				continue
			}
			scan.requirements[requirement.ID] = traceRequirement{status: requirement.Status, path: subject}
		}
	}

	malformedIDMap := make(map[string]string)
	for _, node := range scan.malformed {
		for _, id := range node.IDs {
			malformedIDMap[id] = node.Subject
		}
	}
	if len(s.prefixesByKind(kindRequirement)) == 0 || (len(scan.requirements) == 0 && len(scan.malformed) == 0) {
		scan.unevaluated = &traceUnevaluated{Code: "U_TRACE_EMPTY", Subject: "traceability", Message: "requirement registry is empty"}
	}
	return scan, nil
}

func (s *Store) checkTraceability(report *CheckReport) error {
	_, err := readRegistry(s.registryPath)
	if err != nil {
		addTraceUnevaluated(report, CheckFinding{Code: "U_TRACE_UNSCANNED", Subject: "traceability", Message: err.Error(), Kind: "unevaluated"})
		return nil
	}
	if len(s.prefixesByKind(kindRequirement)) == 0 {
		_, stderr, gitErr := runGit(s.root, "rev-parse", "--show-toplevel")
		if gitErr != nil && isNotGitRepository(stderr) {
			// A non-git ticket-only fixture has no tracked-file graph to
			// evaluate. Real projects are git worktrees; an explicit annotation
			// in one will reach U_TRACE_EMPTY below. Nothing was scanned here,
			// so the dimension is unevaluated with its reason, never a silent
			// pass left behind by the seed (AIRA-86).
			addTraceUnevaluated(report, CheckFinding{Code: "U_TRACE_UNSCANNED", Subject: "traceability", Message: "root is not a git repository, so there is no tracked-file graph to scan", Kind: "unevaluated"})
			return nil
		}
	}
	scan, scanErr := s.scanTraceabilityGraph()
	if scanErr != nil {
		return scanErr
	}
	malformed := make(map[string]string)
	for _, node := range scan.malformed {
		addFinding(report, CheckFinding{Code: "E_REQUIREMENT_INVALID", Subject: node.Subject, Message: node.Message, Kind: "fail"}, "")
		for _, id := range node.IDs {
			malformed[id] = node.Subject
		}
	}
	if scan.unevaluated != nil {
		addTraceUnevaluated(report, CheckFinding{Code: scan.unevaluated.Code, Subject: scan.unevaluated.Subject, Message: scan.unevaluated.Message, Kind: "unevaluated"})
		return nil
	}
	if len(scan.requirements) == 0 && len(malformed) == 0 {
		addTraceUnevaluated(report, CheckFinding{Code: "U_TRACE_EMPTY", Subject: "traceability", Message: "requirement registry is empty", Kind: "unevaluated"})
		return nil
	}
	if len(scan.requirements) == 0 && len(malformed) > 0 {
		addTraceUnevaluated(report, CheckFinding{Code: "U_TRACE_UNSCANNED", Subject: "traceability", Message: "requirement registry contains no readable nodes", Kind: "unevaluated"})
	}
	if err := resolveTraceabilityEdges(report, scan.edges, scan.requirements, malformed); err != nil {
		return err
	}
	// Only this arm scanned the graph and resolved every edge, so only this arm
	// may claim the dimension. Each earlier return recorded its own unevaluated
	// reason instead (AIRA-86).
	establishDimension(report, "traceability")
	return nil
}

func resolveTraceabilityEdges(report *CheckReport, edges []traceEdge, requirements map[string]traceRequirement, malformed map[string]string) error {
	covers := make(map[string]bool)
	verifies := make(map[string]bool)
	for _, edge := range edges {
		subject := fmt.Sprintf("%s:%d", edge.Path, edge.Line)
		switch {
		case malformed[edge.ID] != "":
			addTraceUnevaluated(report, CheckFinding{Code: "U_TRACE_UNSCANNED", Subject: subject, Message: fmt.Sprintf("requirement %s is unreadable at %s", edge.ID, malformed[edge.ID]), Kind: "unevaluated"})
		case requirements[edge.ID].path == "":
			addFinding(report, CheckFinding{Code: "E_TRACE_DANGLING", Subject: subject, Message: fmt.Sprintf("%s annotation references absent requirement %s", edge.Kind, edge.ID), Kind: "fail"}, "traceability")
		case edge.Kind == traceCovers:
			covers[edge.ID] = true
		case edge.Kind == traceVerifies:
			verifies[edge.ID] = true
		}
	}

	ids := make([]string, 0, len(requirements))
	for id := range requirements {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		requirement := requirements[id]
		switch requirement.status {
		case domain.RequirementBuilt:
			if !covers[id] {
				addTraceWarning(report, CheckFinding{Code: "W_TRACE_UNCOVERED", Subject: id, Message: "built requirement has no covers annotation", Kind: "warning"})
			}
			if !verifies[id] {
				addTraceWarning(report, CheckFinding{Code: "W_TRACE_UNVERIFIED", Subject: id, Message: "built requirement has no verifies annotation", Kind: "warning"})
			}
		case domain.RequirementPartial:
			if !covers[id] {
				addTraceWarning(report, CheckFinding{Code: "W_TRACE_UNCOVERED", Subject: id, Message: "partial requirement has no covers annotation", Kind: "warning"})
			}
		}
	}
	return nil
}

func traceDimensionRank(value string) int {
	switch value {
	case "fail":
		return 3
	case "unevaluated":
		return 2
	case "warning":
		return 1
	default:
		return 0
	}
}

func addTraceUnevaluated(report *CheckReport, finding CheckFinding) {
	for _, existing := range report.UnevaluatedFindings {
		if existing.Code == finding.Code && existing.Subject == finding.Subject {
			return
		}
	}
	report.UnevaluatedFindings = append(report.UnevaluatedFindings, finding)
	report.Unevaluated = true
	if traceDimensionRank(report.Dimensions["traceability"]) < traceDimensionRank("unevaluated") {
		report.Dimensions["traceability"] = "unevaluated"
	}
}

func addTraceWarning(report *CheckReport, warning CheckFinding) {
	for _, existing := range report.Warnings {
		if existing.Code == warning.Code && existing.Subject == warning.Subject {
			return
		}
	}
	report.Warnings = append(report.Warnings, warning)
	if traceDimensionRank(report.Dimensions["traceability"]) < traceDimensionRank("warning") {
		report.Dimensions["traceability"] = "warning"
	}
}
