package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"aira/internal/gitcontext"
)

// Compute telemetry is operational data.  A nil bucket is deliberately
// different from a pointer to zero: the former means that the provider did
// not establish that bucket, while the latter is an observed zero.
type ComputeBuckets struct {
	FreshInput *int64 `json:"fresh_input,omitempty"`
	CacheRead  *int64 `json:"cache_read,omitempty"`
	CacheWrite *int64 `json:"cache_write,omitempty"`
	Output     *int64 `json:"output,omitempty"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
}

type Conservation string

const (
	ConservationChecked     Conservation = "checked"
	ConservationMismatch    Conservation = "mismatch"
	ConservationUnevaluated Conservation = "unevaluated"
)

const (
	ComputeCodeInvalid         = "E_COMPUTE_INVALID"
	ComputeCodeProviderUnknown = "E_COMPUTE_PROVIDER_UNKNOWN"
	ComputeCodeConservation    = "E_COMPUTE_CONSERVATION"
	ComputeCodeUnevaluated     = "U_COMPUTE_UNEVALUATED"
)

var computePhases = map[string]struct{}{
	"plan": {}, "plan-review": {}, "plan-fix": {}, "implement": {},
	"work-review": {}, "work-fix": {},
}

func ValidatePhase(phase string) error {
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return nil
	}
	if _, ok := computePhases[phase]; !ok {
		return fmt.Errorf("%s: invalid phase %q", ComputeCodeInvalid, phase)
	}
	return nil
}

func ValidPhase(phase string) bool { return ValidatePhase(phase) == nil }

type ComputeEvent struct {
	ID              string            `json:"id"`
	TicketID        string            `json:"ticket_id,omitempty"`
	Phase           string            `json:"phase,omitempty"`
	Model           string            `json:"model"`
	Provider        string            `json:"provider"`
	At              string            `json:"at"`
	Session         string            `json:"session,omitempty"`
	Agent           string            `json:"agent,omitempty"`
	Source          string            `json:"source"`
	Resources       ResourceUsage     `json:"resources"`
	Buckets         ComputeBuckets    `json:"buckets"`
	ReportedTotal   *int64            `json:"reported_total,omitempty"`
	CostUSD         *float64          `json:"cost_usd,omitempty"`
	ReasoningSubset bool              `json:"reasoning_subset,omitempty"`
	Conservation    Conservation      `json:"conservation"`
	AtSeq           int64             `json:"at_seq"`
	GitContext      ComputeGitContext `json:"git_context"`
}

// ComputeGitContext is the lean, status-preserving compute provenance view.
// Stable repository scope is implied by project_id and Field.Reason is
// deliberately absent from high-volume compute rows.
type ComputeGitContext struct {
	HeadHash   gitcontext.Field `json:"head_hash"`
	HeadRef    gitcontext.Field `json:"head_ref"`
	WorktreeID gitcontext.Field `json:"worktree_id"`
}

func ComputeGitContextFrom(value gitcontext.GitContext) ComputeGitContext {
	field := func(input gitcontext.Field) gitcontext.Field {
		return gitcontext.Field{Value: input.Value, Status: input.Status}
	}
	return ComputeGitContext{HeadHash: field(value.HeadHash), HeadRef: field(value.HeadRef), WorktreeID: field(value.WorktreeID)}
}

type ComputeEventInput struct {
	TicketID   string
	Phase      string
	Model      string
	Provider   string
	At         string
	Session    string
	Agent      string
	Source     string
	Raw        RawUsage
	CostUSD    *float64
	GitContext gitcontext.GitContext
}

// ResourceUsage contains process/cgroup observations. Nil means the runner
// could not establish that metric; it never means an observed zero.
type ResourceUsage struct {
	WallMS  *int64 `json:"wall_ms,omitempty"`
	CPUUser *int64 `json:"cpu_user,omitempty"`
	CPUSys  *int64 `json:"cpu_sys,omitempty"`
	PeakRSS *int64 `json:"peak_rss,omitempty"`
}

type QuotaSnapshot struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	At        string `json:"at"`
	Window    string `json:"window,omitempty"`
	Used      *int64 `json:"used,omitempty"`
	Limit     *int64 `json:"limit,omitempty"`
	Remaining *int64 `json:"remaining,omitempty"`
	ResetAt   string `json:"reset_at,omitempty"`
	Source    string `json:"source"`
	AtSeq     int64  `json:"at_seq"`
}

type QuotaSnapshotInput struct {
	Provider  string
	At        string
	Window    string
	Used      *int64
	Limit     *int64
	Remaining *int64
	ResetAt   string
	Source    string
}

// RawUsage is a provider-neutral envelope.  The provider-specific fields are
// intentionally separate so NormalizeUsage cannot silently reinterpret a
// field from another provider.  Buckets is used by the explicit disjoint
// bucket escape hatch for unknown providers.
type RawUsage struct {
	// Runner/cgroup observations are orthogonal to token authority.
	Resources ResourceUsage

	// Anthropic.
	InputTokens              *int64
	CacheReadInputTokens     *int64
	CacheCreationInputTokens *int64
	OutputTokens             *int64

	// OpenAI.
	PromptTokens     *int64
	CachedTokens     *int64
	CompletionTokens *int64
	ReasoningTokens  *int64
	TotalTokens      *int64

	// Gemini.
	PromptTokenCount        *int64
	CachedContentTokenCount *int64
	CandidatesTokenCount    *int64
	ThoughtsTokenCount      *int64
	TotalTokenCount         *int64

	// Current Codex exec --json usage shape, verified against
	// ~/repos/codex/codex-rs/exec/src/exec_events.rs on 2026-08-13. Codex has
	// dedicated names and does not emit the OpenAI prompt_tokens_details
	// structure, so it intentionally has a dedicated parser below.
	CodexInputTokens           *int64
	CodexCachedInputTokens     *int64
	CodexCacheWriteInputTokens *int64
	CodexOutputTokens          *int64
	CodexReasoningOutputTokens *int64
	CodexTotalTokens           *int64

	// Explicit caller-supplied, already-disjoint buckets.
	Buckets         *ComputeBuckets
	ReasoningSubset bool
	ReportedTotal   *int64
}

func (b ComputeBuckets) clone() ComputeBuckets {
	return ComputeBuckets{FreshInput: cloneInt64(b.FreshInput), CacheRead: cloneInt64(b.CacheRead), CacheWrite: cloneInt64(b.CacheWrite), Output: cloneInt64(b.Output), Reasoning: cloneInt64(b.Reasoning)}
}

func (r ResourceUsage) clone() ResourceUsage {
	return ResourceUsage{WallMS: cloneInt64(r.WallMS), CPUUser: cloneInt64(r.CPUUser), CPUSys: cloneInt64(r.CPUSys), PeakRSS: cloneInt64(r.PeakRSS)}
}

// HasUsage reports whether the raw value contains any token authority.
func (r RawUsage) HasUsage() bool {
	return r.InputTokens != nil || r.CacheReadInputTokens != nil || r.CacheCreationInputTokens != nil || r.OutputTokens != nil ||
		r.PromptTokens != nil || r.CachedTokens != nil || r.CompletionTokens != nil || r.ReasoningTokens != nil || r.TotalTokens != nil ||
		r.PromptTokenCount != nil || r.CachedContentTokenCount != nil || r.CandidatesTokenCount != nil || r.ThoughtsTokenCount != nil || r.TotalTokenCount != nil ||
		r.CodexInputTokens != nil || r.CodexCachedInputTokens != nil || r.CodexCacheWriteInputTokens != nil || r.CodexOutputTokens != nil || r.CodexReasoningOutputTokens != nil || r.CodexTotalTokens != nil ||
		r.Buckets != nil || r.ReportedTotal != nil
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

func (e ComputeEventInput) Validate() error {
	if strings.TrimSpace(e.Model) == "" || strings.TrimSpace(e.Source) == "" || (strings.TrimSpace(e.Provider) == "" && strings.TrimSpace(e.Source) != "run") {
		return errors.New(ComputeCodeInvalid + ": provider, model, and source are required")
	}
	if strings.TrimSpace(e.Provider) == "" && e.Raw.HasUsage() {
		return errors.New(ComputeCodeInvalid + ": provider is required for token usage")
	}
	if err := ValidatePhase(e.Phase); err != nil {
		return err
	}
	if e.CostUSD != nil && (*e.CostUSD < 0 || math.IsNaN(*e.CostUSD) || math.IsInf(*e.CostUSD, 0)) {
		return errors.New(ComputeCodeInvalid + ": cost_usd must be finite and non-negative")
	}
	for name, value := range map[string]*int64{"wall_ms": e.Raw.Resources.WallMS, "cpu_user": e.Raw.Resources.CPUUser, "cpu_sys": e.Raw.Resources.CPUSys, "peak_rss": e.Raw.Resources.PeakRSS} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s: %s must be non-negative", ComputeCodeInvalid, name)
		}
	}
	return nil
}

func (e ComputeEvent) Validate() error {
	if e.ID == "" || e.At == "" || e.AtSeq < 1 {
		return errors.New(ComputeCodeInvalid + ": compute event identity is incomplete")
	}
	if err := (ComputeEventInput{TicketID: e.TicketID, Phase: e.Phase, Model: e.Model, Provider: e.Provider, At: e.At, Session: e.Session, Agent: e.Agent, Source: e.Source, Raw: RawUsage{Resources: e.Resources}, CostUSD: e.CostUSD}).Validate(); err != nil {
		return err
	}
	if err := validateBuckets(e.Buckets); err != nil {
		return err
	}
	switch e.Conservation {
	case ConservationChecked, ConservationMismatch, ConservationUnevaluated:
	default:
		return fmt.Errorf("%s: invalid conservation state %q", ComputeCodeInvalid, e.Conservation)
	}
	if e.ReportedTotal != nil && *e.ReportedTotal < 0 {
		return errors.New(ComputeCodeInvalid + ": reported total must be non-negative")
	}
	return nil
}

func (q QuotaSnapshot) Validate() error {
	if q.ID == "" || q.At == "" || q.AtSeq < 1 {
		return errors.New(ComputeCodeInvalid + ": quota snapshot identity is incomplete")
	}
	if err := (QuotaSnapshotInput{Provider: q.Provider, Source: q.Source, Used: q.Used, Limit: q.Limit, Remaining: q.Remaining}).Validate(); err != nil {
		return err
	}
	return nil
}

func (q QuotaSnapshotInput) Validate() error {
	if strings.TrimSpace(q.Provider) == "" || strings.TrimSpace(q.Source) == "" {
		return errors.New(ComputeCodeInvalid + ": provider and source are required")
	}
	for name, value := range map[string]*int64{"used": q.Used, "limit": q.Limit, "remaining": q.Remaining} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s: %s must be non-negative", ComputeCodeInvalid, name)
		}
	}
	return nil
}

// NormalizeUsage maps a provider payload into disjoint buckets.  It is pure:
// it reads no clock, database, or process state.
func NormalizeUsage(provider string, raw RawUsage) (ComputeBuckets, *int64, Conservation, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if raw.Buckets != nil {
		buckets := raw.Buckets.clone()
		if err := validateBuckets(buckets); err != nil {
			return ComputeBuckets{}, nil, ConservationUnevaluated, err
		}
		return conserve(buckets, raw.ReportedTotal, EffectiveReasoningSubset(provider, raw), true)
	}
	var buckets ComputeBuckets
	var total *int64
	complete := false
	subset := false
	switch provider {
	case "anthropic":
		buckets.FreshInput = cloneInt64(raw.InputTokens)
		buckets.CacheRead = cloneInt64(raw.CacheReadInputTokens)
		buckets.CacheWrite = cloneInt64(raw.CacheCreationInputTokens)
		buckets.Output = cloneInt64(raw.OutputTokens)
		total = cloneInt64(raw.ReportedTotal)
		complete = raw.InputTokens != nil && raw.CacheReadInputTokens != nil && raw.CacheCreationInputTokens != nil && raw.OutputTokens != nil
	case "openai":
		buckets.Output = cloneInt64(raw.CompletionTokens)
		buckets.Reasoning = cloneInt64(raw.ReasoningTokens)
		total = firstInt64(raw.ReportedTotal, raw.TotalTokens)
		if raw.PromptTokens != nil && raw.CachedTokens != nil {
			if *raw.CachedTokens <= *raw.PromptTokens {
				fresh := *raw.PromptTokens - *raw.CachedTokens
				buckets.FreshInput, buckets.CacheRead = &fresh, cloneInt64(raw.CachedTokens)
			} else if total != nil {
				return buckets, total, ConservationMismatch, nil
			}
		}
		complete = raw.PromptTokens != nil && raw.CachedTokens != nil && raw.CompletionTokens != nil
		subset = true
	case "codex":
		buckets.Output = cloneInt64(raw.CodexOutputTokens)
		buckets.Reasoning = cloneInt64(raw.CodexReasoningOutputTokens)
		// Codex's cache-write count is already disjoint from input.
		buckets.CacheWrite = cloneInt64(raw.CodexCacheWriteInputTokens)
		total = firstInt64(raw.ReportedTotal, raw.CodexTotalTokens)
		if raw.CodexInputTokens != nil && raw.CodexCachedInputTokens != nil {
			if *raw.CodexCachedInputTokens <= *raw.CodexInputTokens {
				fresh := *raw.CodexInputTokens - *raw.CodexCachedInputTokens
				buckets.FreshInput, buckets.CacheRead = &fresh, cloneInt64(raw.CodexCachedInputTokens)
			} else if total != nil {
				return buckets, total, ConservationMismatch, nil
			}
		}
		complete = raw.CodexInputTokens != nil && raw.CodexCachedInputTokens != nil && raw.CodexCacheWriteInputTokens != nil && raw.CodexOutputTokens != nil
		subset = true
	case "gemini":
		buckets.Output = cloneInt64(raw.CandidatesTokenCount)
		buckets.Reasoning = cloneInt64(raw.ThoughtsTokenCount)
		total = firstInt64(raw.ReportedTotal, raw.TotalTokenCount)
		if raw.PromptTokenCount != nil && raw.CachedContentTokenCount != nil {
			if *raw.CachedContentTokenCount <= *raw.PromptTokenCount {
				fresh := *raw.PromptTokenCount - *raw.CachedContentTokenCount
				buckets.FreshInput, buckets.CacheRead = &fresh, cloneInt64(raw.CachedContentTokenCount)
			} else if total != nil {
				return buckets, total, ConservationMismatch, nil
			}
		}
		// Thoughts are additive to candidates, so absence is insufficient to
		// claim conservation against a total.
		complete = raw.PromptTokenCount != nil && raw.CachedContentTokenCount != nil && raw.CandidatesTokenCount != nil && raw.ThoughtsTokenCount != nil
		subset = false
	default:
		return ComputeBuckets{}, nil, ConservationUnevaluated, fmt.Errorf("%s: provider %q requires explicit disjoint buckets", ComputeCodeProviderUnknown, provider)
	}
	return conserve(buckets, total, subset, complete)
}

// EffectiveReasoningSubset is the contract used by NormalizeUsage and must
// travel with a persisted event. Explicit buckets carry their own contract;
// provider-native OpenAI and Codex reasoning counts are subsets of output.
func EffectiveReasoningSubset(provider string, raw RawUsage) bool {
	if raw.Buckets != nil {
		return raw.ReasoningSubset
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "codex":
		return true
	default:
		return false
	}
}

func firstInt64(values ...*int64) *int64 {
	for _, value := range values {
		if value != nil {
			return cloneInt64(value)
		}
	}
	return nil
}

func validateBuckets(b ComputeBuckets) error {
	for name, value := range map[string]*int64{"fresh_input": b.FreshInput, "cache_read": b.CacheRead, "cache_write": b.CacheWrite, "output": b.Output, "reasoning": b.Reasoning} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s: %s must be non-negative", ComputeCodeInvalid, name)
		}
	}
	return nil
}

func conserve(b ComputeBuckets, total *int64, reasoningSubset, complete bool) (ComputeBuckets, *int64, Conservation, error) {
	if total == nil {
		return b, nil, ConservationUnevaluated, nil
	}
	if *total < 0 {
		return ComputeBuckets{}, nil, ConservationUnevaluated, errors.New(ComputeCodeInvalid + ": reported total must be non-negative")
	}
	sum, count, overflow := int64(0), 0, false
	add := func(value *int64) {
		if value == nil {
			return
		}
		count++
		if sum > math.MaxInt64-*value {
			overflow = true
			return
		}
		sum += *value
	}
	add(b.FreshInput)
	add(b.CacheRead)
	add(b.CacheWrite)
	add(b.Output)
	if !reasoningSubset {
		add(b.Reasoning)
	}
	if overflow || sum > *total {
		return b, cloneInt64(total), ConservationMismatch, nil
	}
	if !complete || count == 0 {
		return b, cloneInt64(total), ConservationUnevaluated, nil
	}
	if sum != *total {
		return b, cloneInt64(total), ConservationMismatch, nil
	}
	return b, cloneInt64(total), ConservationChecked, nil
}

// PresentBucketSum returns the sum of the buckets actually established by the
// normaliser.  The bool is false only when the integer sum would overflow.
func PresentBucketSum(b ComputeBuckets, reasoningSubset bool) (int64, bool) {
	sum := int64(0)
	add := func(value *int64) bool {
		if value == nil {
			return true
		}
		if sum > math.MaxInt64-*value {
			return false
		}
		sum += *value
		return true
	}
	if !add(b.FreshInput) || !add(b.CacheRead) || !add(b.CacheWrite) || !add(b.Output) {
		return 0, false
	}
	if !reasoningSubset && !add(b.Reasoning) {
		return 0, false
	}
	return sum, true
}

// ParseUsagePayload parses only the exact provider token fields. Unknown
// vendor fields are tolerated. A usage object wrapper is accepted because
// direct API and Codex event payloads commonly wrap the usage record.
func ParseUsagePayload(provider string, data []byte) (RawUsage, error) {
	var root map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return RawUsage{}, fmt.Errorf("%s: invalid usage JSON: %w", ComputeCodeInvalid, err)
	}
	if usage, ok := root["usage"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(usage, &nested) == nil {
			root = nested
		}
	} else if strings.EqualFold(strings.TrimSpace(provider), "gemini") {
		if usage, ok := root["usageMetadata"]; ok {
			var nested map[string]json.RawMessage
			if json.Unmarshal(usage, &nested) == nil {
				root = nested
			}
		}
	}
	get := func(name string) (*int64, error) { return jsonInt64(root[name], name) }
	raw := RawUsage{}
	provider = strings.ToLower(strings.TrimSpace(provider))
	var err error
	set := func(dst **int64, name string) {
		if err != nil {
			return
		}
		*dst, err = get(name)
	}
	switch provider {
	case "anthropic":
		set(&raw.InputTokens, "input_tokens")
		set(&raw.CacheReadInputTokens, "cache_read_input_tokens")
		set(&raw.CacheCreationInputTokens, "cache_creation_input_tokens")
		set(&raw.OutputTokens, "output_tokens")
	case "openai":
		set(&raw.PromptTokens, "prompt_tokens")
		set(&raw.CompletionTokens, "completion_tokens")
		set(&raw.TotalTokens, "total_tokens")
		if details, ok := root["prompt_tokens_details"]; ok {
			err = parseNestedInt(details, "cached_tokens", &raw.CachedTokens)
		}
		if details, ok := root["completion_tokens_details"]; ok && err == nil {
			err = parseNestedInt(details, "reasoning_tokens", &raw.ReasoningTokens)
		}
	case "gemini":
		set(&raw.PromptTokenCount, "promptTokenCount")
		set(&raw.CachedContentTokenCount, "cachedContentTokenCount")
		set(&raw.CandidatesTokenCount, "candidatesTokenCount")
		set(&raw.ThoughtsTokenCount, "thoughtsTokenCount")
		set(&raw.TotalTokenCount, "totalTokenCount")
	case "codex":
		set(&raw.CodexInputTokens, "input_tokens")
		set(&raw.CodexCachedInputTokens, "cached_input_tokens")
		set(&raw.CodexCacheWriteInputTokens, "cache_write_input_tokens")
		set(&raw.CodexOutputTokens, "output_tokens")
		set(&raw.CodexReasoningOutputTokens, "reasoning_output_tokens")
		set(&raw.CodexTotalTokens, "total_tokens")
	default:
		return RawUsage{}, fmt.Errorf("%s: provider %q requires explicit buckets", ComputeCodeProviderUnknown, provider)
	}
	if err != nil {
		return RawUsage{}, err
	}
	return raw, nil
}

func jsonInt64(data json.RawMessage, name string) (*int64, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return nil, fmt.Errorf("%s: %s must be an integer", ComputeCodeInvalid, name)
	}
	v, err := number.Int64()
	if err != nil || v < 0 {
		return nil, fmt.Errorf("%s: %s must be a non-negative integer", ComputeCodeInvalid, name)
	}
	return &v, nil
}

func parseNestedInt(data json.RawMessage, name string, dst **int64) error {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("%s: usage detail %s must be an object", ComputeCodeInvalid, name)
	}
	value, err := jsonInt64(values[name], name)
	*dst = value
	return err
}
