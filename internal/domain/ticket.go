package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// covers: AR-5, AR-6

type Status string

const (
	StatusDraft      Status = "draft"
	StatusPlanned    Status = "planned"
	StatusInProgress Status = "in-progress"
	StatusInReview   Status = "in-review"
	StatusDone       Status = "done"
	StatusRetired    Status = "retired"
	StatusSuperseded Status = "superseded"
)

type Kind string

const (
	KindFeature         Kind = "feature"
	KindBug             Kind = "bug"
	KindChore           Kind = "chore"
	KindSpike           Kind = "spike"
	KindRequirementWork Kind = "requirement-work"
)

type Severity string

const (
	SeverityP0 Severity = "P0"
	SeverityP1 Severity = "P1"
	SeverityP2 Severity = "P2"
)

type RelationKind string

const (
	RelationBlocks       RelationKind = "blocks"
	RelationBlockedBy    RelationKind = "blocked-by"
	RelationParent       RelationKind = "parent"
	RelationChild        RelationKind = "child"
	RelationRelates      RelationKind = "relates"
	RelationDuplicates   RelationKind = "duplicates"
	RelationDuplicatedBy RelationKind = "duplicated-by"
	RelationSupersedes   RelationKind = "supersedes"
	RelationSupersededBy RelationKind = "superseded-by"
	RelationResolves     RelationKind = "resolves"
	RelationResolvedBy   RelationKind = "resolved-by"
)

type Relation struct {
	Kind RelationKind `json:"kind"`
	From string       `json:"from"`
	To   string       `json:"to"`
}

type Ticket struct {
	Schema    int        `json:"schema"`
	ID        string     `json:"id"`
	Project   string     `json:"project"`
	Title     string     `json:"title"`
	Status    Status     `json:"status"`
	Kind      Kind       `json:"kind"`
	Severity  Severity   `json:"severity"`
	Assignee  *string    `json:"assignee"`
	Milestone *string    `json:"milestone"`
	Labels    []string   `json:"labels"`
	Hold      bool       `json:"hold"`
	Relations []Relation `json:"relations"`
}

type CreateTicketInput struct {
	Title    string
	Kind     Kind
	Severity Severity
	Body     string
	Labels   []string
}

var idPattern = regexp.MustCompile(`^[A-Z]{2,}-[1-9][0-9]*$`)
var projectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("E_ID_INVALID: invalid ticket ID %q", id)
	}
	return nil
}

func ValidateProjectSlug(project string) error {
	if !projectPattern.MatchString(project) {
		return fmt.Errorf("E_CONFIG_INVALID: invalid project slug %q", project)
	}
	return nil
}

func ValidateTransition(from, to Status) error {
	allowed := map[Status]map[Status]bool{
		StatusDraft:      {StatusPlanned: true, StatusRetired: true, StatusSuperseded: true},
		StatusPlanned:    {StatusInProgress: true, StatusRetired: true, StatusSuperseded: true},
		StatusInProgress: {StatusInReview: true, StatusPlanned: true, StatusRetired: true, StatusSuperseded: true},
		StatusInReview:   {StatusInProgress: true, StatusDone: true, StatusRetired: true, StatusSuperseded: true},
		StatusDone:       {StatusRetired: true, StatusSuperseded: true},
		StatusRetired:    {},
		StatusSuperseded: {},
	}
	if !allowed[from][to] {
		return fmt.Errorf("E_TRANSITION_INVALID: %s -> %s", from, to)
	}
	return nil
}

