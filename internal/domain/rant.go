package domain

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
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
	ID              string       `json:"id"`
	Body            string       `json:"body"`
	Tags            []string     `json:"tags"`
	Severity        RantSeverity `json:"severity,omitempty"`
	Refs            []RantRef    `json:"refs"`
	Actor           string       `json:"actor"`
	Session         string       `json:"session,omitempty"`
	Model           string       `json:"model,omitempty"`
	ObservedAt      string       `json:"observed_at,omitempty"`
	ReceivedAt      string       `json:"received_at"`
	ResolverVersion string       `json:"resolver_version,omitempty"`
	Seq             int64        `json:"seq"`
	// OriginProjectID names the project a cross-project rant was FILED FROM and
	// is empty for an ordinary local rant. Git provenance can legitimately be
	// unevaluated, so it is not a reliable origin marker; this is the explicit
	// one, recorded from the daemon-validated caller scope so a foreign rant is
	// never indistinguishable from a local one (AIRA-179).
	OriginProjectID string                `json:"origin_project_id,omitempty"`
	Reviewed        bool                  `json:"reviewed"`
	Redacted        bool                  `json:"redacted,omitempty"`
	GitContext      gitcontext.GitContext `json:"git_context"`
	Reviews         []RantReview          `json:"reviews,omitempty"`
}

// Rant listing bounds and filter vocabulary (AIRA-327).
const (
	DefaultRantListLimit = 50
	MaxRantListLimit     = 200
	RantPreviewRunes     = 240

	RantStatusUnreviewed = "unreviewed"
	RantStatusReviewed   = "reviewed"
	RantStatusAll        = "all"

	// RantSeverityUnset is a LISTING filter value only: it selects rants that
	// recorded no severity. Capture refuses it; severity is never inferred.
	RantSeverityUnset = "unset"
)

// RantListOptions selects one page of a project's rants. The zero value is the
// work queue: unreviewed rants, oldest first, DefaultRantListLimit rows.
type RantListOptions struct {
	// Status is unreviewed (also the zero value's meaning), reviewed or all.
	// "Reviewed" means at least one append-only review row exists.
	Status string
	// Severity is papercut, annoyance, blocker, unset (no severity recorded),
	// or empty for any.
	Severity string
	// Tag, when non-empty, restricts the listing to rants carrying this
	// (already-normalised) recorded tag.
	Tag string
	// Actor, when non-empty, is an exact match on the recorded actor.
	Actor string
	// Grep, when non-empty, is a literal substring of the body matched under
	// Unicode simple case folding (RantGrepMatches). A redacted rant has no
	// searchable text: it is never a match, and the listing counts it as
	// unsearched instead.
	Grep string
	// Since is the exclusive rant-sequence cursor.
	Since int64
	// Limit is 1..MaxRantListLimit; zero means DefaultRantListLimit.
	Limit int
	// Newest takes the LAST Limit matches (still returned ascending).
	Newest bool
}

// Validate refuses an option set that cannot be listed honestly, naming the
// field and the offending value.
func (options RantListOptions) Validate() error {
	switch options.Status {
	case "", RantStatusUnreviewed, RantStatusReviewed, RantStatusAll:
	default:
		return fmt.Errorf("%s: rant ls --status %q is invalid; allowed: unreviewed, reviewed, all", CodeRantInvalid, options.Status)
	}
	switch RantSeverity(options.Severity) {
	case "", RantSeverityPapercut, RantSeverityAnnoyance, RantSeverityBlocker, RantSeverityUnset:
	default:
		return fmt.Errorf("%s: rant ls --severity %q is invalid; allowed: papercut, annoyance, blocker, unset", CodeRantInvalid, options.Severity)
	}
	if options.Limit < 0 || options.Limit > MaxRantListLimit {
		return fmt.Errorf("%s: rant ls --limit %d is out of range; allowed: 1..%d", CodeRantInvalid, options.Limit, MaxRantListLimit)
	}
	if options.Since < 0 {
		return fmt.Errorf("%s: rant ls --since %d is invalid; it must be a non-negative integer", CodeRantInvalid, options.Since)
	}
	return nil
}

