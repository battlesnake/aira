// Package core is the transport-neutral AIRA dispatch seam. Adapters provide
// serializable requests and render the returned response; handlers below own
// all store and protocol behaviour.
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"aira/internal/domain"
	"aira/internal/gate"
	"aira/internal/runner"
	"aira/internal/store"
)

type Request struct {
	Verb string         `json:"verb"`
	Args map[string]any `json:"args,omitempty"`
}

type Response struct {
	OK       bool     `json:"ok"`
	Code     string   `json:"code"`
	Data     any      `json:"data,omitempty"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Exit     int      `json:"exit,omitempty"`
}

type Initializer func(context.Context, map[string]any) (any, error)

type argAccessor struct {
	values map[string]any
	reads  map[string]struct{}
}

func newArgAccessor(values map[string]any) *argAccessor {
	return &argAccessor{values: values, reads: make(map[string]struct{})}
}

func (a *argAccessor) record(key string) any {
	a.reads[key] = struct{}{}
	if a.values == nil {
		return nil
	}
	return a.values[key]
}

func (a *argAccessor) present(key string) bool {
	a.reads[key] = struct{}{}
	if a.values == nil {
		return false
	}
	_, ok := a.values[key]
	return ok
}

type Store interface {
	AllocateID(context.Context, string) (string, error)
	CreateTicketWithEvent(context.Context, domain.CreateTicketInput) (domain.Ticket, store.EventKey, error)
	Get(string) (store.TicketRecord, error)
	List(string) ([]store.TicketRecord, error)
	AddFinding(context.Context, domain.ReviewFindingInput) (domain.Finding, store.EventKey, error)
	ListFindings(string) ([]store.FindingRecord, error)
	AddRequirement(context.Context, domain.RequirementInput) (domain.Requirement, store.EventKey, error)
	GetRequirement(string) (store.RequirementRecord, error)
	ListRequirements() ([]store.RequirementRecord, error)
	SetRequirement(context.Context, string, domain.RequirementStatus) (store.EventKey, error)
	ImportRequirements(context.Context, string) (store.ImportRequirementsSummary, error)
	ImportFindingsFile(context.Context, string, bool) (store.ImportSummary, error)
	Search(context.Context, string, string) ([]store.SearchResult, error)
	GetFinding(string) (store.FindingRecord, error)
	SetFinding(context.Context, string, domain.Disposition, string, string) (store.EventKey, error)
	Count(string, string) (store.CountResult, error)
	SetTicket(context.Context, string, string, string) (store.EventKey, error)
	MoveTicket(context.Context, string, domain.Status) (store.EventKey, error)
	Claim(context.Context, string, bool, string) (store.LeaseClaim, error)
	Release(context.Context, string, string) (store.EventKey, error)
	Heartbeat(context.Context, string, string) (domain.Lease, error)
	Touch(context.Context, string, string, []string) (store.AreaTouchResult, error)
	LeaseToken(string) (string, error)
	Link(context.Context, string, domain.RelationKind, string) (store.EventKey, error)
	Unlink(context.Context, string, domain.RelationKind, string) (store.EventKey, error)
	Relations(string) ([]domain.RelationView, error)
	Ready(string) ([]store.ReadyRecord, error)
	Reconcile(context.Context) error
	Rebuild(context.Context) error
	Check(context.Context) (store.CheckReport, error)
}

type reviewStore interface {
	Store
	TicketAreaGlobs(string) ([]string, error)
	ReviewPolicy() store.ReviewPolicy
}

type gateStore interface {
	ListGates() ([]gate.GateDefinition, error)
	GateCheck(context.Context) (store.GateCheckReport, error)
	RunGate(context.Context, string) (store.GateCheckResult, error)
}

type gateAttestationStore interface {
	AttestGate(context.Context, string, string, string) (store.GateCheckResult, error)
}

type gateActionStore interface {
	GateAction(context.Context, string, string, string) (any, error)
}

type gateActionInputStore interface {
	GateActionWithFields(context.Context, string, string, string, map[string]any) (any, error)
}

type handlerData struct {
	Data     any
	Warnings []string
	Verdict  string
}

type outputReadData struct {
	Chunk *runner.OutputChunk
	Err   error
}

func runnerError(code string, err error) error {
	if err == nil {
		err = errors.New(code)
	}
	return fmt.Errorf("%s: %w", code, err)
}

func nonNegativeInt(args *argAccessor, name string) (int64, error) {
	raw := stringArg(args, name)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, runnerError("E_RUN_ARGUMENT_INVALID", fmt.Errorf("%s must be a non-negative integer", name))
	}
	return value, nil
}

// ArgKind is the intentionally small transport-neutral argument vocabulary
// shared by the CLI and generated protocol faces.
type ArgKind string

const (
	ArgKindString     ArgKind = "string"
	ArgKindBool       ArgKind = "bool"
	ArgKindStringList ArgKind = "stringlist"
)

type ArgSpec struct {
	Name        string   `json:"name"`
	Kind        ArgKind  `json:"kind"`
	Required    bool     `json:"required"`
	Positional  bool     `json:"positional"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description"`
}

// SafetyClass describes the operational effect of a dispatch action. It is
// deliberately closed so generated faces cannot silently invent a new class.
type SafetyClass string

const (
	SafetyRead      SafetyClass = "read"
	SafetyMutate    SafetyClass = "mutate"
	SafetyLease     SafetyClass = "lease"
	SafetyReconcile SafetyClass = "reconcile"
	SafetyExecute   SafetyClass = "execute"
)

// Valid reports whether s is one of the closed safety classes.
func (s SafetyClass) Valid() bool {
	switch s {
	case SafetyRead, SafetyMutate, SafetyLease, SafetyReconcile, SafetyExecute:
		return true
	default:
		return false
	}
}

func ValidSafetyClass(s SafetyClass) bool { return s.Valid() }

type OperationArg struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

type OperationSpec struct {
	Name    string         `json:"name"`
	Summary string         `json:"summary"`
	Safety  SafetyClass    `json:"safety"`
	Args    []OperationArg `json:"args"`
	Example []string       `json:"example"`
}

// DispatchDescriptor is the read-only metadata projection used by generated
// faces. The handler itself is deliberately not exported.
type DispatchDescriptor struct {
	Name         string          `json:"name"`
	Usage        string          `json:"usage"`
	Args         []ArgSpec       `json:"args"`
	MCPTool      string          `json:"mcp_tool,omitempty"`
	MCPOperation string          `json:"mcp_operation,omitempty"`
	Summary      string          `json:"summary"`
	Safety       SafetyClass     `json:"safety"`
	Include      bool            `json:"include"`
	Example      []string        `json:"example"`
	Operations   []OperationSpec `json:"operations,omitempty"`
}

type Core struct {
	store       Store
	runner      Runner
	initializer Initializer
	stdin       io.Reader
	outputCap   int64
	verbs       map[string]verbSpec
}

// Runner is the transport-neutral execution seam. The faces never call the
// concrete runner directly; they construct core.Request values and dispatch
// through this interface.
type Runner interface {
	Launch(context.Context, runner.Request) (*runner.RunRecord, error)
	Kill(context.Context, string) (*runner.RunRecord, error)
	Get(string) (*runner.RunRecord, error)
	ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error)
	Reconcile(context.Context) ([]runner.RunRecord, error)
}

type verbSpec struct {
	Name         string
	Usage        string
	Args         []ArgSpec
	MCPTool      string
	MCPOperation string
	Summary      string
	Safety       SafetyClass
	Include      bool
	Example      []string
	Operations   []OperationSpec
	Run          func(context.Context, *argAccessor) (any, error)
}

const ListLimit = store.ListLimit

func New(s Store) *Core {
	c := &Core{store: s}
	c.verbs = c.dispatchTable()
	return c
}

func NewWithRunner(s Store, execution Runner) *Core {
	c := &Core{store: s, runner: execution}
	c.verbs = c.dispatchTable()
	return c
}

func NewWithRunnerInput(s Store, execution Runner, stdin io.Reader) *Core {
	c := NewWithRunner(s, execution)
	c.stdin = stdin
	return c
}

func NewWithRunnerOutputCap(s Store, execution Runner, cap int64) *Core {
	c := NewWithRunner(s, execution)
	c.outputCap = cap
	return c
}

func NewWithInitializer(s Store, initializer Initializer) *Core {
	c := New(s)
	c.initializer = initializer
	return c
}

func NewWithInitializerAndRunner(s Store, execution Runner, initializer Initializer) *Core {
	c := NewWithRunner(s, execution)
	c.initializer = initializer
	return c
}