func (t Ticket) Validate() error {
	if t.Schema != 1 {
		return errors.New("E_CONFIG_INVALID: unsupported ticket schema")
	}
	if err := ValidateID(t.ID); err != nil {
		return err
	}
	if err := ValidateProjectSlug(t.Project); err != nil {
		return err
	}
	if strings.TrimSpace(t.Title) == "" {
		return errors.New("E_CONFIG_INVALID: ticket title is empty")
	}
	if !validStatus(t.Status) || !validKind(t.Kind) || !validSeverity(t.Severity) {
		return errors.New("E_CONFIG_INVALID: ticket enum is invalid")
	}
	for i, label := range t.Labels {
		if label == "" || label != strings.ToLower(label) {
			return errors.New("E_CONFIG_INVALID: labels must be non-empty lowercase strings")
		}
		if i > 0 && t.Labels[i-1] >= label {
			return errors.New("E_CONFIG_INVALID: labels must be unique and sorted")
		}
	}
	for i, r := range t.Relations {
		if !validRelation(r.Kind) || r.From == r.To || ValidateID(r.From) != nil || ValidateID(r.To) != nil {
			return errors.New("E_RELATION_INVALID: invalid relation")
		}
		if CanonicalRelationOwner(r.From, r.To) != t.ID {
			return errors.New("E_RELATION_INVALID: relation is not stored on its canonical lower-ID ticket")
		}
		if i > 0 {
			previous := t.Relations[i-1]
			if relationLess(r, previous) {
				return errors.New("E_RELATION_INVALID: relations must be sorted")
			}
			if r == previous {
				return errors.New("E_RELATION_INVALID: relations must be unique")
			}
		}
	}
	return nil
}

func relationLess(a, b Relation) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.From != b.From {
		return idLess(a.From, b.From)
	}
	return idLess(a.To, b.To)
}

func validStatus(s Status) bool {
	switch s {
	case StatusDraft, StatusPlanned, StatusInProgress, StatusInReview, StatusDone, StatusRetired, StatusSuperseded:
		return true
	default:
		return false
	}
}

func validKind(k Kind) bool {
	switch k {
	case KindFeature, KindBug, KindChore, KindSpike, KindRequirementWork:
		return true
	default:
		return false
	}
}

// ValidKind reports whether k is one of the ticket kinds in the domain.
func ValidKind(k Kind) bool { return validKind(k) }

func validSeverity(s Severity) bool {
	return s == SeverityP0 || s == SeverityP1 || s == SeverityP2
}

// ValidSeverity reports whether s is one of the ticket severities in the domain.
func ValidSeverity(s Severity) bool { return validSeverity(s) }

func validRelation(k RelationKind) bool {
	switch k {
	case RelationBlocks, RelationParent, RelationRelates, RelationDuplicates, RelationSupersedes, RelationResolves:
		return true
	default:
		return false
	}
}

// ValidRelationKind reports whether k is one of the six writable, forward
// relation kinds. Inverse kinds are query projections and cannot be stored.
func ValidRelationKind(k RelationKind) bool { return validRelation(k) }

// Inverse returns the derived query kind for a stored forward relation.
func (k RelationKind) Inverse() RelationKind {
	switch k {
	case RelationBlocks:
		return RelationBlockedBy
	case RelationParent:
		return RelationChild
	case RelationRelates:
		return RelationRelates
	case RelationDuplicates:
		return RelationDuplicatedBy
	case RelationSupersedes:
		return RelationSupersededBy
	case RelationResolves:
		return RelationResolvedBy
	default:
		return ""
	}
}

func (k RelationKind) IsForward() bool { return validRelation(k) }

// CanonicalRelationOwner is the lower-ID endpoint according to the Phase-1
// prefix/numeric ordering. It is the only ticket file allowed to store a
// relation between from and to.
func CanonicalRelationOwner(from, to string) string {
	if idLess(from, to) {
		return from
	}
	return to
}

// IDLess exposes the canonical ticket ordering to storage projections without
// exposing the parser internals used to implement it.
func IDLess(a, b string) bool { return idLess(a, b) }

// RelationView is a stored relation or its pure derived inverse as presented
// from a ticket. It deliberately has no storage marker: inverse rows are not
// persisted and cannot be mistaken for canonical content.
type RelationView struct {
	Kind RelationKind `json:"kind"`
	From string       `json:"from"`
	To   string       `json:"to"`
}

// Lease is a sum type. A free lease has no holder data; a held lease carries
// every field required to evaluate liveness. The database serialises this
// value but is never the source of its liveness semantics.
type Lease struct {
	ticketID string
	state    LeaseState
}

type LeaseState interface{ leaseState() }

type FreeLease struct {
	Generation uint64 `json:"generation"`
}

func (FreeLease) leaseState() {}

type HeldLease struct {
	holderTokenHash     [32]byte
	bootID              string
	lastHeartbeatMonoNS uint64
	ttlNS               uint64
	generation          uint64
	actor               string
	worktree            string
}

func (HeldLease) leaseState() {}

