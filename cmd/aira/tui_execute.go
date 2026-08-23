package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"aira/internal/core"
	"aira/internal/daemon"
)

type executeEntry struct {
	Verb        string
	Summary     string
	Enabled     bool
	PrintOnly   bool
	Unavailable string
}

type executeLaunch struct {
	Entry       executeEntry
	Request     core.Request
	ConfineText string
}

type executeReport struct {
	Execution   string
	Persistence string
}

func (r executeReport) String() string {
	if r.Persistence == "" {
		return "execution: " + r.Execution
	}
	return "execution: " + r.Execution + "\npersistence: " + r.Persistence
}

// buildExecuteList is deliberately an allowlist. SafetyExecute metadata is not
// an admission predicate: run-input and run-kill share that metadata and must
// never appear in this foreground launcher.
func buildExecuteList(descriptors []core.DispatchDescriptor, canExecute bool) []executeEntry {
	byName := make(map[string]core.DispatchDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byName[descriptor.Name] = descriptor
	}
	entries := make([]executeEntry, 0, 4)
	for _, verb := range []string{"run", "git", "time"} {
		descriptor, ok := byName[verb]
		if !ok || descriptor.Safety != core.SafetyExecute {
			continue
		}
		entry := executeEntry{Verb: verb, Summary: descriptor.Summary, Enabled: canExecute}
		if !canExecute {
			entry.Unavailable = "execute unavailable: no terminal dispatcher"
		}
		entries = append(entries, entry)
	}
	if descriptor, ok := byName["confine"]; ok {
		entries = append(entries, executeEntry{Verb: "confine", Summary: descriptor.Summary, Enabled: true, PrintOnly: true})
	}
	return entries
}

func onExecuteOpen(state tuiState) tuiState {
	state = cloneTUIState(state)
	if state.PaletteOpen || state.PaletteDispatching || state.ExecuteRunning {
		return state
	}
	state.ExecuteOpen = true
	state.ExecuteSelected = nil
	state.ExecuteConfirm = nil
	state.ExecuteError = ""
	return state
}

func onExecuteSelect(state tuiState, entry executeEntry) tuiState {
	state = cloneTUIState(state)
	if !state.ExecuteOpen || state.ExecuteRunning {
		return state
	}
	if !entry.Enabled {
		state.ExecuteError = entry.Unavailable
		state.ExecuteSelected = nil
		return state
	}
	selected := entry
	state.ExecuteSelected = &selected
	state.ExecuteConfirm = nil
	state.ExecuteError = ""
	return state
}

func onExecuteSubmit(state tuiState, line string) (tuiState, error) {
	state = cloneTUIState(state)
	if !state.ExecuteOpen || state.ExecuteRunning || state.ExecuteSelected == nil {
		return state, errors.New("execute selection is unavailable")
	}
	entry := *state.ExecuteSelected
	if !entry.Enabled {
		state.ExecuteError = entry.Unavailable
		return state, errors.New(entry.Unavailable)
	}
	launch := executeLaunch{Entry: entry}
	var err error
	if entry.PrintOnly {
		launch.ConfineText, err = renderConfineCommand(line)
	} else {
		launch.Request, err = parseExecuteRequest(entry.Verb, line)
	}
	if err != nil {
		state.ExecuteError = err.Error()
		return state, err
	}
	state.ExecuteError = ""
	state.ExecuteConfirm = &launch
	return state, nil
}

func onExecuteConfirm(state tuiState) (tuiState, *executeLaunch) {
	state = cloneTUIState(state)
	if state.ExecuteRunning || state.ExecuteConfirm == nil {
		return state, nil
	}
	launch := cloneExecuteLaunch(*state.ExecuteConfirm)
	state.ExecuteRunning = true
	state.ExecuteConfirm = nil
	return state, &launch
}

func onExecuteCancel(state tuiState) tuiState {
	state = cloneTUIState(state)
	if state.ExecuteRunning {
		return state
	}
	state.ExecuteOpen = false
	state.ExecuteSelected = nil
	state.ExecuteConfirm = nil
	state.ExecuteError = ""
	return state
}

func onExecuteComplete(state tuiState) tuiState {
	state = cloneTUIState(state)
	state.ExecuteRunning = false
	state.ExecuteOpen = false
	state.ExecuteSelected = nil
	state.ExecuteConfirm = nil
	state.ExecuteError = ""
	return state
}

