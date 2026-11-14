package main

import (
	"encoding/json"
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
	"UNEVALUATED": "the daemon could not establish a result, so it is not a pass and not a fail",
}

// appliedResult words a reply the daemon answered with OK or a verdict.
func appliedResult(response core.Response, data json.RawMessage) paletteResult {
	head := "APPLIED: the daemon accepted the request."
	if meaning, ok := verdictMeaning[response.Code]; ok {
		head = "VERDICT " + response.Code + ": " + meaning + "."
	}
	return paletteResult{Outcome: paletteApplied, Text: replyText(head, data, response.Warnings), Details: rawReply(response)}
}

// rejectedResult words a daemon refusal: its code and message as the CLI
// prints them. It claims nothing about state: a handler may fail after a write.
func rejectedResult(response core.Response, data json.RawMessage) paletteResult {
	message := strings.TrimSpace(response.Error)
	if !strings.HasPrefix(message, response.Code) {
		message = strings.TrimSpace(response.Code + ": " + message)
	}
	head := "REJECTED: the daemon refused the request.\n" + message
	return paletteResult{Outcome: paletteRejected, Text: replyText(head, data, response.Warnings), Details: rawReply(response)}
}

// replyText is head, then one line per field the reply carries that an
// operator reads, then its warnings.
func replyText(head string, data json.RawMessage, warnings []string) string {
	lines := []string{head}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) == nil {
		text := func(key string) string {
			var value string
			for name, raw := range fields {
				if strings.EqualFold(name, key) && json.Unmarshal(raw, &value) == nil {
					return strings.TrimSpace(value)
				}
			}
			return ""
		}
		if name := text("name"); name != "" {
			if owner := text("owner"); owner != "" {
				name += " (owner " + owner + ")"
			}
			lines = append(lines, "Name: "+name)
		}
		for _, key := range []string{"id", "status", "code", "message", "reason", "error", "hint"} {
			if value := text(key); value != "" {
				lines = append(lines, strings.ToUpper(key[:1])+key[1:]+": "+value)
			}
		}
		var before, after *int64
		if json.Unmarshal(fields["old_reserve_bytes"], &before) == nil && json.Unmarshal(fields["new_reserve_bytes"], &after) == nil && before != nil && after != nil {
			lines = append(lines, "Reserve: "+runner.FormatConfineMegabytes(*before)+" → "+runner.FormatConfineMegabytes(*after))
		}
	}
	for _, warning := range warnings {
		lines = append(lines, "Warning: "+warning)
	}
	return strings.Join(lines, "\n")
}

// rawReply is the reply envelope as indented JSON, for Details.
func rawReply(response core.Response) string {
	raw, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}