func (h HeldLease) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		BootID              string `json:"boot_id"`
		LastHeartbeatMonoNS uint64 `json:"last_heartbeat_mono_ns"`
		TTLNS               uint64 `json:"ttl_ns"`
		Generation          uint64 `json:"generation"`
		Actor               string `json:"actor"`
		Worktree            string `json:"worktree"`
	}{
		BootID:              h.BootID(),
		LastHeartbeatMonoNS: h.LastHeartbeatMonoNS(),
		TTLNS:               h.TTLNS(),
		Generation:          h.Generation(),
		Actor:               h.Actor(),
		Worktree:            h.Worktree(),
	})
}

// NewFreeLease constructs the free state. Generation zero is the initial
// state for a ticket that has never been claimed; subsequent free states carry
// the generation after release.
func NewFreeLease(generation uint64) (FreeLease, error) {
	return FreeLease{Generation: generation}, nil
}

// NewLease constructs a lease only from a validated state. State is private so
// callers cannot inject an arbitrary or zero-value HeldLease through a struct
// literal; readers use Held and Free to inspect the sum type.
func NewLease(ticketID string, state LeaseState) (Lease, error) {
	if err := ValidateID(ticketID); err != nil {
		return Lease{}, err
	}
	switch value := state.(type) {
	case FreeLease:
		_ = value
	case HeldLease:
		if !value.valid() {
			return Lease{}, errors.New("E_CONFIG_INVALID: invalid lease state")
		}
	default:
		return Lease{}, errors.New("E_CONFIG_INVALID: missing or unknown lease state")
	}
	return Lease{ticketID: ticketID, state: state}, nil
}

// NewHeldLease constructs a held state after validating every field required
// for lease liveness and token ownership. The token hash is copied so callers
// cannot mutate the domain value through a backing slice.
func NewHeldLease(holderTokenHash []byte, bootID string, lastHeartbeatMonoNS uint64, ttlNS int64, generation uint64, actor, worktree string) (HeldLease, error) {
	if len(holderTokenHash) != 32 {
		return HeldLease{}, errors.New("E_CONFIG_INVALID: malformed lease token hash")
	}
	if strings.TrimSpace(bootID) == "" {
		return HeldLease{}, errors.New("E_CONFIG_INVALID: empty lease boot id")
	}
	if ttlNS <= 0 {
		return HeldLease{}, errors.New("E_CONFIG_INVALID: non-positive lease ttl")
	}
	if generation == 0 {
		return HeldLease{}, errors.New("E_CONFIG_INVALID: zero lease generation")
	}
	if strings.TrimSpace(actor) == "" {
		return HeldLease{}, errors.New("E_CONFIG_INVALID: empty lease actor")
	}
	if strings.TrimSpace(worktree) == "" {
		return HeldLease{}, errors.New("E_CONFIG_INVALID: empty lease worktree")
	}
	var hash [32]byte
	copy(hash[:], holderTokenHash)
	if hash == ([32]byte{}) {
		return HeldLease{}, errors.New("E_CONFIG_INVALID: empty lease token hash")
	}
	return HeldLease{
		holderTokenHash: hash, bootID: bootID, lastHeartbeatMonoNS: lastHeartbeatMonoNS,
		ttlNS: uint64(ttlNS), generation: generation, actor: actor, worktree: worktree,
	}, nil
}

func (h HeldLease) HolderTokenHash() [32]byte   { return h.holderTokenHash }
func (h HeldLease) BootID() string              { return h.bootID }
func (h HeldLease) LastHeartbeatMonoNS() uint64 { return h.lastHeartbeatMonoNS }
func (h HeldLease) TTLNS() uint64               { return h.ttlNS }
func (h HeldLease) Generation() uint64          { return h.generation }
func (h HeldLease) Actor() string               { return h.actor }
func (h HeldLease) Worktree() string            { return h.worktree }

func (h HeldLease) valid() bool {
	return h.holderTokenHash != ([32]byte{}) && len(h.bootID) > 0 && h.ttlNS > 0 && h.generation > 0 &&
		strings.TrimSpace(h.bootID) != "" && strings.TrimSpace(h.actor) != "" && strings.TrimSpace(h.worktree) != ""
}

