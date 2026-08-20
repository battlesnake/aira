package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
	"aira/internal/runner"
	"aira/internal/store"
)

func main() { os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr)) }

// Run is the deliberately small CLI adapter: argv parsing, core request
// construction, and rendering. It contains no ticket or consistency logic.
func Run(argv []string, stdout, stderr io.Writer) int {
	return runWithInputDispatcher(argv, stdout, stderr, os.Stdin, nil)
}

// RunWithDispatcher is the injected in-process substrate used by tests. The
// production entrypoint calls Run and therefore always constructs the
// daemon-backed dispatcher for routed operations.
func RunWithDispatcher(argv []string, stdout, stderr io.Writer, dispatcher Dispatcher) int {
	return runWithInputDispatcher(argv, stdout, stderr, os.Stdin, dispatcher)
}

func runWithInput(argv []string, stdout, stderr io.Writer, stdin io.Reader) int {
	return runWithInputDispatcher(argv, stdout, stderr, stdin, nil)
}

func runWithInputDispatcher(argv []string, stdout, stderr io.Writer, stdin io.Reader, injected Dispatcher) int {
	if len(argv) > 0 && argv[0] == "__supervise" {
		return runSupervisor(argv[1:], stderr)
	}
	if len(argv) > 0 && strings.ToLower(argv[0]) == "daemon" {
		return runDaemonCommand(argv[1:], stdout, stderr)
	}
	if len(argv) > 0 && strings.ToLower(argv[0]) == "mcp" {
		return runMCPWithDispatcher(context.Background(), os.Stdin, stdout, stderr, injected)
	}
	if len(argv) > 0 && strings.ToLower(argv[0]) == "skill" {
		return runSkill(argv[1:], stdout, stderr)
	}
	args, jsonOutput := removeJSON(argv)
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		response := core.New(nil).Do(context.Background(), core.Request{Verb: "help"})
		return render(response, jsonOutput, stdout, stderr)
	}
	verb := strings.ToLower(args[0])
	positional, options, err := parseArgs(verb, args[1:])
	if err != nil {
		code := store.ErrorCode(err)
		if code == "E_INTERNAL" {
			code = "E_SELECTOR_INVALID"
		}
		response := core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		return render(response, jsonOutput, stdout, stderr)
	}
	if verb == "tui" && jsonOutput {
		response := core.Response{Code: "E_SELECTOR_INVALID", Error: "option --json is not valid for tui", Exit: store.ExitForCode("E_SELECTOR_INVALID")}
		return render(response, true, stdout, stderr)
	}

	if verb == "init" {
		requestArgs := map[string]any{}
		if value := options["project"]; value != "" {
			requestArgs["project"] = value
		}
		if value := options["prefixes"]; value != "" {
			requestArgs["prefixes"] = splitComma(value)
		}
		paths, pathErr := daemon.PathsFromEnv()
		if pathErr != nil {
			return render(transportErrorResponse(pathErr), jsonOutput, stdout, stderr)
		}
		project, discoverErr := app.DiscoverBootstrap(context.Background(), ".")
		if discoverErr != nil {
			code := appErrorCode(discoverErr)
			return render(core.Response{Code: code, Error: discoverErr.Error(), Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
		}
		dispatcher := injected
		if dispatcher == nil {
			dispatcher, err = newDaemonDispatcher(stdin, stdout, stderr, jsonOutput)
			if err != nil {
				return render(transportErrorResponse(err), jsonOutput, stdout, stderr)
			}
		}
		response := dispatcher.Dispatch(context.Background(), bootstrapScope(project, paths), core.Request{Verb: "init", Args: requestArgs})
		relativiseInitResponse(&response, ".")
		return render(response, jsonOutput, stdout, stderr)
	}

	request, err := buildRequest(verb, positional, options)
	if err != nil {
		code := store.ErrorCode(err)
		if code == "E_INTERNAL" {
			code = "E_SELECTOR_INVALID"
		}
		return render(core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
	}
	if verb == "test-report" && len(positional) > 0 && strings.EqualFold(positional[0], "add") {
		path := "-"
		if len(positional) == 2 {
			path = positional[1]
		}
		var data []byte
		if path == "-" {
			data, err = io.ReadAll(stdin)
		} else {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			code := "E_TESTREPORT_INVALID"
			return render(core.Response{Code: code, Error: fmt.Sprintf("%s: %v", code, err), Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
		}
		request.Args["raw"] = data
	}
	if verb == "spend" && len(positional) > 0 && strings.EqualFold(positional[0], "add") {
		bucketValues := splitOptionList(options["bucket"])
		usageFile := options["usage-file"]
		if usageFile != "" && len(bucketValues) > 0 {
			code := store.ErrorCode(fmt.Errorf("%s: --usage-file and --bucket are mutually exclusive", domain.ComputeCodeInvalid))
			return render(core.Response{Code: code, Error: fmt.Sprintf("%s: --usage-file and --bucket are mutually exclusive", code), Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
		}
		var data []byte
		if usageFile != "" {
			data, err = os.ReadFile(usageFile)
		} else {
			data, err = io.ReadAll(stdin)
		}
		if err != nil {
			code := domain.ComputeCodeInvalid
			return render(core.Response{Code: code, Error: fmt.Sprintf("%s: %v", code, err), Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
		}
		if len(bucketValues) > 0 {
			if strings.TrimSpace(string(data)) != "" {
				code := domain.ComputeCodeInvalid
				return render(core.Response{Code: code, Error: code + ": payload and --bucket are mutually exclusive", Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
			}
			request.Args["bucket"] = bucketValues
		} else {
			request.Args["raw"] = data
		}
	}
	if err := prepareImportContent(&request); err != nil {
		code := store.ErrorCode(err)
		return render(core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
	}
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		return render(transportErrorResponse(err), jsonOutput, stdout, stderr)
	}
	scope, err := scopeForCWD(context.Background(), ".", paths)
	if err != nil {
		code := appErrorCode(err)
		return render(core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
	}
	if verb == "tui" {
		dispatcher := injected
		if dispatcher == nil {
			dispatcher, err = newDaemonDispatcher(stdin, io.Discard, io.Discard, false)
			if err != nil {
				return render(transportErrorResponse(err), jsonOutput, stdout, stderr)
			}
		}
		return runTUI(context.Background(), dispatcher, scope, stderr)
	}
	faceStdout := &lineTrackingWriter{w: stdout}
	dispatcher := injected
	if dispatcher == nil {
		dispatcher, err = newDaemonDispatcher(stdin, faceStdout, stderr, jsonOutput)
		if err != nil {
			return render(transportErrorResponse(err), jsonOutput, stdout, stderr)
		}
	} else if local, ok := dispatcher.(*inProcessDispatcher); ok {
		local.stdin, local.stdout, local.diagnostics, local.jsonOutput = stdin, faceStdout, stderr, jsonOutput
	}
	if verb == "watch" {
		watchCtx, stopWatch := signal.NotifyContext(context.Background(), syscall.SIGINT)
		defer stopWatch()
		return runWatchLoop(watchCtx, dispatcher, scope, request, jsonOutput, stdout, stderr)
	}
	response := dispatcher.Dispatch(context.Background(), scope, request)
	if verb == "time" && !jsonOutput {
		return renderTime(response, stdout, stderr)
	}
	if verb == "run-log" && !jsonOutput {
		return renderRunLog(response, stdout, stderr)
	}
	if (verb == "run" || verb == "git") && !jsonOutput && response.OK && faceStdout.needsSeparator() {
		_, _ = io.WriteString(stdout, "\n")
	}
	return render(response, jsonOutput, stdout, stderr)
}

func runSupervisor(argv []string, diagnostics io.Writer) int {
	readyForFailure := supervisorReadyFD(argv)
	values := map[string]string{}
	for i := 0; i < len(argv); i += 2 {
		if i+1 >= len(argv) || !strings.HasPrefix(argv[i], "--") {
			writeSupervisorFailure(readyForFailure, "E_RUN_ARGUMENT_INVALID", "malformed supervisor arguments")
			return store.ExitForCode("E_RUN_ARGUMENT_INVALID")
		}
		name := strings.TrimPrefix(argv[i], "--")
		if name != "control" && name != "ready-fd" && name != "ack-fd" && name != "wiring" {
			writeSupervisorFailure(readyForFailure, "E_RUN_ARGUMENT_INVALID", "malformed supervisor arguments")
			return store.ExitForCode("E_RUN_ARGUMENT_INVALID")
		}
		if _, exists := values[name]; exists {
			writeSupervisorFailure(readyForFailure, "E_RUN_ARGUMENT_INVALID", "duplicate supervisor argument")
			return store.ExitForCode("E_RUN_ARGUMENT_INVALID")
		}
		values[name] = argv[i+1]
	}
	readyFD, readyErr := strconv.Atoi(values["ready-fd"])
	ackFD, ackErr := strconv.Atoi(values["ack-fd"])
	if values["control"] == "" || readyErr != nil || ackErr != nil || readyFD < 0 || ackFD < 0 {
		writeSupervisorFailure(readyFD, "E_RUN_ARGUMENT_INVALID", "malformed supervisor arguments")
		return store.ExitForCode("E_RUN_ARGUMENT_INVALID")
	}
	request, err := runner.ConsumeDetachControl(values["control"])
	if err != nil {
		writeSupervisorFailure(readyFD, "E_RUN_ARGUMENT_INVALID", err.Error())
		return store.ExitForCode("E_RUN_ARGUMENT_INVALID")
	}
	var wiringParams core.WiringParams
	var reportContext store.TestReportContext
	wiringRequested := values["wiring"] != ""
	if wiringRequested {
		wiringParams, reportContext, err = core.ConsumeDetachedWiringSidecar(values["wiring"])
		if err != nil {
			writeSupervisorFailure(readyFD, "E_RUN_ARGUMENT_INVALID", err.Error())
			return store.ExitForCode("E_RUN_ARGUMENT_INVALID")
		}
		request.TelemetryPending = core.TelemetryPending
	}
	s, project, err := app.OpenWithDiagnostics(context.Background(), ".", diagnostics)
	if err != nil {
		writeSupervisorFailure(readyFD, "E_RUN_DETACH_FAILED", err.Error())
		return store.ExitForCode("E_RUN_DETACH_FAILED")
	}
	defer s.Close()
	if paths, pathErr := daemon.PathsFromEnv(); pathErr == nil {
		project.Runner.SetAdmitSocketPath(paths.SocketPath)
		project.Runner.SetInputRuntimeDir(paths.RuntimeDir)
	}
	record, superviseErr := project.Runner.SuperviseRequest(context.Background(), request, readyFD, ackFD)
	var telemetryErr error
	if wiringRequested && detachedWiringTerminal(record) {
		wiring, _, settleErr := core.NewWithRunner(s, project.Runner).WireAndSettleDetached(context.Background(), *record, wiringParams, reportContext)
		telemetryErr = settleErr
		if diagnostics != nil && !wiring.WiringComplete {
			for _, warning := range wiring.Warnings {
				_, _ = fmt.Fprintf(diagnostics, "detached telemetry %s: %s: %s\n", warning.Action, warning.Code, warning.Message)
			}
		}
		if telemetryErr != nil && diagnostics != nil {
			_, _ = fmt.Fprintf(diagnostics, "detached telemetry settlement: %v\n", telemetryErr)
		}
	}
	if superviseErr != nil {
		return store.ExitForCode(store.ErrorCode(superviseErr))
	}
	if telemetryErr != nil {
		return store.ExitForCode(store.ErrorCode(telemetryErr))
	}
	return 0
}

func detachedWiringTerminal(record *runner.RunRecord) bool {
	return record != nil && record.Status.Terminal()
}

func supervisorReadyFD(argv []string) int {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] != "--ready-fd" {
			continue
		}
		fd, err := strconv.Atoi(argv[i+1])
		if err == nil && fd >= 0 {
			return fd
		}
	}
	return -1
}

func writeSupervisorFailure(fd int, code, message string) {
	if fd < 0 {
		return
	}
	f := os.NewFile(uintptr(fd), "detach-ready")
	if f == nil {
		return
	}
	_ = json.NewEncoder(f).Encode(map[string]string{"code": code, "error": message})
	_ = f.Close()
}

func removeJSON(argv []string) ([]string, bool) {
	result := make([]string, 0, len(argv))
	jsonOutput := false
	end := len(argv)
	carvedArgv := len(argv) > 0 && (strings.EqualFold(argv[0], "run") || strings.EqualFold(argv[0], "time"))
	if carvedArgv {
		for i := 1; i < len(argv); i++ {
			if argv[i] == "--" {
				end = i
				break
			}
		}
	}
	for i, arg := range argv {
		if i >= end {
			// A post-`--` token belongs to the TARGET. `run` historically also
			// treats a target `--json` as its own face selector (it captures
			// output anyway), but `time` MUST stay byte-transparent: a common
			// child flag (`gh … --json`, `npm … --json`, `jest --json`) must
			// reach the child and never flip AIRA into JSON mode + /dev/null the
			// child's output. So only `run` honours a post-delimiter `--json`.
			if strings.EqualFold(argv[0], "run") && arg == "--json" {
				jsonOutput = true
			}
			result = append(result, arg)
			continue
		}
		if arg == "--json" {
			jsonOutput = true
		} else {
			result = append(result, arg)
		}
	}
	return result, jsonOutput
}

func parseArgs(verb string, argv []string) ([]string, map[string]string, error) {
	if verb == "run" {
		return parseRunArgs(argv)
	}
	if verb == "time" {
		return parseTimeArgs(argv)
	}
	if verb == "git" {
		return parseGitArgs(argv)
	}
	options := map[string]string{}
	var positional []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		if name == "rebuild" || name == "steal" || name == "strict" || (name == "close" && verb == "run-input") || (name == "from-start" && verb == "watch") || (name == "list" && verb == "ready") || ((name == "follow" || name == "full") && verb == "run-log") || (name == "reasoning-subset" && verb == "spend") || (name == "all" && verb == "test-report") || (name == "unreviewed" && verb == "rant") {
			options[name] = "true"
			continue
		}
		gateListValue := verb == "gate" && (name == "argv" || name == "env-allow")
		if i+1 >= len(argv) || (strings.HasPrefix(argv[i+1], "--") && !gateListValue) {
			if strings.HasPrefix(verb, "run-") {
				return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s requires a value", name)
			}
			if verb == "git" {
				return nil, nil, fmt.Errorf("E_GIT_ARG_INVALID: option --%s is not permitted", name)
			}
			return nil, nil, fmt.Errorf("option --%s requires a value", name)
		}
		i++
		if name == "label" {
			if options["labels"] != "" {
				options["labels"] += ","
			}
			options["labels"] += argv[i]
		} else if name == "prefix" {
			if options["prefixes"] != "" {
				options["prefixes"] += ","
			}
			options["prefixes"] += argv[i]
		} else if name == "fields" {
			if options["fields"] != "" {
				options["fields"] += ","
			}
			options["fields"] += argv[i]
		} else if name == "argv" || name == "env-allow" || (name == "config-env" && verb == "test-report") || (name == "bucket" && verb == "spend") || (verb == "rant" && (name == "tag" || name == "ref")) {
			options[name] = appendDelimited(options[name], argv[i])
		} else {
			options[name] = argv[i]
		}
	}
	allowed := map[string]map[string]bool{
		"init":   {"project": true, "prefixes": true},
		"create": {"kind": true, "severity": true, "labels": true, "body": true},
		"rant":   {"tag": true, "severity": true, "ref": true, "idem": true, "by": true, "unreviewed": true, "since": true, "outcome": true, "note": true, "resolved-by": true},
		"new":    {"kind": true, "severity": true, "labels": true, "body": true},
		"show":   {"fields": true}, "get": {"fields": true}, "review": {"paths": true},
		"list": {"by": true, "fields": true}, "ls": {"by": true, "fields": true},
		"grep":   {"kind": true, "by": true, "fields": true},
		"import": {"strict": true},
		"count":  {"by": true}, "reconcile": {"rebuild": true},
		"claim":   {"steal": true, "actor": true},
		"release": {"token": true}, "heartbeat": {"token": true},
		"touch":       {"token": true},
		"ready":       {"list": true},
		"find":        {"category": true, "severity": true, "verdict": true, "source": true, "message": true, "file": true, "requirement": true, "by": true, "fields": true, "disposition": true, "reason": true, "actor": true},
		"req":         {"status": true, "fields": true},
		"test-report": {"format": true, "explain": true, "all": true, "ticket": true, "phase": true, "commit": true, "branch": true, "suite": true, "config": true, "config-env": true, "shard": true, "retry": true},
		"spend":       {"provider": true, "model": true, "source": true, "ticket": true, "phase": true, "at": true, "session": true, "agent": true, "total": true, "cost-usd": true, "usage-file": true, "bucket": true, "reasoning-subset": true, "by": true},
		"quota":       {"provider": true, "source": true, "at": true, "window": true, "used": true, "limit": true, "remaining": true, "reset-at": true},
		"insights":    {},
		"lease":       {},
		"tui":         nil,
		"commands":    {"by": true},
		"git":         {},
		"run-kill":    {"steal": true},
		"run-input":   {"close": true, "steal": true},
		"run-log":     {"stream": true, "from": true, "tail": true, "follow": true, "full": true},
		"watch":       {"from": true, "from-start": true, "verb": true},
		"gate":        {"gate_id": true, "canary_id": true, "verdict": true, "actor": true, "reason": true, "report": true, "checker": true, "predicate": true, "argv": true, "cwd": true, "env-allow": true, "timeout-ms": true, "output-cap-bytes": true, "parser": true, "mutation-kind": true, "mutation-file": true, "mutation-test": true, "mutation-occurrence": true, "mutation-pkgdir": true, "mutation-testname": true, "mutation-seed": true, "mutation-expected-result": true},
	}
	for name := range options {
		if !allowed[verb][name] {
			if strings.HasPrefix(verb, "run-") {
				return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s is not valid for %s", name, verb)
			}
			if verb == "git" {
				return nil, nil, fmt.Errorf("E_GIT_ARG_INVALID: option --%s is not valid for git", name)
			}
			return nil, nil, fmt.Errorf("option --%s is not valid for %s", name, verb)
		}
	}
	return positional, options, nil
}

func parseGitArgs(argv []string) ([]string, map[string]string, error) {
	positionals := make([]string, 0, len(argv))
	boundary := false
	for _, arg := range argv {
		if !boundary && arg == "--" {
			boundary = true
			continue
		}
		if !boundary && strings.HasPrefix(arg, "--") {
			return nil, nil, fmt.Errorf("E_GIT_ARG_INVALID: git options are not permitted")
		}
		positionals = append(positionals, arg)
	}
	return positionals, map[string]string{}, nil
}

func parseRunArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	delimiter := -1
	for i, arg := range argv {
		if arg == "--" {
			delimiter = i
			break
		}
	}
	if delimiter < 0 {
		return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: run requires the standalone -- launch delimiter")
	}
	for i := 0; i < delimiter; i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: run options must precede the launch delimiter")
		}
		name := strings.TrimPrefix(arg, "--")
		if name == "merge" || name == "realtime" || name == "pty" || name == "detach" || name == "stdin-connect" || name == "follow" || name == "no-stdin" || name == "store-stdin" || name == "no-admit" || name == "strict-wiring" {
			options[name] = "true"
			continue
		}
		if i+1 >= delimiter || strings.HasPrefix(argv[i+1], "--") {
			return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s requires a value", name)
		}
		i++
		value := argv[i]
		switch name {
		case "prefix", "env", "config-env":
			options[name] = appendDelimited(options[name], value)
		case "cwd", "stdin", "timeout", "ticket", "phase", "label", "tool", "report", "report-stream", "suite", "shard", "retry", "usage", "provider":
			if options[name] != "" {
				return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s may occur once", name)
			}
			options[name] = value
		default:
			return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s is not valid for run", name)
		}
	}
	target := append([]string(nil), argv[delimiter+1:]...)
	if len(target) == 0 {
		return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: run target argv is empty")
	}
	return target, options, nil
}

func parseTimeArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	delimiter := -1
	for i, arg := range argv {
		if arg == "--" {
			delimiter = i
			break
		}
	}
	if delimiter < 0 {
		return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: time requires the standalone -- launch delimiter")
	}
	for i := 0; i < delimiter; i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: time options must precede the launch delimiter")
		}
		name := strings.TrimPrefix(arg, "--")
		if name == "no-prefix" {
			options[name] = "true"
			continue
		}
		if i+1 >= delimiter || strings.HasPrefix(argv[i+1], "--") {
			return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s requires a value", name)
		}
		i++
		value := argv[i]
		switch name {
		case "prefix", "env":
			options[name] = appendDelimited(options[name], value)
		case "cwd", "timeout", "ticket", "phase", "label":
			if options[name] != "" {
				return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s may occur once", name)
			}
			options[name] = value
		default:
			return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s is not valid for time", name)
		}
	}
	if options["no-prefix"] == "true" && options["prefix"] != "" {
		return nil, nil, errors.New("E_RUN_ARGUMENT_INVALID: --prefix and --no-prefix are mutually exclusive")
	}
	target := append([]string(nil), argv[delimiter+1:]...)
	if len(target) == 0 {
		return nil, nil, errors.New("E_RUN_ARGUMENT_INVALID: time target argv is empty")
	}
	return target, options, nil
}

const optionListSeparator = "\x00"

func appendDelimited(existing, value string) string {
	if existing == "" {
		return value
	}
	return existing + optionListSeparator + value
}

func splitOptionList(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, optionListSeparator)
}

func canonicalOptionList(value string) []string {
	values := splitOptionList(value)
	if values == nil {
		return []string{}
	}
	return values
}

func buildRequest(verb string, positional []string, options map[string]string) (core.Request, error) {
	args := map[string]any{}
	switch verb {
	case "init":
		if len(positional) != 0 {
			return core.Request{}, fmt.Errorf("init accepts no positional arguments")
		}
		if options["project"] != "" {
			args["project"] = options["project"]
		}
		if options["prefixes"] != "" {
			args["prefixes"] = splitComma(options["prefixes"])
		}
	case "run":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("E_RUN_ARGUMENT_INVALID: run target argv is empty")
		}
		args["argv"] = append([]string(nil), positional...)
		// nil means “use the configured project prefix”; a non-nil token list
		// is an explicit per-run override.
		args["prefix"] = splitOptionList(options["prefix"])
		args["cwd"] = options["cwd"]
		args["env"] = canonicalOptionList(options["env"])
		args["merge"] = options["merge"] == "true"
		args["realtime"] = options["realtime"] == "true"
		args["pty"] = options["pty"] == "true"
		args["detach"] = options["detach"] == "true"
		args["stdin_connect"] = options["stdin-connect"] == "true"
		args["follow"] = options["follow"] == "true"
		args["stdin"] = options["stdin"]
		args["no_stdin"] = options["no-stdin"] == "true"
		args["store_stdin"] = options["store-stdin"] == "true"
		args["no_admit"] = options["no-admit"] == "true"
		args["timeout"] = options["timeout"]
		args["ticket"] = options["ticket"]
		args["phase"] = options["phase"]
		args["label"] = options["label"]
		args["tool"] = options["tool"]
		args["report"] = options["report"]
		args["report_stream"] = options["report-stream"]
		args["suite"] = options["suite"]
		args["config_env"] = canonicalOptionList(options["config-env"])
		args["shard"] = options["shard"]
		args["retry"] = options["retry"]
		args["usage"] = options["usage"]
		args["provider"] = options["provider"]
		args["strict_wiring"] = options["strict-wiring"] == "true"
	case "time":
		if len(positional) == 0 {
			return core.Request{}, errors.New("E_RUN_ARGUMENT_INVALID: time target argv is empty")
		}
		args["argv"] = append([]string(nil), positional...)
		if options["prefix"] != "" {
			args["prefix"] = splitOptionList(options["prefix"])
		}
		args["no_prefix"] = options["no-prefix"] == "true"
		args["cwd"], args["env"], args["timeout"] = options["cwd"], canonicalOptionList(options["env"]), options["timeout"]
		args["ticket"], args["phase"], args["label"] = options["ticket"], options["phase"], options["label"]
	case "commands":
		if len(positional) == 0 || (positional[0] != "ls" && positional[0] != "count") {
			return core.Request{}, errors.New("commands requires ls|count")
		}
		if positional[0] == "count" && options["by"] == "" {
			return core.Request{}, errors.New("commands count requires --by <field>")
		}
		args["subverb"], args["query"], args["by"] = positional[0], strings.Join(positional[1:], " "), options["by"]
	case "git":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("E_GIT_ARG_INVALID: git requires clone|fetch|push|ls-remote")
		}
		subverb := strings.ToLower(positional[0])
		args["subverb"] = subverb
		// A standalone "--" separates the remote from refspecs; the real CLI strips it in
		// parseGitArgs, so tolerate it here too and keep buildRequest delimiter-agnostic.
		{
			filtered := make([]string, 0, len(positional))
			filtered = append(filtered, positional[0])
			for _, p := range positional[1:] {
				if p != "--" {
					filtered = append(filtered, p)
				}
			}
			positional = filtered
		}
		switch subverb {
		case "clone":
			if len(positional) < 2 || len(positional) > 3 {
				return core.Request{}, fmt.Errorf("E_GIT_ARG_INVALID: git clone requires <url> [dir]")
			}
			args["url"] = positional[1]
			if len(positional) == 3 {
				args["dir"] = positional[2]
			}
		case "fetch", "push", "ls-remote":
			if len(positional) > 1 {
				args["remote"] = positional[1]
			}
			if len(positional) > 2 {
				args["refspecs"] = append([]string(nil), positional[2:]...)
			}
		default:
			return core.Request{}, fmt.Errorf("E_GIT_ARG_INVALID: unknown git operation %q", subverb)
		}
		for _, value := range positional[1:] {
			if strings.HasPrefix(value, "-") {
				return core.Request{}, fmt.Errorf("E_GIT_ARG_INVALID: git arguments may not begin with '-'")
			}
		}
	case "run-kill":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("E_RUN_ARGUMENT_INVALID: run-kill requires <run-id>")
		}
		args["run_id"] = positional[0]
		args["steal"] = options["steal"] == "true"
	case "run-input":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("E_RUN_ARGUMENT_INVALID: run-input requires <run-id>")
		}
		args["run_id"] = positional[0]
		args["close"] = options["close"] == "true"
		args["steal"] = options["steal"] == "true"
	case "run-log":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("E_RUN_ARGUMENT_INVALID: run-log requires <run-id>")
		}
		args["run_id"] = positional[0]
		args["stream"] = options["stream"]
		args["follow"] = options["follow"] == "true"
		args["from"] = options["from"]
		args["tail"] = options["tail"]
		args["full"] = options["full"] == "true"
	case "watch":
		if len(positional) > 1 {
			return core.Request{}, fmt.Errorf("watch accepts at most one selector")
		}
		if options["from"] != "" && options["from-start"] == "true" {
			return core.Request{}, fmt.Errorf("watch --from and --from-start are mutually exclusive")
		}
		if len(positional) == 1 {
			args["target"] = positional[0]
		}
		args["verbs"] = splitComma(options["verb"])
		args["wait_ms"] = int64(20_000)
		// The cursor is sent as a decimal STRING so a > 2^53 sequence survives the
		// daemon's request-arg decode without float64 rounding (Sol build r1 #3).
		switch {
		case options["from-start"] == "true":
			args["from"] = "0"
		case options["from"] != "":
			from, err := strconv.ParseInt(options["from"], 10, 64)
			if err != nil || from < 0 {
				return core.Request{}, fmt.Errorf("E_SELECTOR_INVALID: watch --from requires a non-negative integer")
			}
			args["from"] = strconv.FormatInt(from, 10)
		default:
			args["from_now"] = true
		}
	case "tui":
		if len(positional) != 0 {
			return core.Request{}, fmt.Errorf("tui accepts no positional arguments")
		}
		return core.Request{Verb: "tui"}, nil
	case "lease":
		if len(positional) != 1 || strings.ToLower(positional[0]) != "ls" {
			return core.Request{}, fmt.Errorf("lease requires ls")
		}
		args["subverb"] = "ls"
	case "id":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("id requires <prefix>")
		}
		args["prefix"] = positional[0]
	case "create", "new":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("create requires one title")
		}
		args["title"] = strings.Join(positional, " ")
		args["kind"], args["severity"], args["body"], args["labels"] = options["kind"], options["severity"], options["body"], splitComma(options["labels"])
	case "rant":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("rant requires <text> or ls|get|review|redact")
		}
		switch strings.ToLower(positional[0]) {
		case "capture":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("rant capture requires one quoted text argument")
			}
			args["subverb"], args["text"], args["tags"], args["severity"], args["refs"], args["idempotency_key"] = "capture", positional[1], splitOptionList(options["tag"]), options["severity"], splitOptionList(options["ref"]), options["idem"]
		case "ls", "list":
			if len(positional) != 1 {
				return core.Request{}, fmt.Errorf("rant ls accepts no positional arguments")
			}
			args["subverb"], args["by"], args["unreviewed"] = "ls", options["by"], options["unreviewed"] == "true"
			if tags := splitOptionList(options["tag"]); len(tags) > 0 {
				args["tags"] = tags
			}
			if options["since"] != "" {
				value, err := strconv.ParseInt(options["since"], 10, 64)
				if err != nil || value < 0 {
					return core.Request{}, fmt.Errorf("E_RANT_INVALID: --since requires a non-negative integer")
				}
				args["since"] = strconv.FormatInt(value, 10)
			}
		case "get":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("rant get requires RANT-n")
			}
			args["subverb"], args["selector"] = "get", positional[1]
		case "review":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("rant review requires RANT-n")
			}
			args["subverb"], args["selector"], args["outcome"], args["note"], args["resolved_by"] = "review", positional[1], options["outcome"], options["note"], options["resolved-by"]
		case "redact":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("rant redact requires RANT-n")
			}
			args["subverb"], args["selector"] = "redact", positional[1]
		default:
			if len(positional) != 1 {
				return core.Request{}, fmt.Errorf("rant text must be one quoted argument")
			}
			args["subverb"], args["text"], args["tags"], args["severity"], args["refs"], args["idempotency_key"] = "capture", positional[0], splitOptionList(options["tag"]), options["severity"], splitOptionList(options["ref"]), options["idem"]
		}
	case "show", "get":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("show requires <selector>")
		}
		args["selector"] = positional[0]
		if rawFields, provided := options["fields"]; provided {
			args["fields"] = splitComma(rawFields)
		}
	case "review":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("review requires <selector>")
		}
		args["selector"] = positional[0]
		if rawPaths, provided := options["paths"]; provided {
			args["paths"] = splitComma(rawPaths)
		}
	case "find":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("find requires add|ls|show|set")
		}
		subverb := strings.ToLower(positional[0])
		args["subverb"] = subverb
		switch subverb {
		case "add":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("find add requires <ticket-id>")
			}
			args["ticket"], args["category"], args["severity"], args["verdict"], args["source"], args["message"] = positional[1], options["category"], options["severity"], options["verdict"], options["source"], options["message"]
			args["requirement"] = options["requirement"]
			if rawFile := options["file"]; rawFile != "" {
				idx := strings.LastIndexByte(rawFile, ':')
				if idx <= 0 || idx == len(rawFile)-1 {
					return core.Request{}, fmt.Errorf("find add --file requires path:line")
				}
				line, err := strconv.Atoi(rawFile[idx+1:])
				if err != nil || line <= 0 {
					return core.Request{}, fmt.Errorf("find add --file requires a positive line")
				}
				args["file"], args["line"] = rawFile[:idx], line
			}
		case "ls", "list":
			args["query"], args["by"], args["fields"] = strings.Join(positional[1:], " "), options["by"], splitComma(options["fields"])
		case "show":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("find show requires <id>")
			}
			args["selector"] = positional[1]
		case "set":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("find set requires <id>")
			}
			args["selector"], args["disposition"], args["reason"], args["actor"] = positional[1], options["disposition"], options["reason"], options["actor"]
		default:
			return core.Request{}, fmt.Errorf("find requires add|ls|show|set")
		}
	case "req":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("req requires add|ls|show|set|import")
		}
		subverb := strings.ToLower(positional[0])
		args["subverb"] = subverb
		switch subverb {
		case "add":
			if len(positional) < 2 {
				return core.Request{}, fmt.Errorf("req add requires <text>")
			}
			args["text"], args["status"] = strings.Join(positional[1:], " "), options["status"]
		case "ls":
			if len(positional) != 1 {
				return core.Request{}, fmt.Errorf("req ls accepts no positional arguments")
			}
			args["fields"] = splitComma(options["fields"])
		case "show":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("req show requires <selector>")
			}
			args["selector"] = positional[1]
		case "set":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("req set requires <selector>")
			}
			args["selector"], args["status"] = positional[1], options["status"]
		case "import":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("req import requires <file>")
			}
			args["file"] = positional[1]
		default:
			return core.Request{}, fmt.Errorf("req requires add|ls|show|set|import")
		}
	case "spend":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("spend requires add|ls")
		}
		subverb := strings.ToLower(positional[0])
		args["subverb"] = subverb
		switch subverb {
		case "add":
			if len(positional) != 1 {
				return core.Request{}, fmt.Errorf("spend add accepts no positional arguments")
			}
			for option, argument := range map[string]string{"provider": "provider", "model": "model", "source": "source", "ticket": "ticket", "phase": "phase", "at": "at", "session": "session", "agent": "agent", "total": "total", "cost-usd": "cost-usd"} {
				if value, ok := options[option]; ok {
					args[argument] = value
				}
			}
			args["reasoning-subset"] = options["reasoning-subset"] == "true"
			if value, ok := options["bucket"]; ok {
				args["bucket"] = splitOptionList(value)
			}
		case "ls", "list":
			if len(positional) > 1 {
				args["query"] = strings.Join(positional[1:], " ")
			}
			args["by"] = options["by"]
		default:
			return core.Request{}, fmt.Errorf("spend requires add|ls")
		}
	case "quota":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("quota requires add|ls")
		}
		subverb := strings.ToLower(positional[0])
		args["subverb"] = subverb
		switch subverb {
		case "add":
			if len(positional) != 1 {
				return core.Request{}, fmt.Errorf("quota add accepts no positional arguments")
			}
			for option, argument := range map[string]string{"provider": "provider", "source": "source", "at": "at", "window": "window", "used": "used", "limit": "limit", "remaining": "remaining", "reset-at": "reset-at"} {
				if value, ok := options[option]; ok {
					args[argument] = value
				}
			}
		case "ls", "list":
			if len(positional) > 1 {
				args["query"] = strings.Join(positional[1:], " ")
			}
		default:
			return core.Request{}, fmt.Errorf("quota requires add|ls")
		}
	case "insights":
		args["subverb"] = "show"
		if len(positional) == 0 {
			break
		}
		switch strings.ToLower(positional[0]) {
		case "ls":
			if len(positional) != 1 {
				return core.Request{}, fmt.Errorf("insights ls accepts no positional arguments")
			}
			args["subverb"] = "ls"
		case "show":
			if len(positional) > 2 {
				return core.Request{}, fmt.Errorf("insights show accepts at most one gauge name")
			}
			if len(positional) == 2 {
				args["name"] = positional[1]
			}
		default:
			if len(positional) != 1 {
				return core.Request{}, fmt.Errorf("insights accepts ls|show <name> or a gauge name")
			}
			args["name"] = positional[0]
		}
	case "test-report":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("test-report requires add|ls|show|flaky")
		}
		subverb := strings.ToLower(positional[0])
		args["subverb"] = subverb
		switch subverb {
		case "add":
			if len(positional) > 2 {
				return core.Request{}, fmt.Errorf("test-report add accepts an optional report file")
			}
			if options["format"] == "" {
				return core.Request{}, fmt.Errorf("test-report add requires --format go-json|junit")
			}
		case "ls", "list":
			if len(positional) != 1 {
				return core.Request{}, fmt.Errorf("test-report ls accepts no positional selector")
			}
		case "show":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("test-report show requires <report-id>")
			}
			args["selector"] = positional[1]
		case "flaky":
			if len(positional) > 2 {
				return core.Request{}, fmt.Errorf("test-report flaky accepts at most one test selector")
			}
			if len(positional) == 2 {
				args["selector"] = positional[1]
			}
			if options["explain"] != "" {
				args["explain"] = options["explain"]
			}
			args["all"] = options["all"] == "true"
		default:
			return core.Request{}, fmt.Errorf("test-report requires add|ls|show|flaky")
		}
		for option, argument := range map[string]string{"format": "format", "ticket": "ticket", "phase": "phase", "commit": "commit", "branch": "branch", "suite": "suite", "config": "config", "shard": "shard", "retry": "retry"} {
			if options[option] != "" {
				args[argument] = options[option]
			}
		}
		if rawRetry := options["retry"]; rawRetry != "" {
			value, err := strconv.Atoi(rawRetry)
			if err != nil || value < 0 {
				return core.Request{}, fmt.Errorf("E_ARGUMENT_INVALID: --retry must be a non-negative integer")
			}
		}
		if subverb == "add" {
			args["raw"] = ""
		}
		if rawEnv := options["config-env"]; rawEnv != "" {
			entries := make([]runner.EnvEntry, 0)
			for _, item := range splitOptionList(rawEnv) {
				key, value, ok := strings.Cut(item, "=")
				if !ok || key == "" {
					return core.Request{}, fmt.Errorf("E_ARGUMENT_INVALID: --config-env requires K=V")
				}
				entries = append(entries, runner.EnvEntry{Key: []byte(key), Value: []byte(value)})
			}
			digest, err := runner.EnvDigest(entries)
			if err != nil {
				return core.Request{}, err
			}
			args["env_digest"] = digest
		}
	case "list", "ls":
		args["query"] = strings.Join(positional, " ")
		args["by"], args["fields"] = options["by"], splitComma(options["fields"])
	case "grep":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("grep requires <query>")
		}
		args["query"], args["kind"], args["by"], args["fields"] = strings.Join(positional, " "), options["kind"], options["by"], splitComma(options["fields"])
	case "import":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("import requires <file>")
		}
		args["file"], args["strict"] = positional[0], options["strict"] == "true"
	case "count":
		if options["by"] == "" {
			return core.Request{}, fmt.Errorf("count requires --by <field>")
		}
		args["query"], args["by"] = strings.Join(positional, " "), options["by"]
	case "set":
		if len(positional) != 2 {
			return core.Request{}, fmt.Errorf("set requires <selector> <field=value>")
		}
		field, value, ok := strings.Cut(positional[1], "=")
		if !ok || field == "" {
			return core.Request{}, fmt.Errorf("set requires <field=value>")
		}
		args["selector"], args["field"], args["value"] = positional[0], field, value
	case "mv":
		if len(positional) != 2 {
			return core.Request{}, fmt.Errorf("mv requires <selector> <status>")
		}
		args["selector"], args["status"] = positional[0], positional[1]
	case "claim":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("claim requires <id>")
		}
		args["selector"], args["steal"], args["actor"] = positional[0], options["steal"] == "true", options["actor"]
	case "release", "heartbeat":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("%s requires <id>", verb)
		}
		args["selector"], args["token"] = positional[0], options["token"]
	case "touch":
		if len(positional) < 1 {
			return core.Request{}, fmt.Errorf("touch requires <id> [<glob>...]")
		}
		args["selector"], args["token"] = positional[0], options["token"]
		args["globs"] = positional[1:]
	case "link":
		if len(positional) == 2 && positional[0] == "ls" {
			args["list"], args["selector"] = true, positional[1]
		} else if len(positional) == 3 {
			args["from"], args["kind"], args["to"] = positional[0], positional[1], positional[2]
		} else {
			return core.Request{}, fmt.Errorf("link requires <from> <kind> <to> or ls <id>")
		}
	case "unlink":
		if len(positional) != 3 {
			return core.Request{}, fmt.Errorf("unlink requires <from> <kind> <to>")
		}
		args["from"], args["kind"], args["to"] = positional[0], positional[1], positional[2]
	case "ready":
		if len(positional) > 1 {
			return core.Request{}, fmt.Errorf("ready accepts at most one selector")
		}
		if options["list"] == "true" && len(positional) > 0 {
			return core.Request{}, fmt.Errorf("ready --list accepts no selector")
		}
		if len(positional) == 1 {
			args["selector"] = positional[0]
		}
	case "reconcile":
		if len(positional) != 0 {
			return core.Request{}, fmt.Errorf("reconcile accepts no positional arguments")
		}
		args["rebuild"] = options["rebuild"] == "true"
	case "check":
		if len(positional) != 0 {
			return core.Request{}, fmt.Errorf("check accepts no positional arguments")
		}
	case "gate":
		if len(positional) == 0 {
			return core.Request{}, fmt.Errorf("gate requires an operation")
		}
		args["subverb"] = strings.ToLower(positional[0])
		switch args["subverb"] {
		case "ls", "check":
			if len(positional) != 1 {
				return core.Request{}, fmt.Errorf("gate %s accepts no positional arguments", args["subverb"])
			}
		case "add", "show", "set", "run", "attest", "prove", "review":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("gate %s requires <gate-id>", args["subverb"])
			}
			args["gate_id"] = positional[1]
			if args["subverb"] == "attest" {
				if options["verdict"] == "" || options["actor"] == "" {
					return core.Request{}, fmt.Errorf("gate attest requires --verdict and --actor")
				}
				args["verdict"], args["actor"] = options["verdict"], options["actor"]
			}
		case "canary-run", "canary-show":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("gate %s requires <canary-id>", args["subverb"])
			}
			args["canary_id"] = positional[1]
		case "baseline-pin", "baseline-show":
			if len(positional) != 2 {
				return core.Request{}, fmt.Errorf("gate %s requires <gate-id>", args["subverb"])
			}
			args["gate_id"] = positional[1]
			if args["subverb"] == "baseline-pin" && options["report"] == "" {
				return core.Request{}, fmt.Errorf("gate baseline-pin requires --report <TR-id>[,<TR-id>…]")
			}
		case "baseline":
			if len(positional) < 3 || (positional[1] != "pin" && positional[1] != "show") || len(positional) != 3 {
				return core.Request{}, fmt.Errorf("gate baseline requires pin|show <gate-id>")
			}
			if positional[1] == "pin" && options["report"] == "" {
				return core.Request{}, fmt.Errorf("gate baseline pin requires --report <TR-id>[,<TR-id>…]")
			}
			if positional[1] == "pin" {
				args["subverb"] = "baseline-pin"
			} else {
				args["subverb"] = "baseline-show"
			}
			args["gate_id"] = positional[2]
		default:
			return core.Request{}, fmt.Errorf("unknown gate operation %q", args["subverb"])
		}
		for option, argument := range map[string]string{"checker": "checker", "predicate": "predicate", "cwd": "cwd", "timeout-ms": "timeout_ms", "output-cap-bytes": "output_cap_bytes", "parser": "parser", "mutation-kind": "mutation_kind", "mutation-file": "mutation_file", "mutation-test": "mutation_test", "mutation-occurrence": "mutation_occurrence", "mutation-pkgdir": "mutation_pkgdir", "mutation-testname": "mutation_testname", "mutation-seed": "mutation_seed", "mutation-expected-result": "mutation_expected_result"} {
			if value := options[option]; value != "" {
				args[argument] = value
			}
		}
		if options["report"] != "" {
			args["report"] = options["report"]
		}
		if options["reason"] != "" {
			args["reason"] = options["reason"]
		}
		if options["actor"] != "" {
			args["actor"] = options["actor"]
		}
		if value := options["argv"]; value != "" {
			args["argv"] = splitOptionList(value)
		}
		if value := options["env-allow"]; value != "" {
			args["env_allow"] = splitOptionList(value)
		}
	default:
		return core.Request{}, fmt.Errorf("E_UNKNOWN_VERB: unknown verb %q", verb)
	}
	return core.Request{Verb: verb, Args: args}, nil
}

