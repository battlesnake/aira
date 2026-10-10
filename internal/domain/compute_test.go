package domain

import (
	"strings"
	"testing"

	"aira/internal/gitcontext"
)

func i64(value int64) *int64 { return &value }

func TestNormalizeAnthropicDirect(t *testing.T) {
	buckets, total, state, err := NormalizeUsage("anthropic", RawUsage{
		InputTokens: i64(100), CacheReadInputTokens: i64(20), CacheCreationInputTokens: i64(5), OutputTokens: i64(30), ReportedTotal: i64(155),
	})
	if err != nil {
		t.Fatal(err)
	}
	if buckets.FreshInput == nil || *buckets.FreshInput != 100 || buckets.CacheRead == nil || *buckets.CacheRead != 20 || buckets.CacheWrite == nil || *buckets.CacheWrite != 5 || buckets.Output == nil || *buckets.Output != 30 || total == nil || *total != 155 || state != ConservationChecked {
		t.Fatalf("anthropic normalisation = %#v total=%v state=%s", buckets, total, state)
	}
}

func TestNormalizeOpenAISubset(t *testing.T) {
	buckets, _, state, err := NormalizeUsage("openai", RawUsage{PromptTokens: i64(1000), CachedTokens: i64(300), CompletionTokens: i64(200), TotalTokens: i64(1200)})
	if err != nil {
		t.Fatal(err)
	}
	if *buckets.FreshInput != 700 || *buckets.CacheRead != 300 || *buckets.Output != 200 || state != ConservationChecked {
		t.Fatalf("openai normalisation = %#v state=%s", buckets, state)
	}
}

func TestNormalizeMismatchIsAWarningState(t *testing.T) {
	buckets, total, state, err := NormalizeUsage("openai", RawUsage{PromptTokens: i64(100), CachedTokens: i64(20), CompletionTokens: i64(50), TotalTokens: i64(10)})
	if err != nil {
		t.Fatal(err)
	}
	if buckets.FreshInput == nil || total == nil || state != ConservationMismatch {
		t.Fatalf("mismatch result = %#v total=%v state=%s", buckets, total, state)
	}
}

func TestAbsentBucketIsNotZero(t *testing.T) {
	buckets, _, state, err := NormalizeUsage("anthropic", RawUsage{InputTokens: i64(10), CacheReadInputTokens: i64(0), CacheCreationInputTokens: i64(0)})
	if err != nil {
		t.Fatal(err)
	}
	if buckets.Output != nil || state != ConservationUnevaluated {
		t.Fatalf("absent output = %#v state=%s", buckets, state)
	}
}

func TestExplicitZeroBucketIsPresent(t *testing.T) {
	buckets, _, _, err := NormalizeUsage("anthropic", RawUsage{OutputTokens: i64(0)})
	if err != nil {
		t.Fatal(err)
	}
	if buckets.Output == nil || *buckets.Output != 0 {
		t.Fatalf("explicit zero output = %#v", buckets)
	}
}

func TestCachedGreaterThanPromptNeverFabricatesFreshInput(t *testing.T) {
	for _, test := range []struct {
		name  string
		total *int64
		want  Conservation
	}{
		{"with total", i64(1200), ConservationMismatch}, {"without total", nil, ConservationUnevaluated},
	} {
		t.Run(test.name, func(t *testing.T) {
			buckets, _, state, err := NormalizeUsage("openai", RawUsage{PromptTokens: i64(1000), CachedTokens: i64(1200), CompletionTokens: i64(10), ReportedTotal: test.total})
			if err != nil {
				t.Fatal(err)
			}
			if buckets.FreshInput != nil || buckets.CacheRead != nil || state != test.want {
				t.Fatalf("drift result = %#v state=%s", buckets, state)
			}
		})
	}
}

func TestMissingCacheDetailDoesNotFabricateBuckets(t *testing.T) {
	buckets, _, state, err := NormalizeUsage("openai", RawUsage{PromptTokens: i64(100), CompletionTokens: i64(20), TotalTokens: i64(120)})
	if err != nil {
		t.Fatal(err)
	}
	if buckets.FreshInput != nil || buckets.CacheRead != nil || state != ConservationUnevaluated {
		t.Fatalf("partial result = %#v state=%s", buckets, state)
	}
}