// Do dispatches every verb through the same table used to generate Help.
func (c *Core) Do(ctx context.Context, req Request) Response {
	verb := strings.ToLower(strings.TrimSpace(req.Verb))
	if verb == "new" {
		verb = "create"
	}
	if verb == "get" {
		verb = "show"
	}
	if verb == "ls" {
		verb = "list"
	}
	spec, ok := c.verbs[verb]
	if !ok {
		code := "E_UNKNOWN_VERB"
		return Response{Code: code, Error: fmt.Sprintf("unknown verb %q", req.Verb), Exit: store.ExitForCode(code)}
	}
	data, err := spec.Run(ctx, newArgAccessor(req.Args))
	if err != nil {
		code := store.ErrorCode(err)
		return Response{Code: code, Error: err.Error(), Exit: errorExit(code)}
	}
	warnings := []string(nil)
	verdict := ""
	if wrapped, ok := data.(handlerData); ok {
		data, warnings, verdict = wrapped.Data, wrapped.Warnings, wrapped.Verdict
	}
	if output, ok := data.(outputReadData); ok {
		if output.Err != nil {
			code := store.ErrorCode(output.Err)
			return Response{OK: false, Code: code, Data: output.Chunk, Error: output.Err.Error(), Exit: store.ExitForCode(code)}
		}
		data = output.Chunk
	}
	if record, ok := runRecord(data); ok {
		if code := runRecordCode(record); code != "" {
			return Response{OK: false, Code: code, Data: data, Error: code, Exit: store.ExitForCode(code)}
		}
	}
	if report, ok := data.(store.CheckReport); ok {
		code := strings.ToUpper(report.Verdict)
		if report.Unevaluated {
			code = "UNEVALUATED"
		}
		return Response{OK: true, Code: code, Data: report, Warnings: warnings, Exit: exitCode(report)}
	}
	if verdict != "" {
		return Response{OK: true, Code: strings.ToUpper(verdict), Data: data, Warnings: warnings, Exit: verdictExit(verdict)}
	}
	return Response{OK: true, Code: "OK", Data: data, Warnings: warnings}
}

func runRecord(data any) (runner.RunRecord, bool) {
	switch value := data.(type) {
	case runner.RunRecord:
		return value, true
	case *runner.RunRecord:
		if value != nil {
			return *value, true
		}
	}
	return runner.RunRecord{}, false
}

func runRecordCode(record runner.RunRecord) string {
	if len(record.ErrorCodes) > 0 {
		return record.ErrorCodes[0]
	}
	if record.Status == runner.StatusKilled {
		return "E_RUN_KILLED"
	}
	if record.Status == runner.StatusExited && record.ExitCode != nil && *record.ExitCode != 0 {
		return "E_RUN_FAILED"
	}
	if record.Status == runner.StatusLost {
		return "U_RUN_EXIT_UNKNOWN"
	}
	return ""
}

// copyExample deep-copies an example argv while preserving the nil-vs-empty
// distinction: a nil example means "no example declared" (a generator error for
// an included action), whereas a non-nil but empty example is a deliberate,
// valid example for an argument-less verb such as `check` (`aira check`). A
// plain append([]string(nil), ...) would collapse an empty slice back to nil
// and lose that distinction.
func copyExample(example []string) []string {
	if example == nil {
		return nil
	}
	return append([]string{}, example...)
}