func splitComma(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func bootstrapScope(project app.Project, paths daemon.Paths) daemon.WorktreeScope {
	return daemon.WorktreeScope{
		Root: project.Root, CommonDir: project.CommonDir, GitDir: project.GitDir,
		ProjectID: project.ProjectID, WorktreeID: project.WorktreeID,
		StateID: paths.StateID, Bootstrap: true,
	}
}

func prepareImportContent(request *core.Request) error {
	if request == nil {
		return nil
	}
	canonical := core.CanonicalVerb(request.Verb)
	isImport := canonical == "import"
	if canonical == "req" {
		subverb, _ := request.Args["subverb"].(string)
		isImport = strings.EqualFold(subverb, "import")
	}
	if !isImport {
		return nil
	}
	path, _ := request.Args["file"].(string)
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("E_IMPORT_INVALID: cannot resolve import file %q: %w", path, err)
	}
	data, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("E_NOT_FOUND: import file %q does not exist", path)
	}
	if err != nil {
		return fmt.Errorf("E_IMPORT_INVALID: cannot read import file %q: %w", path, err)
	}
	request.Args["file"] = abs
	request.Content = data
	request.HasContent = true
	return nil
}

func relativiseInitResponse(response *core.Response, cwd string) {
	if response == nil || !response.OK {
		return
	}
	var result app.InitResult
	switch data := response.Data.(type) {
	case app.InitResult:
		result = data
	case *app.InitResult:
		if data == nil {
			return
		}
		result = *data
	default:
		encoded := response.RawData
		if len(encoded) == 0 {
			var err error
			encoded, err = json.Marshal(response.Data)
			if err != nil {
				return
			}
		}
		if err := json.Unmarshal(encoded, &result); err != nil {
			return
		}
	}
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		return
	}
	for _, path := range []*string{&result.Root, &result.Config} {
		if !filepath.IsAbs(*path) {
			continue
		}
		if relative, err := filepath.Rel(cwdAbs, *path); err == nil {
			*path = filepath.ToSlash(relative)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return
	}
	response.Data = result
	response.RawData = encoded
}

