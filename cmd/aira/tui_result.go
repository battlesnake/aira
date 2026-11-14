package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-342. The ONE formatter of what a palette or row action did. Every result
// the TUI shows (applied, rejected, outcome unknown, or refused before sending)
// becomes paletteResult.Text: plain sentences that say what happened to which
// job, and for a refusal the stable code, the message with its byte counts shown
// as sizes, and what to do about it. The raw payload is kept in Details, shown
// only behind the dialog's `d` toggle.
//
// The outcome labels stay the classifier's mutually exclusive APPLIED /
// REJECTED / UNEVALUATED words: they are what the honesty tests pin, and they
// lead every result, so the sentence after them can never be read as a
// different verdict.

// paletteRemedies is what to do about a refusal, for the codes an `aira top`
// row action (or the input that feeds one) can produce. A code with no entry
// gets no remedy line rather than a generic guess.
var paletteRemedies = map[string]string{
	runner.CodeConfineOwnerUnverified:    "it is another session's job (or this session's identity is not attested), so the change has to override the owner check: aira top does that when you confirm a job that is not yours; from the CLI add --steal.",
	runner.CodeConfineNotFound:           "the job has finished, or left the queue, since the screen was drawn: refresh (r) and pick it again.",
	runner.CodeConfineNotQueued:          "the job is no longer waiting (it has been admitted, or it is a sub-reservation that follows its parent), so there is nothing to push.",
	runner.CodeConfineNotRunning:         "the job holds no reserve of its own to lower. If it is still waiting for admission, push it to the front (f) or wait until it runs; if it is a worker or sub-reservation, its reserve follows its parent job, so lower the parent's reserve instead.",
	runner.CodeConfineResizeNotReduction: "a resize only ever lowers a reserve: enter a size smaller than the job's current reserve.",
	runner.CodeConfineResizeBelowUsage:   "the job already uses more than that: enter a size at or above its live memory use.",
	"E_CONFINE_ARGUMENT_INVALID":         "sizes look like 512M, 2G or 1.5G (the unit is required) and must be at least 1M.",
	"E_TUI_INLINE_ROW_KIND":              "f works on jobs still waiting in the queue, e on running jobs: pick a row of the right kind.",
}

// paletteRemedy is what to do about a refusal: the code's entry, except for
// the one code whose remedy depends on its cause. A resize refused for an
// unreadable live memory use is permanent in ci-shim mode (no job has a cgroup
// of its own there), so retrying is offered only for the causes a retry can
// clear.
func paletteRemedy(code, message string) string {
	if code != runner.CodeConfineResizeUsageUnevaluated {
		return paletteRemedies[code]
	}
	switch {
	case strings.Contains(message, runner.ConfineUsageReasonShim):
		return "this daemon runs in ci-shim mode, where jobs have no cgroup of their own, so it can never read a job's memory use: lowering a reserve is not available in this mode, and retrying will not help."
	case strings.Contains(message, runner.ConfineUsageReasonNoScope):
		return "the job is still starting or has just ended: refresh (r), and if it is still running, try again in a moment."
	default:
		return "the daemon could not read the job's live memory use this time, so it refused rather than guess: try again in a moment."
	}
}

// appliedResult words a request the daemon applied; data is its reply payload.
func appliedResult(request core.Request, data json.RawMessage) paletteResult {
	return paletteResult{Outcome: paletteApplied, Text: "APPLIED — " + appliedSentence(request, data), Details: prettyPayload(data)}
}

// rejectedResult words a refusal. effect is what the refusal establishes about
// the request, and only that: "nothing was sent" for a request that never left
// this process; empty for a daemon error, which establishes NOTHING about
// whether a change was made (a handler may fail after its write, e.g. `set`
// rereads the ticket after writing it), so no rollback is claimed.
func rejectedResult(effect, code, message string) paletteResult {
	return paletteResult{Outcome: paletteRejected, Text: refusalText("REJECTED", effect, code, message), Details: rawRefusal(code, message)}
}

// unknownResult words an outcome this face cannot establish. It never guesses:
// the request may or may not have taken effect, and it says so.
func unknownResult(reason string) paletteResult {
	return paletteResult{
		Outcome: paletteOutcomeUnknown,
		Text:    "UNEVALUATED — outcome unknown: " + reason + ".\nThe request may or may not have taken effect; refresh (r) to check.",
		Details: reason,
	}
}