// DispatchDescriptors returns a stable, detached view of the exact dispatch
// table used by Do. Callers cannot mutate the table through this projection.
func (c *Core) DispatchDescriptors() []DispatchDescriptor {
	result := make([]DispatchDescriptor, 0, len(c.verbs))
	for _, spec := range c.verbs {
		args := make([]ArgSpec, len(spec.Args))
		for i, arg := range spec.Args {
			args[i] = arg
			args[i].Enum = append([]string(nil), arg.Enum...)
		}
		operations := make([]OperationSpec, len(spec.Operations))
		for i, operation := range spec.Operations {
			operations[i] = operation
			operations[i].Args = append([]OperationArg(nil), operation.Args...)
			operations[i].Example = copyExample(operation.Example)
		}
		result = append(result, DispatchDescriptor{
			Name: spec.Name, Usage: spec.Usage, Args: args, MCPTool: spec.MCPTool,
			MCPOperation: spec.MCPOperation, Summary: spec.Summary, Safety: spec.Safety,
			Include: spec.Include, Example: copyExample(spec.Example), Operations: operations,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (c *Core) dispatchTable() map[string]verbSpec {
	verbs := map[string]verbSpec{
		"help": {Name: "help", Usage: "help", Run: func(_ context.Context, _ *argAccessor) (any, error) { return c.Help(), nil }},
		"init": {Name: "init", Usage: "init", Args: []ArgSpec{stringSpec("project", false, false, "Project slug"), listSpec("prefixes", false, false, "ID prefixes")}, MCPTool: "aira_init", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			if c.initializer == nil {
				return nil, fmt.Errorf("E_CONFIG_INVALID: init is unavailable without a project initializer")
			}
			return c.initializer(ctx, map[string]any{"project": stringArg(args, "project"), "prefixes": stringSlice(args, "prefixes")})
		}},
		"id": {Name: "id", Usage: "id <prefix>", Args: []ArgSpec{stringSpec("prefix", true, true, "ID prefix")}, MCPTool: "aira_id", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			prefix := stringArg(args, "prefix")
			id, err := c.store.AllocateID(ctx, prefix)
			return map[string]any{"id": id}, err
		}},
		"create": {Name: "create", Usage: "create <title> [--kind K --severity S --label L --body B]", Args: []ArgSpec{stringSpec("title", true, true, "Ticket title"), stringSpec("kind", false, false, "Ticket kind", "feature", "bug", "chore", "spike", "requirement-work"), stringSpec("severity", false, false, "Ticket severity", "P0", "P1", "P2"), stringSpec("body", false, false, "Ticket body"), listSpec("labels", false, false, "Ticket labels")}, MCPTool: "aira_create", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			input := domain.CreateTicketInput{Title: stringArg(args, "title"), Body: stringArg(args, "body"), Kind: domain.Kind(stringArg(args, "kind")), Severity: domain.Severity(stringArg(args, "severity")), Labels: stringSlice(args, "labels")}
			ticket, event, err := c.store.CreateTicketWithEvent(ctx, input)
			if err != nil {
				return nil, err
			}
			return mutationData(map[string]any{"id": ticket.ID, "path": ".aira/tickets/" + ticket.ID + ".md", "ticket": ticket}, event), nil
		}},
		"show": {Name: "show", Usage: "show <selector>", Args: []ArgSpec{stringSpec("selector", true, true, "Exact ticket selector"), listSpec("fields", false, false, "Optional projected fields")}, MCPTool: "aira_get", Run: func(_ context.Context, args *argAccessor) (any, error) {
			record, err := c.store.Get(stringArg(args, "selector"))
			if err != nil {
				return nil, err
			}
			fields := stringSlice(args, "fields")
			projected := projectRecord(record, fields)
			if len(fields) == 0 {
				projected["body"] = record.Body
			}
			return handlerData{Data: projected, Warnings: record.Warnings}, nil
		}},
		"review": {Name: "review", Usage: "review <selector> [--paths a,b]", Args: []ArgSpec{stringSpec("selector", true, true, "Exact ticket selector"), listSpec("paths", false, false, "Optional paths under review"), listSpec("fields", false, false, "Optional projected fields")}, MCPTool: "aira_review", Run: func(_ context.Context, args *argAccessor) (any, error) {
			selector := stringArg(args, "selector")
			pathsProvided := args.present("paths")
			var argumentPaths []string
			if pathsProvided {
				argumentPaths = stringSlice(args, "paths")
			}
			_ = stringSlice(args, "fields")
			rs, ok := c.store.(reviewStore)
			if !ok {
				return nil, errors.New("E_CONFIG_INVALID: review store is unavailable")
			}
			record, err := c.store.Get(selector)
			if err != nil {
				return nil, err
			}
			pathsSource := "area-hints"
			paths := argumentPaths
			if pathsProvided {
				pathsSource = "arg"
			} else {
				paths, err = rs.TicketAreaGlobs(record.Ticket.ID)
				if err != nil {
					return nil, err
				}
			}
			normalizedPaths := make([]string, 0, len(paths))
			for _, rawPath := range paths {
				path, normalizeErr := store.NormalizeAreaGlob(rawPath)
				if normalizeErr != nil {
					return nil, normalizeErr
				}
				normalizedPaths = append(normalizedPaths, path)
			}
			if len(normalizedPaths) == 0 && pathsSource == "area-hints" {
				pathsSource = "none"
			}
			recommendation, err := store.RecommendReviewTier(normalizedPaths, string(record.Ticket.Kind), string(record.Ticket.Severity), rs.ReviewPolicy())
			if err != nil {
				return nil, err
			}
			recommendation.PathsSource = pathsSource
			findings, findingsErr := c.store.ListFindings("ticket:" + record.Ticket.ID + " disposition:open")
			findingData := any(projectFindingRecords(findings, nil))
			if findingsErr != nil {
				findingData = map[string]any{"unevaluated": true, "code": "U_REVIEW_SECTION_UNEVALUATED"}
			}
			relations, relationsErr := c.store.Relations(record.Ticket.ID)
			relationData := any(relations)
			if relationsErr != nil {
				relationData = map[string]any{"unevaluated": true, "code": "U_REVIEW_SECTION_UNEVALUATED"}
			}
			routing := [4][]string{
				{"self-review"},
				{"codex"},
				{"codex", "fable-final"},
				{"codex", "fable-final", "additional-lineage"},
			}
			return map[string]any{
				"ticket": map[string]any{
					"id": record.Ticket.ID, "title": record.Ticket.Title, "kind": record.Ticket.Kind,
					"severity": record.Ticket.Severity, "status": record.Ticket.Status,
					"milestone": record.Ticket.Milestone, "labels": record.Ticket.Labels,
				},
				"paths":              map[string]any{"source": pathsSource, "values": normalizedPaths},
				"tier":               map[string]any{"recommended": recommendation.Tier, "basis": recommendation.Basis, "default_tier": *rs.ReviewPolicy().DefaultTier},
				"routing":            append([]string(nil), routing[recommendation.Tier]...),
				"findings":           findingData,
				"relations":          relationData,
				"report_instruction": `aira find add <id> --source codex --verdict confirmed|refuted|plausible --category <cat> --severity P0|P1|P2 --message "<...>" [--file path:line]`,
			}, nil
		}},
		"grep": {Name: "grep", Usage: "grep <query> [--kind ticket|finding --by kind --fields F,...]", Args: []ArgSpec{stringSpec("query", true, true, "Search query"), stringSpec("kind", false, false, "Result kind", "ticket", "finding"), stringSpec("by", false, false, "Distribution field", "kind"), listSpec("fields", false, false, "Optional projected fields")}, MCPTool: "aira_grep", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			rows, err := c.store.Search(ctx, stringArg(args, "query"), stringArg(args, "kind"))
			if err != nil {
				if store.ErrorCode(err) == "E_INDEX_UNEVALUATED" {
					return handlerData{Data: map[string]any{"total": 0, "rows": []map[string]any{}, "unevaluated": true}, Verdict: "unevaluated"}, nil
				}
				return nil, err
			}
			by := stringArg(args, "by")
			if by != "" && by != "kind" {
				return nil, fmt.Errorf("E_SELECTOR_INVALID: unsupported grep distribution field %q", by)
			}
			data := map[string]any{"total": len(rows), "rows": projectSearchResults(rows, stringSlice(args, "fields"))}
			if len(rows) > ListLimit {
				data["rows"] = projectSearchResults(rows[:ListLimit], stringSlice(args, "fields"))
				data["distribution"] = searchDistribution(rows)
				data["truncated"] = true
			}
			return handlerData{Data: data}, nil
		}},
		"import": {Name: "import", Usage: "import <file> [--strict]", Args: []ArgSpec{stringSpec("file", true, true, "Findings file"), boolSpec("strict", false, false, "Reject partial imports")}, MCPTool: "aira_import", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			summary, err := c.store.ImportFindingsFile(ctx, stringArg(args, "file"), boolArg(args, "strict"))
			if err != nil {
				return nil, err
			}
			// Partial import (some records skipped) is surfaced honestly: the
			// summary lists every skip and the verdict is fail so the nonzero
			// exit makes the caller notice the invalid records; a fully-clean
			// import passes.
			verdict := "pass"
			if len(summary.Skipped) > 0 {
				verdict = "fail"
			}
			return handlerData{Data: summary, Verdict: verdict}, nil
		}},
		"find": {Name: "find", Usage: "find add|ls|show|set ...", Args: []ArgSpec{stringSpec("subverb", true, true, "Finding operation", "add", "ls", "show", "set"), stringSpec("ticket", false, true, "Ticket ID"), stringSpec("category", false, false, "Finding category"), stringSpec("severity", false, false, "Finding severity", "P0", "P1", "P2"), stringSpec("verdict", false, false, "Finding verdict", "confirmed", "refuted", "plausible"), stringSpec("source", false, false, "Finding source"), stringSpec("message", false, false, "Finding message"), stringSpec("file", false, false, "Finding file"), stringSpec("line", false, false, "Finding line"), stringSpec("requirement", false, false, "Requirement ID"), stringSpec("query", false, false, "Finding query"), stringSpec("by", false, false, "Distribution field"), listSpec("fields", false, false, "Optional projected fields"), stringSpec("selector", false, true, "Finding selector"), stringSpec("disposition", false, false, "Finding disposition", "open", "fixed", "waived"), stringSpec("reason", false, false, "Waiver reason"), stringSpec("actor", false, false, "Waiver actor")}, MCPTool: "aira_finding", MCPOperation: "subverb", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			subverb := strings.ToLower(stringArg(args, "subverb"))
			switch subverb {
			case "add":
				finding, event, err := c.store.AddFinding(ctx, domain.ReviewFindingInput{
					TicketID: stringArg(args, "ticket"), Category: stringArg(args, "category"),
					Severity: domain.Severity(stringArg(args, "severity")), Verdict: domain.Verdict(stringArg(args, "verdict")),
					Source: stringArg(args, "source"), Message: stringArg(args, "message"), File: stringArg(args, "file"),
					Line: intArg(args, "line"), RequirementID: stringArg(args, "requirement"),
				})
				if err != nil {
					return nil, err
				}
				return mutationData(map[string]any{"id": finding.Key, "finding": finding}, event), nil
			case "ls", "list":
				rows, err := c.store.ListFindings(stringArg(args, "query"))
				if err != nil {
					return nil, err
				}
				by := stringArg(args, "by")
				if by == "" {
					by = "subtype"
				}
				if !store.ValidFindingDistributionField(by) {
					return nil, fmt.Errorf("E_SELECTOR_INVALID: unsupported finding distribution field %q", by)
				}
				data := map[string]any{"total": len(rows), "rows": projectFindingRecords(rows, stringSlice(args, "fields"))}
				if len(rows) > ListLimit {
					data["rows"] = projectFindingRecords(rows[:ListLimit], stringSlice(args, "fields"))
					data["distribution"] = findingDistribution(rows, by)
					data["truncated"] = true
				}
				return handlerData{Data: data, Warnings: findingRecordWarnings(rows)}, nil
			case "show":
				record, err := c.store.GetFinding(stringArg(args, "selector"))
				if err != nil {
					return nil, err
				}
				return handlerData{Data: projectFindingRecord(record, nil), Warnings: record.Warnings}, nil
			case "set":
				key := stringArg(args, "selector")
				disposition := domain.Disposition(stringArg(args, "disposition"))
				event, err := c.store.SetFinding(ctx, key, disposition, stringArg(args, "reason"), stringArg(args, "actor"))
				if err != nil {
					return nil, err
				}
				updated, err := c.store.GetFinding(key)
				if err != nil {
					return nil, err
				}
				return mutationData(projectFindingRecord(updated, nil), event), nil
			default:
				return nil, fmt.Errorf("E_UNKNOWN_VERB: unknown find sub-verb %q", subverb)
			}
		}},
		"req": {Name: "req", Usage: "req add|ls|show|set|import ...", Args: []ArgSpec{stringSpec("subverb", true, true, "Requirement operation", "add", "ls", "show", "set", "import"), stringSpec("text", false, true, "Requirement statement"), stringSpec("status", false, false, "Requirement status", "built", "partial", "designed", "planned", "boundary", "retired", "superseded"), listSpec("fields", false, false, "Optional projected fields"), stringSpec("selector", false, true, "Requirement selector"), stringSpec("file", false, true, "Requirements registry file")}, MCPTool: "aira_requirement", MCPOperation: "subverb", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			subverb := strings.ToLower(stringArg(args, "subverb"))
			switch subverb {
			case "add":
				requirement, event, err := c.store.AddRequirement(ctx, domain.RequirementInput{Text: stringArg(args, "text"), Status: domain.RequirementStatus(stringArg(args, "status"))})
				if err != nil {
					return nil, err
				}
				return mutationData(map[string]any{"id": requirement.ID, "requirement": requirement}, event), nil
			case "ls":
				rows, err := c.store.ListRequirements()
				if err != nil {
					return nil, err
				}
				fields := stringSlice(args, "fields")
				data := map[string]any{"total": len(rows), "rows": projectRequirementRecords(rows, fields)}
				if len(rows) > ListLimit {
					data["rows"] = projectRequirementRecords(rows[:ListLimit], fields)
					data["distribution"] = requirementDistribution(rows, "status")
					data["truncated"] = true
				}
				return handlerData{Data: data}, nil
			case "show":
				record, err := c.store.GetRequirement(stringArg(args, "selector"))
				if err != nil {
					return nil, err
				}
				return projectRequirementRecord(record, nil), nil
			case "set":
				selector := stringArg(args, "selector")
				event, err := c.store.SetRequirement(ctx, selector, domain.RequirementStatus(stringArg(args, "status")))
				if err != nil {
					return nil, err
				}
				updated, err := c.store.GetRequirement(selector)
				if err != nil {
					return nil, err
				}
				return mutationData(projectRequirementRecord(updated, nil), event), nil
			case "import":
				summary, err := c.store.ImportRequirements(ctx, stringArg(args, "file"))
				if err != nil {
					return nil, err
				}
				return summary, nil
			default:
				return nil, fmt.Errorf("E_UNKNOWN_VERB: unknown req sub-verb %q", subverb)
			}
		}},
		"claim": {Name: "claim", Usage: "claim <id> [--steal --actor NAME]", Args: []ArgSpec{stringSpec("selector", true, true, "Ticket selector"), boolSpec("steal", false, false, "Steal an expired lease"), stringSpec("actor", false, false, "Lease actor")}, MCPTool: "aira_claim", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			record, err := c.store.Get(stringArg(args, "selector"))
			if err != nil {
				return nil, err
			}
			claim, err := c.store.Claim(ctx, record.Ticket.ID, boolArg(args, "steal"), stringArg(args, "actor"))
			if err != nil {
				return nil, err
			}
			return claim, nil
		}},
		"release": {Name: "release", Usage: "release <id>", Args: []ArgSpec{stringSpec("selector", true, true, "Ticket selector"), stringSpec("token", false, false, "Lease token")}, MCPTool: "aira_release", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			record, err := c.store.Get(stringArg(args, "selector"))
			if err != nil {
				return nil, err
			}
			token := stringArg(args, "token")
			if token == "" {
				token, err = c.store.LeaseToken(record.Ticket.ID)
				if err != nil {
					return nil, err
				}
			}
			event, err := c.store.Release(ctx, record.Ticket.ID, token)
			return mutationData(map[string]any{"id": record.Ticket.ID, "released": true}, event), err
		}},
		"heartbeat": {Name: "heartbeat", Usage: "heartbeat <id>", Args: []ArgSpec{stringSpec("selector", true, true, "Ticket selector"), stringSpec("token", false, false, "Lease token")}, MCPTool: "aira_heartbeat", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			record, err := c.store.Get(stringArg(args, "selector"))
			if err != nil {
				return nil, err
			}
			token := stringArg(args, "token")
			if token == "" {
				token, err = c.store.LeaseToken(record.Ticket.ID)
				if err != nil {
					return nil, err
				}
			}
			lease, err := c.store.Heartbeat(ctx, record.Ticket.ID, token)
			return lease, err
		}},
		"touch": {Name: "touch", Usage: "touch <id> <glob...>", Args: []ArgSpec{stringSpec("selector", true, true, "Ticket selector"), stringSpec("token", false, false, "Lease token"), listSpec("globs", false, true, "Area globs")}, MCPTool: "aira_touch", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			record, err := c.store.Get(stringArg(args, "selector"))
			if err != nil {
				return nil, err
			}
			token := stringArg(args, "token")
			if token == "" {
				token, err = c.store.LeaseToken(record.Ticket.ID)
				if err != nil {
					return nil, err
				}
			}
			result, err := c.store.Touch(ctx, record.Ticket.ID, token, stringSlice(args, "globs"))
			if err != nil {
				return nil, err
			}
			warnings := make([]string, 0, len(result.Warnings))
			for _, warning := range result.Warnings {
				warnings = append(warnings, warning.Code)
			}
			return handlerData{Data: result, Warnings: warnings}, nil
		}},
		"link": {Name: "link", Usage: "link <from> <kind> <to> | link ls <id>", Args: []ArgSpec{boolSpec("list", false, false, "List relations"), stringSpec("selector", false, true, "Ticket selector"), stringSpec("from", false, true, "Source ticket"), stringSpec("kind", false, true, "Relation kind", "blocks", "blocked-by", "parent", "child", "relates", "duplicates", "duplicated-by", "supersedes", "superseded-by", "resolves", "resolved-by"), stringSpec("to", false, true, "Target ticket")}, MCPTool: "aira_link", MCPOperation: "link", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			if boolArg(args, "list") {
				return c.store.Relations(stringArg(args, "selector"))
			}
			from, err := c.store.Get(stringArg(args, "from"))
			if err != nil {
				return nil, err
			}
			to, err := c.store.Get(stringArg(args, "to"))
			if err != nil {
				return nil, err
			}
			event, err := c.store.Link(ctx, from.Ticket.ID, domain.RelationKind(stringArg(args, "kind")), to.Ticket.ID)
			if err != nil {
				return nil, err
			}
			return mutationData(map[string]any{"from": from.Ticket.ID, "kind": stringArg(args, "kind"), "to": to.Ticket.ID}, event), nil
		}},
		"unlink": {Name: "unlink", Usage: "unlink <from> <kind> <to>", Args: []ArgSpec{stringSpec("from", true, true, "Source ticket"), stringSpec("kind", true, true, "Relation kind", "blocks", "blocked-by", "parent", "child", "relates", "duplicates", "duplicated-by", "supersedes", "superseded-by", "resolves", "resolved-by"), stringSpec("to", true, true, "Target ticket")}, MCPTool: "aira_link", MCPOperation: "unlink", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			from, err := relationSelectorID(stringArg(args, "from"))
			if err != nil {
				return nil, err
			}
			to, err := relationSelectorID(stringArg(args, "to"))
			if err != nil {
				return nil, err
			}
			event, err := c.store.Unlink(ctx, from, domain.RelationKind(stringArg(args, "kind")), to)
			if err != nil {
				return nil, err
			}
			return mutationData(map[string]any{"from": from, "kind": stringArg(args, "kind"), "to": to}, event), nil
		}},
		"ready": {Name: "ready", Usage: "ready [<selector>] | ready --list", Args: []ArgSpec{stringSpec("selector", false, true, "Optional ticket selector")}, MCPTool: "aira_ready", Run: func(_ context.Context, args *argAccessor) (any, error) {
			selector := stringArg(args, "selector")
			rows, err := c.store.Ready(selector)
			if err != nil {
				return nil, err
			}
			if selector != "" {
				if len(rows) != 1 {
					return nil, fmt.Errorf("E_NOT_FOUND: ready selector matched no ticket")
				}
				return handlerData{Data: rows[0], Warnings: readyWarnings(rows), Verdict: readyVerdict(rows)}, nil
			}
			data := map[string]any{"total": len(rows), "rows": projectReadyRecords(rows)}
			if len(rows) > ListLimit {
				data["rows"] = projectReadyRecords(rows[:ListLimit])
				data["distribution"] = readyDistribution(rows)
				data["truncated"] = true
			}
			return handlerData{Data: data, Warnings: readyWarnings(rows), Verdict: readyVerdict(rows)}, nil
		}},
		"list": {Name: "list", Usage: "list [query] [--by F] [--fields F,...]", Args: []ArgSpec{stringSpec("query", false, true, "Ticket query"), stringSpec("by", false, false, "Distribution field"), listSpec("fields", false, false, "Optional projected fields")}, MCPTool: "aira_list", Run: func(_ context.Context, args *argAccessor) (any, error) {
			rows, err := c.store.List(stringArg(args, "query"))
			if err != nil {
				return nil, err
			}
			by := stringArg(args, "by")
			if by == "" {
				by = "status"
			}
			data := map[string]any{"total": len(rows), "rows": projectRecords(rows, stringSlice(args, "fields"))}
			if len(rows) > ListLimit {
				data["rows"] = projectRecords(rows[:ListLimit], stringSlice(args, "fields"))
				data["distribution"] = distribution(rows, by)
				data["truncated"] = true
			}
			return handlerData{Data: data, Warnings: recordWarnings(rows)}, nil
		}},
		"count": {Name: "count", Usage: "count [query] --by F", Args: []ArgSpec{stringSpec("query", false, true, "Ticket query"), stringSpec("by", true, false, "Count dimension")}, MCPTool: "aira_count", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			result, err := c.store.Count(stringArg(args, "query"), stringArg(args, "by"))
			return handlerData{Data: result, Warnings: result.Warnings}, err
		}},
		"set": {Name: "set", Usage: "set <selector> <field=value>", Args: []ArgSpec{stringSpec("selector", true, true, "Ticket selector"), stringSpec("field", true, true, "Ticket field"), stringSpec("value", true, true, "New field value")}, MCPTool: "aira_transition", MCPOperation: "set", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			record, err := c.store.Get(stringArg(args, "selector"))
			if err != nil {
				return nil, err
			}
			field, value := stringArg(args, "field"), stringArg(args, "value")
			if field == "" {
				return nil, fmt.Errorf("E_SELECTOR_INVALID: set requires field=value")
			}
			event, err := c.store.SetTicket(ctx, record.Ticket.ID, field, value)
			if err != nil {
				return nil, err
			}
			updated, err := c.store.Get(record.Ticket.ID)
			if err != nil {
				return nil, err
			}
			return mutationData(projectRecord(updated, nil), event), nil
		}},
		"mv": {Name: "mv", Usage: "mv <selector> <status>", Args: []ArgSpec{stringSpec("selector", true, true, "Ticket selector"), stringSpec("status", true, true, "Target status", "draft", "planned", "in-progress", "in-review", "done", "retired", "superseded")}, MCPTool: "aira_transition", MCPOperation: "mv", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			record, err := c.store.Get(stringArg(args, "selector"))
			if err != nil {
				return nil, err
			}
			event, err := c.store.MoveTicket(ctx, record.Ticket.ID, domain.Status(stringArg(args, "status")))
			if err != nil {
				return nil, err
			}
			return mutationData(map[string]any{"id": record.Ticket.ID, "status": stringArg(args, "status")}, event), nil
		}},
		"run": {Name: "run", Usage: "run [options] -- <argv...>", Args: []ArgSpec{
			listSpec("argv", true, true, "Exact target argv after the launch delimiter"),
			listSpec("prefix", false, false, "Optional exact launch-prefix argv"),
			stringSpec("cwd", false, false, "Launch working directory"),
			listSpec("env", false, false, "Exact KEY=VALUE environment overrides"),
			boolSpec("merge", false, false, "Capture stdout and stderr as one kernel stream"),
			stringSpec("stdin", false, false, "Launch-time stdin file or -"),
			boolSpec("store_stdin", false, false, "Persist supplied launch stdin"),
		}, MCPTool: "aira_run", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			request := runner.Request{
				Argv: stringSlice(args, "argv"), Cwd: stringArg(args, "cwd"), Env: stringSlice(args, "env"),
				Prefix: stringSlice(args, "prefix"), Merge: boolArg(args, "merge"), StdinPath: stringArg(args, "stdin"),
				StoreStdin: boolArg(args, "store_stdin"),
			}
			if request.StdinPath == "-" {
				request.Stdin = c.stdin
			}
			if c.runner == nil {
				return nil, runnerError("E_RUN_SCOPE_UNAVAILABLE", errors.New("runner is unavailable"))
			}
			stdinPath := request.StdinPath
			if stdinPath == "-" {
				if c.stdin == nil {
					return nil, runnerError("E_RUN_STDIN_INVALID", errors.New("stdin '-' is unavailable on this face"))
				}
			}
			record, err := c.runner.Launch(ctx, request)
			if err != nil {
				return nil, err
			}
			return record, nil
		}},
		"run-kill": {Name: "run-kill", Usage: "run-kill <run-id>", Args: []ArgSpec{stringSpec("run_id", true, true, "Run identifier")}, MCPTool: "aira_run_kill", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			runID := stringArg(args, "run_id")
			if c.runner == nil {
				return nil, runnerError("E_RUN_SCOPE_UNAVAILABLE", errors.New("runner is unavailable"))
			}
			return c.runner.Kill(ctx, runID)
		}},
		"run-log": {Name: "run-log", Usage: "run-log <run-id> [--stream out|err|merged] [--follow --from N --tail N --full]", Args: []ArgSpec{
			stringSpec("run_id", true, true, "Run identifier"),
			stringSpec("stream", false, false, "Captured stream", "out", "err", "merged"),
			boolSpec("follow", false, false, "Observe until terminal"),
			stringSpec("from", false, false, "Byte offset"),
			stringSpec("tail", false, false, "Number of bytes from the end"),
			boolSpec("full", false, false, "Opt into the complete selected output"),
		}, MCPTool: "aira_run_output", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			runID := stringArg(args, "run_id")
			stream := stringArg(args, "stream")
			follow := boolArg(args, "follow")
			full := boolArg(args, "full")
			from, err := nonNegativeInt(args, "from")
			tail, tailErr := nonNegativeInt(args, "tail")
			if err != nil {
				return nil, err
			}
			if tailErr != nil {
				return nil, tailErr
			}
			if c.runner == nil {
				return nil, runnerError("E_RUN_SCOPE_UNAVAILABLE", errors.New("runner is unavailable"))
			}
			chunk, readErr := c.runner.ReadOutput(ctx, runner.OutputRequest{
				RunID: runID, Stream: stream, From: from,
				Tail: tail, Full: full, Follow: follow, MaxBytes: c.outputCap,
			})
			return outputReadData{Chunk: chunk, Err: readErr}, nil
		}},
		"reconcile": {Name: "reconcile", Usage: "reconcile [--rebuild]", Args: []ArgSpec{boolSpec("rebuild", false, false, "Rebuild derived indexes")}, MCPTool: "aira_reconcile", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			if err := c.store.Reconcile(ctx); err != nil {
				return nil, err
			}
			if boolArg(args, "rebuild") {
				if err := c.store.Rebuild(ctx); err != nil {
					return nil, err
				}
			}
			data := map[string]any{"reconciled": true}
			if c.runner != nil {
				runs, err := c.runner.Reconcile(ctx)
				if err != nil {
					return nil, err
				}
				data["runs"] = runs
			}
			return data, nil
		}},
		"check": {Name: "check", Usage: "check", Args: []ArgSpec{}, MCPTool: "aira_check", Run: func(ctx context.Context, _ *argAccessor) (any, error) {
			report, err := c.store.Check(ctx)
			if err != nil {
				return nil, err
			}
			if c.runner != nil {
				runs, reconcileErr := c.runner.Reconcile(ctx)
				if reconcileErr != nil {
					return nil, reconcileErr
				}
				for _, run := range runs {
					if run.Status == runner.StatusStarting || run.Status == runner.StatusRunning {
						report.Warnings = append(report.Warnings, store.CheckFinding{Code: "U_RUN_RECONCILE_REQUIRED", Subject: run.ID, Message: "live run scope is orphaned and remains explicitly killable", Kind: "warning"})
					}
				}
			}
			return report, nil
		}},
		"gate": {Name: "gate", Usage: "gate <operation> [args]", Args: []ArgSpec{
			stringSpec("subverb", true, true, "Gate operation", "add", "ls", "show", "set", "run", "check", "attest", "prove", "review", "canary-run", "canary-show"),
			stringSpec("gate_id", false, true, "Gate identifier"), stringSpec("canary_id", false, true, "Canary identifier"), stringSpec("verdict", false, false, "Attestation verdict", "pass", "fail"), stringSpec("actor", false, false, "Attestation actor"),
			stringSpec("checker", false, false, "Gate checker", "check-dimension", "command", "manual-attestation"), stringSpec("predicate", false, false, "Command predicate", "exit-zero", "tests-green"),
			listSpec("argv", false, false, "Exact command argv tokens"), stringSpec("cwd", false, false, "Command root or relative subdirectory"), listSpec("env_allow", false, false, "Allow-listed environment names"),
			stringSpec("timeout_ms", false, false, "Command timeout in milliseconds"), stringSpec("output_cap_bytes", false, false, "Combined output cap in bytes"), stringSpec("parser", false, false, "Command output parser", "go-test-json-v1"),
			stringSpec("mutation_kind", false, false, "Typed mutation kind", "go-negate-assertion", "go-inject-failing-test"), stringSpec("mutation_file", false, false, "Mutation target file"), stringSpec("mutation_test", false, false, "Mutation target test"),
			stringSpec("mutation_occurrence", false, false, "Mutation assertion occurrence"), stringSpec("mutation_pkgdir", false, false, "Mutation package directory"), stringSpec("mutation_testname", false, false, "Injected mutation test name"), stringSpec("mutation_seed", false, false, "Mutation numeric seed"), stringSpec("mutation_expected_result", false, false, "Mutation expected result", "fail"),
		}, MCPTool: "aira_gate", MCPOperation: "subverb", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			subverb := strings.ToLower(stringArg(args, "subverb"))
			switch subverb {
			case "show", "add", "set", "run", "attest", "prove", "review":
				_ = stringArg(args, "gate_id")
				if subverb == "attest" {
					_ = stringArg(args, "verdict")
					_ = stringArg(args, "actor")
				}
			case "canary-run", "canary-show":
				_ = stringArg(args, "canary_id")
			}
			inputFields := map[string]any{}
			switch subverb {
			case "add", "set":
				inputFields = gateDefinitionInputFields(args)
			case "canary-run", "canary-show":
				inputFields = mutationInputFields(args)
			}
			gs, ok := c.store.(gateStore)
			if !ok {
				return nil, fmt.Errorf("E_CONFIG_INVALID: gate store is unavailable")
			}
			switch subverb {
			case "ls":
				return gs.ListGates()
			case "show":
				id := stringArg(args, "gate_id")
				gates, err := gs.ListGates()
				if err != nil {
					return nil, err
				}
				for _, item := range gates {
					if item.ID == id {
						return item, nil
					}
				}
				return nil, fmt.Errorf("E_NOT_FOUND: gate %s", id)
			case "run":
				id := stringArg(args, "gate_id")
				if id == "" {
					return nil, errors.New("E_GATE_INVALID: gate run requires a gate id")
				}
				result, err := gs.RunGate(ctx, id)
				if err != nil {
					return nil, err
				}
				return handlerData{Data: result, Verdict: result.Verdict}, nil
			case "check":
				report, err := gs.GateCheck(ctx)
				if err != nil {
					return nil, err
				}
				return handlerData{Data: report, Verdict: report.Verdict}, nil
			case "attest":
				as, ok := c.store.(gateAttestationStore)
				if !ok {
					return nil, errors.New("E_GATE_ATTESTATION_INVALID: attestation is unavailable")
				}
				result, err := as.AttestGate(ctx, stringArg(args, "gate_id"), stringArg(args, "verdict"), stringArg(args, "actor"))
				if err != nil {
					return nil, err
				}
				return handlerData{Data: result, Verdict: result.Verdict}, nil
			case "add", "set", "prove", "review", "canary-run", "canary-show":
				actionStore, ok := c.store.(gateActionStore)
				if !ok {
					return nil, errors.New("E_GATE_INVALID: gate action is unavailable")
				}
				if inputStore, ok := c.store.(gateActionInputStore); ok {
					return inputStore.GateActionWithFields(ctx, subverb, stringArg(args, "gate_id"), stringArg(args, "canary_id"), inputFields)
				}
				return actionStore.GateAction(ctx, subverb, stringArg(args, "gate_id"), stringArg(args, "canary_id"))
			default:
				return nil, fmt.Errorf("E_GATE_INVALID: unknown gate operation %q", subverb)
			}
		}},
	}
	applyDispatchMetadata(verbs)
	return verbs
}

