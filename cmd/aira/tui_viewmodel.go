package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"aira/internal/store"
)

// listEnvelope models the conditional list/ready/find response envelope.
// Distribution and Truncated retain their zero values when the keys are absent.
type listEnvelope struct {
	Total        int              `json:"total"`
	Rows         []map[string]any `json:"rows"`
	Distribution map[string]int   `json:"distribution,omitempty"`
	Truncated    bool             `json:"truncated,omitempty"`
}

type gaugeFetch struct {
	Name      string
	Result    store.GaugeResult
	ErrorCode string
}

func ticketListViewModel(data listEnvelope) panelModel {
	model := panelModel{Headers: []string{"ID", "Status", "Kind", "Severity", "Assignee", "Milestone"}}
	for _, item := range data.Rows {
		hold, _ := item["hold"].(bool)
		model.Rows = append(model.Rows, tableRow{ID: textCell(item["id"]), Hold: hold, Cells: []string{
			textCell(item["id"]), textCell(item["status"]), textCell(item["kind"]), textCell(item["severity"]),
			textCell(item["assignee"]), textCell(item["milestone"]),
		}})
	}
	model.Footer = envelopeFooter(data)
	return model
}

func readyListViewModel(data listEnvelope) panelModel {
	model := panelModel{Headers: []string{"ID", "Status", "Kind", "Severity", "Ready", "Verdict"}}
	for _, item := range data.Rows {
		// The batch `ready` verb returns an authoritative per-row `ready` bool.
		// Present-true → yes, present-false → no. An ABSENT or malformed field is
		// NOT evidence of "not ready" — render it UNEVALUATED, never a fabricated "no".
		ready := "UNEVALUATED"
		style := ""
		if value, ok := item["ready"].(bool); ok {
			if value {
				ready = "yes"
			} else {
				ready = "no"
			}
		} else {
			style = "unevaluated"
		}
		model.Rows = append(model.Rows, tableRow{ID: textCell(item["id"]), Style: style, Cells: []string{
			textCell(item["id"]), textCell(item["status"]), textCell(item["kind"]), textCell(item["severity"]), ready, textCell(item["verdict"]),
		}})
	}
	model.Footer = envelopeFooter(data)
	return model
}

func findingListViewModel(data listEnvelope) panelModel {
	model := panelModel{Headers: []string{"ID", "Ticket", "Severity", "Status", "Evaluation"}}
	for _, item := range data.Rows {
		status := textCell(item["disposition"])
		if status == "" {
			status = textCell(item["verdict"])
		}
		evaluation := "evaluated"
		style := ""
		if value, _ := item["unevaluated"].(bool); value {
			evaluation = "UNEVALUATED"
			if reason := textCell(item["error"]); reason != "" {
				evaluation += ": " + reason
			}
			style = "unevaluated"
		}
		model.Rows = append(model.Rows, tableRow{ID: textCell(item["id"]), Style: style, Cells: []string{
			textCell(item["id"]), textCell(item["ticket"]), textCell(item["severity"]), status, evaluation,
		}})
	}
	model.Footer = envelopeFooter(data)
	return model
}

func leaseListViewModel(rows []store.HeldLeaseRow, tokenSnapshots ...map[string]string) panelModel {
	model := panelModel{Headers: []string{"Ticket", "Actor", "Worktree", "Generation", "TTL", "State", "Age"}}
	var tokens map[string]string
	if len(tokenSnapshots) > 0 {
		tokens = tokenSnapshots[0]
	}
	for _, lease := range rows {
		state := "HELD"
		style := ""
		if lease.AgeNote == "stale (prior boot)" {
			state = "STALE"
			style = "stale"
		} else if lease.Expired {
			state = "EXPIRED"
			style = "expired"
		}
		age := lease.AgeNote
		if age != "" {
			age += " — as of last refresh"
		}
		model.Rows = append(model.Rows, tableRow{ID: lease.TicketID, Style: style, LeaseToken: tokens[lease.TicketID], LeaseVersion: lease.Generation, Cells: []string{
			lease.TicketID, lease.Actor, lease.WorktreeID, fmt.Sprint(lease.Generation), time.Duration(lease.TTLNanos).String(), state, age,
		}})
	}
	return model
}

func insightViewModel(fetches []gaugeFetch) panelModel {
	model := panelModel{}
	for _, fetch := range fetches {
		name := fetch.Name
		if name == "" {
			name = fetch.Result.Name
		}
		tile := gaugeTile{Name: name}
		switch {
		case fetch.ErrorCode != "":
			tile.Value = "ERROR"
			tile.ErrorCode = fetch.ErrorCode
		case fetch.Result.Unevaluated:
			tile.Value = "UNEVALUATED"
			tile.Unevaluated = true
			tile.Reason = fetch.Result.UnevaluatedReason
		default:
			tile.Value = gaugeValue(fetch.Result)
			tile.Direction = fetch.Result.Direction
			if fetch.Result.Baseline != nil {
				tile.Baseline = honestJSONValue(fetch.Result.Baseline)
			}
		}
		model.Tiles = append(model.Tiles, tile)
	}
	return model
}

func gaugeValue(result store.GaugeResult) string {
	value := result.Value
	if value == nil && result.Breakdown != nil {
		value = result.Breakdown
	}
	if value == nil && result.Fields != nil {
		value = result.Fields
	}
	if value == nil && result.Distributions != nil {
		value = result.Distributions
	}
	if value == nil {
		return "(no observed value)"
	}
	return strings.TrimSpace(honestJSONValue(value) + " " + string(result.Kind))
}

func honestJSONValue(value any) string {
	raw, err := json.Marshal(value)
	if err == nil {
		return strings.Trim(string(raw), `"`)
	}
	return fmt.Sprint(value)
}

func eventViewModel(events []store.WatchEvent) panelModel {
	model := panelModel{Headers: []string{"Seq", "At", "Actor", "Verb", "Target"}}
	for _, event := range events {
		model.Rows = append(model.Rows, tableRow{Cells: []string{
			fmt.Sprint(event.Seq), event.At, event.Actor, event.Verb, event.Target,
		}})
	}
	return model
}

func envelopeFooter(data listEnvelope) string {
	parts := make([]string, 0, 2)
	if data.Truncated {
		parts = append(parts, fmt.Sprintf("TRUNCATED: showing %d of %d", len(data.Rows), data.Total))
	}
	if data.Distribution != nil {
		keys := make([]string, 0, len(data.Distribution))
		for key := range data.Distribution {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		cells := make([]string, 0, len(keys))
		for _, key := range keys {
			cells = append(cells, fmt.Sprintf("%s=%d", key, data.Distribution[key]))
		}
		parts = append(parts, "distribution: "+strings.Join(cells, ", "))
	}
	return strings.Join(parts, " | ")
}

func textCell(value any) string {
	if value == nil {
		return ""
	}
	switch value := value.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		raw, err := json.Marshal(value)
		if err == nil && string(raw) != "null" {
			return strings.Trim(string(raw), `"`)
		}
		return fmt.Sprint(value)
	}
}
