package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"aira/internal/domain"
)

// AreaTouchResult is the durable result of replacing one holder's advisory
// area hints. Hints are repo-relative, slash-separated, sorted, and unique.
// Warnings describe live cross-worktree collisions; they never make Touch
// fail.
type AreaTouchResult struct {
	ID       string         `json:"id"`
	Worktree string         `json:"worktree_id"`
	Hints    []string       `json:"hints"`
	Warnings []CheckFinding `json:"warnings,omitempty"`
}

type liveAreaClaim struct {
	ticketID string
	worktree string
	globs    []string
}

type liveAreaLease struct {
	worktree   string
	generation uint64
}

// NormalizeAreaGlob converts a hint to a repo-relative slash form. Leading
// "./" and repeated separators are cleaned, while Windows-qualified and
// backslash-containing patterns, traversal/absolute patterns, malformed glob
// classes, empty patterns, NULs, and invalid UTF-8 are rejected. A wildcard
// never crosses a slash unless it is the complete ** segment.
func NormalizeAreaGlob(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.IndexByte(raw, 0) >= 0 || !utf8.ValidString(raw) {
		return "", fmt.Errorf("E_GLOB_INVALID: empty, NUL, or invalid UTF-8 glob")
	}
	if strings.Contains(raw, `\`) || isWindowsDriveQualified(raw) {
		return "", fmt.Errorf("E_GLOB_INVALID: glob must be repo-relative: %q", raw)
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("E_GLOB_INVALID: glob must be repo-relative: %q", raw)
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", fmt.Errorf("E_GLOB_INVALID: glob escapes repository: %q", raw)
		}
	}
	cleaned := path.Clean(raw)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("E_GLOB_INVALID: glob escapes repository: %q", raw)
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == "" {
			continue
		}
		if _, err := path.Match(segment, ""); err != nil {
			return "", fmt.Errorf("E_GLOB_INVALID: malformed glob %q: %v", raw, err)
		}
	}
	return cleaned, nil
}

func isWindowsDriveQualified(raw string) bool {
	return len(raw) >= 2 && ((raw[0] >= 'A' && raw[0] <= 'Z') || (raw[0] >= 'a' && raw[0] <= 'z')) && raw[1] == ':'
}

// NormalizeAreaGlobs normalises, sorts, and deduplicates a replacement set.
func NormalizeAreaGlobs(raw []string) ([]string, error) {
	result := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, value := range raw {
		glob, err := NormalizeAreaGlob(value)
		if err != nil {
			return nil, err
		}
		if !seen[glob] {
			seen[glob] = true
			result = append(result, glob)
		}
	}
	sort.Strings(result)
	return result, nil
}

// AreaGlobsOverlap reports whether the two normalised glob languages have a
// common repo-relative path. Literal segments, *, ?, and bracket classes are
// intersected exactly at the segment level; a complete ** segment matches
// zero or more non-empty path segments. The product-state search is finite and
// deterministic. For the supported syntax it has no intentional false
// negatives; its advisory boundary may still yield a false positive for an
// abstract path that a particular filesystem cannot materialise (for example,
// a directory-only path). It therefore errs toward warning, without treating
// unrelated directory prefixes as overlapping.
//
// Pinned examples (the tests in area_test.go are the executable table):
//
// | left | right | result |
// |---|---|---|
// | src/file.go | src/file.go | overlap |
// | src/** | src/foo/bar.go | overlap |
// | src/*.go | src/foo.go | overlap |
// | src/** | test/** | no overlap |
// | a/b/* | a/*/c | overlap (a/b/c) |
// | src/*.go | src/foo/bar.go | no overlap |
func AreaGlobsOverlap(left, right string) (bool, error) {
	left, err := NormalizeAreaGlob(left)
	if err != nil {
		return false, err
	}
	right, err = NormalizeAreaGlob(right)
	if err != nil {
		return false, err
	}
	leftSegments, rightSegments := strings.Split(left, "/"), strings.Split(right, "/")
	leftPatterns := make([][]areaToken, len(leftSegments))
	rightPatterns := make([][]areaToken, len(rightSegments))
	for i, segment := range leftSegments {
		leftPatterns[i], err = parseAreaSegment(segment)
		if err != nil {
			return false, err
		}
	}
	for i, segment := range rightSegments {
		rightPatterns[i], err = parseAreaSegment(segment)
		if err != nil {
			return false, err
		}
	}

	type state struct{ left, right int }
	queue := []state{{}}
	seen := map[state]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		if current.left == len(leftPatterns) && current.right == len(rightPatterns) {
			return true, nil
		}
		if current.left < len(leftPatterns) && isAreaDoubleStar(leftSegments[current.left]) {
			queue = append(queue, state{current.left + 1, current.right})
		}
		if current.right < len(rightPatterns) && isAreaDoubleStar(rightSegments[current.right]) {
			queue = append(queue, state{current.left, current.right + 1})
		}
		if current.left >= len(leftPatterns) || current.right >= len(rightPatterns) {
			continue
		}
		leftStar := isAreaDoubleStar(leftSegments[current.left])
		rightStar := isAreaDoubleStar(rightSegments[current.right])
		switch {
		case leftStar && rightStar:
			// Both stars can consume the same arbitrary segment, but their
			// epsilon edges above make a consuming edge unnecessary for
			// intersection reachability.
		case leftStar:
			queue = append(queue, state{current.left, current.right + 1})
		case rightStar:
			queue = append(queue, state{current.left + 1, current.right})
		default:
			if areaSegmentsOverlap(leftPatterns[current.left], rightPatterns[current.right]) {
				queue = append(queue, state{current.left + 1, current.right + 1})
			}
		}
	}
	return false, nil
}

type areaTokenKind uint8

const (
	areaLiteral areaTokenKind = iota
	areaAny
	areaStar
	areaClass
)

type areaRange struct{ lo, hi rune }
type areaClassSet struct {
	negated bool
	ranges  []areaRange
}
type areaToken struct {
	kind  areaTokenKind
	value rune
	class areaClassSet
}

func isAreaDoubleStar(segment string) bool { return segment == "**" }

func parseAreaSegment(segment string) ([]areaToken, error) {
	if segment == "" {
		return nil, fmt.Errorf("E_GLOB_INVALID: empty path segment")
	}
	var result []areaToken
	runes := []rune(segment)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '*':
			result = append(result, areaToken{kind: areaStar})
		case '?':
			result = append(result, areaToken{kind: areaAny})
		case '[':
			end := i + 1
			for end < len(runes) && runes[end] != ']' {
				end++
			}
			if end >= len(runes) {
				return nil, fmt.Errorf("E_GLOB_INVALID: malformed glob segment %q", segment)
			}
			class, err := parseAreaClass(runes[i+1 : end])
			if err != nil {
				return nil, fmt.Errorf("E_GLOB_INVALID: malformed glob segment %q: %w", segment, err)
			}
			result = append(result, areaToken{kind: areaClass, class: class})
			i = end
		default:
			result = append(result, areaToken{kind: areaLiteral, value: runes[i]})
		}
	}
	return result, nil
}

func parseAreaClass(content []rune) (areaClassSet, error) {
	if len(content) == 0 {
		return areaClassSet{}, errors.New("empty character class")
	}
	result := areaClassSet{}
	if content[0] == '^' || content[0] == '!' {
		result.negated = true
		content = content[1:]
	}
	if len(content) == 0 {
		return areaClassSet{}, errors.New("empty character class")
	}
	for i := 0; i < len(content); i++ {
		lo, hi := content[i], content[i]
		if i+2 < len(content) && content[i+1] == '-' {
			hi = content[i+2]
			if lo > hi {
				return areaClassSet{}, errors.New("descending character range")
			}
			i += 2
		}
		result.ranges = append(result.ranges, areaRange{lo: lo, hi: hi})
	}
	return result, nil
}

func areaSegmentsOverlap(left, right []areaToken) bool {
	type state struct{ left, right int }
	queue := []state{{}}
	seen := map[state]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		if current.left == len(left) && current.right == len(right) {
			return true
		}
		if current.left < len(left) && left[current.left].kind == areaStar {
			queue = append(queue, state{current.left + 1, current.right})
		}
		if current.right < len(right) && right[current.right].kind == areaStar {
			queue = append(queue, state{current.left, current.right + 1})
		}
		if current.left >= len(left) || current.right >= len(right) {
			continue
		}
		l, r := left[current.left], right[current.right]
		if l.kind == areaStar && r.kind == areaStar {
			continue
		}
		if l.kind == areaStar {
			queue = append(queue, state{current.left, current.right + 1})
			continue
		}
		if r.kind == areaStar {
			queue = append(queue, state{current.left + 1, current.right})
			continue
		}
		if areaTokensCompatible(l, r) {
			queue = append(queue, state{current.left + 1, current.right + 1})
		}
	}
	return false
}

func areaTokensCompatible(left, right areaToken) bool {
	if left.kind == areaAny || right.kind == areaAny {
		return true
	}
	if left.kind == areaLiteral && right.kind == areaLiteral {
		return left.value == right.value
	}
	if left.kind == areaLiteral && right.kind == areaClass {
		return areaClassContains(right.class, left.value)
	}
	if right.kind == areaLiteral && left.kind == areaClass {
		return areaClassContains(left.class, right.value)
	}
	if left.kind == areaClass && right.kind == areaClass {
		return areaClassesIntersect(left.class, right.class)
	}
	return false
}

func areaClassContains(class areaClassSet, value rune) bool {
	contained := false
	for _, r := range class.ranges {
		if value >= r.lo && value <= r.hi {
			contained = true
			break
		}
	}
	if class.negated {
		return !contained
	}
	return contained
}

func areaClassesIntersect(left, right areaClassSet) bool {
	if left.negated && right.negated {
		return true
	}
	if left.negated {
		left, right = right, left
	}
	if right.negated {
		for _, r := range left.ranges {
			for candidate := r.lo; candidate <= r.hi; candidate++ {
				if areaClassContains(right, candidate) {
					return true
				}
				if candidate == utf8.MaxRune {
					break
				}
			}
		}
		return false
	}
	for _, leftRange := range left.ranges {
		for _, rightRange := range right.ranges {
			if leftRange.lo <= rightRange.hi && rightRange.lo <= leftRange.hi {
				return true
			}
		}
	}
	return false
}

// Touch replaces hints only after proving that token owns a live lease. The
// liveness sample is taken inside the writer transaction, matching heartbeat;
// no lease column, event counter, event row, outbox row, or journal is touched.
func (s *Store) Touch(ctx context.Context, ticketID, token string, rawGlobs []string) (AreaTouchResult, error) {
	if err := domain.ValidateID(ticketID); err != nil {
		return AreaTouchResult{}, err
	}
	if err := s.ticketExists(ticketID); err != nil {
		return AreaTouchResult{}, err
	}
	globs, err := NormalizeAreaGlobs(rawGlobs)
	if err != nil {
		return AreaTouchResult{}, err
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(tokenBytes) != 32 {
		return AreaTouchResult{}, ErrLeaseToken
	}
	tokenHash := sha256.Sum256(tokenBytes)
	result := AreaTouchResult{ID: ticketID, Worktree: s.worktreeID, Hints: append([]string(nil), globs...)}
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		bootID, monoNS, err := s.sampleClock()
		if err != nil {
			return err
		}
		row, err := readLeaseRow(ctx, conn, s.projectID, ticketID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseExpired
		}
		if err != nil {
			return err
		}
		lease, err := leaseFromRow(ticketID, row)
		if err != nil {
			return err
		}
		held, ok := lease.Held()
		if !ok || !held.IsLive(bootID, monoNS) {
			return ErrLeaseExpired
		}
		if held.HolderTokenHash() != tokenHash {
			return ErrLeaseToken
		}
		if s.worktreeID != held.Worktree() {
			return ErrLeaseToken
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM area_hints WHERE project_id=? AND ticket_id=? AND worktree_id=?`, s.projectID, ticketID, s.worktreeID); err != nil {
			return err
		}
		for _, glob := range globs {
			if _, err := conn.ExecContext(ctx, `INSERT INTO area_hints(project_id, ticket_id, worktree_id, generation, glob) VALUES(?, ?, ?, ?, ?)`, s.projectID, ticketID, s.worktreeID, int64(held.Generation()), glob); err != nil {
				return err
			}
		}
		claims, err := liveAreaClaims(ctx, conn, s.projectID, bootID, monoNS)
		if err != nil {
			return err
		}
		result.Warnings = areaWarningsForClaim(claims, ticketID, s.worktreeID)
		return nil
	})
	if err != nil {
		return AreaTouchResult{}, err
	}
	return result, nil
}