func applyDispatchMetadata(verbs map[string]verbSpec) {
	metadata := map[string]verbMetadata{
		"init":      {summary: "Initialise AIRA for the current project", safety: SafetyReconcile, example: []string{"--project", "demo", "--prefix", "AIRA"}},
		"id":        {summary: "Allocate the next ticket identifier", safety: SafetyMutate, example: []string{"AIRA"}},
		"create":    {summary: "Create a ticket", safety: SafetyMutate, example: []string{"AIRA ticket", "--kind", "feature", "--severity", "P1", "--body", "body", "--label", "label"}},
		"show":      {summary: "Show one ticket", safety: SafetyRead, example: []string{"AIRA-1", "--fields", "id"}},
		"review":    {summary: "Assemble a review briefing", safety: SafetyRead, example: []string{"AIRA-1", "--paths", "internal/store/gate.go,docs/x.md"}},
		"grep":      {summary: "Search indexed tickets and findings", safety: SafetyRead, example: []string{"ticket", "--kind", "ticket", "--by", "kind", "--fields", "id"}},
		"import":    {summary: "Import findings from a JSONL file", safety: SafetyMutate, example: []string{"findings.jsonl", "--strict"}},
		"claim":     {summary: "Claim a ticket lease", safety: SafetyLease, example: []string{"AIRA-1", "--steal", "--actor", "codex"}},
		"release":   {summary: "Release a ticket lease", safety: SafetyLease, example: []string{"AIRA-1", "--token", "token"}},
		"heartbeat": {summary: "Renew a ticket lease", safety: SafetyLease, example: []string{"AIRA-1", "--token", "token"}},
		"touch":     {summary: "Record ticket area ownership", safety: SafetyMutate, example: []string{"AIRA-1", "**/*.go", "--token", "token"}},
		"unlink":    {summary: "Remove a ticket relation", safety: SafetyMutate, example: []string{"AIRA-1", "blocks", "AIRA-2"}},
		"ready":     {summary: "List tickets ready to work on", safety: SafetyRead, example: []string{"--list"}},
		"list":      {summary: "List tickets", safety: SafetyRead, example: []string{"kind:feature", "--by", "status", "--fields", "id"}},
		"count":     {summary: "Count tickets by a dimension", safety: SafetyRead, example: []string{"kind:feature", "--by", "status"}},
		"set":       {summary: "Set a ticket field", safety: SafetyMutate, example: []string{"AIRA-1", "status=planned"}},
		"mv":        {summary: "Move a ticket to a new status", safety: SafetyMutate, example: []string{"AIRA-1", "planned"}},
		"run":       {summary: "Launch a foreground subprocess in an owned scope", safety: SafetyExecute, example: []string{"--merge", "--", "printf", "hello"}},
		"run-kill":  {summary: "Kill an owned run scope", safety: SafetyExecute, example: []string{"RUN-1"}},
		"run-log":   {summary: "Read captured run output", safety: SafetyRead, example: []string{"RUN-1", "--stream", "out"}},
		"reconcile": {summary: "Reconcile derived project state", safety: SafetyReconcile, example: []string{"--rebuild"}},
		"check":     {summary: "Check project consistency", safety: SafetyReconcile, example: []string{}},
		"gate": {summary: "Manage proof-backed gates", safety: SafetyRead, operations: []OperationSpec{
			{Name: "add", Summary: "Add a gate definition", Safety: SafetyMutate, Args: gateDefinitionOperationArgs(), Example: []string{"add", "unit-tests", "--checker", "command", "--predicate", "tests-green", "--argv", "/usr/local/bin/go", "--argv", "test", "--argv", "-json", "--argv", "./...", "--cwd", "root", "--env-allow", "PATH", "--timeout-ms", "60000", "--output-cap-bytes", "8388608", "--parser", "go-test-json-v1"}},
			{Name: "ls", Summary: "List gate definitions", Safety: SafetyRead, Args: nil, Example: []string{"ls"}},
			{Name: "show", Summary: "Show a gate definition", Safety: SafetyRead, Args: []OperationArg{{Name: "gate_id", Required: true}}, Example: []string{"show", "traceability"}},
			{Name: "set", Summary: "Set gate policy", Safety: SafetyMutate, Args: gateDefinitionOperationArgs(), Example: []string{"set", "unit-tests", "--timeout-ms", "60000"}},
			{Name: "run", Summary: "Evaluate a gate", Safety: SafetyReconcile, Args: []OperationArg{{Name: "gate_id", Required: true}}, Example: []string{"run", "traceability"}},
			{Name: "check", Summary: "Read the latest gate result", Safety: SafetyRead, Args: nil, Example: []string{"check"}},
			{Name: "attest", Summary: "Answer a manual gate challenge", Safety: SafetyMutate, Args: []OperationArg{{Name: "gate_id", Required: true}, {Name: "verdict", Required: true}, {Name: "actor", Required: true}}, Example: []string{"attest", "review", "--verdict", "pass", "--actor", "human"}},
			{Name: "prove", Summary: "Record proof of fire", Safety: SafetyMutate, Args: []OperationArg{{Name: "gate_id", Required: true}}, Example: []string{"prove", "traceability"}},
			{Name: "review", Summary: "Request manual gate review", Safety: SafetyRead, Args: []OperationArg{{Name: "gate_id", Required: true}}, Example: []string{"review", "review"}},
			{Name: "canary-run", Summary: "Run a named canary", Safety: SafetyReconcile, Args: append([]OperationArg{{Name: "canary_id", Required: true}}, mutationOperationArgs()...), Example: []string{"canary-run", "unit-tests-mutation", "--mutation-kind", "go-inject-failing-test", "--mutation-pkgdir", ".", "--mutation-testname", "TestInjected"}},
			{Name: "canary-show", Summary: "Show a canary declaration", Safety: SafetyRead, Args: append([]OperationArg{{Name: "canary_id", Required: true}}, mutationOperationArgs()...), Example: []string{"canary-show", "unit-tests-mutation"}},
		}},
	}
	metadata["find"] = verbMetadata{summary: "Manage review findings", safety: SafetyMutate, operations: []OperationSpec{
		{Name: "add", Summary: "Add a review finding", Safety: SafetyMutate, Args: []OperationArg{
			{Name: "ticket", Required: true}, {Name: "category"}, {Name: "severity"}, {Name: "verdict"}, {Name: "source"}, {Name: "message"},
			{Name: "file"}, {Name: "line"}, {Name: "requirement"},
		}, Example: []string{"add", "AIRA-1", "--category", "bug", "--severity", "P1", "--verdict", "confirmed", "--source", "codex", "--message", "bad", "--file", "x.go:12", "--requirement", "REQ-1"}},
		{Name: "ls", Summary: "List review findings", Safety: SafetyRead, Args: []OperationArg{{Name: "query"}, {Name: "by"}, {Name: "fields"}}, Example: []string{"ls", "subtype:any", "--by", "source", "--fields", "id"}},
		{Name: "show", Summary: "Show one review finding", Safety: SafetyRead, Args: []OperationArg{{Name: "selector", Required: true}}, Example: []string{"show", "f-1"}},
		{Name: "set", Summary: "Set a review finding disposition", Safety: SafetyMutate, Args: []OperationArg{{Name: "selector", Required: true}, {Name: "disposition"}, {Name: "reason"}, {Name: "actor"}}, Example: []string{"set", "f-1", "--disposition", "waived", "--reason", "accepted", "--actor", "human"}},
	}}
	metadata["req"] = verbMetadata{summary: "Manage requirements", safety: SafetyMutate, operations: []OperationSpec{
		{Name: "add", Summary: "Add a requirement", Safety: SafetyMutate, Args: []OperationArg{{Name: "text", Required: true}, {Name: "status"}}, Example: []string{"add", "The system must remain correct.", "--status", "planned"}},
		{Name: "ls", Summary: "List requirements", Safety: SafetyRead, Args: []OperationArg{{Name: "fields"}}, Example: []string{"ls", "--fields", "id"}},
		{Name: "show", Summary: "Show one requirement", Safety: SafetyRead, Args: []OperationArg{{Name: "selector", Required: true}}, Example: []string{"show", "AR-1"}},
		{Name: "set", Summary: "Set a requirement status", Safety: SafetyMutate, Args: []OperationArg{{Name: "selector", Required: true}, {Name: "status", Required: true}}, Example: []string{"set", "AR-1", "--status", "built"}},
		{Name: "import", Summary: "Import a requirements registry preserving IDs", Safety: SafetyMutate, Args: []OperationArg{{Name: "file", Required: true}}, Example: []string{"import", "REQUIREMENTS.md"}},
	}}
	metadata["link"] = verbMetadata{summary: "Manage ticket relations", safety: SafetyMutate, operations: []OperationSpec{
		{Name: "link", Summary: "Create a ticket relation", Safety: SafetyMutate, Args: []OperationArg{{Name: "from", Required: true}, {Name: "kind", Required: true}, {Name: "to", Required: true}}, Example: []string{"AIRA-1", "blocks", "AIRA-2"}},
		{Name: "list", Summary: "List ticket relations", Safety: SafetyRead, Args: []OperationArg{{Name: "selector", Required: true}}, Example: []string{"ls", "AIRA-1"}},
	}}
	for name, spec := range verbs {
		if name == "help" {
			continue
		}
		entry, ok := metadata[name]
		if !ok {
			panic("missing dispatch metadata for " + name)
		}
		spec.Summary, spec.Safety, spec.Include = entry.summary, entry.safety, true
		spec.Example = copyExample(entry.example)
		spec.Operations = append([]OperationSpec(nil), entry.operations...)
		verbs[name] = spec
	}
}

