package domain

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"aira/internal/gitcontext"
)

const (
	MaxRantBodyBytes        = 8 << 10
	MaxRantTags             = 16
	MaxRantTagBytes         = 64
	MaxRantRefs             = 16
	MaxRantIdempotencyBytes = 256
	MaxRantNoteBytes        = 8 << 10
	RedactedRantBody        = "[redacted]"

	CodeRantInvalid             = "E_RANT_INVALID"
	CodeRantTooLarge            = "E_RANT_TOO_LARGE"
	CodeRantIdempotencyConflict = "E_RANT_IDEMPOTENCY_CONFLICT"
	CodeRantRefInvalid          = "E_RANT_REF_INVALID"
	CodeRantRedacted            = "E_RANT_REDACTED"
	CodeRantRedactionIncomplete = "E_RANT_REDACTION_INCOMPLETE"
)

type RantSeverity string

const (
	RantSeverityPapercut  RantSeverity = "papercut"
	RantSeverityAnnoyance RantSeverity = "annoyance"
	RantSeverityBlocker   RantSeverity = "blocker"
)

type RantRefKind string

const (
	RantRefRun     RantRefKind = "run"
	RantRefTicket  RantRefKind = "ticket"
	RantRefFinding RantRefKind = "finding"
	RantRefGate    RantRefKind = "gate"
)

type RantOutcome string

const (
	RantOutcomeActioned      RantOutcome = "actioned"
	RantOutcomePlanned       RantOutcome = "planned"
	RantOutcomeDuplicate     RantOutcome = "duplicate"
	RantOutcomeWontFix       RantOutcome = "wont-fix"
	RantOutcomeNeedsEvidence RantOutcome = "needs-evidence"
)

type RantRef struct {
	Kind RantRefKind `json:"kind"`
	ID   string      `json:"id"`
}

type RantInput struct {
	Body, IdempotencyKey, Actor, Session, Model string
	Tags                                        []string
	Severity                                    RantSeverity
	Refs                                        []RantRef
}

type RantReviewInput struct {
	Reviewer   string
	Note       string
	Outcome    RantOutcome
	ResolvedBy *RantRef
}

type RantReview struct {
	ID         int64       `json:"id"`
	Reviewer   string      `json:"reviewer"`
	At         string      `json:"at"`
	Note       string      `json:"note,omitempty"`
	Outcome    RantOutcome `json:"outcome,omitempty"`
	ResolvedBy *RantRef    `json:"resolved_by,omitempty"`
}

type Rant struct {
	ID              string                `json:"id"`
	Body            string                `json:"body"`
	Tags            []string              `json:"tags"`
	Severity        RantSeverity          `json:"severity,omitempty"`
	Refs            []RantRef             `json:"refs"`
	Actor           string                `json:"actor"`
	Session         string                `json:"session,omitempty"`
	Model           string                `json:"model,omitempty"`
	ObservedAt      string                `json:"observed_at,omitempty"`
	ReceivedAt      string                `json:"received_at"`
	ResolverVersion string                `json:"resolver_version,omitempty"`
	Seq             int64                 `json:"seq"`
	Reviewed        bool                  `json:"reviewed"`
	Redacted        bool                  `json:"redacted,omitempty"`
	GitContext      gitcontext.GitContext `json:"git_context"`
	Reviews         []RantReview          `json:"reviews,omitempty"`
}

type RantListOptions struct {
	By         string
	Unreviewed bool
	Since      int64
}

var rantTagBreaks = regexp.MustCompile(`[^a-z0-9]+`)
var rantIDPattern = regexp.MustCompile(`^RANT-[1-9][0-9]*$`)
var gateRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var findingRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

