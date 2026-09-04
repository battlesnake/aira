package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"aira/internal/domain"
)

// ReviewPathTier maps an area glob to a review tier. Rules are intentionally
// unordered: recommendation takes the maximum contribution from all matches.
type ReviewPathTier struct {
	Glob string `json:"glob"`
	Tier int    `json:"tier"`
}

// ReviewPolicy is the static project policy used by the review-depth engine.
// DefaultTier is a pointer because an omitted value inside a present review
// block is malformed, while the absent review block has a documented default.
type ReviewPolicy struct {
	DefaultTier   *int                    `json:"default_tier"`
	PathTiers     []ReviewPathTier        `json:"path_tiers,omitempty"`
	KindFloor     map[domain.Kind]int     `json:"kind_floor,omitempty"`
	SeverityFloor map[domain.Severity]int `json:"severity_floor,omitempty"`
	Configured    bool                    `json:"-"`
}

func defaultReviewPolicy() ReviewPolicy {
	tier := 3
	return ReviewPolicy{DefaultTier: &tier}
}

// LoadReviewPolicy decodes the optional project.review JSON block while
// retaining presence information for every field that has fail-closed rules.
// A nil or empty raw value means the whole review block was absent; JSON null
// means a malformed present block.
func LoadReviewPolicy(raw json.RawMessage) (ReviewPolicy, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return defaultReviewPolicy(), nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ReviewPolicy{}, errors.New("E_CONFIG_INVALID: review must be an object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return ReviewPolicy{}, errors.New("E_CONFIG_INVALID: review must be an object")
	}
	for key := range object {
		switch key {
		case "default_tier", "path_tiers", "kind_floor", "severity_floor":
		default:
			return ReviewPolicy{}, fmt.Errorf("E_CONFIG_INVALID: unknown review field %q", key)
		}
	}

	defaultRaw, ok := object["default_tier"]
	if !ok || bytes.Equal(bytes.TrimSpace(defaultRaw), []byte("null")) {
		return ReviewPolicy{}, errors.New("E_CONFIG_INVALID: review.default_tier must be an explicit tier")
	}
	defaultTier, err := decodeReviewInt(defaultRaw, "review.default_tier")
	if err != nil {
		return ReviewPolicy{}, err
	}
	policy := ReviewPolicy{DefaultTier: &defaultTier, Configured: true}

	if rawValue, present := object["path_tiers"]; present {
		if bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) {
			return ReviewPolicy{}, errors.New("E_CONFIG_INVALID: review.path_tiers must not be null")
		}
		rules, err := decodeReviewPathTiers(rawValue)
		if err != nil {
			return ReviewPolicy{}, fmt.Errorf("E_CONFIG_INVALID: invalid review.path_tiers: %w", err)
		}
		policy.PathTiers = rules
	}
	if rawValue, present := object["kind_floor"]; present {
		floors, err := decodeReviewFloors(rawValue, true)
		if err != nil {
			return ReviewPolicy{}, err
		}
		policy.KindFloor = floors.kind
	}
	if rawValue, present := object["severity_floor"]; present {
		floors, err := decodeReviewFloors(rawValue, false)
		if err != nil {
			return ReviewPolicy{}, err
		}
		policy.SeverityFloor = floors.severity
	}
	return ValidateReviewPolicy(policy)
}

func decodeReviewPathTiers(raw json.RawMessage) ([]ReviewPathTier, error) {
	var rawRules []json.RawMessage
	if err := json.Unmarshal(raw, &rawRules); err != nil || rawRules == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("[]")) {
		return nil, errors.New("path_tiers must be an array")
	}
	rules := make([]ReviewPathTier, 0, len(rawRules))
	for index, rawRule := range rawRules {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(rawRule, &object); err != nil || object == nil {
			return nil, fmt.Errorf("path_tiers[%d] must be an object", index)
		}
		for key := range object {
			if key != "glob" && key != "tier" {
				return nil, fmt.Errorf("path_tiers[%d] has unknown field %q", index, key)
			}
		}
		rawGlob, hasGlob := object["glob"]
		if !hasGlob || bytes.Equal(bytes.TrimSpace(rawGlob), []byte("null")) {
			return nil, fmt.Errorf("path_tiers[%d].glob must be present and non-null", index)
		}
		var glob string
		if err := json.Unmarshal(rawGlob, &glob); err != nil {
			return nil, fmt.Errorf("path_tiers[%d].glob must be a string", index)
		}
		rawTier, hasTier := object["tier"]
		if !hasTier || bytes.Equal(bytes.TrimSpace(rawTier), []byte("null")) {
			return nil, fmt.Errorf("path_tiers[%d].tier must be present and non-null", index)
		}
		tier, err := decodeReviewInt(rawTier, fmt.Sprintf("review.path_tiers[%d].tier", index))
		if err != nil {
			return nil, err
		}
		rules = append(rules, ReviewPathTier{Glob: glob, Tier: tier})
	}
	return rules, nil
}