type verbMetadata struct {
	summary    string
	safety     SafetyClass
	example    []string
	operations []OperationSpec
}

func (c *Core) Help() []map[string]string {
	result := make([]map[string]string, 0, len(c.verbs))
	for _, spec := range c.verbs {
		result = append(result, map[string]string{"verb": spec.Name, "usage": spec.Usage})
	}
	sort.Slice(result, func(i, j int) bool { return result[i]["verb"] < result[j]["verb"] })
	return result
}

func stringArg(args *argAccessor, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args.record(key).(string)
	return value
}

func stringSpec(name string, required, positional bool, description string, enum ...string) ArgSpec {
	return ArgSpec{Name: name, Kind: ArgKindString, Required: required, Positional: positional, Enum: append([]string(nil), enum...), Description: description}
}

func boolSpec(name string, required, positional bool, description string) ArgSpec {
	return ArgSpec{Name: name, Kind: ArgKindBool, Required: required, Positional: positional, Description: description}
}

func listSpec(name string, required, positional bool, description string) ArgSpec {
	return ArgSpec{Name: name, Kind: ArgKindStringList, Required: required, Positional: positional, Description: description}
}

func intArg(args *argAccessor, key string) int {
	if args == nil {
		return 0
	}
	value := args.record(key)
	if value, ok := value.(int); ok {
		return value
	}
	if value, ok := value.(float64); ok {
		return int(value)
	}
	if value, ok := value.(string); ok {
		parsed, _ := strconv.Atoi(value)
		return parsed
	}
	return 0
}

