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

type Core struct {
	store       *store.Store
	initializer Initializer
	verbs       map[string]verbSpec
}

type verbSpec struct {
	Name  string
	Usage string
	Run   func(context.Context, map[string]any) (any, error)
}

const ListLimit = store.ListLimit

func New(s *store.Store) *Core {
	c := &Core{store: s}
	c.verbs = c.dispatchTable()
	return c
}

func NewWithInitializer(s *store.Store, initializer Initializer) *Core {
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
		return Response{Code: "E_SELECTOR_INVALID", Error: fmt.Sprintf("unknown verb %q", req.Verb)}
	}
	data, err := spec.Run(ctx, req.Args)
	if err != nil {
		code := store.ErrorCode(err)
		return Response{Code: code, Error: err.Error(), Exit: errorExit(code)}
	}
	if report, ok := data.(store.CheckReport); ok {
		return Response{OK: true, Code: strings.ToUpper(report.Verdict), Data: report, Exit: exitCode(report)}
	}
	return Response{OK: true, Code: "OK", Data: data}
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
			ticket, err := c.store.CreateTicket(ctx, input)
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": ticket.ID, "path": ".aira/tickets/" + ticket.ID + ".md", "ticket": ticket}, nil
		}},
		"show": {Name: "show", Usage: "show <selector>", Run: func(_ context.Context, args map[string]any) (any, error) {
			record, err := c.store.Get(stringArg(args, "selector"))
			if err != nil {
				return nil, err
			}
			return projectRecord(record, stringSlice(args, "fields")), nil
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
			return data, nil
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
			if err := c.store.SetTicket(ctx, record.Ticket.ID, field, value); err != nil {
				return nil, err
			}
			updated, err := c.store.Get(record.Ticket.ID)
			if err != nil {
				return nil, err
			}
			return projectRecord(updated, nil), nil
		}},
		"mv": {Name: "mv", Usage: "mv <selector> <status>", Run: func(ctx context.Context, args map[string]any) (any, error) {
			record, err := c.store.Get(stringArg(args, "selector"))
			if err != nil {
				return nil, err
			}
			if err := c.store.MoveTicket(ctx, record.Ticket.ID, domain.Status(stringArg(args, "status"))); err != nil {
				return nil, err
			}
			return map[string]any{"id": record.Ticket.ID, "status": stringArg(args, "status")}, nil
		}},
		"reconcile": {Name: "reconcile", Usage: "reconcile [--rebuild]", Run: func(ctx context.Context, args map[string]any) (any, error) {
			if err := c.store.Reconcile(ctx); err != nil {
				return nil, err
			}
			if err := c.store.Rebuild(ctx); err != nil {
				return nil, err
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
	switch code {
	case "E_CONFIG_MISSING", "E_CONFIG_INVALID", "E_NOT_PROJECT", "E_SELECTOR_INVALID", "E_NOT_FOUND", "E_SELECTOR_AMBIGUOUS":
		return 2
	case "E_DB_BUSY", "E_DB_CORRUPT", "E_RECONCILE_REQUIRED", "E_INTERNAL", "E_JOURNAL_CORRUPT":
		return 4
	default:
		return 1
	}
}
