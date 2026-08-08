// Package core is the transport-neutral AIRA dispatch seam. Adapters provide
// serializable requests and render the returned response; handlers below own
// all store and protocol behaviour.
package core

import (
	"context"
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
	Count(string, string) (store.CountResult, error)
	SetTicket(context.Context, string, string, string) (store.EventKey, error)
	MoveTicket(context.Context, string, domain.Status) (store.EventKey, error)
	Reconcile(context.Context) error
	Rebuild(context.Context) error
	Check(context.Context) (store.CheckReport, error)
}

type handlerData struct {
	Data     any
	Warnings []string
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
	if wrapped, ok := data.(handlerData); ok {
		data, warnings = wrapped.Data, wrapped.Warnings
	}
	if report, ok := data.(store.CheckReport); ok {
		return Response{OK: true, Code: strings.ToUpper(report.Verdict), Data: report, Warnings: warnings, Exit: exitCode(report)}
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
			return c.store.Count(stringArg(args, "query"), stringArg(args, "by"))
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

func projectRecord(record store.TicketRecord, fields []string) map[string]any {
	all := map[string]any{
		"id": record.Ticket.ID, "project": record.Ticket.Project, "title": record.Ticket.Title,
		"status": record.Ticket.Status, "kind": record.Ticket.Kind, "severity": record.Ticket.Severity,
		"assignee": record.Ticket.Assignee, "milestone": record.Ticket.Milestone, "labels": record.Ticket.Labels,
		"hold": record.Ticket.Hold, "relations": record.Ticket.Relations, "body": record.Body, "path": record.Path,
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
		return 0
	}
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