func render(response core.Response, jsonOutput bool, stdout, stderr io.Writer) int {
	var writeErr error
	if jsonOutput {
		data, _ := json.Marshal(response)
		_, writeErr = fmt.Fprintln(stdout, string(data))
	} else if response.OK {
		writeErr = renderHuman(response, stdout)
	} else if response.Error != "" {
		_, _ = fmt.Fprintln(stderr, response.Error)
	}
	if writeErr == nil && response.OK {
		if flusher, ok := stdout.(interface{ Flush() error }); ok {
			writeErr = flusher.Flush()
		}
	}
	if response.AfterWrite != nil {
		if err := response.AfterWrite(response.OK && writeErr == nil); err != nil {
			_, _ = fmt.Fprintf(stderr, "E_RUN_DETACH_FAILED: %v\n", err)
			return exitForError("E_RUN_DETACH_FAILED")
		}
	}
	if writeErr != nil {
		return exitForError("E_RUN_DETACH_FAILED")
	}
	if response.Exit != 0 {
		return response.Exit
	}
	if response.OK {
		return 0
	}
	return exitForError(response.Code)
}

type lineTrackingWriter struct {
	w     io.Writer
	wrote bool
	last  byte
}

func (w *lineTrackingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		w.wrote = true
		w.last = p[n-1]
	}
	return n, err
}

