// Package core is the transport-neutral AIRA dispatch seam. Adapters provide
// serializable requests and render the returned response; handlers below own
// all store and protocol behaviour.
package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"aira/internal/domain"
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

type Store interface {
	AllocateID(context.Context, string) (string, error)
	CreateTicketWithEvent(context.Context, domain.CreateTicketInput) (domain.Ticket, store.EventKey, error)
	Get(string) (store.TicketRecord, error)
	List(string) ([]store.TicketRecord, error)
	AddFinding(context.Context, domain.ReviewFindingInput) (domain.Finding, store.EventKey, error)
	ListFindings(string) ([]store.FindingRecord, error)
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

type handlerData struct {
	Data     any
	Warnings []string
	Verdict  string
}

type Core struct {
	store       Store
	initializer Initializer
	verbs       map[string]verbSpec
}

type verbSpec struct {
	Name  string
	Usage string
	Run   func(context.Context, map[string]any) (any, error)
}

const ListLimit = store.ListLimit

func New(s Store) *Core {
	c := &Core{store: s}
	c.verbs = c.dispatchTable()
	return c
}

func NewWithInitializer(s Store, initializer Initializer) *Core {
	c := New(s)
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
	data, err := spec.Run(ctx, req.Args)
	if err != nil {
		code := store.ErrorCode(err)
		return Response{Code: code, Error: err.Error(), Exit: errorExit(code)}
	}
	warnings := []string(nil)
	verdict := ""
	if wrapped, ok := data.(handlerData); ok {
		data, warnings, verdict = wrapped.Data, wrapped.Warnings, wrapped.Verdict
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

func (c *Core) dispatchTable() map[string]verbSpec {
	return map[string]verbSpec{
		"help": {Name: "help", Usage: "help", Run: func(context.Context, map[string]any) (any, error) { return c.Help(), nil }},
		"init": {Name: "init", Usage: "init", Run: func(ctx context.Context, args map[string]any) (any, error) {
			if c.initializer == nil {
				return nil, fmt.Errorf("E_CONFIG_INVALID: init is unavailable without a project initializer")
			}
			return c.initializer(ctx, args)
		}},
		"id": {Name: "id", Usage: "id <prefix>", Run: func(ctx context.Context, args map[string]any) (any, error) {
			prefix := stringArg(args, "prefix")
			id, err := c.store.AllocateID(ctx, prefix)
			return map[string]any{"id": id}, err
		}},
		"create": {Name: "create", Usage: "create <title> [--kind K --severity S --label L --body B]", Run: func(ctx context.Context, args map[string]any) (any, error) {
			input := domain.CreateTicketInput{Title: stringArg(args, "title"), Body: stringArg(args, "body"), Kind: domain.Kind(stringArg(args, "kind")), Severity: domain.Severity(stringArg(args, "severity")), Labels: stringSlice(args, "labels")}
			ticket, event, err := c.store.CreateTicketWithEvent(ctx, input)
			if err != nil {
				return nil, err
			}
			return mutationData(map[string]any{"id": ticket.ID, "path": ".aira/tickets/" + ticket.ID + ".md", "ticket": ticket}, event), nil
		}},
		"show": {Name: "show", Usage: "show <selector>", Run: func(_ context.Context, args map[string]any) (any, error) {
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
		"grep": {Name: "grep", Usage: "grep <query> [--kind ticket|finding --by kind --fields F,...]", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"find": {Name: "find", Usage: "find add|ls|show|set ...", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"claim": {Name: "claim", Usage: "claim <id> [--steal --actor NAME]", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"release": {Name: "release", Usage: "release <id>", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"heartbeat": {Name: "heartbeat", Usage: "heartbeat <id>", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"touch": {Name: "touch", Usage: "touch <id> <glob...>", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"link": {Name: "link", Usage: "link <from> <kind> <to> | link ls <id>", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"unlink": {Name: "unlink", Usage: "unlink <from> <kind> <to>", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"ready": {Name: "ready", Usage: "ready [<selector>] | ready --list", Run: func(_ context.Context, args map[string]any) (any, error) {
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
		"list": {Name: "list", Usage: "list [query] [--by F] [--fields F,...]", Run: func(_ context.Context, args map[string]any) (any, error) {
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
		"count": {Name: "count", Usage: "count [query] --by F", Run: func(ctx context.Context, args map[string]any) (any, error) {
			result, err := c.store.Count(stringArg(args, "query"), stringArg(args, "by"))
			return handlerData{Data: result, Warnings: result.Warnings}, err
		}},
		"set": {Name: "set", Usage: "set <selector> <field=value>", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"mv": {Name: "mv", Usage: "mv <selector> <status>", Run: func(ctx context.Context, args map[string]any) (any, error) {
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
		"reconcile": {Name: "reconcile", Usage: "reconcile [--rebuild]", Run: func(ctx context.Context, args map[string]any) (any, error) {
			if err := c.store.Reconcile(ctx); err != nil {
				return nil, err
			}
			if boolArg(args, "rebuild") {
				if err := c.store.Rebuild(ctx); err != nil {
					return nil, err
				}
			}
			return map[string]any{"reconciled": true}, nil
		}},
		"check": {Name: "check", Usage: "check", Run: func(ctx context.Context, _ map[string]any) (any, error) {
			return c.store.Check(ctx)
		}},
	}
}

func (c *Core) Help() []map[string]string {
	result := make([]map[string]string, 0, len(c.verbs))
	for _, spec := range c.verbs {
		result = append(result, map[string]string{"verb": spec.Name, "usage": spec.Usage})
	}
	sort.Slice(result, func(i, j int) bool { return result[i]["verb"] < result[j]["verb"] })
	return result
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return value
}

func intArg(args map[string]any, key string) int {
	if args == nil {
		return 0
	}
	if value, ok := args[key].(int); ok {
		return value
	}
	if value, ok := args[key].(float64); ok {
		return int(value)
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

func boolArg(args map[string]any, key string) bool {
	value, _ := args[key].(bool)
	return value
}

func stringSlice(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	value := args[key]
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

func findingDistribution(records []store.FindingRecord, by string) map[string]int {
	result := map[string]int{}
	for _, record := range records {
		for _, value := range storeFindingDistributionValues(record.Finding, by) {
			result[value]++
		}
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