// IsLive is a pure function of the held state and the supplied clock sample.
// It intentionally uses subtraction to make expiry safe across uint64 wrap.
func (h HeldLease) IsLive(bootID string, monoNowNS uint64) bool {
	if !h.valid() || h.bootID == "" || bootID == "" || h.bootID != bootID || monoNowNS < h.lastHeartbeatMonoNS {
		return false
	}
	return monoNowNS-h.lastHeartbeatMonoNS < h.ttlNS
}

// Valid reports whether the lease contains a valid sum-type state.
func (l Lease) Valid() bool {
	switch state := l.state.(type) {
	case FreeLease:
		return true
	case HeldLease:
		return state.valid()
	default:
		return false
	}
}

func (l Lease) TicketID() string { return l.ticketID }

func (l Lease) Held() (HeldLease, bool) {
	h, ok := l.state.(HeldLease)
	if !ok || !h.valid() {
		return HeldLease{}, false
	}
	return h, ok
}

func (l Lease) Free() (FreeLease, bool) {
	f, ok := l.state.(FreeLease)
	if !ok {
		return FreeLease{}, false
	}
	return f, ok
}

func (l Lease) MarshalJSON() ([]byte, error) {
	if !l.Valid() {
		return nil, errors.New("E_CONFIG_INVALID: cannot marshal invalid lease")
	}
	return json.Marshal(struct {
		TicketID string     `json:"ticket_id"`
		State    LeaseState `json:"state"`
	}{TicketID: l.TicketID(), State: l.state})
}

func RenderTicket(ticket Ticket, body string) ([]byte, error) {
	ticket.Schema = 1
	ticket.Labels = uniqueSortedLabels(ticket.Labels)
	ticket.Relations = append([]Relation(nil), ticket.Relations...)
	if ticket.Relations == nil {
		ticket.Relations = []Relation{}
	}
	sort.Slice(ticket.Relations, func(i, j int) bool {
		a, b := ticket.Relations[i], ticket.Relations[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.From != b.From {
			return idLess(a.From, b.From)
		}
		return idLess(a.To, b.To)
	})
	if err := ticket.Validate(); err != nil {
		return nil, err
	}
	if body == "" {
		body = "\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	header, err := json.Marshal(ticket)
	if err != nil {
		return nil, err
	}
	return bytes.Join([][]byte{[]byte("---\n"), header, []byte("\n---\n"), []byte(body)}, nil), nil
}

func uniqueSortedLabels(labels []string) []string {
	if len(labels) == 0 {
		return []string{}
	}
	copyLabels := append([]string(nil), labels...)
	sort.Strings(copyLabels)
	result := copyLabels[:0]
	for _, label := range copyLabels {
		if len(result) == 0 || result[len(result)-1] != label {
			result = append(result, label)
		}
	}
	return result
}

func ParseTicket(data []byte) (Ticket, string, error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return Ticket{}, "", errors.New("E_CONFIG_INVALID: missing ticket frontmatter")
	}
	rest := data[len("---\n"):]
	marker := []byte("\n---\n")
	idx := bytes.Index(rest, marker)
	if idx < 0 {
		return Ticket{}, "", errors.New("E_CONFIG_INVALID: malformed ticket frontmatter")
	}
	var ticket Ticket
	dec := json.NewDecoder(bytes.NewReader(rest[:idx]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ticket); err != nil {
		return Ticket{}, "", fmt.Errorf("E_CONFIG_INVALID: %w", err)
	}
	if err := ticket.Validate(); err != nil {
		return Ticket{}, "", err
	}
	body := string(rest[idx+len(marker):])
	if !strings.HasSuffix(body, "\n") {
		return Ticket{}, "", errors.New("E_CONFIG_INVALID: ticket body must end in newline")
	}
	return ticket, body, nil
}

func idLess(a, b string) bool {
	if a == b {
		return false
	}
	ap, an := splitID(a)
	bp, bn := splitID(b)
	if ap != bp {
		return ap < bp
	}
	an = strings.TrimLeft(an, "0")
	bn = strings.TrimLeft(bn, "0")
	if an == "" {
		an = "0"
	}
	if bn == "" {
		bn = "0"
	}
	if len(an) != len(bn) {
		return len(an) < len(bn)
	}
	return an < bn
}

func splitID(id string) (string, string) {
	idx := strings.LastIndexByte(id, '-')
	if idx < 0 {
		return id, ""
	}
	return id[:idx], id[idx+1:]
}