func (w *lineTrackingWriter) needsSeparator() bool {
	return w.wrote && w.last != '\n'
}

func renderHuman(response core.Response, out io.Writer) error {
	if response.Code == "PASS" || response.Code == "FAIL" || response.Code == "UNEVALUATED" {
		if report, ok := response.Data.(interface{}); ok {
			data := response.RawData
			if len(data) == 0 {
				data, _ = json.Marshal(report)
			}
			if _, err := fmt.Fprintf(out, "verdict: %s\n%s\n", strings.ToLower(response.Code), data); err != nil {
				return err
			}
		}
		for _, warning := range response.Warnings {
			if _, err := fmt.Fprintf(out, "warning: %s\n", warning); err != nil {
				return err
			}
		}
		return nil
	}
	var data []byte
	if len(response.RawData) > 0 {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, response.RawData, "", "  "); err == nil {
			data = formatted.Bytes()
		}
	}
	if len(data) == 0 {
		data, _ = json.MarshalIndent(response.Data, "", "  ")
	}
	if _, err := fmt.Fprintln(out, string(data)); err != nil {
		return err
	}
	for _, warning := range response.Warnings {
		if _, err := fmt.Fprintf(out, "warning: %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

func renderRunLog(response core.Response, stdout, stderr io.Writer) int {
	if chunk, ok := response.Data.(*runner.OutputChunk); ok && chunk != nil {
		_, _ = stdout.Write(chunk.Bytes)
		metadata := map[string]any{
			"run_id": chunk.RunID, "stream": chunk.Stream, "offset": chunk.Offset,
			"next_offset": chunk.NextOffset, "total_bytes": chunk.TotalBytes,
			"complete": chunk.Complete, "truncated": chunk.Truncated,
			"output_state": chunk.OutputState, "run_status": chunk.RunStatus,
			"error_codes": chunk.ErrorCodes,
		}
		data, _ := json.Marshal(metadata)
		_, _ = fmt.Fprintln(stderr, string(data))
	} else if response.Error != "" {
		_, _ = fmt.Fprintln(stderr, response.Error)
	}
	if response.Exit != 0 {
		return response.Exit
	}
	if !response.OK {
		return exitForError(response.Code)
	}
	return 0
}

func renderTime(response core.Response, stdout, stderr io.Writer) int {
	var writeErr error
	for _, warning := range response.Warnings {
		if _, err := fmt.Fprintf(stderr, "warning: %s\n", warning); err != nil && writeErr == nil {
			writeErr = err
		}
	}
	if !response.OK && response.Error != "" {
		if _, err := fmt.Fprintln(stderr, response.Error); err != nil && writeErr == nil {
			writeErr = err
		}
	}
	if writeErr == nil && response.OK {
		if flusher, ok := stdout.(interface{ Flush() error }); ok {
			writeErr = flusher.Flush()
		}
	}
	if response.AfterWrite != nil {
		if err := response.AfterWrite(response.OK && writeErr == nil); err != nil {
			_, _ = fmt.Fprintf(stderr, "E_RUN_DETACH_FAILED: %v\n", err)
			return exitForError("E_RUN_DETACH_FAILED")
		}
	}
	if writeErr != nil {
		return exitForError("E_RUN_DETACH_FAILED")
	}
	if response.Exit != 0 {
		return response.Exit
	}
	if response.OK {
		return 0
	}
	return exitForError(response.Code)
}

func appErrorCode(err error) string {
	return store.ErrorCode(err)
}

func exitForError(code string) int {
	return store.ExitForCode(code)
}
