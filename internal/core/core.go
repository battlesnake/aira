// Package core is the transport-neutral AIRA dispatch seam. Adapters provide
// serializable requests and render the returned response; handlers below own
// all store and protocol behaviour.
package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"aira/internal/domain"
	"aira/internal/gate"
	"aira/internal/gitremote"
	"aira/internal/runner"
	"aira/internal/store"
)

type Request struct {
	Verb       string         `json:"verb"`
	Args       map[string]any `json:"args,omitempty"`
	Content    []byte         `json:"content,omitempty"`
	HasContent bool           `json:"has_content"`
}

type Response struct {
	OK         bool             `json:"ok"`
	Code       string           `json:"code"`
	Data       any              `json:"data,omitempty"`
	Error      string           `json:"error,omitempty"`
	Warnings   []string         `json:"warnings,omitempty"`
	Exit       int              `json:"exit,omitempty"`
	AfterWrite func(bool) error `json:"-"`
	RawData    json.RawMessage  `json:"-"`
}

// MarshalJSON retains the daemon's already-encoded data member. This keeps
// integer spelling and object field order identical across transport faces.
func (response Response) MarshalJSON() ([]byte, error) {
	data := response.RawData
	if len(data) == 0 && response.Data != nil {
		var err error
		data, err = json.Marshal(response.Data)
		if err != nil {
			return nil, err
		}
	}
	type responseJSON struct {
		OK       bool            `json:"ok"`
		Code     string          `json:"code"`
		Data     json.RawMessage `json:"data,omitempty"`
		Error    string          `json:"error,omitempty"`
		Warnings []string        `json:"warnings,omitempty"`
		Exit     int             `json:"exit,omitempty"`
	}
	return json.Marshal(responseJSON{
		OK: response.OK, Code: response.Code, Data: data, Error: response.Error,
		Warnings: response.Warnings, Exit: response.Exit,
	})
}

type Initializer func(context.Context, map[string]any) (any, error)

type argAccessor struct {
	values  map[string]any
	reads   map[string]struct{}
	content []byte
}

func newArgAccessor(values map[string]any, content ...[]byte) *argAccessor {
	accessor := &argAccessor{values: values, reads: make(map[string]struct{})}
	if len(content) > 0 {
		accessor.content = make([]byte, len(content[0]))
		copy(accessor.content, content[0])
	}
	return accessor
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
	CountFindings(string, string) (store.FindingCountResult, error)
	ComputeGauge(string) (store.GaugeResult, error)
	ComputeAllGauges() ([]store.GaugeResult, error)
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
	AddTestReport(context.Context, domain.TestReportInput) (store.TestReportAddResult, error)
	ListTestReports(string) ([]domain.TestReport, error)
	GetTestReport(string) (domain.TestReport, error)
	FlakyTests(string) ([]domain.FlakyTest, error)
	FlakyCellSummary(context.Context) (store.FlakyCellSummary, error)
	ReconcileFlaky(context.Context) error
	AddComputeEvent(context.Context, domain.ComputeEventInput) (store.ComputeEventAddResult, error)
	ListComputeEvents(string) ([]domain.ComputeEvent, error)
	SpendByPhase(context.Context, string) ([]store.ComputePhaseSummary, error)
	AddQuotaSnapshot(context.Context, domain.QuotaSnapshotInput) (store.QuotaSnapshotAddResult, error)
	ListQuotaSnapshots(string) ([]domain.QuotaSnapshot, error)
	PinGateBaseline(context.Context, string, []string, string, string) (store.GateBaseline, error)
	ShowGateBaseline(string) (store.GateBaseline, error)
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
	Code     string
	Error    string
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
	store          Store
	runner         Runner
	gitops         GitOps
	initializer    Initializer
	stdin          io.Reader
	outputCap      int64
	reportMaxBytes int64
	face           FaceOutput
	verbs          map[string]verbSpec
}

// FaceOutput is fixed when a Core is constructed. Live output is deliberately
// a face concern: MCP and JSON faces leave Live false so their protocols are
// never contaminated by child bytes.
type FaceOutput struct {
	Stdout io.Writer
	Stderr io.Writer
	Live   bool
}

// Runner is the transport-neutral execution seam. The faces never call the
// concrete runner directly; they construct core.Request values and dispatch
// through this interface.
type Runner interface {
	Launch(context.Context, runner.Request) (*runner.RunRecord, error)
	Kill(context.Context, string, bool) (*runner.RunRecord, error)
	Get(string) (*runner.RunRecord, error)
	ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error)
	Reconcile(context.Context) ([]runner.RunRecord, error)
}

type detachedRunner interface {
	LaunchDetached(context.Context, runner.Request, string) (*runner.DetachLaunch, error)
	DetachOutputDir() string
}

type runInputRunner interface {
	Input(context.Context, runner.RunInputRequest) (*runner.RunInputResult, error)
}

type supervisorLivenessRunner interface {
	SupervisorLiveness(runner.RunRecord) runner.SupervisorLiveness
}

type pendingDetachData struct {
	record     runner.RunRecord
	launch     *runner.DetachLaunch
	wiringPath string
}

// GitOps is the transport-neutral git network-operation seam.
type GitOps interface {
	Run(context.Context, gitremote.Request) (*gitremote.Result, error)
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
	c := &Core{store: s, reportMaxBytes: defaultRunReportMaxBytes}
	c.verbs = c.dispatchTable()
	return c
}

func NewWithRunner(s Store, execution Runner) *Core {
	c := &Core{store: s, runner: execution, reportMaxBytes: runnerReportMaxBytes(execution)}
	c.verbs = c.dispatchTable()
	return c
}

func NewWithRunnerInput(s Store, execution Runner, stdin io.Reader) *Core {
	c := NewWithRunnerFace(s, execution, stdin, FaceOutput{})
	return c
}

func NewWithRunnerFace(s Store, execution Runner, stdin io.Reader, face FaceOutput) *Core {
	c := &Core{store: s, runner: execution, face: face, reportMaxBytes: runnerReportMaxBytes(execution)}
	c.stdin = stdin
	c.verbs = c.dispatchTable()
	return c
}

func NewWithRunnerOutputCap(s Store, execution Runner, cap int64) *Core {
	c := NewWithRunner(s, execution)
	c.outputCap = cap
	return c
}