func relationSelectorID(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("E_SELECTOR_INVALID: relation selector is empty")
	}
	id, err := store.ParseSelector(raw)
	if err != nil {
		return "", err
	}
	if err := domain.ValidateID(id); err != nil {
		return "", errors.New("E_SELECTOR_INVALID: relation selector must be an exact ID or file anchor")
	}
	return id, nil
}

func boolArg(args *argAccessor, key string) bool {
	if args == nil {
		return false
	}
	value, _ := args.record(key).(bool)
	return value
}

func stringSlice(args *argAccessor, key string) []string {
	if args == nil {
		return nil
	}
	value := args.record(key)
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if item, ok := value.(string); ok {
				result = append(result, item)
			}
		}
		return result
	case string:
		if values == "" {
			return nil
		}
		parts := strings.Split(values, ",")
		return parts
	default:
		return nil
	}
}

func gateDefinitionInputFields(args *argAccessor) map[string]any {
	fields := map[string]any{}
	for _, name := range []string{"checker", "predicate", "cwd", "timeout_ms", "output_cap_bytes", "parser", "mutation_kind", "mutation_file", "mutation_test", "mutation_occurrence", "mutation_pkgdir", "mutation_testname", "mutation_seed", "mutation_expected_result"} {
		if value := stringArg(args, name); value != "" {
			fields[name] = value
		}
	}
	if values := stringSlice(args, "argv"); len(values) > 0 {
		fields["argv"] = values
	}
	if values := stringSlice(args, "env_allow"); len(values) > 0 {
		fields["env_allow"] = values
	}
	return fields
}