func (input RantInput) Normalised() (RantInput, error) {
	if !utf8.ValidString(input.Body) || strings.ContainsRune(input.Body, '\x00') || strings.TrimSpace(input.Body) == "" {
		return RantInput{}, errors.New(CodeRantInvalid + ": body must be non-empty UTF-8 without NUL")
	}
	if len([]byte(input.Body)) > MaxRantBodyBytes {
		return RantInput{}, errors.New(CodeRantTooLarge + ": body exceeds 8192 bytes")
	}
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if len([]byte(input.IdempotencyKey)) > MaxRantIdempotencyBytes || strings.ContainsRune(input.IdempotencyKey, '\x00') || !utf8.ValidString(input.IdempotencyKey) {
		return RantInput{}, errors.New(CodeRantInvalid + ": idempotency key is invalid")
	}
	input.Actor = strings.TrimSpace(input.Actor)
	if input.Actor == "" {
		input.Actor = "unknown"
	}
	input.Session, input.Model = strings.TrimSpace(input.Session), strings.TrimSpace(input.Model)
	input.Severity = RantSeverity(strings.ToLower(strings.TrimSpace(string(input.Severity))))
	if input.Severity != "" && input.Severity != RantSeverityPapercut && input.Severity != RantSeverityAnnoyance && input.Severity != RantSeverityBlocker {
		return RantInput{}, errors.New(CodeRantInvalid + ": severity is invalid")
	}
	tags := make(map[string]struct{}, len(input.Tags))
	for _, raw := range input.Tags {
		tag := strings.Trim(rantTagBreaks.ReplaceAllString(strings.ToLower(strings.TrimSpace(raw)), "-"), "-")
		if tag == "" || len([]byte(tag)) > MaxRantTagBytes {
			return RantInput{}, errors.New(CodeRantInvalid + ": tag is invalid")
		}
		tags[tag] = struct{}{}
	}
	if len(tags) > MaxRantTags {
		return RantInput{}, fmt.Errorf("%s: at most %d tags are allowed", CodeRantInvalid, MaxRantTags)
	}
	input.Tags = make([]string, 0, len(tags))
	for tag := range tags {
		input.Tags = append(input.Tags, tag)
	}
	sort.Strings(input.Tags)
	if len(input.Refs) > MaxRantRefs {
		return RantInput{}, fmt.Errorf("%s: at most %d references are allowed", CodeRantInvalid, MaxRantRefs)
	}
	refs := make(map[string]RantRef, len(input.Refs))
	for _, ref := range input.Refs {
		ref.Kind = RantRefKind(strings.ToLower(strings.TrimSpace(string(ref.Kind))))
		ref.ID = strings.TrimSpace(ref.ID)
		if err := ref.Validate(); err != nil {
			return RantInput{}, err
		}
		refs[string(ref.Kind)+"\x00"+ref.ID] = ref
	}
	input.Refs = make([]RantRef, 0, len(refs))
	for _, ref := range refs {
		input.Refs = append(input.Refs, ref)
	}
	sort.Slice(input.Refs, func(i, j int) bool {
		if input.Refs[i].Kind != input.Refs[j].Kind {
			return input.Refs[i].Kind < input.Refs[j].Kind
		}
		return input.Refs[i].ID < input.Refs[j].ID
	})
	return input, nil
}

func (ref RantRef) Validate() error {
	switch ref.Kind {
	case RantRefRun:
		if !regexp.MustCompile(`^RUN-[1-9][0-9]*$`).MatchString(ref.ID) {
			return errors.New(CodeRantRefInvalid + ": run reference is invalid")
		}
	case RantRefTicket:
		if ValidateID(ref.ID) != nil {
			return errors.New(CodeRantRefInvalid + ": ticket reference is invalid")
		}
	case RantRefFinding:
		if !findingRefPattern.MatchString(ref.ID) {
			return errors.New(CodeRantRefInvalid + ": finding reference is invalid")
		}
	case RantRefGate:
		if !gateRefPattern.MatchString(ref.ID) {
			return errors.New(CodeRantRefInvalid + ": gate reference is invalid")
		}
	default:
		return errors.New(CodeRantRefInvalid + ": reference kind is invalid")
	}
	return nil
}

func ValidateRantID(id string) error {
	if !rantIDPattern.MatchString(id) {
		return fmt.Errorf("%s: invalid rant ID %q", CodeRantInvalid, id)
	}
	return nil
}

func (input RantReviewInput) Validate() error {
	if !utf8.ValidString(input.Note) || strings.ContainsRune(input.Note, '\x00') || len([]byte(input.Note)) > MaxRantNoteBytes {
		return errors.New(CodeRantInvalid + ": review note is invalid")
	}
	switch input.Outcome {
	case "", RantOutcomeActioned, RantOutcomePlanned, RantOutcomeDuplicate, RantOutcomeWontFix, RantOutcomeNeedsEvidence:
	default:
		return errors.New(CodeRantInvalid + ": review outcome is invalid")
	}
	if input.ResolvedBy != nil {
		if err := input.ResolvedBy.Validate(); err != nil {
			return err
		}
	}
	return nil
}
