package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// TestReport is the normalised, DB-resident test archive record. Reports are
// operational telemetry: they deliberately have no file/journal identity.
type TestReport struct {
	ID             string       `json:"id"`
	TicketID       string       `json:"ticket_id,omitempty"`
	Phase          string       `json:"phase,omitempty"`
	Commit         string       `json:"commit"`
	Branch         string       `json:"branch"`
	WorktreeID     string       `json:"worktree_id"`
	Agent          string       `json:"agent,omitempty"`
	Session        string       `json:"session,omitempty"`
	At             string       `json:"at"`
	RunRef         string       `json:"run_ref,omitempty"`
	SuiteID        string       `json:"suite_id"`
	Runner         string       `json:"runner"`
	Config         string       `json:"config"`
	EnvDigest      string       `json:"env_digest"`
	Shard          string       `json:"shard"`
	RetryIndex     int          `json:"retry_index"`
	ParserComplete bool         `json:"parser_complete"`
	Coverage       *Coverage    `json:"coverage,omitempty"`
	Format         string       `json:"format"`
	SourceDigest   string       `json:"source_digest"`
	AtSeq          int64        `json:"at_seq"`
	Pinned         bool         `json:"pinned,omitempty"`
	Results        []TestResult `json:"results"`
}

type Coverage struct {
	Pct          *float64 `json:"pct,omitempty"`
	LinesCovered *int64   `json:"lines_covered,omitempty"`
	LinesTotal   *int64   `json:"lines_total,omitempty"`
}

type TestOutcome string

const (
	OutcomePass  TestOutcome = "pass"
	OutcomeFail  TestOutcome = "fail"
	OutcomeSkip  TestOutcome = "skip"
	OutcomeError TestOutcome = "error"
)

type TestResult struct {
	Name       string      `json:"name"`
	Outcome    TestOutcome `json:"outcome"`
	DurationNS *int64      `json:"duration_ns,omitempty"`
	Message    string      `json:"message,omitempty"`
}

// TestReportInput is the caller/parser-normalised report before DB-owned
// fields (id, source digest, at, and ingress sequence) are assigned.
type TestReportInput struct {
	TicketID       string
	Phase          string
	Commit         string
	Branch         string
	WorktreeID     string
	Agent          string
	Session        string
	At             string
	RunRef         string
	SuiteID        string
	Runner         string
	Config         string
	EnvDigest      string
	Shard          string
	RetryIndex     int
	ParserComplete bool
	// ForceParserIncomplete is a caller-established upper bound on parser
	// completeness (for example, an incomplete or capped runner capture).
	ForceParserIncomplete bool
	Coverage              *Coverage
	Format                string
	SourceDigest          string
	Raw                   []byte
	Results               []TestResult
}

var shardPattern = regexp.MustCompile(`^[1-9][0-9]*/[1-9][0-9]*$`)

func (r TestReportInput) Normalized() TestReportInput {
	r.TicketID = strings.TrimSpace(r.TicketID)
	r.Phase = strings.TrimSpace(r.Phase)
	r.Commit = strings.TrimSpace(r.Commit)
	r.Branch = strings.TrimSpace(r.Branch)
	r.WorktreeID = strings.TrimSpace(r.WorktreeID)
	r.Agent = strings.TrimSpace(r.Agent)
	r.Session = strings.TrimSpace(r.Session)
	r.At = strings.TrimSpace(r.At)
	r.RunRef = strings.TrimSpace(r.RunRef)
	r.SuiteID = strings.TrimSpace(r.SuiteID)
	r.Runner = strings.TrimSpace(r.Runner)
	r.Config = strings.TrimSpace(r.Config)
	r.EnvDigest = strings.TrimSpace(r.EnvDigest)
	r.Shard = strings.TrimSpace(r.Shard)
	r.Format = strings.ToLower(strings.TrimSpace(r.Format))
	r.SourceDigest = strings.TrimSpace(r.SourceDigest)
	r.Results = append([]TestResult(nil), r.Results...)
	for i := range r.Results {
		r.Results[i].Name = strings.TrimSpace(r.Results[i].Name)
		r.Results[i].Message = strings.TrimSpace(r.Results[i].Message)
	}
	return r
}