func mutationInputFields(args *argAccessor) map[string]any {
	fields := map[string]any{}
	for _, name := range []string{"mutation_kind", "mutation_file", "mutation_test", "mutation_occurrence", "mutation_pkgdir", "mutation_testname", "mutation_seed", "mutation_expected_result"} {
		if value := stringArg(args, name); value != "" {
			fields[name] = value
		}
	}
	return fields
}

func gateDefinitionOperationArgs() []OperationArg {
	names := []string{"gate_id", "checker", "predicate", "argv", "cwd", "env_allow", "timeout_ms", "output_cap_bytes", "parser", "mutation_kind", "mutation_file", "mutation_test", "mutation_occurrence", "mutation_pkgdir", "mutation_testname", "mutation_seed", "mutation_expected_result"}
	args := make([]OperationArg, 0, len(names))
	for _, name := range names {
		args = append(args, OperationArg{Name: name, Required: name == "gate_id"})
	}
	return args
}

func mutationOperationArgs() []OperationArg {
	names := []string{"mutation_kind", "mutation_file", "mutation_test", "mutation_occurrence", "mutation_pkgdir", "mutation_testname", "mutation_seed", "mutation_expected_result"}
	args := make([]OperationArg, 0, len(names))
	for _, name := range names {
		args = append(args, OperationArg{Name: name})
	}
	return args
}