// RantGrepMatches reports whether pattern occurs in body as a literal
// substring, case-insensitively under Unicode simple case folding: two runes
// match when strings.EqualFold would equate them ("TÖÖ" contains "töö", "Σ",
// "σ" and "ς" are one letter). Full folding is not applied ("ß" does not
// match "ss"), and nothing is a wildcard. Any non-empty pattern, whitespace
// included, is searched as given.
func RantGrepMatches(body, pattern string) bool {
	return strings.Contains(strings.Map(foldRune, body), strings.Map(foldRune, pattern))
}

// foldRune maps a rune to the smallest member of its simple case-folding
// orbit, so two runes fold to the same value exactly when they are equal
// under simple case folding.
func foldRune(r rune) rune {
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < least {
			least = f
		}
	}
	return least
}

// RantSummary is the compact row `rant ls` returns by default: the metadata a
// triage pass sorts on plus a bounded body preview, never the git context or
// review prose (those stay behind `rant get` and `rant ls --full`).
type RantSummary struct {
	ID         string       `json:"id"`
	Seq        int64        `json:"seq"`
	ReceivedAt string       `json:"received_at"`
	Severity   RantSeverity `json:"severity"`
	Tags       []string     `json:"tags"`
	Refs       []RantRef    `json:"refs"`
	Actor      string       `json:"actor"`
	// OriginProjectID is carried so a cross-project rant is never
	// indistinguishable from a local one in a listing (AIRA-179).
	OriginProjectID string `json:"origin_project_id,omitempty"`
	Reviewed        bool   `json:"reviewed"`
	// Outcome is the most recent typed outcome any review recorded, or empty
	// when no review recorded one. A later note-only review does not erase
	// it: this is the same outcome capture's sharing_recorded_tags
	// loop-closer reports (store.sharingRecordedTags, outcome<>'').
	Outcome  RantOutcome `json:"outcome"`
	Redacted bool        `json:"redacted,omitempty"`
	// Body is the first RantPreviewRunes runes. When it was cut, BodyTruncated
	// is set and BodyBytes is the full body's length in bytes.
	Body          string `json:"body"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`
	BodyBytes     int    `json:"body_bytes,omitempty"`
}

// Summary projects a rant onto its compact listing row.
func (rant Rant) Summary() RantSummary {
	summary := RantSummary{ID: rant.ID, Seq: rant.Seq, ReceivedAt: rant.ReceivedAt, Severity: rant.Severity,
		Tags: append([]string{}, rant.Tags...), Refs: append([]RantRef{}, rant.Refs...), Actor: rant.Actor, OriginProjectID: rant.OriginProjectID,
		Reviewed: rant.Reviewed, Redacted: rant.Redacted, Body: rant.Body}
	for i := len(rant.Reviews) - 1; i >= 0; i-- {
		if rant.Reviews[i].Outcome != "" {
			summary.Outcome = rant.Reviews[i].Outcome
			break
		}
	}
	runes := 0
	for cut := range rant.Body {
		if runes == RantPreviewRunes {
			summary.Body, summary.BodyTruncated, summary.BodyBytes = rant.Body[:cut], true, len(rant.Body)
			break
		}
		runes++
	}
	return summary
}

var rantTagBreaks = regexp.MustCompile(`[^a-z0-9]+`)
var rantIDPattern = regexp.MustCompile(`^RANT-[1-9][0-9]*$`)
var gateRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var findingRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

// NormaliseRantTag canonicalises a tag exactly as capture does (lowercase,
// whitespace/punctuation runs → single hyphen, trimmed), so a filter typed as
// "Slow Tests" matches the stored "slow-tests". Shared by capture and ls --tag.
func NormaliseRantTag(raw string) string {
	return strings.Trim(rantTagBreaks.ReplaceAllString(strings.ToLower(strings.TrimSpace(raw)), "-"), "-")
}

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
		return RantInput{}, fmt.Errorf("%s: rant --severity %q is invalid; allowed: papercut, annoyance, blocker", CodeRantInvalid, input.Severity)
	}
	tags := make(map[string]struct{}, len(input.Tags))
	for _, raw := range input.Tags {
		tag := NormaliseRantTag(raw)
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