func decodeReviewInt(raw json.RawMessage, field string) (int, error) {
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("E_CONFIG_INVALID: %s must be an integer", field)
	}
	return value, nil
}

type reviewFloors struct {
	kind     map[domain.Kind]int
	severity map[domain.Severity]int
}

func decodeReviewFloors(raw json.RawMessage, kind bool) (reviewFloors, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return reviewFloors{}, errors.New("E_CONFIG_INVALID: review floor map must not be null")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return reviewFloors{}, errors.New("E_CONFIG_INVALID: review floor must be an object")
	}
	result := reviewFloors{}
	if kind {
		result.kind = make(map[domain.Kind]int, len(values))
	} else {
		result.severity = make(map[domain.Severity]int, len(values))
	}
	for key, value := range values {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return reviewFloors{}, fmt.Errorf("E_CONFIG_INVALID: review floor %q must not be null", key)
		}
		parsed, err := decodeReviewInt(value, "review floor "+key)
		if err != nil {
			return reviewFloors{}, err
		}
		if parsed < 0 || parsed > 3 {
			return reviewFloors{}, fmt.Errorf("E_CONFIG_INVALID: review floor %q is outside 0..3", key)
		}
		if kind {
			value := domain.Kind(key)
			if !domain.ValidKind(value) {
				return reviewFloors{}, fmt.Errorf("E_CONFIG_INVALID: unknown review kind %q", key)
			}
			result.kind[value] = parsed
		} else {
			value := domain.Severity(key)
			if !domain.ValidSeverity(value) {
				return reviewFloors{}, fmt.Errorf("E_CONFIG_INVALID: unknown review severity %q", key)
			}
			result.severity[value] = parsed
		}
	}
	return result, nil
}

// ValidateReviewPolicy validates and canonicalises a policy before it enters
// a Store. The all-zero value is the absent-review-block default.
func ValidateReviewPolicy(policy ReviewPolicy) (ReviewPolicy, error) {
	if policy.DefaultTier == nil && !policy.Configured && len(policy.PathTiers) == 0 && len(policy.KindFloor) == 0 && len(policy.SeverityFloor) == 0 {
		return defaultReviewPolicy(), nil
	}
	if policy.DefaultTier == nil {
		return ReviewPolicy{}, errors.New("E_CONFIG_INVALID: review.default_tier must be an explicit tier")
	}
	if *policy.DefaultTier < 1 || *policy.DefaultTier > 3 {
		return ReviewPolicy{}, errors.New("E_CONFIG_INVALID: review.default_tier is outside 1..3")
	}
	result := policy
	result.PathTiers = append([]ReviewPathTier(nil), policy.PathTiers...)
	for i := range result.PathTiers {
		if result.PathTiers[i].Tier < 0 || result.PathTiers[i].Tier > 3 {
			return ReviewPolicy{}, fmt.Errorf("E_CONFIG_INVALID: review.path_tiers[%d].tier is outside 0..3", i)
		}
		glob, err := NormalizeAreaGlob(result.PathTiers[i].Glob)
		if err != nil {
			return ReviewPolicy{}, fmt.Errorf("E_CONFIG_INVALID: review.path_tiers[%d].glob: %s", i, err)
		}
		result.PathTiers[i].Glob = glob
	}
	result.KindFloor = copyKindFloors(policy.KindFloor)
	for key, value := range result.KindFloor {
		if !domain.ValidKind(key) {
			return ReviewPolicy{}, fmt.Errorf("E_CONFIG_INVALID: unknown review kind %q", key)
		}
		if value < 0 || value > 3 {
			return ReviewPolicy{}, fmt.Errorf("E_CONFIG_INVALID: review floor %q is outside 0..3", key)
		}
	}
	result.SeverityFloor = copySeverityFloors(policy.SeverityFloor)
	for key, value := range result.SeverityFloor {
		if !domain.ValidSeverity(key) {
			return ReviewPolicy{}, fmt.Errorf("E_CONFIG_INVALID: unknown review severity %q", key)
		}
		if value < 0 || value > 3 {
			return ReviewPolicy{}, fmt.Errorf("E_CONFIG_INVALID: review floor %q is outside 0..3", key)
		}
	}
	return result, nil
}