func TestReasoningAdditivityPerProvider(t *testing.T) {
	openAI, _, state, err := NormalizeUsage("openai", RawUsage{PromptTokens: i64(100), CachedTokens: i64(0), CompletionTokens: i64(20), ReasoningTokens: i64(15), TotalTokens: i64(120)})
	if err != nil || state != ConservationChecked {
		t.Fatalf("openai: buckets=%#v state=%s err=%v", openAI, state, err)
	}
	gemini, _, state, err := NormalizeUsage("gemini", RawUsage{PromptTokenCount: i64(100), CachedContentTokenCount: i64(0), CandidatesTokenCount: i64(20), ThoughtsTokenCount: i64(15), TotalTokenCount: i64(135)})
	if err != nil || state != ConservationChecked {
		t.Fatalf("gemini: buckets=%#v state=%s err=%v", gemini, state, err)
	}
}

func TestUnknownProviderRequiresExplicitBuckets(t *testing.T) {
	_, _, _, err := NormalizeUsage("mystery", RawUsage{OutputTokens: i64(1)})
	if err == nil || !strings.HasPrefix(err.Error(), ComputeCodeProviderUnknown) {
		t.Fatalf("unknown provider error = %v", err)
	}
	buckets, total, state, err := NormalizeUsage("mystery", RawUsage{Buckets: &ComputeBuckets{Output: i64(4), Reasoning: i64(2)}, ReportedTotal: i64(4), ReasoningSubset: true})
	if err != nil || total == nil || buckets.Output == nil || state != ConservationChecked {
		t.Fatalf("explicit unknown = %#v total=%v state=%s err=%v", buckets, total, state, err)
	}
}