// clientRefusalResult words a refusal the TUI made itself, before sending
// anything (an invalid size, an action on the wrong kind of row): text is the
// usual "CODE: message" error string.
func clientRefusalResult(text string) paletteResult {
	code, message := splitCodedError(text)
	return paletteResult{Outcome: paletteRejected, Text: refusalText("NOT SENT", "nothing was sent", code, message), Details: rawRefusal(code, message)}
}

// pendingResult is the placeholder shown while a request is in flight.
func pendingResult() paletteResult {
	return paletteResult{Text: "Sending the request…"}
}

func refusalText(label, effect, code, message string) string {
	var out strings.Builder
	out.WriteString(label)
	if code != "" {
		fmt.Fprintf(&out, " (%s)", code)
	}
	if effect != "" {
		out.WriteString(": " + effect)
	}
	out.WriteString(".")
	if message = humanizeDaemonMessage(code, message); message != "" {
		out.WriteString("\n" + message)
	}
	if remedy := paletteRemedy(code, message); remedy != "" {
		out.WriteString("\nWhat to do: " + remedy)
	}
	return out.String()
}

func rawRefusal(code, message string) string {
	if code == "" {
		return message
	}
	return "code: " + code + "\nmessage: " + message
}

// splitCodedError splits "E_CODE: message" into its code and message. Text with
// no leading code is all message.
func splitCodedError(text string) (string, string) {
	text = strings.TrimSpace(text)
	head, rest, found := strings.Cut(text, ":")
	if found && codedErrorPattern.MatchString(head) {
		return head, strings.TrimSpace(rest)
	}
	return "", text
}

var (
	codedErrorPattern = regexp.MustCompile(`^[EU]_[A-Z0-9_]+$`)
	scopeIDPattern    = regexp.MustCompile(`CONFINE-[^\s;,()]+`)
	// byteCountPattern finds the byte counts the confine refusals print: "N
	// bytes", "(asked for N)" and "reserve (N)".
	byteCountPattern = regexp.MustCompile(`(\d+) bytes|\(asked for (\d+)\)|reserve \((\d+)\)`)
)

// humanizeDaemonMessage makes a refusal's message readable without changing what
// it says: the duplicated leading code goes, a confine scope id becomes its job
// name, and a byte count becomes a size. The untouched text stays in Details.
func humanizeDaemonMessage(code, message string) string {
	message = strings.TrimSpace(message)
	if code != "" {
		message = strings.TrimSpace(strings.TrimPrefix(message, code+":"))
	}
	message = scopeIDPattern.ReplaceAllStringFunc(message, func(scopeID string) string {
		if name, _, _, _, ok := runner.ParseConfineScopeID(scopeID); ok && name != "" {
			return name
		}
		return scopeID
	})
	return byteCountPattern.ReplaceAllStringFunc(message, func(match string) string {
		groups := byteCountPattern.FindStringSubmatch(match)
		for index, digits := range groups[1:] {
			if digits == "" {
				continue
			}
			value, err := strconv.ParseInt(digits, 10, 64)
			if err != nil {
				return match
			}
			size := humanSize(value)
			switch index {
			case 0:
				return size
			case 1:
				return "(asked for " + size + ")"
			default:
				return "reserve (" + size + ")"
			}
		}
		return match
	})
}

// humanSize shows a byte count the way an operator types one, and never hides a
// difference: a whole or half GiB is 2G or 1.5G, any other whole MiB is 2047M,
// and a count that is not a whole MiB (a live memory reading) is marked as
// rounded, ~1177.4M. Two different MiB-aligned sizes therefore never print alike
// ("2047M → 2046M", not "2G → 2G").
func humanSize(value int64) string {
	const (
		kib = int64(1) << 10
		mib = int64(1) << 20
		gib = int64(1) << 30
	)
	switch {
	case value >= gib && value%(gib/2) == 0:
		text := strconv.FormatFloat(float64(value)/float64(gib), 'f', 1, 64)
		return strings.TrimSuffix(text, ".0") + "G"
	case value >= mib && value%mib == 0:
		return strconv.FormatInt(value/mib, 10) + "M"
	case value >= mib:
		return "~" + strconv.FormatFloat(float64(value)/float64(mib), 'f', 1, 64) + "M"
	case value >= kib && value%kib == 0:
		return strconv.FormatInt(value/kib, 10) + "K"
	default:
		return strconv.FormatInt(value, 10) + "B"
	}
}