func copyKindFloors(values map[domain.Kind]int) map[domain.Kind]int {
	if values == nil {
		return nil
	}
	result := make(map[domain.Kind]int, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func copySeverityFloors(values map[domain.Severity]int) map[domain.Severity]int {
	if values == nil {
		return nil
	}
	result := make(map[domain.Severity]int, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func reviewPolicyHasConfiguration(policy ReviewPolicy) bool {
	return policy.Configured || policy.DefaultTier == nil || len(policy.PathTiers) > 0 || len(policy.KindFloor) > 0 || len(policy.SeverityFloor) > 0
}

func sortedReviewStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

// Recommendation is the explained output of RecommendReviewTier.
type Recommendation struct {
	Tier        int      `json:"tier"`
	Basis       []string `json:"basis"`
	PathsSource string   `json:"paths_source,omitempty"`
}

// RecommendReviewTier computes the fail-closed review depth. Every known
// contributor is accumulated and the recommendation is their maximum; an
// unmatched path or unrecognised ticket enum contributes the configured
// default instead of silently contributing zero.
func RecommendReviewTier(paths []string, kind, severity string, policy ReviewPolicy) (Recommendation, error) {
	validated, err := ValidateReviewPolicy(policy)
	if err != nil {
		return Recommendation{}, err
	}
	defaultTier := *validated.DefaultTier
	result := Recommendation{PathsSource: "none"}
	contributions := []int{}
	basis := []string{}

	normalizedPaths := make([]string, 0, len(paths))
	for _, rawPath := range paths {
		path, err := NormalizeAreaGlob(rawPath)
		if err != nil {
			return Recommendation{}, err
		}
		normalizedPaths = append(normalizedPaths, path)
	}
	if len(normalizedPaths) > 0 {
		result.PathsSource = "arg"
	}

	if !reviewPolicyHasConfiguration(validated) {
		// The absent block is deliberately visible in the explanation. It is
		// the one default case where the project supplied no review policy at all.
		result.Tier = defaultTier
		result.Basis = []string{"no-policy-configured"}
		return result, nil
	}

	if len(normalizedPaths) == 0 {
		contributions = append(contributions, defaultTier)
		basis = append(basis, "no-paths ⇒ default_tier")
	} else {
		for _, path := range normalizedPaths {
			matched := false
			for _, rule := range validated.PathTiers {
				overlaps, err := AreaGlobsOverlap(path, rule.Glob)
				if err != nil {
					return Recommendation{}, err
				}
				if !overlaps {
					continue
				}
				matched = true
				contributions = append(contributions, rule.Tier)
				basis = append(basis, fmt.Sprintf("path %s ↦ rule %s ⇒ tier %d", path, rule.Glob, rule.Tier))
			}
			if !matched {
				contributions = append(contributions, defaultTier)
				basis = append(basis, fmt.Sprintf("path %s unmatched ⇒ default_tier", path))
			}
		}
	}

	kindValue := domain.Kind(kind)
	if !domain.ValidKind(kindValue) {
		contributions = append(contributions, defaultTier)
		basis = append(basis, "unrecognised-kind ⇒ default_tier")
	} else if floor, ok := validated.KindFloor[kindValue]; ok {
		contributions = append(contributions, floor)
		basis = append(basis, fmt.Sprintf("kind ⇒ floor %d", floor))
	}

	severityValue := domain.Severity(severity)
	if !domain.ValidSeverity(severityValue) {
		contributions = append(contributions, defaultTier)
		basis = append(basis, "unrecognised-severity ⇒ default_tier")
	} else if floor, ok := validated.SeverityFloor[severityValue]; ok {
		contributions = append(contributions, floor)
		basis = append(basis, fmt.Sprintf("severity ⇒ floor %d", floor))
	}

	for _, contribution := range contributions {
		if contribution > result.Tier {
			result.Tier = contribution
		}
	}
	result.Basis = sortedReviewStrings(basis)
	if len(result.Basis) == 0 {
		// This is only reachable for a malformed caller policy/input, but keep
		// the explained-output invariant explicit at the API boundary.
		result.Basis = []string{strings.TrimSpace("no-contributors")}
	}
	return result, nil
}

// ReviewPolicy returns the validated, immutable policy loaded when the store
// opened. The maps and slices are detached so callers cannot mutate store
// state between recommendations.
func (s *Store) ReviewPolicy() ReviewPolicy {
	policy := s.reviewPolicy
	policy.DefaultTier = reviewTierPointer(*s.reviewPolicy.DefaultTier)
	policy.PathTiers = append([]ReviewPathTier(nil), s.reviewPolicy.PathTiers...)
	policy.KindFloor = copyKindFloors(s.reviewPolicy.KindFloor)
	policy.SeverityFloor = copySeverityFloors(s.reviewPolicy.SeverityFloor)
	return policy
}

func reviewTierPointer(value int) *int { return &value }

// TicketAreaGlobs returns the distinct advisory area hints ever recorded for
// ticketID in this project. It intentionally does not filter by worktree or
// lease generation: review depth must be conservative when only hints are
// available.
func (s *Store) TicketAreaGlobs(ticketID string) ([]string, error) {
	if err := domain.ValidateID(ticketID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT DISTINCT glob FROM area_hints WHERE project_id=? AND ticket_id=? ORDER BY glob`, s.projectID, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var glob string
		if err := rows.Scan(&glob); err != nil {
			return nil, err
		}
		result = append(result, glob)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return []string{}, nil
	}
	return result, nil
}