// WithGitOps attaches git network operations to a runner-bearing face.
func (c *Core) WithGitOps(g GitOps) *Core {
	c.gitops = g
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
	verb := CanonicalVerb(req.Verb)
	spec, ok := c.verbs[verb]
	if !ok {
		code := "E_UNKNOWN_VERB"
		return Response{Code: code, Error: fmt.Sprintf("unknown verb %q", req.Verb), Exit: store.ExitForCode(code)}
	}
	args := newArgAccessor(req.Args)
	if req.HasContent || req.Content != nil {
		args = newArgAccessor(req.Args, req.Content)
	}
	data, err := spec.Run(ctx, args)
	if err != nil {
		code := store.ErrorCode(err)
		return Response{Code: code, Error: err.Error(), Exit: errorExit(code)}
	}
	warnings := []string(nil)
	verdict := ""
	handlerCode := ""
	handlerError := ""
	if wrapped, ok := data.(handlerData); ok {
		data, warnings, verdict, handlerCode, handlerError = wrapped.Data, wrapped.Warnings, wrapped.Verdict, wrapped.Code, wrapped.Error
	}
	var afterWrite func(bool) error
	if pending, ok := data.(pendingDetachData); ok {
		data = runResponseData{RunRecord: pending.record, Wiring: runWiring{
			Report: runReportWiring{Code: codeReportNotRequested}, Compute: runComputeWiring{Tokens: "unevaluated", Code: codeComputeNotRequested},
			TestsGreenObserved: runObservation{Not: "detached-unevaluated"}, WiringComplete: false, Warnings: []runWiringWarning{},
		}}
		afterWrite = func(delivered bool) error {
			err := pending.launch.Complete(delivered)
			if !delivered {
				if removeErr := removeDetachedWiringSidecar(pending.wiringPath); err == nil {
					err = removeErr
				}
			}
			return err
		}
	}
	data = c.presentRunData(data)
	if handlerCode != "" {
		if handlerError == "" {
			handlerError = handlerCode
		}
		return Response{OK: false, Code: handlerCode, Data: data, Error: handlerError, Exit: store.ExitForCode(handlerCode)}
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
	return Response{OK: true, Code: "OK", Data: data, Warnings: warnings, AfterWrite: afterWrite}
}

func runRecord(data any) (runner.RunRecord, bool) {
	switch value := data.(type) {
	case runner.RunRecord:
		return value, true
	case *runner.RunRecord:
		if value != nil {
			return *value, true
		}
	case runResponseData:
		return value.RunRecord, true
	case *runResponseData:
		if value != nil {
			return value.RunRecord, true
		}
	}
	return runner.RunRecord{}, false
}

func (c *Core) presentRunData(data any) any {
	switch value := data.(type) {
	case runner.RunRecord:
		return c.presentRunRecord(value)
	case *runner.RunRecord:
		if value != nil {
			presented := c.presentRunRecord(*value)
			return &presented
		}
	case runResponseData:
		value.RunRecord = c.presentRunRecord(value.RunRecord)
		return value
	case *runResponseData:
		if value != nil {
			copy := *value
			copy.RunRecord = c.presentRunRecord(copy.RunRecord)
			return &copy
		}
	}
	return data
}

func runRecordCode(record runner.RunRecord) string {
	if len(record.ErrorCodes) > 0 {
		return record.ErrorCodes[0]
	}
	if record.Status == runner.StatusKilled {
		return "E_RUN_KILLED"
	}
	if record.Status == runner.StatusOOMKilled {
		return "E_RUN_OOM_KILLED"
	}
	if record.Status == runner.StatusExited && record.ExitCode != nil && *record.ExitCode != 0 {
		return "E_RUN_FAILED"
	}
	if record.Status == runner.StatusLost {
		return "U_RUN_EXIT_UNKNOWN"
	}
	return ""
}

func parseRunTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("timeout must be a positive duration"))
	}
	return timeout, nil
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
		"show": {Name: "show", Usage: "show <selector>", Args: []ArgSpec{stringSpec("selector", true, true, "Exact ticket or run selector"), listSpec("fields", false, false, "Optional projected ticket fields")}, MCPTool: "aira_get", Run: func(_ context.Context, args *argAccessor) (any, error) {
			selector := stringArg(args, "selector")
			fields := stringSlice(args, "fields")
			if strings.HasPrefix(selector, "RUN-") && c.runner != nil {
				if len(fields) != 0 {
					return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("run records do not support ticket field projection"))
				}
				record, err := c.runner.Get(selector)
				if err != nil {
					return nil, err
				}
				presented := c.presentRunRecord(*record)
				return &presented, nil
			}
			record, err := c.store.Get(selector)
			if err != nil {
				return nil, err
			}
			projected := projectRecord(record, fields)
			if len(fields) == 0 {
				projected["body"] = record.Body
			}
			return handlerData{Data: projected, Warnings: record.Warnings}, nil
		}},
		"review": {Name: "review", Usage: "review <selector> [--paths a,b]", Args: []ArgSpec{stringSpec("selector", true, true, "Exact ticket selector"), listSpec("paths", false, false, "Optional paths under review")}, MCPTool: "aira_review", Run: func(_ context.Context, args *argAccessor) (any, error) {
			selector := stringArg(args, "selector")
			pathsProvided := args.present("paths")
			var argumentPaths []string
			if pathsProvided {
				argumentPaths = stringSlice(args, "paths")
			}
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
			var summary store.ImportSummary
			var err error
			if args.content != nil {
				importer, ok := c.store.(interface {
					ImportFindingsBytes(context.Context, []byte, bool) (store.ImportSummary, error)
				})
				if !ok {
					return nil, errors.New("E_IMPORT_INVALID: byte import is unavailable")
				}
				summary, err = importer.ImportFindingsBytes(ctx, args.content, boolArg(args, "strict"))
			} else {
				summary, err = c.store.ImportFindingsFile(ctx, stringArg(args, "file"), boolArg(args, "strict"))
			}
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
				if stringArg(args, "by") != "" {
					count, countErr := c.store.CountFindings(stringArg(args, "query"), by)
					if countErr != nil {
						return nil, countErr
					}
					data["total"], data["distribution"] = count.Total, count.Distribution
					if len(rows) > ListLimit {
						data["rows"] = projectFindingRecords(rows[:ListLimit], stringSlice(args, "fields"))
						data["truncated"] = true
					}
				} else if len(rows) > ListLimit {
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
				var summary store.ImportRequirementsSummary
				var err error
				if args.content != nil {
					importer, ok := c.store.(interface {
						ImportRequirementsBytes(context.Context, []byte) (store.ImportRequirementsSummary, error)
					})
					if !ok {
						return nil, errors.New("E_IMPORT_INVALID: byte import is unavailable")
					}
					summary, err = importer.ImportRequirementsBytes(ctx, args.content)
				} else {
					summary, err = c.store.ImportRequirements(ctx, stringArg(args, "file"))
				}
				if err != nil {
					return nil, err
				}
				return summary, nil
			default:
				return nil, fmt.Errorf("E_UNKNOWN_VERB: unknown req sub-verb %q", subverb)
			}
		}},
		"spend": {Name: "spend", Usage: "spend add|ls ...", Args: []ArgSpec{
			stringSpec("subverb", true, true, "Spend operation", "add", "ls"), stringSpec("provider", false, false, "LLM provider"), stringSpec("model", false, false, "Model"), stringSpec("source", false, false, "Ingest source"), stringSpec("ticket", false, false, "Ticket ID"), stringSpec("phase", false, false, "Work phase"), stringSpec("at", false, false, "Timestamp"), stringSpec("session", false, false, "Session"), stringSpec("agent", false, false, "Agent"), stringSpec("total", false, false, "Reported total"), stringSpec("cost-usd", false, false, "Caller-supplied cost"), stringSpec("query", false, false, "Event query"), stringSpec("by", false, false, "Live distribution field"), boolSpec("reasoning-subset", false, false, "Reasoning is a subset of output"), listSpec("bucket", false, false, "Explicit disjoint bucket K=V"), stringSpec("raw", false, false, "Provider usage payload"), stringSpec("usage-file", false, false, "Provider usage file"),
		}, MCPTool: "aira_spend", MCPOperation: "subverb", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			store, ok := c.store.(interface {
				AddComputeEvent(context.Context, domain.ComputeEventInput) (store.ComputeEventAddResult, error)
				ListComputeEvents(string) ([]domain.ComputeEvent, error)
				SpendByPhase(context.Context, string) ([]store.ComputePhaseSummary, error)
			})
			if !ok {
				return nil, errors.New("E_COMPUTE_INVALID: compute store is unavailable")
			}
			subverb := strings.ToLower(stringArg(args, "subverb"))
			switch subverb {
			case "add":
				raw, err := usageArgs(args, stringArg(args, "provider"))
				if err != nil {
					return nil, err
				}
				input := domain.ComputeEventInput{TicketID: stringArg(args, "ticket"), Phase: stringArg(args, "phase"), Model: stringArg(args, "model"), Provider: stringArg(args, "provider"), At: stringArg(args, "at"), Session: stringArg(args, "session"), Agent: stringArg(args, "agent"), Source: stringArg(args, "source"), Raw: raw}
				if cost, present, err := optionalFloatArg(args, "cost-usd"); err != nil {
					return nil, err
				} else if present {
					input.CostUSD = &cost
				}
				result, err := store.AddComputeEvent(ctx, input)
				if err != nil {
					return nil, err
				}
				warnings := []string{}
				if result.Event.Conservation == domain.ConservationMismatch {
					warnings = append(warnings, domain.ComputeCodeConservation)
				}
				return handlerData{Data: result, Warnings: warnings}, nil
			case "ls", "list":
				if by := stringArg(args, "by"); by == "phase" {
					rows, err := store.SpendByPhase(ctx, stringArg(args, "query"))
					if err != nil {
						return nil, err
					}
					total := 0
					for _, row := range rows {
						total += row.Events
					}
					return map[string]any{"total": total, "rows": rows}, nil
				}
				rows, err := store.ListComputeEvents(stringArg(args, "query"))
				if err != nil {
					return nil, err
				}
				data := map[string]any{"total": len(rows), "rows": rows}
				if by := stringArg(args, "by"); by != "" {
					distribution, err := computeDistribution(rows, by)
					if err != nil {
						return nil, err
					}
					data["distribution"] = distribution
				}
				return data, nil
			default:
				return nil, fmt.Errorf("E_UNKNOWN_VERB: unknown spend sub-verb %q", subverb)
			}
		}},
		"quota": {Name: "quota", Usage: "quota add|ls ...", Args: []ArgSpec{
			stringSpec("subverb", true, true, "Quota operation", "add", "ls"), stringSpec("provider", false, false, "Provider"), stringSpec("source", false, false, "Ingest source"), stringSpec("at", false, false, "Timestamp"), stringSpec("window", false, false, "Quota window"), stringSpec("used", false, false, "Used tokens"), stringSpec("limit", false, false, "Limit tokens"), stringSpec("remaining", false, false, "Remaining tokens"), stringSpec("reset-at", false, false, "Reset timestamp"), stringSpec("query", false, false, "Snapshot query"),
		}, MCPTool: "aira_quota", MCPOperation: "subverb", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			store, ok := c.store.(interface {
				AddQuotaSnapshot(context.Context, domain.QuotaSnapshotInput) (store.QuotaSnapshotAddResult, error)
				ListQuotaSnapshots(string) ([]domain.QuotaSnapshot, error)
			})
			if !ok {
				return nil, errors.New("E_COMPUTE_INVALID: quota store is unavailable")
			}
			subverb := strings.ToLower(stringArg(args, "subverb"))
			switch subverb {
			case "add":
				input := domain.QuotaSnapshotInput{Provider: stringArg(args, "provider"), Source: stringArg(args, "source"), At: stringArg(args, "at"), Window: stringArg(args, "window"), ResetAt: stringArg(args, "reset-at")}
				for name, dst := range map[string]**int64{"used": &input.Used, "limit": &input.Limit, "remaining": &input.Remaining} {
					value, present, err := optionalIntArg(args, name)
					if err != nil {
						return nil, err
					}
					if present {
						*dst = &value
					}
				}
				return store.AddQuotaSnapshot(ctx, input)
			case "ls", "list":
				rows, err := store.ListQuotaSnapshots(stringArg(args, "query"))
				if err != nil {
					return nil, err
				}
				return map[string]any{"total": len(rows), "rows": rows}, nil
			default:
				return nil, fmt.Errorf("E_UNKNOWN_VERB: unknown quota sub-verb %q", subverb)
			}
		}},
		"test-report": {Name: "test-report", Usage: "test-report add|ls|show|flaky ...", Args: []ArgSpec{
			stringSpec("subverb", true, true, "Test report operation", "add", "ls", "show", "flaky"),
			boolSpec("all", false, false, "Return the complete flaky cell-state summary"),
			stringSpec("format", false, false, "Report format", "go-json", "junit"), stringSpec("selector", false, true, "Report or test selector"),
			stringSpec("explain", false, false, "Explain one flaky test"), stringSpec("ticket", false, false, "Ticket ID"), stringSpec("phase", false, false, "Work phase"),
			stringSpec("commit", false, false, "Commit identity"), stringSpec("branch", false, false, "Branch identity"), stringSpec("suite", false, false, "Suite identity"),
			stringSpec("config", false, false, "Opaque test configuration"), stringSpec("env_digest", false, false, "Environment digest"),
			stringSpec("shard", false, false, "Shard i/n"), stringSpec("retry", false, false, "Retry index"),
			stringSpec("raw", false, false, "Raw report bytes"),
		}, MCPTool: "aira_test_report", MCPOperation: "subverb", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			subverb := strings.ToLower(stringArg(args, "subverb"))
			switch subverb {
			case "add":
				input := domain.TestReportInput{Format: stringArg(args, "format"), TicketID: stringArg(args, "ticket"), Phase: stringArg(args, "phase"), Commit: stringArg(args, "commit"), Branch: stringArg(args, "branch"), SuiteID: stringArg(args, "suite"), Config: stringArg(args, "config"), EnvDigest: stringArg(args, "env_digest"), Shard: stringArg(args, "shard"), RetryIndex: intArg(args, "retry")}
				if raw := args.record("raw"); raw != nil {
					switch value := raw.(type) {
					case []byte:
						input.Raw = value
					case string:
						input.Raw = []byte(value)
					}
				}
				return c.store.AddTestReport(ctx, input)
			case "ls", "list":
				reports, err := c.store.ListTestReports(stringArg(args, "selector"))
				if err != nil {
					return nil, err
				}
				return map[string]any{"total": len(reports), "rows": reports}, nil
			case "show":
				return c.store.GetTestReport(stringArg(args, "selector"))
			case "flaky":
				selectorArg := stringArg(args, "selector")
				explain := stringArg(args, "explain")
				if boolArg(args, "all") {
					summary, err := c.store.FlakyCellSummary(ctx)
					if err != nil {
						return nil, err
					}
					return summary, nil
				}
				selector := explain
				if selector == "" {
					selector = selectorArg
				}
				tests, err := c.store.FlakyTests(selector)
				if err != nil {
					return nil, err
				}
				if selector != "" && len(tests) == 1 && tests[0].State == domain.FlakyStateUnevaluated {
					return handlerData{Data: tests[0], Code: "U_TESTREPORT_INCOMPARABLE"}, nil
				}
				if selector == "" {
					flaky := make([]domain.FlakyTest, 0, len(tests))
					for _, test := range tests {
						if test.State == domain.FlakyStateFlaky {
							flaky = append(flaky, test)
						}
					}
					tests = flaky
				}
				return map[string]any{"total": len(tests), "rows": tests}, nil
			default:
				return nil, fmt.Errorf("E_ARGUMENT_INVALID: unknown test-report sub-verb %q", subverb)
			}
		}},
		"insights": {Name: "insights", Usage: "insights [ls|show <name>]", Args: []ArgSpec{
			stringSpec("subverb", false, true, "Insight operation", "ls", "show"),
			stringSpec("name", false, true, "Gauge name"),
		}, MCPTool: "aira_insights", MCPOperation: "subverb", Run: func(_ context.Context, args *argAccessor) (any, error) {
			operation := strings.ToLower(stringArg(args, "subverb"))
			if operation == "" {
				operation = "show"
			}
			if operation == "ls" {
				return store.GaugeRegistryRows(), nil
			}
			if operation != "show" {
				return nil, fmt.Errorf("E_UNKNOWN_VERB: unknown insights sub-verb %q", operation)
			}
			if _, ok := c.store.(interface {
				ComputeGauge(string) (store.GaugeResult, error)
			}); !ok {
				return nil, errors.New("E_CONFIG_INVALID: insight store is unavailable")
			}
			if stringArg(args, "name") == "" {
				all, err := c.store.ComputeAllGauges()
				if err != nil {
					return nil, err
				}
				for _, gauge := range all {
					if gauge.Unevaluated {
						return handlerData{Data: all, Verdict: "unevaluated"}, nil
					}
				}
				return all, nil
			}
			result, err := c.store.ComputeGauge(stringArg(args, "name"))
			if err != nil {
				return nil, err
			}
			if result.Unevaluated {
				return handlerData{Data: result, Verdict: "unevaluated"}, nil
			}
			return result, nil
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
			if stringArg(args, "by") != "" {
				count, countErr := c.store.Count(stringArg(args, "query"), by)
				if countErr != nil {
					return nil, countErr
				}
				data["total"], data["distribution"] = count.Total, count.Distribution
				if len(rows) > ListLimit {
					data["rows"] = projectRecords(rows[:ListLimit], stringSlice(args, "fields"))
					data["truncated"] = true
				}
			} else if len(rows) > ListLimit {
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
		"git": {Name: "git", Usage: "git clone|fetch|push|ls-remote ...", Args: []ArgSpec{
			stringSpec("subverb", true, true, "Git network operation", "clone", "fetch", "push", "ls-remote"),
			stringSpec("remote", false, true, "Remote name (defaults to origin)"),
			stringSpec("url", false, true, "Clone URL"),
			listSpec("refspecs", false, true, "Exact refspecs"),
			stringSpec("dir", false, true, "Optional clone destination directory"),
		}, MCPTool: "aira_git", MCPOperation: "subverb", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			subverb := strings.ToLower(strings.TrimSpace(stringArg(args, "subverb")))
			switch subverb {
			case "clone", "fetch", "push", "ls-remote":
			default:
				return nil, fmt.Errorf("%s: unknown git operation %q", gitremote.CodeArgInvalid, subverb)
			}
			request := gitremote.Request{Verb: subverb}
			if subverb == "clone" {
				request.URL, request.Dir = stringArg(args, "url"), stringArg(args, "dir")
			} else {
				request.Remote, request.Refspecs = stringArg(args, "remote"), stringSlice(args, "refspecs")
			}
			if c.gitops == nil {
				return nil, fmt.Errorf("%s: git ops unavailable on this face", gitremote.CodeSSHUnavailable)
			}
			if c.face.Live {
				request.LiveStdout, request.LiveStderr = c.face.Stdout, c.face.Stderr
			}
			result, err := c.gitops.Run(ctx, request)
			var gitErr *gitremote.Error
			if errors.As(err, &gitErr) {
				return handlerData{Code: gitErr.Code(), Data: gitErr.Data(), Error: gitErr.Error()}, nil
			}
			return result, err
		}},
		"run": {Name: "run", Usage: "run [options] -- <argv...>", Args: []ArgSpec{
			listSpec("argv", true, true, "Exact target argv after the launch delimiter"),
			listSpec("prefix", false, false, "Optional exact launch-prefix argv"),
			stringSpec("cwd", false, false, "Launch working directory"),
			listSpec("env", false, false, "Exact KEY=VALUE environment overrides"),
			boolSpec("merge", false, false, "Capture stdout and stderr as one kernel stream"),
			boolSpec("realtime", false, false, "Apply realtime stdio buffering when libstdbuf is available"),
			boolSpec("pty", false, false, "Capture through a controlling PTY and merge stdout with stderr"),
			boolSpec("detach", false, false, "Return a handle while a per-run supervisor owns the child"),
			boolSpec("stdin_connect", false, false, "Accept live stdin bytes over a per-run socket"),
			boolSpec("follow", false, false, "Keep the launching face attached to the run"),
			stringSpec("stdin", false, false, "Launch-time stdin file or -"),
			boolSpec("no_stdin", false, false, "Explicitly launch with null stdin"),
			boolSpec("store_stdin", false, false, "Persist supplied launch stdin"),
			boolSpec("no_admit", false, false, "Bypass configured memory admission"),
			stringSpec("timeout", false, false, "Positive run timeout duration"),
			stringSpec("ticket", false, false, "Ticket ID"),
			stringSpec("phase", false, false, "Work phase"),
			stringSpec("label", false, false, "Run label"),
			stringSpec("tool", false, false, "Tool/model identity"),
			stringSpec("report", false, false, "Captured test report format", "go-json", "junit"),
			stringSpec("report_stream", false, false, "Captured stream to parse as the report", "out", "err", "merged"),
			stringSpec("suite", false, false, "Test suite identity"),
			listSpec("config_env", false, false, "Environment-scoped test configuration K=V"),
			stringSpec("shard", false, false, "Test shard i/n"),
			stringSpec("retry", false, false, "Test retry index"),
			stringSpec("usage", false, false, "Provider usage JSON file"),
			stringSpec("provider", false, false, "Usage provider"),
			boolSpec("strict_wiring", false, false, "Fail a successful child when telemetry wiring is incomplete"),
		}, MCPTool: "aira_run", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			timeout, err := parseRunTimeout(stringArg(args, "timeout"))
			if err != nil {
				return nil, err
			}
			noStdin := boolArg(args, "no_stdin")
			if noStdin && stringArg(args, "stdin") != "" {
				return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("no_stdin and stdin are mutually exclusive"))
			}
			params := wiringParamsFromArgs(args)
			request := runner.Request{
				Argv: stringSlice(args, "argv"), Cwd: stringArg(args, "cwd"), Env: stringSlice(args, "env"),
				Ticket: stringArg(args, "ticket"), Phase: stringArg(args, "phase"), Label: stringArg(args, "label"), Tool: params.Tool,
				Prefix: stringSlice(args, "prefix"), Merge: boolArg(args, "merge"), Realtime: boolArg(args, "realtime"), PTY: boolArg(args, "pty"), StdinPath: stringArg(args, "stdin"),
				StoreStdin: boolArg(args, "store_stdin"), NoAdmit: boolArg(args, "no_admit"), Timeout: timeout,
				Detach: boolArg(args, "detach"), StdinConnect: boolArg(args, "stdin_connect"),
			}
			if request.StdinConnect && (!request.Detach || request.PTY || request.StdinPath != "" || noStdin || request.StoreStdin) {
				return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("--stdin-connect requires --detach and is incompatible with --stdin, --no-stdin, --pty, and --store-stdin"))
			}
			follow := boolArg(args, "follow")
			if request.PTY {
				request.Merge = true
			}
			if c.face.Live {
				if request.Merge {
					request.LiveStdout = c.face.Stdout
				} else {
					request.LiveStdout = c.face.Stdout
					request.LiveStderr = c.face.Stderr
				}
			}
			if request.StdinPath == "-" {
				request.Stdin = c.stdin
			}
			if request.Detach {
				if follow || request.PTY || request.StdinPath == "-" {
					return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("detach is incompatible with follow, pty, and stdin '-'"))
				}
				if params.StrictWiring {
					return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("detach is incompatible with strict wiring"))
				}
				if strings.TrimSpace(params.Usage) == "-" {
					return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("detached --usage requires a file and does not accept -"))
				}
				if c.runner == nil {
					return nil, runnerError("E_RUN_SCOPE_UNAVAILABLE", errors.New("runner is unavailable"))
				}
				detacher, ok := c.runner.(detachedRunner)
				if !ok {
					return nil, runnerError("E_RUN_SCOPE_UNAVAILABLE", errors.New("detached runner is unavailable"))
				}
				wiringPath := ""
				if params.requested() {
					var reportContext store.TestReportContext
					if contextual, ok := c.store.(testReportContextStore); ok {
						reportContext = contextual.TestReportContext(ctx)
					}
					wiringPath, err = writeDetachedWiringSidecar(detacher.DetachOutputDir(), params, reportContext)
					if err != nil {
						return nil, runnerError("E_RUN_DETACH_FAILED", err)
					}
					request.TelemetryPending = TelemetryPending
				}
				launch, err := detacher.LaunchDetached(ctx, request, wiringPath)
				if err != nil {
					_ = removeDetachedWiringSidecar(wiringPath)
					return nil, err
				}
				launch.Record.Telemetry = request.TelemetryPending
				return pendingDetachData{record: launch.Record, launch: launch, wiringPath: wiringPath}, nil
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
			// Snapshot the VCS identity BEFORE launch (Sol build-review): the child may
			// change HEAD/branch/worktree, and a report must be attributed to the code that
			// was actually tested, not to whatever the working tree became afterwards.
			var reportContext store.TestReportContext
			if strings.TrimSpace(params.Report) != "" {
				if contextual, ok := c.store.(testReportContextStore); ok {
					reportContext = contextual.TestReportContext(ctx)
				}
			}
			record, err := c.runner.Launch(ctx, request)
			if err != nil {
				return nil, err
			}
			wiring := c.wireTerminalRun(ctx, params, *record, reportContext)
			data := runResponseData{RunRecord: *record, Wiring: wiring}
			if params.StrictWiring && runRecordCode(*record) == "" && !wiring.WiringComplete {
				return handlerData{Data: data, Code: "E_RUN_WIRING_INCOMPLETE"}, nil
			}
			return data, nil
		}},
		"run-input": {Name: "run-input", Usage: "run-input <run-id> [--close] [--steal]", Args: []ArgSpec{
			stringSpec("run_id", true, true, "Run identifier"),
			stringSpec("data", false, false, "Base64 bytes accepted for delivery (MCP; maximum 1 MiB)"),
			boolSpec("close", false, false, "Close the child's stdin after accepted bytes"),
			boolSpec("steal", false, false, "Override foreign run ownership"),
		}, MCPTool: "aira_run_input", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			request := runner.RunInputRequest{RunID: stringArg(args, "run_id"), Close: boolArg(args, "close"), Steal: boolArg(args, "steal")}
			dataPresent := args.present("data")
			inputRunner, ok := c.runner.(runInputRunner)
			if !ok {
				return nil, runnerError("E_RUN_SCOPE_UNAVAILABLE", errors.New("run input is unavailable"))
			}
			if dataPresent {
				encoded := stringArg(args, "data")
				if len(encoded) > base64.StdEncoding.EncodedLen(runner.MaxRunInputFrameBytes) {
					return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("run-input data exceeds the maximum frame size"))
				}
				data, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("run-input data must be valid base64"))
				}
				if len(data) > runner.MaxRunInputFrameBytes {
					return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("run-input data exceeds the maximum frame size"))
				}
				request.Reader = bytes.NewReader(data)
			} else {
				request.Reader = c.stdin
			}
			if request.Reader == nil && !request.Close {
				return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("run-input requires data or --close"))
			}
			result, err := inputRunner.Input(ctx, request)
			var inputErr *runner.RunInputError
			if errors.As(err, &inputErr) {
				return handlerData{Code: inputErr.Code, Error: inputErr.Error(), Data: map[string]any{
					"run_id": request.RunID, "accepted": inputErr.Committed,
				}}, nil
			}
			return result, err
		}},
		"run-kill": {Name: "run-kill", Usage: "run-kill <run-id> [--steal]", Args: []ArgSpec{stringSpec("run_id", true, true, "Run identifier"), boolSpec("steal", false, false, "Override foreign run ownership")}, MCPTool: "aira_run_kill", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			runID := stringArg(args, "run_id")
			steal := boolArg(args, "steal")
			if c.runner == nil {
				return nil, runnerError("E_RUN_SCOPE_UNAVAILABLE", errors.New("runner is unavailable"))
			}
			record, err := c.runner.Kill(ctx, runID, steal)
			var foreign *runner.ForeignOwnerError
			if errors.As(err, &foreign) {
				return handlerData{Code: "E_RUN_FOREIGN_OWNER", Data: map[string]any{
					"run_id": foreign.RunID, "owner": foreign.Owner, "caller_owner": foreign.CallerOwner,
					"hint": "pass --steal to override",
				}}, nil
			}
			return record, err
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
					if store.ErrorCode(err) == "U_INDEX_UNESTABLISHED" {
						return handlerData{Data: map[string]any{"reconciled": true}, Verdict: "unevaluated"}, nil
					}
					return nil, err
				}
			}
			data := map[string]any{"reconciled": true}
			if c.runner != nil {
				runs, err := c.runner.Reconcile(ctx)
				if err != nil {
					return nil, err
				}
				for i := range runs {
					runs[i] = c.presentRunRecord(runs[i])
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
						if run.Detached {
							// Surface EVERY unevaluated/attention residual honestly — not a
							// hardcoded subset — so unknown-liveness / uninspectable-scope
							// (U_RUN_RECONCILE_REQUIRED) is never silently dropped alongside
							// stalled / launch-stalled / capture-incomplete.
							for _, code := range run.ErrorCodes {
								if strings.HasPrefix(code, "U_") {
									report.Warnings = append(report.Warnings, store.CheckFinding{Code: code, Subject: run.ID, Message: "detached run requires operator attention", Kind: "warning"})
								}
							}
							continue
						}
						report.Warnings = append(report.Warnings, store.CheckFinding{Code: "U_RUN_RECONCILE_REQUIRED", Subject: run.ID, Message: "live run scope is orphaned and remains explicitly killable", Kind: "warning"})
					}
				}
			}
			return report, nil
		}},
		"gate": {Name: "gate", Usage: "gate <operation> [args]", Args: []ArgSpec{
			stringSpec("subverb", true, true, "Gate operation", "add", "ls", "show", "set", "run", "check", "attest", "prove", "review", "canary-run", "canary-show", "baseline-pin", "baseline-show"),
			stringSpec("gate_id", false, true, "Gate identifier"), stringSpec("canary_id", false, true, "Canary identifier"), stringSpec("verdict", false, false, "Attestation verdict", "pass", "fail"), stringSpec("actor", false, false, "Attestation actor"), stringSpec("reason", false, false, "Baseline pin reason"), stringSpec("report", false, false, "Comma-separated test report IDs"),
			stringSpec("checker", false, false, "Gate checker", "check-dimension", "command", "manual-attestation", "ratchet"), stringSpec("predicate", false, false, "Command predicate", "exit-zero", "tests-green"),
			listSpec("argv", false, false, "Exact command argv tokens"), stringSpec("cwd", false, false, "Command root or relative subdirectory"), listSpec("env_allow", false, false, "Allow-listed environment names"),
			stringSpec("timeout_ms", false, false, "Command timeout in milliseconds"), stringSpec("output_cap_bytes", false, false, "Combined output cap in bytes"), stringSpec("parser", false, false, "Command output parser", "go-test-json-v1"),
			stringSpec("mutation_kind", false, false, "Typed mutation kind", "go-negate-assertion", "go-inject-failing-test"), stringSpec("mutation_file", false, false, "Mutation target file"), stringSpec("mutation_test", false, false, "Mutation target test"),
			stringSpec("mutation_occurrence", false, false, "Mutation assertion occurrence"), stringSpec("mutation_pkgdir", false, false, "Mutation package directory"), stringSpec("mutation_testname", false, false, "Injected mutation test name"), stringSpec("mutation_seed", false, false, "Mutation numeric seed"), stringSpec("mutation_expected_result", false, false, "Mutation expected result", "fail"),
		}, MCPTool: "aira_gate", MCPOperation: "subverb", Run: func(ctx context.Context, args *argAccessor) (any, error) {
			subverb := strings.ToLower(stringArg(args, "subverb"))
			switch subverb {
			case "show", "add", "set", "run", "attest", "prove", "review", "baseline-pin", "baseline-show":
				_ = stringArg(args, "gate_id")
				if subverb == "attest" {
					_ = stringArg(args, "verdict")
					_ = stringArg(args, "actor")
				}
				if subverb == "baseline-pin" {
					_ = stringArg(args, "report")
					_ = stringArg(args, "reason")
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
			case "baseline-pin":
				if stringArg(args, "gate_id") == "" || stringArg(args, "report") == "" {
					return nil, errors.New("E_GATE_BASELINE_INVALID: baseline pin requires gate id and report")
				}
				return c.store.PinGateBaseline(ctx, stringArg(args, "gate_id"), strings.Split(stringArg(args, "report"), ","), stringArg(args, "actor"), stringArg(args, "reason"))
			case "baseline-show":
				return c.store.ShowGateBaseline(stringArg(args, "gate_id"))
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
		"git": {summary: "Run a bounded authenticated git network operation", safety: SafetyExecute, operations: []OperationSpec{
			{Name: "clone", Summary: "Clone a remote repository", Safety: SafetyExecute, Args: []OperationArg{{Name: "url", Required: true}, {Name: "dir"}}, Example: []string{"clone", "file:///repo", "repo"}},
			{Name: "fetch", Summary: "Fetch remote refs", Safety: SafetyExecute, Args: []OperationArg{{Name: "remote"}, {Name: "refspecs"}}, Example: []string{"fetch", "origin"}},
			{Name: "push", Summary: "Push explicit refs", Safety: SafetyExecute, Args: []OperationArg{{Name: "remote"}, {Name: "refspecs"}}, Example: []string{"push", "origin", "--", "HEAD:main"}},
			{Name: "ls-remote", Summary: "List remote refs", Safety: SafetyExecute, Args: []OperationArg{{Name: "remote"}, {Name: "refspecs"}}, Example: []string{"ls-remote", "origin"}},
		}},
		"run":       {summary: "Launch a subprocess in an owned scope", safety: SafetyExecute, example: []string{"--merge", "--", "printf", "hello"}},
		"run-input": {summary: "Send bytes accepted for delivery to a detached run", safety: SafetyExecute, example: []string{"RUN-1", "--close"}},
		"run-kill":  {summary: "Kill an owned run scope", safety: SafetyExecute, example: []string{"RUN-1"}},
		"run-log":   {summary: "Read captured run output", safety: SafetyRead, example: []string{"RUN-1", "--stream", "out"}},
		"reconcile": {summary: "Reconcile derived project state", safety: SafetyReconcile, example: []string{"--rebuild"}},
		"check":     {summary: "Check project consistency", safety: SafetyReconcile, example: []string{}},
		"insights": {summary: "Read honest drillable insight gauges", safety: SafetyRead, operations: []OperationSpec{
			{Name: "ls", Summary: "List registered insight gauges", Safety: SafetyRead, Args: nil, Example: []string{"ls"}},
			{Name: "show", Summary: "Compute one or all live insight gauges", Safety: SafetyRead, Args: []OperationArg{{Name: "name"}}, Example: []string{"show", "reviewer-verdict-ratio"}},
		}},
		"test-report": {summary: "Archive and compare test reports", safety: SafetyRead, operations: []OperationSpec{
			{Name: "add", Summary: "Ingest a test report", Safety: SafetyMutate, Args: []OperationArg{{Name: "format", Required: true}, {Name: "raw", Required: true}, {Name: "ticket"}, {Name: "phase"}, {Name: "commit"}, {Name: "branch"}, {Name: "suite"}, {Name: "config"}, {Name: "env_digest"}, {Name: "shard"}, {Name: "retry"}}, Example: []string{"add", "--format", "go-json"}},
			{Name: "ls", Summary: "List archived test reports", Safety: SafetyRead, Args: []OperationArg{{Name: "selector"}}, Example: []string{"ls"}},
			{Name: "show", Summary: "Show one archived test report", Safety: SafetyRead, Args: []OperationArg{{Name: "selector", Required: true}}, Example: []string{"show", "TR-1"}},
			{Name: "flaky", Summary: "Compute three-state flaky evidence", Safety: SafetyRead, Args: []OperationArg{{Name: "selector"}, {Name: "explain"}, {Name: "all"}}, Example: []string{"flaky", "--all"}},
		}},
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
			{Name: "baseline-pin", Summary: "Pin a durable ratchet baseline", Safety: SafetyMutate, Args: []OperationArg{{Name: "gate_id", Required: true}, {Name: "report", Required: true}, {Name: "reason"}, {Name: "actor"}}, Example: []string{"baseline-pin", "unit-tests", "--report", "TR-1", "--actor", "release-bot"}},
			{Name: "baseline-show", Summary: "Show the active ratchet baseline", Safety: SafetyRead, Args: []OperationArg{{Name: "gate_id", Required: true}}, Example: []string{"baseline-show", "unit-tests"}},
		}},
		"spend": {summary: "Record and list raw compute events", safety: SafetyMutate, operations: []OperationSpec{
			{Name: "add", Summary: "Ingest one compute event", Safety: SafetyMutate, Args: []OperationArg{{Name: "provider", Required: true}, {Name: "model", Required: true}, {Name: "source", Required: true}, {Name: "ticket"}, {Name: "phase"}, {Name: "at"}, {Name: "session"}, {Name: "agent"}, {Name: "raw"}, {Name: "usage-file"}, {Name: "bucket"}, {Name: "total"}, {Name: "cost-usd"}, {Name: "reasoning-subset"}}, Example: []string{"add", "--provider", "openai", "--model", "gpt-5", "--source", "manual", "--bucket", "fresh_input=700", "--bucket", "cache_read=300", "--bucket", "output=200", "--total", "1200", "--reasoning-subset"}},
			{Name: "ls", Summary: "List raw compute events", Safety: SafetyRead, Args: []OperationArg{{Name: "query"}, {Name: "by"}}, Example: []string{"ls", "provider:openai", "--by", "phase"}},
		}},
		"quota": {summary: "Record and list raw quota snapshots", safety: SafetyMutate, operations: []OperationSpec{
			{Name: "add", Summary: "Record one quota snapshot", Safety: SafetyMutate, Args: []OperationArg{{Name: "provider", Required: true}, {Name: "source"}, {Name: "at"}, {Name: "used"}, {Name: "limit"}, {Name: "remaining"}, {Name: "reset-at"}, {Name: "window"}}, Example: []string{"add", "--provider", "openai", "--source", "manual", "--used", "10", "--limit", "100"}},
			{Name: "ls", Summary: "List raw quota snapshots", Safety: SafetyRead, Args: []OperationArg{{Name: "query"}}, Example: []string{"ls", "provider:openai"}},
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

func usageArgs(args *argAccessor, provider string) (domain.RawUsage, error) {
	reasoningSubset := boolArg(args, "reasoning-subset")
	_ = args.record("usage-file")
	rawValue := args.record("raw")
	bucketValues := stringSlice(args, "bucket")
	hasPayload := false
	var payload []byte
	switch value := rawValue.(type) {
	case []byte:
		payload = value
		hasPayload = len(value) > 0
	case string:
		payload = []byte(value)
		hasPayload = strings.TrimSpace(value) != ""
	}
	if hasPayload && len(bucketValues) > 0 {
		return domain.RawUsage{}, errors.New(domain.ComputeCodeInvalid + ": payload and --bucket are mutually exclusive")
	}
	var raw domain.RawUsage
	if hasPayload {
		parsed, err := domain.ParseUsagePayload(provider, payload)
		if err != nil {
			return domain.RawUsage{}, err
		}
		raw = parsed
	} else if len(bucketValues) > 0 {
		buckets := domain.ComputeBuckets{}
		seen := map[string]bool{}
		for _, item := range bucketValues {
			key, value, ok := strings.Cut(item, "=")
			if !ok || key == "" || seen[key] {
				return domain.RawUsage{}, errors.New(domain.ComputeCodeInvalid + ": bucket must be a unique K=V")
			}
			seen[key] = true
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 {
				return domain.RawUsage{}, errors.New(domain.ComputeCodeInvalid + ": bucket values must be non-negative int64")
			}
			cell := &parsed
			switch key {
			case "fresh_input":
				buckets.FreshInput = cell
			case "cache_read":
				buckets.CacheRead = cell
			case "cache_write":
				buckets.CacheWrite = cell
			case "output":
				buckets.Output = cell
			case "reasoning":
				buckets.Reasoning = cell
			default:
				return domain.RawUsage{}, fmt.Errorf("%s: unknown bucket %q", domain.ComputeCodeInvalid, key)
			}
		}
		raw.Buckets, raw.ReasoningSubset = &buckets, reasoningSubset
	} else {
		return domain.RawUsage{}, errors.New(domain.ComputeCodeInvalid + ": exactly one usage input is required")
	}
	if total, present, err := optionalIntArg(args, "total"); err != nil {
		return domain.RawUsage{}, err
	} else if present {
		raw.ReportedTotal = &total
	}
	return raw, nil
}

func optionalIntArg(args *argAccessor, name string) (int64, bool, error) {
	if !args.present(name) {
		return 0, false, nil
	}
	value := args.values[name]
	var text string
	switch typed := value.(type) {
	case string:
		text = typed
	case int:
		return int64(typed), true, nil
	case int64:
		return typed, true, nil
	case float64:
		if typed != float64(int64(typed)) {
			return 0, true, fmt.Errorf("%s: %s must be an integer", domain.ComputeCodeInvalid, name)
		}
		return int64(typed), true, nil
	default:
		return 0, true, fmt.Errorf("%s: %s must be an integer", domain.ComputeCodeInvalid, name)
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed < 0 {
		return 0, true, fmt.Errorf("%s: %s must be a non-negative int64", domain.ComputeCodeInvalid, name)
	}
	return parsed, true, nil
}

func optionalFloatArg(args *argAccessor, name string) (float64, bool, error) {
	if !args.present(name) {
		return 0, false, nil
	}
	value := stringArg(args, name)
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 {
		return 0, true, fmt.Errorf("%s: %s must be a non-negative number", domain.ComputeCodeInvalid, name)
	}
	return parsed, true, nil
}

func computeDistribution(rows []domain.ComputeEvent, by string) (map[string]int, error) {
	if by != "provider" && by != "ticket" && by != "phase" && by != "conservation" {
		return nil, fmt.Errorf("E_SELECTOR_INVALID: unsupported compute distribution field %q", by)
	}
	result := map[string]int{}
	for _, row := range rows {
		value := ""
		switch by {
		case "provider":
			value = row.Provider
		case "ticket":
			value = row.TicketID
		case "phase":
			value = row.Phase
		case "conservation":
			value = string(row.Conservation)
		}
		if value == "" {
			value = "(none)"
		}
		result[value]++
	}
	return result, nil
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