func TestCodexUsageShapeIsDedicated(t *testing.T) {
	raw, err := ParseUsagePayload("codex", []byte(`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":25,"cache_write_input_tokens":4,"output_tokens":20,"reasoning_output_tokens":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	buckets, _, state, err := NormalizeUsage("codex", raw)
	if err != nil || state != ConservationUnevaluated || *buckets.FreshInput != 75 || *buckets.CacheRead != 25 || *buckets.CacheWrite != 4 || *buckets.Output != 20 || *buckets.Reasoning != 7 {
		t.Fatalf("codex result = %#v state=%s err=%v", buckets, state, err)
	}
}

func TestCodexWithoutCacheWriteIsUnevaluated(t *testing.T) {
	_, _, state, err := NormalizeUsage("codex", RawUsage{
		CodexInputTokens: i64(100), CodexCachedInputTokens: i64(20), CodexOutputTokens: i64(30), CodexTotalTokens: i64(130),
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != ConservationUnevaluated {
		t.Fatalf("codex without cache_write state=%s, want %s", state, ConservationUnevaluated)
	}
}

func TestComputePhaseValidation(t *testing.T) {
	if err := (ComputeEventInput{Provider: "openai", Model: "gpt", Source: "manual", Phase: "unknown"}).Validate(); err == nil || !strings.HasPrefix(err.Error(), ComputeCodeInvalid) {
		t.Fatalf("phase validation = %v", err)
	}
	if err := (ComputeEventInput{Provider: "openai", Model: "gpt", Source: "manual", Phase: "work-review"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestComputeValidationToleratesUnstampedGitContext(t *testing.T) {
	valid := ComputeEventInput{Provider: "openai", Model: "gpt", Source: "manual"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("zero-valued git context: %v", err)
	}
	valid.GitContext = gitcontext.GitContext{
		HeadHash:   gitcontext.Field{Status: gitcontext.StatusUnevaluated},
		HeadRef:    gitcontext.Field{Status: gitcontext.StatusUnevaluated},
		WorktreeID: gitcontext.Field{Status: gitcontext.StatusUnevaluated},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("explicitly unevaluated git context: %v", err)
	}
}

func TestComputeEventGitContextViewSurvivesValidationReconstruction(t *testing.T) {
	observed := gitcontext.GitContext{
		HeadHash:   gitcontext.Field{Value: "abc123", Status: gitcontext.StatusValue, Reason: "not-retained"},
		HeadRef:    gitcontext.Field{Value: "refs/heads/main", Status: gitcontext.StatusMismatch, Reason: "not-retained"},
		WorktreeID: gitcontext.Field{Status: gitcontext.StatusNone, Reason: "not-retained"},
	}
	event := ComputeEvent{
		ID: "CE-1", Model: "gpt", Provider: "openai", At: "2026-08-19T00:00:00Z", Source: "manual",
		Conservation: ConservationUnevaluated, AtSeq: 1, GitContext: ComputeGitContextFrom(observed),
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("read event validation rejected git context: %v", err)
	}
	if event.GitContext.HeadHash.Value != "abc123" || event.GitContext.HeadHash.Status != gitcontext.StatusValue ||
		event.GitContext.HeadRef.Status != gitcontext.StatusMismatch || event.GitContext.WorktreeID.Status != gitcontext.StatusNone {
		t.Fatalf("git context view lost state: %#v", event.GitContext)
	}
	if event.GitContext.HeadHash.Reason != "" || event.GitContext.HeadRef.Reason != "" || event.GitContext.WorktreeID.Reason != "" {
		t.Fatalf("lean git context retained reasons: %#v", event.GitContext)
	}
}

// verifies: AIRA-284 — turn-id/session/agent charset and the turn-id-needs-session rule.
func TestComputeInputValidatesIdentityTokens(t *testing.T) {
	base := ComputeEventInput{Model: "m", Provider: "anthropic", Source: "claude-mod", Session: "sess-1", Agent: "ag.1", TurnID: "turn:1_a-b.c"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid identity refused: %v", err)
	}
	bad := map[string]func(*ComputeEventInput){
		"turn-id with space":        func(in *ComputeEventInput) { in.TurnID = "a b" },
		"turn-id with slash":        func(in *ComputeEventInput) { in.TurnID = "a/b" },
		"turn-id over 128":          func(in *ComputeEventInput) { in.TurnID = strings.Repeat("a", 129) },
		"session with space":        func(in *ComputeEventInput) { in.Session = "s s" },
		"session with newline":      func(in *ComputeEventInput) { in.Session = "s\n" },
		"agent with quote":          func(in *ComputeEventInput) { in.Agent = "a'b" },
		"turn-id without session":   func(in *ComputeEventInput) { in.Session = "" },
		"session over 128 no turn":  func(in *ComputeEventInput) { in.TurnID = ""; in.Session = strings.Repeat("s", 129) },
		"agent non-ascii":           func(in *ComputeEventInput) { in.Agent = "agént" },
		"turn-id only whitespace":   func(in *ComputeEventInput) { in.TurnID = "  " },
		"turn-id with unicode dash": func(in *ComputeEventInput) { in.TurnID = "a–b" },
	}
	for name, mutate := range bad {
		in := base
		mutate(&in)
		if err := in.Validate(); err == nil || !strings.HasPrefix(err.Error(), ComputeCodeInvalid) {
			t.Errorf("%s: err=%v, want %s", name, err, ComputeCodeInvalid)
		}
	}
	// A 128-character value is the boundary and is accepted.
	edge := base
	edge.TurnID, edge.Session, edge.Agent = strings.Repeat("t", 128), strings.Repeat("s", 128), strings.Repeat("a", 128)
	if err := edge.Validate(); err != nil {
		t.Fatalf("128-char boundary refused: %v", err)
	}
	// Legacy producers (no turn id, no session/agent) are unaffected.
	if err := (ComputeEventInput{Model: "m", Source: "run"}).Validate(); err != nil {
		t.Fatalf("legacy shape refused: %v", err)
	}
}

func TestComputeEventValidateTicketStatus(t *testing.T) {
	ok := ComputeEvent{ID: "CE-1", At: "2026-10-09T00:00:00Z", AtSeq: 1, Model: "m", Source: "claude-mod", Provider: "anthropic", Conservation: ConservationUnevaluated}
	for _, tc := range []struct {
		status, ticket string
		wantErr        bool
	}{
		{"", "", false}, {"unknown", "", false}, {"unknown", "T-1", false},
		{"lease-held", "T-1", false}, {"lease-held", "", true},
		{"declared", "T-1", false}, {"declared", "", true},
		{"none", "", false}, {"none", "T-1", true},
		{"unevaluated", "", false}, {"unevaluated", "T-1", true},
		{"bogus", "", true},
	} {
		e := ok
		e.TicketStatus, e.TicketID = tc.status, tc.ticket
		err := e.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("status=%q ticket=%q err=%v wantErr=%v", tc.status, tc.ticket, err, tc.wantErr)
		}
	}
}