func projectRecords(records []store.TicketRecord, fields []string) []map[string]any {
	result := make([]map[string]any, 0, len(records))
	for _, record := range records {
		result = append(result, projectRecord(record, fields))
	}
	return result
}

func projectFindingRecords(records []store.FindingRecord, fields []string) []map[string]any {
	result := make([]map[string]any, 0, len(records))
	for _, record := range records {
		result = append(result, projectFindingRecord(record, fields))
	}
	return result
}

func projectRequirementRecords(records []store.RequirementRecord, fields []string) []map[string]any {
	result := make([]map[string]any, 0, len(records))
	for _, record := range records {
		result = append(result, projectRequirementRecord(record, fields))
	}
	return result
}

func projectSearchResults(records []store.SearchResult, fields []string) []map[string]any {
	result := make([]map[string]any, 0, len(records))
	for _, record := range records {
		all := map[string]any{"kind": record.Kind, "id": record.ID, "snippet": record.Snippet, "rank": record.Rank}
		if len(fields) == 0 {
			result = append(result, all)
			continue
		}
		projected := make(map[string]any, len(fields))
		for _, field := range fields {
			field = strings.TrimSpace(field)
			if value, ok := all[field]; ok {
				projected[field] = value
			}
		}
		result = append(result, projected)
	}
	return result
}

func searchDistribution(records []store.SearchResult) map[string]int {
	result := map[string]int{}
	for _, record := range records {
		result[record.Kind]++
	}
	return result
}

func projectFindingRecord(record store.FindingRecord, fields []string) map[string]any {
	finding := record.Finding
	all := map[string]any{
		"id": finding.Key, "subtype": finding.Subtype, "ticket": finding.TicketID, "category": finding.Category,
		"severity": finding.Severity, "verdict": finding.Verdict, "source": finding.Source,
		"message": finding.Message, "requirement": finding.RequirementID, "file": finding.File, "line": finding.Line,
		"disposition": finding.Disposition, "waiver_reason": finding.WaiverReason, "waiver_actor": finding.WaiverActor,
		"code": finding.Code, "subject": finding.Subject, "details": finding.Details, "path": record.Path,
		"worktree_id": record.WorktreeID,
	}
	if record.Unevaluated {
		all["unevaluated"] = true
		all["error"] = record.Error
	}
	if len(fields) == 0 {
		return all
	}
	result := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, ok := all[strings.TrimSpace(field)]; ok {
			result[strings.TrimSpace(field)] = value
		}
	}
	return result
}

func projectRequirementRecord(record store.RequirementRecord, fields []string) map[string]any {
	requirement := record.Requirement
	all := map[string]any{"id": requirement.ID, "status": requirement.Status, "text": requirement.Text, "path": record.Path}
	if len(fields) == 0 {
		return all
	}
	result := make(map[string]any, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if value, ok := all[field]; ok {
			result[field] = value
		}
	}
	return result
}

func findingDistribution(records []store.FindingRecord, by string) map[string]int {
	result := map[string]int{}
	for _, record := range records {
		for _, value := range storeFindingDistributionValues(record.Finding, by) {
			result[value]++
		}
	}
	return result
}

func requirementDistribution(records []store.RequirementRecord, by string) map[string]int {
	result := map[string]int{}
	if by != "status" {
		return result
	}
	for _, record := range records {
		result[string(record.Requirement.Status)]++
	}
	return result
}

func storeFindingDistributionValues(finding domain.Finding, by string) []string {
	value := ""
	switch by {
	case "subtype":
		value = string(finding.Subtype)
	case "category":
		value = finding.Category
	case "source":
		value = finding.Source
	case "verdict":
		value = string(finding.Verdict)
	case "disposition":
		value = string(finding.Disposition)
	case "severity":
		value = string(finding.Severity)
	case "ticket":
		value = finding.TicketID
	}
	if value == "" {
		value = "(none)"
	}
	return []string{value}
}

func findingRecordWarnings(records []store.FindingRecord) []string {
	seen := map[string]bool{}
	var result []string
	for _, record := range records {
		for _, warning := range record.Warnings {
			if !seen[warning] {
				seen[warning] = true
				result = append(result, warning)
			}
		}
	}
	return result
}

func projectRecord(record store.TicketRecord, fields []string) map[string]any {
	all := map[string]any{
		"id": record.Ticket.ID, "project": record.Ticket.Project, "title": record.Ticket.Title,
		"status": record.Ticket.Status, "kind": record.Ticket.Kind, "severity": record.Ticket.Severity,
		"assignee": record.Ticket.Assignee, "milestone": record.Ticket.Milestone, "labels": record.Ticket.Labels,
		"hold": record.Ticket.Hold, "relations": record.Ticket.Relations, "body": record.Body, "path": record.Path,
	}
	if record.Relations != nil {
		all["relations"] = record.Relations
	}
	if len(fields) == 0 {
		delete(all, "body")
		return all
	}
	result := make(map[string]any, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if value, ok := all[field]; ok {
			result[field] = value
		}
	}
	return result
}

func projectReadyRecord(record store.ReadyRecord) map[string]any {
	if record.Ticket.Ticket.ID == "" {
		return map[string]any{"path": record.Ticket.Path, "ready": record.Ready, "blockers": record.Blockers, "verdict": record.Verdict, "findings": record.Findings}
	}
	result := projectRecord(record.Ticket, nil)
	result["ready"] = record.Ready
	result["blockers"] = record.Blockers
	result["verdict"] = record.Verdict
	if len(record.Findings) > 0 {
		result["findings"] = record.Findings
	}
	return result
}

func readyWarnings(rows []store.ReadyRecord) []string {
	var warnings []string
	for _, row := range rows {
		for _, finding := range row.Findings {
			seen := false
			for _, warning := range warnings {
				if warning == finding.Code {
					seen = true
					break
				}
			}
			if !seen {
				warnings = append(warnings, finding.Code)
			}
		}
	}
	return warnings
}

func readyVerdict(rows []store.ReadyRecord) string {
	for _, row := range rows {
		if row.Verdict == "fail" {
			return "fail"
		}
	}
	for _, row := range rows {
		if row.Verdict == "unevaluated" {
			return "unevaluated"
		}
	}
	return "pass"
}

func projectReadyRecords(records []store.ReadyRecord) []map[string]any {
	result := make([]map[string]any, 0, len(records))
	for _, record := range records {
		result = append(result, projectReadyRecord(record))
	}
	return result
}

func readyDistribution(records []store.ReadyRecord) map[string]int {
	result := map[string]int{}
	for _, record := range records {
		result[string(record.Ticket.Ticket.Status)]++
	}
	return result
}

func distribution(rows []store.TicketRecord, by string) map[string]int {
	result := map[string]int{}
	for _, row := range rows {
		values := []string{}
		switch by {
		case "status":
			values = []string{string(row.Ticket.Status)}
		case "kind":
			values = []string{string(row.Ticket.Kind)}
		case "severity":
			values = []string{string(row.Ticket.Severity)}
		case "hold":
			values = []string{fmt.Sprint(row.Ticket.Hold)}
		case "project":
			values = []string{row.Ticket.Project}
		case "assignee":
			if row.Ticket.Assignee == nil {
				values = []string{"(none)"}
			} else {
				values = []string{*row.Ticket.Assignee}
			}
		case "milestone":
			if row.Ticket.Milestone == nil {
				values = []string{"(none)"}
			} else {
				values = []string{*row.Ticket.Milestone}
			}
		case "label":
			values = row.Ticket.Labels
			if len(values) == 0 {
				values = []string{"(none)"}
			}
		default:
			values = []string{"(unknown)"}
		}
		for _, value := range values {
			result[value]++
		}
	}
	return result
}

func exitCode(report store.CheckReport) int {
	switch report.Verdict {
	case "fail":
		return 1
	case "unevaluated":
		return 3
	default:
		if report.Unevaluated {
			return 3
		}
		return 0
	}
}

func verdictExit(verdict string) int {
	if verdict == "fail" {
		return 1
	}
	if verdict == "unevaluated" {
		return 3
	}
	return 0
}

func errorExit(code string) int {
	return store.ExitForCode(code)
}

func mutationData(data map[string]any, event store.EventKey) map[string]any {
	data["project_id"] = event.ProjectID
	data["seq"] = event.Seq
	data["event"] = event
	return data
}

func recordWarnings(records []store.TicketRecord) []string {
	seen := map[string]bool{}
	var result []string
	for _, record := range records {
		for _, warning := range record.Warnings {
			if !seen[warning] {
				seen[warning] = true
				result = append(result, warning)
			}
		}
	}
	return result
}