// onExecuteResume keeps ExecuteRunning set while scheduling the complete
// source-of-truth refresh. The runtime clears it only after Sync and command
// submission, which keeps the signal swallow guard active through resume.
func onExecuteResume(state tuiState) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	var commands []tuiCmd
	for _, view := range dataViews {
		var refresh []tuiCmd
		state, refresh = requestPanelRefresh(state, view)
		commands = append(commands, refresh...)
	}
	return state, commands
}

func cloneExecuteLaunch(launch executeLaunch) executeLaunch {
	launch.Request = clonePaletteRequest(launch.Request)
	return launch
}

// shellWords is a deliberately small POSIX-ish lexer for one argv field. It
// supports whitespace separation, single/double quotes, and backslash escapes;
// it does not perform expansion, substitution, globbing, or operator parsing.
func shellWords(input string) ([]string, error) {
	const (
		unquoted = iota
		singleQuoted
		doubleQuoted
	)
	state := unquoted
	escaped := false
	started := false
	var word strings.Builder
	words := []string{}
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for _, r := range input {
		if escaped {
			word.WriteRune(r)
			started = true
			escaped = false
			continue
		}
		switch state {
		case singleQuoted:
			if r == '\'' {
				state = unquoted
			} else {
				word.WriteRune(r)
			}
			started = true
		case doubleQuoted:
			switch r {
			case '"':
				state = unquoted
			case '\\':
				escaped = true
			default:
				word.WriteRune(r)
			}
			started = true
		default:
			switch {
			case unicode.IsSpace(r):
				flush()
			case r == '\'':
				state = singleQuoted
				started = true
			case r == '"':
				state = doubleQuoted
				started = true
			case r == '\\':
				escaped = true
				started = true
			default:
				word.WriteRune(r)
				started = true
			}
		}
	}
	if escaped {
		return nil, errors.New("E_TUI_EXECUTE_ARGV: unterminated backslash escape")
	}
	if state != unquoted {
		return nil, errors.New("E_TUI_EXECUTE_ARGV: unterminated quote")
	}
	flush()
	return words, nil
}

func parseExecuteRequest(verb, line string) (core.Request, error) {
	argv, err := shellWords(line)
	if err != nil {
		return core.Request{}, err
	}
	if !containsString(argv, "--") {
		return core.Request{}, fmt.Errorf("E_TUI_EXECUTE_ARGV: %s requires the standalone -- launch delimiter", verb)
	}
	var positional []string
	var options map[string]string
	switch verb {
	case "run":
		positional, options, err = parseRunArgs(argv)
	case "time":
		positional, options, err = parseTimeArgs(argv)
	case "git":
		positional, options, err = parseGitArgs(argv)
	default:
		return core.Request{}, fmt.Errorf("E_TUI_EXECUTE_ARGV: unsupported execute verb %q", verb)
	}
	if err != nil {
		return core.Request{}, err
	}
	request, err := buildRequest(verb, positional, options)
	if err != nil {
		return core.Request{}, err
	}
	if _, route := core.ClassifyRequest(request); route != core.RouteClient {
		return core.Request{}, fmt.Errorf("E_TUI_EXECUTE_ROUTE: %s is not client-routed", verb)
	}
	if verb == "run" {
		if detached, _ := request.Args["detach"].(bool); detached {
			return core.Request{}, errors.New("E_TUI_EXECUTE_ARGV: detached run is unavailable from the foreground launcher")
		}
	}
	return request, nil
}