// appliedSentence says what an applied request did. The two `aira top` row
// actions say it in their own words; every other verb names itself, its target
// and what it sent, and its reply stays behind `d`.
func appliedSentence(request core.Request, data json.RawMessage) string {
	switch request.Verb {
	case "confine-resize":
		var result runner.ConfineResizeResult
		if json.Unmarshal(data, &result) == nil && result.Name != "" {
			freed := result.OldReserveBytes - result.NewReserveBytes
			text := fmt.Sprintf("the reserve of %s (owner %s) is now %s, down from %s", result.Name, result.Owner, humanSize(result.NewReserveBytes), humanSize(result.OldReserveBytes))
			if freed > 0 {
				text += "; " + humanSize(freed) + " went back to the pool for queued jobs"
			}
			return text + ".\nIts memory cap is unchanged, and a daemon restart restores the original reserve."
		}
	case "confine-front":
		var result runner.ConfineFrontResult
		if json.Unmarshal(data, &result) == nil && result.Name != "" {
			return fmt.Sprintf("%s (owner %s) is now first in the admission queue: it is admitted as soon as it fits, and every job behind it waits until then.", result.Name, result.Owner)
		}
	}
	verb := request.Verb
	if operation, ok := request.Args["subverb"].(string); ok && operation != "" {
		verb += " " + operation
	}
	if selector, ok := request.Args["selector"].(string); ok && selector != "" {
		verb += " " + selector
	}
	// Any other verb's reply is a record, not a sentence: one sentence names
	// what the request was about (the id the daemon gave it, when that is not
	// the selector already said, e.g. a created ticket's) and the values it
	// sent, and the reply itself stays in Details, never an outline of it.
	text := verb + " succeeded"
	selector, _ := request.Args["selector"].(string)
	if id := replyID(data); id != "" && id != selector {
		text += ", id " + id
	}
	if change := requestChange(request.Args); change != "" {
		text += " (" + change + ")"
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return text + "."
	}
	return text + ". Press d to see what the daemon returned."
}

// replyID is the id a reply names: its own "id", else its record's or
// ticket's. Empty when it names none.
func replyID(data json.RawMessage) string {
	var reply struct {
		ID     any `json:"id"`
		Record struct {
			ID any `json:"id"`
		} `json:"record"`
		Ticket struct {
			ID any `json:"id"`
		} `json:"ticket"`
	}
	if json.Unmarshal(data, &reply) != nil {
		return ""
	}
	for _, id := range []any{reply.ID, reply.Record.ID, reply.Ticket.ID} {
		if text, ok := id.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// requestChange is what a request sent, as "name = value" pairs in name order:
// `set`'s field and value read "status = done"; the selector and operation are
// already named. Each value is on one line and at most requestValueRunes long.
func requestChange(args map[string]any) string {
	names := make([]string, 0, len(args))
	for name := range args {
		if name != "selector" && name != "subverb" && name != "field" && name != "value" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var pairs []string
	if field, ok := args["field"].(string); ok && field != "" {
		pairs = append(pairs, field+" = "+requestValue(args["value"]))
	}
	for _, name := range names {
		pairs = append(pairs, name+" = "+requestValue(args[name]))
	}
	return strings.Join(pairs, ", ")
}

const requestValueRunes = 40

func requestValue(value any) string {
	var text string
	switch typed := value.(type) {
	case nil:
		text = ""
	case string:
		text = typed
	case []string:
		text = strings.Join(typed, ",")
	case []any:
		parts := make([]string, len(typed))
		for index, item := range typed {
			parts[index] = fmt.Sprint(item)
		}
		text = strings.Join(parts, ",")
	default:
		text = fmt.Sprint(typed)
	}
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > requestValueRunes {
		text = string(runes[:requestValueRunes-1]) + "…"
	}
	if text == "" || strings.ContainsAny(text, " ,()") {
		return strconv.Quote(text)
	}
	return text
}

// prettyPayload is the raw payload for Details: indented JSON, which is what an
// operator who asked for the raw reply wants to see.
func prettyPayload(data json.RawMessage) string {
	if len(bytes.TrimSpace(data)) == 0 {
		return ""
	}
	var out bytes.Buffer
	if json.Indent(&out, data, "", "  ") != nil {
		return string(data)
	}
	return out.String()
}