func (r TestReportInput) Validate() error {
	r = r.Normalized()
	if r.Format != "go-json" && r.Format != "junit" {
		return fmt.Errorf("E_TESTREPORT_INVALID: unsupported report format %q", r.Format)
	}
	if r.Shard == "" {
		return errors.New("E_TESTREPORT_INVALID: shard is empty")
	}
	if !shardPattern.MatchString(r.Shard) {
		return fmt.Errorf("E_TESTREPORT_INVALID: shard %q must be i/n", r.Shard)
	}
	if r.RetryIndex < 0 {
		return errors.New("E_TESTREPORT_INVALID: retry index must be non-negative")
	}
	for _, result := range r.Results {
		if result.Name == "" || strings.ContainsRune(result.Name, 0) {
			return errors.New("E_TESTREPORT_INVALID: test result name is invalid")
		}
		switch result.Outcome {
		case OutcomePass, OutcomeFail, OutcomeSkip, OutcomeError:
		default:
			return fmt.Errorf("E_TESTREPORT_INVALID: unknown test outcome %q", result.Outcome)
		}
		if result.DurationNS != nil && *result.DurationNS < 0 {
			return errors.New("E_TESTREPORT_INVALID: test duration is negative")
		}
	}
	return nil
}

func (r TestReport) Validate() error {
	input := TestReportInput{
		TicketID: r.TicketID, Phase: r.Phase, Commit: r.Commit, Branch: r.Branch,
		WorktreeID: r.WorktreeID, Agent: r.Agent, Session: r.Session, At: r.At,
		RunRef: r.RunRef, SuiteID: r.SuiteID, Runner: r.Runner, Config: r.Config,
		EnvDigest: r.EnvDigest, Shard: r.Shard, RetryIndex: r.RetryIndex,
		ParserComplete: r.ParserComplete, Coverage: r.Coverage, Format: r.Format,
		SourceDigest: r.SourceDigest, Results: r.Results,
	}
	if err := input.Validate(); err != nil {
		return err
	}
	if r.ID == "" || r.At == "" || r.SourceDigest == "" || r.AtSeq < 1 {
		return errors.New("E_TESTREPORT_INVALID: DB-owned report fields are incomplete")
	}
	return nil
}

type FlakyState string

const (
	FlakyStateFlaky       FlakyState = "flaky"
	FlakyStateClean       FlakyState = "clean"
	FlakyStateUnevaluated FlakyState = "unevaluated"
)

type FlakyCell struct {
	Commit    string     `json:"commit"`
	SuiteID   string     `json:"suite_id"`
	Config    string     `json:"config"`
	EnvDigest string     `json:"env_digest"`
	Shard     string     `json:"shard"`
	State     FlakyState `json:"state"`
	Reason    string     `json:"reason"`
	Evidence  int        `json:"evidence"`
	Passes    []string   `json:"passes,omitempty"`
	Failures  []string   `json:"failures,omitempty"`
}

type FlakyTest struct {
	Name   string      `json:"name"`
	State  FlakyState  `json:"state"`
	Reason string      `json:"reason"`
	Cells  []FlakyCell `json:"cells,omitempty"`
}

func (f FlakyTest) Validate() error {
	if f.Name == "" {
		return errors.New("E_TESTREPORT_INVALID: flaky test name is empty")
	}
	switch f.State {
	case FlakyStateFlaky, FlakyStateClean, FlakyStateUnevaluated:
		return nil
	default:
		return fmt.Errorf("E_TESTREPORT_INVALID: unknown flaky state %q", f.State)
	}
}