func renderConfineCommand(line string) (string, error) {
	argv, err := shellWords(line)
	if err != nil {
		return "", err
	}
	if !containsString(argv, "--") {
		return "", errors.New("E_CONFINE_ARGUMENT_INVALID: confine requires the standalone -- launch delimiter")
	}
	positional, options, err := parseConfineArgs(argv)
	if err != nil {
		return "", err
	}
	parts := []string{"aira", "confine"}
	for _, name := range []string{"slice", "name"} {
		if value := options[name]; value != "" {
			parts = append(parts, "--"+name, shellQuote(value))
		}
	}
	parts = append(parts, "--")
	for _, value := range positional {
		parts = append(parts, shellQuote(value))
	}
	return strings.Join(parts, " "), nil
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func dispatchExecuteLaunch(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, launch executeLaunch) executeReport {
	if launch.Entry.PrintOnly || launch.Entry.Verb == "confine" {
		return executeReport{Execution: "print only — not launched\n" + launch.ConfineText}
	}
	if dispatcher == nil || !launch.Entry.Enabled {
		return executeReport{Execution: "not launched: execute unavailable"}
	}
	response := dispatcher.Dispatch(ctx, scope, launch.Request)
	return classifyExecuteResponse(launch.Entry.Verb, launch.Request, response)
}

func classifyExecuteResponse(verb string, request core.Request, response core.Response) executeReport {
	telemetry := verb == "run" && !core.StoreFreeCarved(request.Verb, request.Args)
	data := decodeExecuteData(response)
	if executeNotLaunchedCode(response.Code) || !response.OK && !executeHasLaunchEvidence(verb, response.Code, data) {
		report := executeReport{Execution: "not launched: " + response.Code}
		if telemetry {
			report.Persistence = "not attempted: child was not launched"
		}
		return report
	}
	report := executeReport{}
	switch verb {
	case "time":
		report.Execution = fmt.Sprintf("process exit %d", response.Exit)
	case "run":
		switch {
		case isRunExecutionCode(response.Code, data.Status != ""):
			report.Execution = response.Code
		case data.Status != "":
			report.Execution = data.Status
			if data.ExitCode != nil {
				report.Execution += fmt.Sprintf(" (exit %d)", *data.ExitCode)
			}
		case response.OK:
			report.Execution = "completed"
		default:
			report.Execution = "U_RUN_EXIT_UNKNOWN"
		}
		if telemetry {
			if data.Wiring.WiringComplete {
				report.Persistence = "persisted"
			} else {
				report.Persistence = "relay unknown"
			}
		}
	case "git":
		gitData := decodeGitExecuteData(response)
		if gitData.ExitCode != nil {
			report.Execution = fmt.Sprintf("gitops exit %d", *gitData.ExitCode)
			if response.Code != "" && response.Code != "OK" {
				report.Execution += " (" + response.Code + ")"
			}
		} else if !response.OK {
			report.Execution = response.Code
		} else {
			report.Execution = fmt.Sprintf("gitops exit %d", response.Exit)
		}
	default:
		report.Execution = response.Code
	}
	return report
}

func executeHasLaunchEvidence(verb, code string, data executeData) bool {
	switch verb {
	case "run":
		return data.Status != "" || isRunExecutionCode(code, data.Status != "")
	case "git":
		return strings.HasPrefix(code, "E_GIT_") || strings.HasPrefix(code, "U_GIT_")
	default:
		return false
	}
}

func isRunExecutionCode(code string, hasRunRecord bool) bool {
	if code == "E_RUN_WIRING_INCOMPLETE" && hasRunRecord {
		return false
	}
	return strings.HasPrefix(code, "E_RUN_") || strings.HasPrefix(code, "U_RUN_")
}

func executeNotLaunchedCode(code string) bool {
	switch code {
	case daemon.CodeUnavailable, daemon.CodeTimeout, daemon.CodeProjectInvalid, "E_CONFIG_INVALID":
		return true
	default:
		return false
	}
}

type executeData struct {
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Wiring   struct {
		WiringComplete bool `json:"wiring_complete"`
		Warnings       []struct {
			Code string `json:"code"`
		} `json:"warnings"`
	} `json:"wiring"`
}

type gitExecuteData struct {
	ExitCode *int `json:"exit_code"`
}

func decodeExecuteData(response core.Response) executeData {
	raw := response.RawData
	if len(raw) == 0 && response.Data != nil {
		raw, _ = json.Marshal(response.Data)
	}
	var data executeData
	_ = json.Unmarshal(raw, &data)
	return data
}

func decodeGitExecuteData(response core.Response) gitExecuteData {
	raw := response.RawData
	if len(raw) == 0 && response.Data != nil {
		raw, _ = json.Marshal(response.Data)
	}
	var data gitExecuteData
	_ = json.Unmarshal(raw, &data)
	return data
}

// runTUISignalLoop owns only the TUI cancellation policy. Registered signals
// are swallowed while a foreground execute runs; the child receives terminal
// signals independently through foreground-process-group delivery.
func runTUISignalLoop(ctx context.Context, signals <-chan os.Signal, executeRunning func() bool, cancel func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			if !executeRunning() {
				cancel()
			}
		}
	}
}
