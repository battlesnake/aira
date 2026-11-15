package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-342. The one formatter of a palette or row action's daemon reply. The
// shown text is built ONLY from what the reply carries: a missing field is left
// out, never shown as 0 or empty, and a change is stated only when the reply
// carries both its before and after values. The raw reply goes to Details,
// shown behind the dialog's `d` toggle.

// verdictMeaning is the verdict vocabulary a successful reply may carry in
// place of OK (core.Do: a check report or a handler verdict).
var verdictMeaning = map[string]string{
	"PASS":        "the result is a pass",
	"FAIL":        "the result is a fail",
	"UNEVALUATED": "the daemon could not establish a result, so it is not a pass",
}

// appliedResult words a reply the daemon answered with OK or a verdict. A
// check that established a fail and could not establish the rest also carries
// UNEVALUATED (core.Do, exit 1): its report's own verdict keeps the fail.
func appliedResult(response core.Response, data json.RawMessage) paletteResult {
	head := "APPLIED: the daemon accepted the request."
	if meaning, ok := verdictMeaning[response.Code]; ok {
		var fields map[string]json.RawMessage
		if response.Code == "UNEVALUATED" && json.Unmarshal(data, &fields) == nil && strings.EqualFold(replyString(fields, "verdict"), "fail") {
			meaning = "the result is a fail, and the daemon could not establish the rest"
		}
		head = "VERDICT " + response.Code + ": " + meaning + "."
	}
	return paletteResult{Outcome: paletteApplied, Text: replyText(head, data, response.Warnings), Details: rawReply(response)}
}

// rejectedResult words a daemon refusal: its code and message as the CLI
// prints them. It claims nothing about state: a handler may fail after a write.
func rejectedResult(response core.Response, data json.RawMessage) paletteResult {
	head := "REJECTED: the daemon refused the request.\n" + replyError(response)
	return paletteResult{Outcome: paletteRejected, Text: replyText(head, data, response.Warnings), Details: rawReply(response)}
}

// unconfirmedResult words U_CONFINE_KILL_UNCONFIRMED (AIRA-271): the daemon
// may have written cgroup.kill but could not prove the job ended, so this is
// neither a refusal nor a kill. The outcome is unknown, so the views refresh.
func unconfirmedResult(response core.Response, data json.RawMessage) paletteResult {
	head := "UNEVALUATED — outcome unknown\n" + replyError(response)
	return paletteResult{Outcome: paletteOutcomeUnknown, Text: replyText(head, data, response.Warnings), Details: rawReply(response)}
}

// replyError is a refusal's code and message as the CLI prints them.
func replyError(response core.Response) string {
	message := strings.TrimSpace(response.Error)
	if !strings.HasPrefix(message, response.Code) {
		message = strings.TrimSpace(response.Code + ": " + message)
	}
	return message
}

// replyText is head, then one line per field the reply carries that an
// operator reads, then one line per entry of its result and finding lists (a
// gate check's Results, a check's findings: where a verdict's reason is), then
// its warnings.
func replyText(head string, data json.RawMessage, warnings []string) string {
	lines := []string{head}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) == nil {
		if name := replyString(fields, "name"); name != "" {
			if owner := replyString(fields, "owner"); owner != "" {
				name += " (owner " + owner + ")"
			}
			lines = append(lines, "Name: "+name)
		}
		for _, key := range []string{"id", "status", "phase", "code", "message", "reason", "error", "hint"} {
			if value := replyString(fields, key); value != "" {
				lines = append(lines, strings.ToUpper(key[:1])+key[1:]+": "+value)
			}
		}
		var before, after *int64
		if json.Unmarshal(fields["old_reserve_bytes"], &before) == nil && json.Unmarshal(fields["new_reserve_bytes"], &after) == nil && before != nil && after != nil {
			lines = append(lines, "Reserve: "+runner.FormatConfineMegabytes(*before)+" → "+runner.FormatConfineMegabytes(*after))
		}
		lines = append(lines, replyEntries(fields)...)
	}
	for _, warning := range warnings {
		lines = append(lines, "Warning: "+warning)
	}
	return strings.Join(lines, "\n")
}

// replyEntryLimit is how many result and finding lines a reply shows; the rest
// are counted, and Details (`d`) has them all.
const replyEntryLimit = 5

// replyEntries is one `• ` line per entry of the reply's result and finding
// lists, those that are not a pass first (they carry a verdict's reason), at
// most replyEntryLimit of them, then how many more there are.
func replyEntries(fields map[string]json.RawMessage) []string {
	var reasons, passes []string
	for _, list := range []string{"results", "findings", "unevaluated_findings", "warnings"} {
		var entries []map[string]json.RawMessage
		if json.Unmarshal(replyField(fields, list), &entries) != nil {
			continue
		}
		for _, entry := range entries {
			line := replyEntry(entry)
			if list == "warnings" && line != "" {
				line = "warning: " + line
			}
			switch {
			case line == "":
			case strings.EqualFold(replyString(entry, "verdict"), "pass"):
				passes = append(passes, "• "+line)
			default:
				reasons = append(reasons, "• "+line)
			}
		}
	}
	all := append(reasons, passes...)
	if len(all) <= replyEntryLimit {
		return all
	}
	return append(all[:replyEntryLimit:replyEntryLimit], fmt.Sprintf("• … and %d more (d shows them)", len(all)-replyEntryLimit))
}

// replyEntry is one result or finding: what it is about, then its verdict,
// code, message and refusing bindings, each only when the entry carries it.
func replyEntry(entry map[string]json.RawMessage) string {
	var parts []string
	for _, key := range []string{"verdict", "code", "message"} {
		if value := replyString(entry, key); value != "" {
			parts = append(parts, value)
		}
	}
	var mismatch []string
	if json.Unmarshal(replyField(entry, "mismatch"), &mismatch) == nil && len(mismatch) > 0 {
		parts = append(parts, "(does not match: "+strings.Join(mismatch, ", ")+")")
	}
	line := strings.Join(parts, " ")
	for _, key := range []string{"gateid", "subject"} {
		if about := replyString(entry, key); about != "" && line != "" {
			return about + ": " + line
		}
	}
	return line
}

// replyField is the field named key, matched without regard to case (typed
// replies keep Go field names, maps use snake_case); nil when absent.
func replyField(fields map[string]json.RawMessage, key string) json.RawMessage {
	for name, raw := range fields {
		if strings.EqualFold(name, key) {
			return raw
		}
	}
	return nil
}

// replyString is the field named key when it is a string, trimmed; "" otherwise.
func replyString(fields map[string]json.RawMessage, key string) string {
	var value string
	if json.Unmarshal(replyField(fields, key), &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// rawReply is the reply envelope as indented JSON, for Details.
func rawReply(response core.Response) string {
	raw, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}