func liveAreaClaims(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectID, bootID string, monoNS uint64) ([]liveAreaClaim, error) {
	leases := map[string]liveAreaLease{}
	rows, err := queryer.QueryContext(ctx, `SELECT ticket_id, state, generation, holder_token_hash, boot_id,
        last_heartbeat_mono_ns, ttl_ns, actor, worktree_id FROM leases WHERE project_id=? AND state='held'`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ticketID string
		var row leaseRow
		if err := rows.Scan(&ticketID, &row.state, &row.generation, &row.holderTokenHash, &row.bootID, &row.lastHeartbeatMonoNS, &row.ttlNS, &row.actor, &row.worktree); err != nil {
			_ = rows.Close()
			return nil, err
		}
		lease, err := leaseFromRow(ticketID, row)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		held, ok := lease.Held()
		if ok && held.IsLive(bootID, monoNS) {
			leases[ticketID] = liveAreaLease{worktree: held.Worktree(), generation: held.Generation()}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	hintRows, err := queryer.QueryContext(ctx, `SELECT ticket_id, worktree_id, generation, glob FROM area_hints WHERE project_id=? ORDER BY ticket_id, worktree_id, generation, glob`, projectID)
	if err != nil {
		return nil, err
	}
	claims := map[string]*liveAreaClaim{}
	for hintRows.Next() {
		var ticketID, worktree, glob string
		var generation int64
		if err := hintRows.Scan(&ticketID, &worktree, &generation, &glob); err != nil {
			_ = hintRows.Close()
			return nil, err
		}
		if generation < 0 {
			_ = hintRows.Close()
			return nil, errors.New("E_CONFIG_INVALID: negative area hint generation")
		}
		lease, ok := leases[ticketID]
		if !ok || lease.worktree != worktree || lease.generation != uint64(generation) {
			continue // stale after release or an expired/stealing lease.
		}
		key := ticketID + "\x00" + worktree
		claim := claims[key]
		if claim == nil {
			claim = &liveAreaClaim{ticketID: ticketID, worktree: worktree}
			claims[key] = claim
		}
		claim.globs = append(claim.globs, glob)
	}
	if err := hintRows.Err(); err != nil {
		_ = hintRows.Close()
		return nil, err
	}
	_ = hintRows.Close()
	result := make([]liveAreaClaim, 0, len(claims))
	for _, claim := range claims {
		sort.Strings(claim.globs)
		result = append(result, *claim)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ticketID != result[j].ticketID {
			return result[i].ticketID < result[j].ticketID
		}
		return result[i].worktree < result[j].worktree
	})
	return result, nil
}

func areaWarningsForClaim(claims []liveAreaClaim, ticketID, worktree string) []CheckFinding {
	all := areaOverlapWarnings(claims)
	var result []CheckFinding
	for _, warning := range all {
		if strings.Contains(warning.Subject, ticketID+"@"+worktree) {
			result = append(result, warning)
		}
	}
	return result
}

func areaOverlapWarnings(claims []liveAreaClaim) []CheckFinding {
	var result []CheckFinding
	for i := 0; i < len(claims); i++ {
		for j := i + 1; j < len(claims); j++ {
			left, right := claims[i], claims[j]
			if left.worktree == right.worktree {
				continue
			}
			var overlaps []string
			for _, leftGlob := range left.globs {
				for _, rightGlob := range right.globs {
					overlap, err := AreaGlobsOverlap(leftGlob, rightGlob)
					if err == nil && overlap {
						overlaps = append(overlaps, leftGlob+" <-> "+rightGlob)
					}
				}
			}
			if len(overlaps) == 0 {
				continue
			}
			sort.Strings(overlaps)
			subject := left.ticketID + "@" + left.worktree + " <-> " + right.ticketID + "@" + right.worktree
			result = append(result, CheckFinding{
				Code: "W_AREA_OVERLAP", Subject: subject,
				Message: fmt.Sprintf("%s overlaps on glob(s): %s", subject, strings.Join(overlaps, ", ")),
				Kind:    "warning",
			})
		}
	}
	return result
}

func (s *Store) areaOverlapWarnings(ctx context.Context) ([]CheckFinding, error) {
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return nil, err
	}
	claims, err := liveAreaClaims(ctx, s.db, s.projectID, bootID, monoNS)
	if err != nil {
		return nil, err
	}
	return areaOverlapWarnings(claims), nil
}
