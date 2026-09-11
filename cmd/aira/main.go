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
	"text/tabwriter"
	"time"
	"unicode"

	"golang.org/x/term"

	"aira/internal/app"
	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
	installcmd "aira/internal/install"
	"aira/internal/runner"
	"aira/internal/store"
)

func main() { os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr)) }

var runConfined = runner.Confine
var reserveConfined = runner.ConfineReserve

// The AIRA-22 detached-confine seams, injectable for the same reason
// runConfined is: the CLI's job is transcription and rendering, and its tests
// must be able to prove the composition without launching real supervisors.
var launchConfineDetached = runner.LaunchConfineDetached
var superviseConfineDetached = runner.SuperviseConfineDetached
var confineDetachStatusFor = runner.ConfineDetachStatusFor
var confineDetachStatusList = runner.ConfineDetachStatusList
var runInstaller = installcmd.Run
var runSliceAnchor = installcmd.RunSliceAnchor

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
	if len(argv) > 0 && argv[0] == "__slice-anchor" {
		return runSliceAnchor()
	}
	if len(argv) > 0 && strings.EqualFold(argv[0], "install") {
		if err := runInstaller(argv[1:], stdout); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return codes.ExitForCode(store.ErrorCode(err))
		}
		return 0
	}
	if len(argv) > 0 && argv[0] == "__confine-setup" {
		return runner.RunConfineSetup(argv[1:], stderr)
	}
	if len(argv) > 0 && argv[0] == "__supervise" {
		return runSupervisor(argv[1:], stderr)
	}
	if len(argv) > 0 && argv[0] == "__confine-supervise" {
		return runConfineSupervisor(argv[1:])
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
	// The scope override is stripped BEFORE removeJSON so that removeJSON still
	// sees the verb at args[0] and keeps its post-`--` carve-out intact
	// (AIRA-82).
	args, scopeDirOption, scopeDirErr := removeScopeDir(argv)
	args, jsonOutput := removeJSON(args)
	// renderJSON is the TTY-aware rendering default: JSON unless stdout is a
	// real terminal and --json was not explicitly requested. Piped/redirected
	// output (the dominant case for an agent invoking this CLI via a
	// subprocess) defaults to JSON; an interactive terminal defaults to
	// genuine human-readable text (AIRA-57). Verbs whose current behaviour
	// doesn't route through the generic (previously JSON-dump-as-"human")
	// rendering decision at all deliberately keep using the explicit
	// jsonOutput flag below, unaffected by this default: confine and friends
	// (which reject --json outright), watch, run/git/time's live byte
	// streaming during dispatch, and the deliberate non-JSON suppression
	// contracts of time's and run-log's trailing summaries (see below).
	renderJSON := jsonOutput || !stdoutIsTerminal(stdout)
	if scopeDirErr != nil {
		code := store.ErrorCode(scopeDirErr)
		return render(core.Response{Code: code, Error: scopeDirErr.Error(), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
	}
	// A verb that resolves no project/worktree scope refuses the override rather
	// than accepting and discarding it: silently ignoring an explicit scope is
	// the same confidently-wrong reporting AIRA-82 is about.
	if scopeDirOption != "" {
		target := "help"
		if len(args) > 0 {
			target = strings.ToLower(args[0])
		}
		if !verbAcceptsScopeDir(target) {
			message := fmt.Sprintf("E_SELECTOR_INVALID: option %s is not valid for %s", scopeDirFlag, target)
			return render(core.Response{Code: "E_SELECTOR_INVALID", Error: message, Exit: codes.ExitForCode("E_SELECTOR_INVALID")}, renderJSON, stdout, stderr)
		}
	}
	scopeDir, scopeDirResolveErr := resolveScopeDir(scopeDirOption)
	if scopeDirResolveErr != nil {
		code := store.ErrorCode(scopeDirResolveErr)
		return render(core.Response{Code: code, Error: scopeDirResolveErr.Error(), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
	}
	// AIRA-202. Intercepted HERE, beside help, rather than added to buildRequest's
	// switch: version resolves no project, opens no store, and must answer with
	// the daemon down. This is also the one point that catches the verb and the
	// flag spellings together -- buildRequest has its OWN enumerated switch whose
	// default raises E_UNKNOWN_VERB before core.Do is reached, so registering the
	// verb in the core dispatch table alone would leave `aira version` broken
	// while `--version` worked.
	if len(args) > 0 && isVersionSpelling(args[0]) {
		dispatcher := injected
		var dispatcherErr error
		if dispatcher == nil {
			// A dispatcher that cannot even be constructed is not fatal here: the
			// client half is still establishable locally, and runVersionCommand
			// reports the daemon half as unevaluated. The CONSTRUCTION error is
			// carried through rather than swallowed -- reporting a generic "no
			// dispatcher was available" when the real cause was, say, an unresolvable
			// XDG_STATE_HOME is the same substitute-a-plausible-cause defect this
			// verb exists to stop.
			var production *daemonDispatcher
			if production, dispatcherErr = newDaemonDispatcher(stdin, stdout, stderr, renderJSON); dispatcherErr == nil {
				dispatcher = production
			}
		}
		return runVersionCommand(context.Background(), dispatcher, dispatcherErr, renderJSON, stdout, stderr)
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		response := core.New(nil).Do(context.Background(), core.Request{Verb: "help"})
		if !renderJSON && response.OK {
			return renderHelp(response, stdout, stderr)
		}
		return render(response, renderJSON, stdout, stderr)
	}
	verb := strings.ToLower(args[0])
	// AIRA-176. `aira worktree <subverb>` is spelled as one canonical verb
	// (`worktree-register` / `worktree-audit`) everywhere below it, exactly as
	// `confine --list` is spelled `confine-list`. The rewrite happens here, before
	// parseArgs, so the option allow-map and buildRequest arm are per-subverb and
	// an option valid for one is refused for the other.
	if verb == "worktree" {
		rewritten, worktreeErr := canonicalWorktreeVerb(args)
		if worktreeErr != nil {
			code := store.ErrorCode(worktreeErr)
			return render(core.Response{Code: code, Error: worktreeErr.Error(), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
		}
		args = rewritten
		verb = args[0]
	}
	positional, options, err := parseArgs(verb, args[1:])
	if err != nil {
		if verb == "worker-admit" {
			// worker-admit's caller is the aitest supervisor, which reads
			// one structured stdout line and nothing else. An argument
			// mistake that produced only a rendered error left it with no
			// outcome at all, which it can only read as "the relay
			// produced nothing" -> daemon unusable -> run the rest of the
			// suite unconfined. That is the AIRA-42 misclassification in
			// its purest form, one layer above runWorkerAdmitCommand, so
			// this verb's pre-dispatch failures speak the same channel.
			return writeWorkerAdmitOutcome(stdout, stderr, runner.WorkerAdmitOutcome{
				State: runner.WorkerAdmitStateArgumentInvalid, Class: runner.WorkerAdmitClassRequestInvalid,
				Reason: runner.WorkerAdmitReasonArgumentsInvalid, Detail: err.Error(),
			}, nil, "E_CONFINE_ARGUMENT_INVALID")
		}
		code := store.ErrorCode(err)
		if code == "E_INTERNAL" {
			code = "E_SELECTOR_INVALID"
		}
		response := core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}
		return render(response, renderJSON, stdout, stderr)
	}
	if verb == "tui" && jsonOutput {
		response := core.Response{Code: "E_SELECTOR_INVALID", Error: "option --json is not valid for tui", Exit: codes.ExitForCode("E_SELECTOR_INVALID")}
		return render(response, true, stdout, stderr)
	}
	// AIRA-127. `aira top` is handled HERE, before project discovery, because it
	// resolves no project: confine state is machine-wide and `confine --list`
	// needs none. Routing it through the scope resolution below would make a
	// machine-wide monitor refuse to start outside an AIRA project — which is
	// most of the directories an operator watching the slice is standing in.
	if verb == "top" {
		if jsonOutput {
			response := core.Response{Code: "E_SELECTOR_INVALID", Error: "option --json is not valid for top", Exit: codes.ExitForCode("E_SELECTOR_INVALID")}
			return render(response, true, stdout, stderr)
		}
		if len(positional) != 0 {
			response := core.Response{Code: "E_SELECTOR_INVALID", Error: "E_SELECTOR_INVALID: top accepts no positional arguments", Exit: codes.ExitForCode("E_SELECTOR_INVALID")}
			return render(response, renderJSON, stdout, stderr)
		}
		dispatcher := injected
		if dispatcher == nil {
			var dispatcherErr error
			dispatcher, dispatcherErr = newDaemonDispatcher(stdin, io.Discard, io.Discard, false)
			if dispatcherErr != nil {
				return render(transportErrorResponse(dispatcherErr), renderJSON, stdout, stderr)
			}
		}
		return runTop(context.Background(), dispatcher, stdin, stdout, stderr)
	}
	if verb == "confine" {
		// AIRA-22: --status joins --list/--kill as a management form. Unlike those
		// two it is answered LOCALLY rather than through the daemon: it reads a
		// plain filesystem record, and routing it through the daemon would make the
		// survivability verb depend on the component most likely to have been
		// restarted during exactly the long pause it exists to survive.
		status := options["status"] == "true"
		management := options["list"] == "true" || options["kill"] != "" || status || options["budget"] == "true"
		if jsonOutput && !management {
			response := core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: option --json is not valid for confine", Exit: codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}
			return render(response, true, stdout, stderr)
		}
		if status {
			return runConfineStatusCommand(context.Background(), options, jsonOutput, stdout, stderr)
		}
		if management {
			return runConfineManagementCommand(context.Background(), options, jsonOutput, stdout, stderr, injected)
		}
		return runConfineCommand(context.Background(), positional, options, stdin, stdout, stderr)
	}
	if verb == "confine-reserve" {
		if jsonOutput {
			response := core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: option --json is not valid for confine-reserve", Exit: codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}
			return render(response, true, stdout, stderr)
		}
		return runConfineReserveCommand(context.Background(), options, stdin, stdout, stderr)
	}
	// AIRA-185. `drain` and its internal placeholder are handled HERE, beside
	// confine and before project discovery, for the same reason `top` is: a drain
	// is machine-wide slice work that resolves no project, and routing it through
	// the scope resolution below would make it refuse to start in most of the
	// directories an operator deploying from is standing in.
	if verb == "drain" {
		if jsonOutput {
			response := core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: option --json is not valid for drain", Exit: codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}
			return render(response, true, stdout, stderr)
		}
		return runDrainCommand(context.Background(), positional, options, stdin, stdout, stderr)
	}
	if verb == "drain-hold" {
		if jsonOutput {
			response := core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: option --json is not valid for drain-hold", Exit: codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}
			return render(response, true, stdout, stderr)
		}
		return runDrainHoldCommand(stdout)
	}
	if verb == "aitest-bootstrap" {
		if jsonOutput {
			response := core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: option --json is not valid for aitest-bootstrap", Exit: codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}
			return render(response, true, stdout, stderr)
		}
		return runAitestBootstrapCommand(context.Background(), options, stdout, stderr)
	}
	if verb == "worker-admit" {
		if jsonOutput {
			// Reported on the structured channel rather than as a rendered
			// JSON error, for the same reason as the parseArgs branch
			// above: stdout belongs to the outcome channel for this verb,
			// and a caller that gets no outcome line falls back
			// unconfined.
			return writeWorkerAdmitOutcome(stdout, stderr, runner.WorkerAdmitOutcome{
				State: runner.WorkerAdmitStateArgumentInvalid, Class: runner.WorkerAdmitClassRequestInvalid,
				Reason: runner.WorkerAdmitReasonArgumentsInvalid,
				Detail: "option --json is not valid for worker-admit",
			}, nil, "E_CONFINE_ARGUMENT_INVALID")
		}
		return runWorkerAdmitCommand(context.Background(), options, stdin, stdout, stderr)
	}
	if verb == "worker-peak" {
		if jsonOutput {
			_, _ = fmt.Fprintln(stderr, "E_CONFINE_ARGUMENT_INVALID: option --json is not valid for worker-peak")
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		return runWorkerPeakCommand(context.Background(), options, stderr)
	}
	if verb == "confine-list" || verb == "confine-kill" || verb == "confine-budget" {
		request, requestErr := buildRequest(verb, positional, options)
		if requestErr != nil {
			code := store.ErrorCode(requestErr)
			if code == "E_INTERNAL" {
				code = "E_CONFINE_ARGUMENT_INVALID"
			}
			return render(core.Response{Code: code, Error: requestErr.Error(), Exit: codes.ExitForCode(code)}, jsonOutput, stdout, stderr)
		}
		owner, ownerErr := resolveConfineOwner(context.Background(), options["owner"])
		if ownerErr != nil {
			return render(core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: --owner: " + ownerErr.Error(), Exit: codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}, jsonOutput, stdout, stderr)
		}
		request.Args["owner"] = owner
		return dispatchConfineManagementRequest(context.Background(), request, jsonOutput, stdout, stderr, injected)
	}
	// AIRA-196. Handled HERE, beside the rest of the confine family and BEFORE
	// project discovery, for the reason the family shares: a detached confine job
	// is machine-wide and resolves no project, so routing these through scope
	// resolution below would make them refuse to run in most directories an
	// operator is standing in when they want to read a running gate's log.
	if verb == "confine-log" || verb == "confine-input" {
		request, requestErr := buildRequest(verb, positional, options)
		if requestErr != nil {
			code := store.ErrorCode(requestErr)
			if code == "E_INTERNAL" {
				code = "E_CONFINE_ARGUMENT_INVALID"
			}
			return render(core.Response{Code: code, Error: requestErr.Error(), Exit: codes.ExitForCode(code)}, jsonOutput, stdout, stderr)
		}
		owner, ownerErr := resolveConfineOwner(context.Background(), options["owner"])
		if ownerErr != nil {
			return render(core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: --owner: " + ownerErr.Error(), Exit: codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}, jsonOutput, stdout, stderr)
		}
		request.Args["owner"] = owner
		return dispatchConfineJobIORequest(context.Background(), request, jsonOutput, stdin, stdout, stderr, injected)
	}
	if verb == "eject" {
		request, requestErr := buildRequest(verb, positional, options)
		if requestErr != nil {
			code := store.ErrorCode(requestErr)
			return render(core.Response{Code: code, Error: requestErr.Error(), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
		}
		if request.Args["project"] == "" && request.Args["prefix"] == "" {
			project, discoverErr := app.Discover(context.Background(), scopeDir)
			if discoverErr != nil {
				return render(core.Response{Code: "E_NO_PROJECT", Error: "E_NO_PROJECT: no selector and no current .aira/config", Exit: codes.ExitForCode("E_NO_PROJECT")}, renderJSON, stdout, stderr)
			}
			request.Args["project"] = project.ProjectID
		}
		dispatcher := injected
		if dispatcher == nil {
			dispatcher, err = newDaemonDispatcher(stdin, stdout, stderr, renderJSON)
			if err != nil {
				return render(transportErrorResponse(err), renderJSON, stdout, stderr)
			}
		}
		return render(dispatcher.Dispatch(context.Background(), daemon.WorktreeScope{}, request), renderJSON, stdout, stderr)
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
			return render(transportErrorResponse(pathErr), renderJSON, stdout, stderr)
		}
		project, discoverErr := app.DiscoverBootstrap(context.Background(), scopeDir)
		if discoverErr != nil {
			code := appErrorCode(discoverErr)
			return render(core.Response{Code: code, Error: discoverErr.Error(), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
		}
		dispatcher := injected
		if dispatcher == nil {
			dispatcher, err = newDaemonDispatcher(stdin, stdout, stderr, renderJSON)
			if err != nil {
				return render(transportErrorResponse(err), renderJSON, stdout, stderr)
			}
		}
		response := dispatcher.Dispatch(context.Background(), bootstrapScope(project, paths), core.Request{Verb: "init", Args: requestArgs})
		relativiseInitResponse(&response, scopeDir)
		return render(response, renderJSON, stdout, stderr)
	}

	request, err := buildRequest(verb, positional, options)
	if err != nil {
		code := store.ErrorCode(err)
		if code == "E_INTERNAL" {
			code = "E_SELECTOR_INVALID"
		}
		return render(core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
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
			return render(core.Response{Code: code, Error: fmt.Sprintf("%s: %v", code, err), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
		}
		request.Args["raw"] = data
	}
	if verb == "spend" && len(positional) > 0 && strings.EqualFold(positional[0], "add") {
		bucketValues := splitOptionList(options["bucket"])
		usageFile := options["usage-file"]
		if usageFile != "" && len(bucketValues) > 0 {
			code := store.ErrorCode(fmt.Errorf("%s: --usage-file and --bucket are mutually exclusive", domain.ComputeCodeInvalid))
			return render(core.Response{Code: code, Error: fmt.Sprintf("%s: --usage-file and --bucket are mutually exclusive", code), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
		}
		var data []byte
		if usageFile != "" {
			data, err = os.ReadFile(usageFile)
		} else {
			data, err = io.ReadAll(stdin)
		}
		if err != nil {
			code := domain.ComputeCodeInvalid
			return render(core.Response{Code: code, Error: fmt.Sprintf("%s: %v", code, err), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
		}
		if len(bucketValues) > 0 {
			if strings.TrimSpace(string(data)) != "" {
				code := domain.ComputeCodeInvalid
				return render(core.Response{Code: code, Error: code + ": payload and --bucket are mutually exclusive", Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
			}
			request.Args["bucket"] = bucketValues
		} else {
			request.Args["raw"] = data
		}
	}
	if err := prepareImportContent(&request); err != nil {
		code := store.ErrorCode(err)
		return render(core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
	}
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		return render(transportErrorResponse(err), renderJSON, stdout, stderr)
	}
	scope, err := scopeForCWD(context.Background(), scopeDir, paths)
	if err != nil {
		code := appErrorCode(err)
		return render(core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
	}
	// AIRA-176. Identity is stamped from the RESOLVED SCOPE ROOT, not the process
	// cwd, so `--dir` and MCP's scope override both attribute a binding to the
	// checkout actually being registered.
	if ownerErr := stampWorktreeOwner(context.Background(), scope, &request); ownerErr != nil {
		code := store.ErrorCode(ownerErr)
		return render(core.Response{Code: code, Error: ownerErr.Error(), Exit: codes.ExitForCode(code)}, renderJSON, stdout, stderr)
	}
	if verb == "tui" {
		dispatcher := injected
		var executeDispatcher Dispatcher
		if dispatcher == nil {
			dispatcher, err = newDaemonDispatcher(stdin, io.Discard, io.Discard, false)
			if err != nil {
				return render(transportErrorResponse(err), renderJSON, stdout, stderr)
			}
			executeDispatcher, err = newDaemonDispatcher(stdin, stdout, stderr, false)
			if err != nil {
				return render(transportErrorResponse(err), renderJSON, stdout, stderr)
			}
		}
		return runTUI(context.Background(), dispatcher, executeDispatcher, scope, stdin, stdout, stderr)
	}
	faceStdout := &lineTrackingWriter{w: stdout}
	dispatcher := injected
	if dispatcher == nil {
		// The dispatcher's own jsonOutput field controls run/git/time's live
		// output streaming (captured-for-JSON vs streamed-raw), which must stay
		// tied to the EXPLICIT --json flag rather than the TTY-aware default:
		// a piped `aira run -- cmd` must keep streaming raw child bytes exactly
		// as before, never silently switch to buffering them into a JSON blob
		// merely because stdout isn't a terminal.
		dispatcher, err = newDaemonDispatcher(stdin, faceStdout, stderr, jsonOutput)
		if err != nil {
			return render(transportErrorResponse(err), renderJSON, stdout, stderr)
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
	// time, like run-log, stays keyed on the explicit flag: its own summary
	// says output is not captured, and renderTime deliberately suppresses the
	// trailing response body in non-JSON mode (the timed command's own output
	// already streamed live) — that suppression is a deliberate contract, not
	// an instance of the generic JSON-dump bug, so it must not start emitting
	// a JSON envelope merely because stdout isn't a terminal.
	if verb == "time" && !jsonOutput {
		return renderTime(response, stdout, stderr)
	}
	// run-log stays keyed on the explicit flag: its non-JSON mode already
	// writes raw replayed bytes to stdout (with JSON metadata on stderr),
	// which is exactly the byte-transparent behaviour a piped/subprocess
	// consumer wants — switching it to a JSON-enveloped (base64) body by
	// default when piped would be a regression, not a fix.
	if verb == "run-log" && !jsonOutput {
		return renderRunLog(response, stdout, stderr)
	}
	if (verb == "run" || verb == "git") && !renderJSON && response.OK && faceStdout.needsSeparator() {
		_, _ = io.WriteString(stdout, "\n")
	}
	return render(response, renderJSON, stdout, stderr)
}

func runSupervisor(argv []string, diagnostics io.Writer) int {
	readyForFailure := supervisorReadyFD(argv)
	values := map[string]string{}
	for i := 0; i < len(argv); i += 2 {
		if i+1 >= len(argv) || !strings.HasPrefix(argv[i], "--") {
			writeSupervisorFailure(readyForFailure, "E_RUN_ARGUMENT_INVALID", "malformed supervisor arguments")
			return codes.ExitForCode("E_RUN_ARGUMENT_INVALID")
		}
		name := strings.TrimPrefix(argv[i], "--")
		if name != "control" && name != "ready-fd" && name != "ack-fd" && name != "wiring" {
			writeSupervisorFailure(readyForFailure, "E_RUN_ARGUMENT_INVALID", "malformed supervisor arguments")
			return codes.ExitForCode("E_RUN_ARGUMENT_INVALID")
		}
		if _, exists := values[name]; exists {
			writeSupervisorFailure(readyForFailure, "E_RUN_ARGUMENT_INVALID", "duplicate supervisor argument")
			return codes.ExitForCode("E_RUN_ARGUMENT_INVALID")
		}
		values[name] = argv[i+1]
	}
	readyFD, readyErr := strconv.Atoi(values["ready-fd"])
	ackFD, ackErr := strconv.Atoi(values["ack-fd"])
	if values["control"] == "" || readyErr != nil || ackErr != nil || readyFD < 0 || ackFD < 0 {
		writeSupervisorFailure(readyFD, "E_RUN_ARGUMENT_INVALID", "malformed supervisor arguments")
		return codes.ExitForCode("E_RUN_ARGUMENT_INVALID")
	}
	request, err := runner.ConsumeDetachControl(values["control"])
	if err != nil {
		writeSupervisorFailure(readyFD, "E_RUN_ARGUMENT_INVALID", err.Error())
		return codes.ExitForCode("E_RUN_ARGUMENT_INVALID")
	}
	var wiringParams core.WiringParams
	var reportContext store.TestReportContext
	wiringRequested := values["wiring"] != ""
	if wiringRequested {
		wiringParams, reportContext, err = core.ConsumeDetachedWiringSidecar(values["wiring"])
		if err != nil {
			writeSupervisorFailure(readyFD, "E_RUN_ARGUMENT_INVALID", err.Error())
			return codes.ExitForCode("E_RUN_ARGUMENT_INVALID")
		}
		request.TelemetryPending = core.TelemetryPending
	}
	// AIRA-85: the supervisor opens NO read-write state.db handle. It builds the
	// project's local execution dependencies without a store, then takes a
	// read-only view whose writes relay to the DB-owning daemon, so the daemon
	// stays the single writer. app.OpenWithDiagnostics is deliberately not used
	// here — it opens state.db read-write (and would migrate and register from a
	// background process the daemon knows nothing about).
	project, err := app.OpenWithoutStore(context.Background(), ".", diagnostics)
	if err != nil {
		writeSupervisorFailure(readyFD, "E_RUN_DETACH_FAILED", err.Error())
		return codes.ExitForCode("E_RUN_DETACH_FAILED")
	}
	paths, pathErr := daemon.PathsFromEnv()
	if pathErr == nil {
		project.Runner.SetAdmitSocketPath(paths.SocketPath)
		project.Runner.SetInputRuntimeDir(paths.RuntimeDir)
	} else if diagnostics != nil {
		// Not fatal — the run itself is still supervised, exactly as before this
		// change. But it is no longer silent: with no daemon endpoint there is no
		// relay, so any telemetry this run settles will be reported incomplete
		// rather than recorded, and the reason belongs on stderr.
		_, _ = fmt.Fprintf(diagnostics, "detached supervisor: daemon paths unavailable, telemetry cannot be relayed: %v\n", pathErr)
	}
	telemetryStore, err := supervisorTelemetryStore(project, paths)
	if err != nil {
		writeSupervisorFailure(readyFD, "E_RUN_DETACH_FAILED", err.Error())
		return codes.ExitForCode("E_RUN_DETACH_FAILED")
	}
	defer telemetryStore.Close()
	telemetryStore.SetRunner(project.Runner)
	project.Runner.SetSupervisorLeaseReader(telemetryStore.SupervisorLeaseLive)
	record, superviseErr := project.Runner.SuperviseRequest(context.Background(), request, readyFD, ackFD)
	var telemetryErr error
	if wiringRequested && detachedWiringTerminal(record) {
		wiring, _, settleErr := core.NewWithRunner(telemetryStore, project.Runner).WireAndSettleDetached(context.Background(), *record, wiringParams, reportContext)
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
		return codes.ExitForCode(store.ErrorCode(superviseErr))
	}
	if telemetryErr != nil {
		return codes.ExitForCode(store.ErrorCode(telemetryErr))
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
	carvedArgv := len(argv) > 0 && (strings.EqualFold(argv[0], "run") || strings.EqualFold(argv[0], "time") || strings.EqualFold(argv[0], "confine"))
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

// stdoutIsTerminal reports whether w is the process's own stdout attached to
// a real terminal. This deliberately checks the OUTPUT stream, not stdin: a
// command's rendering decision depends on who is READING its result, and
// this project's own daemon/MCP faces separately read structured input from
// stdin in some paths, so stdin's TTY-ness is a different question entirely
// (AIRA-57). A non-*os.File writer (a buffer, a pipe abstraction in tests)
// is never a terminal.
func stdoutIsTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

func parseArgs(verb string, argv []string) ([]string, map[string]string, error) {
	if verb == "install" {
		return parseInstallDescriptorArgs(argv)
	}
	if verb == "confine" {
		return parseConfineArgs(argv)
	}
	if verb == "confine-reserve" {
		return parseConfineReserveArgs(argv)
	}
	if verb == "drain" {
		return parseDrainArgs(argv)
	}
	if verb == "drain-hold" {
		return parseDrainHoldArgs(argv)
	}
	if verb == "aitest-bootstrap" {
		return parseAitestBootstrapArgs(argv)
	}
	if verb == "worker-admit" {
		return parseWorkerAdmitArgs(argv)
	}
	if verb == "worker-peak" {
		return parseWorkerPeakArgs(argv)
	}
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
		if name == "rebuild" || name == "steal" || name == "strict" || ((name == "purge" || name == "force") && verb == "eject") || (name == "close" && (verb == "run-input" || verb == "confine-input")) || (name == "from-start" && verb == "watch") || (name == "list" && verb == "ready") || ((name == "follow" || name == "full") && (verb == "run-log" || verb == "confine-log")) || (name == "reasoning-subset" && verb == "spend") || (name == "all" && verb == "test-report") || (name == "unreviewed" && verb == "rant") {
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
		} else if name == "prefix" && verb == "init" {
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
		"eject":  {"project": true, "prefix": true, "purge": true, "force": true},
		"create": {"kind": true, "severity": true, "labels": true, "body": true},
		"rant":   {"tag": true, "severity": true, "ref": true, "idem": true, "by": true, "unreviewed": true, "since": true, "outcome": true, "note": true, "resolved-by": true, "project": true, "prefix": true},
		"new":    {"kind": true, "severity": true, "labels": true, "body": true},
		"show":   {"fields": true}, "get": {"fields": true}, "review": {"paths": true},
		"list": {"by": true, "fields": true}, "ls": {"by": true, "fields": true},
		"grep":   {"kind": true, "by": true, "fields": true},
		"import": {"strict": true},
		"count":  {"by": true}, "reconcile": {"rebuild": true},
		"claim":   {"steal": true, "actor": true},
		"release": {"token": true}, "heartbeat": {"token": true},
		"touch": {"token": true},
		"ready": {"list": true},
		// AIRA-176. --owner is accepted on register only; audit has no identity of
		// its own to declare. Neither takes --owner-attested: attestation is
		// DERIVED from the resolved owner by the face and can never be asserted on
		// the command line, or the evidence grade would be forgeable.
		"worktree-register": {"base": true, "owner": true},
		"worktree-audit":    {"base": true},
		"find":              {"category": true, "severity": true, "verdict": true, "source": true, "message": true, "file": true, "requirement": true, "by": true, "fields": true, "disposition": true, "reason": true, "actor": true},
		"req":               {"status": true, "fields": true},
		"test-report":       {"format": true, "explain": true, "all": true, "ticket": true, "phase": true, "commit": true, "branch": true, "suite": true, "config": true, "config-env": true, "shard": true, "retry": true},
		"spend":             {"provider": true, "model": true, "source": true, "ticket": true, "phase": true, "at": true, "session": true, "agent": true, "total": true, "cost-usd": true, "usage-file": true, "bucket": true, "reasoning-subset": true, "by": true},
		"quota":             {"provider": true, "source": true, "at": true, "window": true, "used": true, "limit": true, "remaining": true, "reset-at": true},
		"insights":          {},
		"lease":             {},
		"tui":               nil,
		// AIRA-127. `top` takes no options today; the entry exists so an unknown
		// one is refused by name rather than silently accepted and discarded.
		"top":            nil,
		"commands":       {"by": true},
		"git":            {},
		"run-kill":       {"steal": true},
		"run-input":      {"close": true, "steal": true},
		"run-log":        {"stream": true, "from": true, "tail": true, "follow": true, "full": true, "grep": true},
		"confine-list":   {"slice": true, "owner": true},
		"confine-budget": {"slice": true, "owner": true},
		"confine-kill":   {"steal": true, "slice": true, "owner": true},
		// AIRA-196. No --slice on either: both address a job through the durable
		// record store, never a cgroup slice, and an accepted-and-ignored --slice
		// is exactly the silently discarded scope AIRA-82 refuses.
		"confine-log":   {"stream": true, "from": true, "tail": true, "follow": true, "full": true, "grep": true, "owner": true},
		"confine-input": {"close": true, "steal": true, "owner": true},
		"watch":         {"from": true, "from-start": true, "verb": true},
		"gate":          {"gate_id": true, "canary_id": true, "verdict": true, "actor": true, "checker": true, "predicate": true, "argv": true, "cwd": true, "env-allow": true, "timeout-ms": true, "output-cap-bytes": true, "parser": true, "mutation-kind": true, "mutation-file": true, "mutation-test": true, "mutation-occurrence": true, "mutation-pkgdir": true, "mutation-testname": true, "mutation-content": true, "mutation-seed": true, "mutation-expected-result": true},
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

func parseInstallDescriptorArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") {
			return nil, nil, fmt.Errorf("E_INSTALL_ARGUMENT_INVALID: unexpected argument %q", arg)
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if _, exists := options[name]; exists {
			return nil, nil, fmt.Errorf("E_INSTALL_ARGUMENT_INVALID: option --%s may occur once", name)
		}
		switch name {
		case "allow-overcommit", "dry-run", "status", "ci":
			if hasValue {
				return nil, nil, fmt.Errorf("E_INSTALL_ARGUMENT_INVALID: option --%s does not take a value", name)
			}
			options[name] = "true"
		case "memory-max", "memory-high", "watchdog", "watchdog-interval", "slice-ceiling":
			if !hasValue {
				if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
					return nil, nil, fmt.Errorf("E_INSTALL_ARGUMENT_INVALID: option --%s requires a value", name)
				}
				i++
				value = argv[i]
			}
			if value == "" {
				return nil, nil, fmt.Errorf("E_INSTALL_ARGUMENT_INVALID: option --%s requires a value", name)
			}
			options[name] = value
		default:
			return nil, nil, fmt.Errorf("E_INSTALL_ARGUMENT_INVALID: option --%s is not valid for install", name)
		}
	}
	return nil, options, nil
}

func parseConfineArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	delimiter := -1
	for i, arg := range argv {
		if arg == "--" {
			delimiter = i
			break
		}
	}
	if delimiter < 0 {
		return parseConfineManagementArgs(argv)
	}
	for i := 0; i < delimiter; i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: confine options must precede the launch delimiter")
		}
		name := strings.TrimPrefix(arg, "--")
		// AIRA-101 adds --exclusive here, in the VALUELESS branch and only in the
		// launch form. parseConfineManagementArgs keeps rejecting it, so `aira
		// confine --exclusive` with no `--` argv is an argument error rather than a
		// silently ignored no-op on a --list/--kill invocation.
		//
		// AIRA-182 moved both branches' vocabularies out to
		// confineLaunchValuelessOptions / confineLaunchValuedOptions so the
		// did-you-mean suggestion below reads the SAME list this check reads. The
		// membership tests are otherwise exactly what they were. AIRA-196 adds
		// --stdin-connect to the valueless list there; it is refused below unless
		// --detach is also present.
		if confineLaunchOptionValueless(name) {
			if _, exists := options[name]; exists {
				return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s may occur once", name)
			}
			options[name] = "true"
			continue
		}
		// AIRA-138 adds --timeout and --cpu-timeout here, in the VALUED branch and
		// only in the launch form. parseConfineManagementArgs keeps rejecting them,
		// so `aira confine --timeout 5m --list` is an argument error rather than a
		// silently ignored no-op — the same discipline --exclusive already follows.
		if !confineLaunchOptionTakesValue(name) {
			// AIRA-182. The suggestion is APPENDED to the unchanged refusal — same
			// code, same sentence — and is empty whenever nothing in the vocabulary
			// is close enough to name honestly.
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s is not valid for confine%s",
				name, optionDidYouMean(name, confineLaunchOptionNames()))
		}
		if i+1 >= delimiter || strings.HasPrefix(argv[i+1], "--") {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s requires a value", name)
		}
		if _, exists := options[name]; exists {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s may occur once", name)
		}
		i++
		options[name] = argv[i]
	}
	target := append([]string(nil), argv[delimiter+1:]...)
	if len(target) == 0 || target[0] == "" {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: confine target argv is empty")
	}
	// AIRA-196. --stdin-connect is meaningful ONLY when detached: a foreground
	// confine already passes the caller's own stdin straight through, so there is
	// nothing for a socket to add and accepting the flag there would be a request
	// silently discarded. Refused here, synchronously, rather than ignored.
	if options["stdin-connect"] == "true" && options["detach"] != "true" {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: --stdin-connect requires --detach; a foreground confine already reads the caller's own stdin")
	}
	if _, _, err := parseScopeMemoryOptions(options, "E_CONFINE_ARGUMENT_INVALID"); err != nil {
		return nil, nil, err
	}
	if raw, present := options["memory-reserve"]; present {
		value, err := runner.ParseMemorySize(raw)
		if err != nil || value < 1<<20 {
			if err == nil {
				err = errors.New("must be at least 1MiB")
			}
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --memory-reserve: %w", err)
		}
	}
	if raw, present := options["admit-timeout"]; present {
		wait, err := time.ParseDuration(raw)
		// Reject below 1ms: the wire value is max_wait_ms (Milliseconds() truncates
		// toward zero), so a sub-1ms timeout reaches the daemon as 0 — the deferred
		// zero-wait evaluator race that falsely rejects an admissible job.
		if err != nil || wait < time.Millisecond {
			if err == nil {
				err = errors.New("must be at least 1ms")
			}
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --admit-timeout: %w", err)
		}
	}
	// AIRA-138. Both job bounds are validated SYNCHRONOUSLY here, so the caller
	// learns before any daemon round trip. A zero or negative value is an argument
	// error and never "no bound": a bound the operator asked for and silently did
	// not get is a fake pass.
	for _, name := range []string{"timeout", "cpu-timeout"} {
		raw, present := options[name]
		if !present {
			continue
		}
		if _, err := parseConfineJobBound(raw); err != nil {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --%s: %w", name, err)
		}
	}
	return target, options, nil
}

// parseConfineJobBound parses one --timeout/--cpu-timeout value. It is the ONE
// definition of what those options accept, shared by the parse-time refusal and
// the request transcription, so the two cannot drift into accepting different
// languages (AIRA-138).
func parseConfineJobBound(raw string) (time.Duration, error) {
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, errors.New("must be positive")
	}
	return value, nil
}

// drainWaitOperation is the ONE spelling of `aira drain`'s only public
// operation, taken from the dispatch table rather than restated here so the
// generated help can never advertise an operation this parser rejects.
const drainWaitOperation = core.DrainWaitOperation

// drainHoldName is the confine scope NAME every drain hold launches under. It
// is fixed rather than caller-chosen because the name is what `confine --kill`
// selects on and what the scope id embeds, and a drain's human-readable purpose
// travels in --reason instead — Name could not carry it in any case
// (ValidateConfineIdentity rejects spaces and colons, and the daemon requires
// the name to match the scope id).
const drainHoldName = "drain"

// parseDrainArgs parses `aira drain wait [--timeout D] [--admit-timeout D]
// [--reason TEXT]` (AIRA-185).
//
// It is its own parser, like confine's, for one reason: the generic parseArgs
// loop treats any non-`--` token as a positional and would silently accept
// `aira drain nonsense`. A drain holds up every other session on this machine,
// so an operation this parser does not recognise is refused by name rather than
// guessed at.
func parseDrainArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	var positional []string
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		if name != "timeout" && name != "admit-timeout" && name != "reason" {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s is not valid for drain", name)
		}
		if _, exists := options[name]; exists {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s may occur once", name)
		}
		// --reason takes free text, which may legitimately begin with "--"
		// ("--force was needed"), so only the DURATION options refuse a value that
		// looks like another flag. The bound is still that a value must exist.
		if index+1 >= len(argv) {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s requires a value", name)
		}
		if name != "reason" && strings.HasPrefix(argv[index+1], "--") {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s requires a value", name)
		}
		index++
		options[name] = argv[index]
	}
	if len(positional) != 1 || positional[0] != drainWaitOperation {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: drain requires exactly one operation, and the only one is `wait`, e.g. aira drain wait --reason \"deploy\"")
	}
	// Both bounds are validated SYNCHRONOUSLY, before any daemon round trip,
	// through the SAME helper confine's own --timeout uses, so the two verbs
	// cannot drift into accepting different duration languages.
	if raw, present := options["timeout"]; present {
		if _, err := parseConfineJobBound(raw); err != nil {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --timeout: %w", err)
		}
	}
	if raw, present := options["admit-timeout"]; present {
		wait, err := time.ParseDuration(raw)
		if err != nil || wait < time.Millisecond || wait > runner.AdmitWaitCeiling {
			if err == nil {
				err = fmt.Errorf("must be in [1ms,%s]", runner.AdmitWaitCeiling)
			}
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --admit-timeout: %w", err)
		}
	}
	if raw, present := options["reason"]; present {
		if strings.TrimSpace(raw) == "" {
			return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: --reason requires text; omit the flag to hold with no stated reason")
		}
	}
	return positional, options, nil
}

// parseDrainHoldArgs parses the INTERNAL placeholder verb `aira drain-hold`,
// which takes nothing. It is the process `aira drain wait` launches inside the
// confine scope, never something an operator runs directly, so every argument is
// refused rather than ignored.
func parseDrainHoldArgs(argv []string) ([]string, map[string]string, error) {
	if len(argv) != 0 {
		return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: drain-hold takes no arguments (got %q)", argv[0])
	}
	return nil, map[string]string{}, nil
}

func parseConfineReserveArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: unexpected confine-reserve argument %q", arg)
		}
		name := strings.TrimPrefix(arg, "--")
		if _, exists := options[name]; exists {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s may occur once", name)
		}
		if name == "pinned" {
			options[name] = "true"
			continue
		}
		if name != "bytes" && name != "signature" && name != "slice" && name != "max-wait" {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s is not valid for confine-reserve", name)
		}
		if index+1 >= len(argv) || strings.HasPrefix(argv[index+1], "--") {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s requires a value", name)
		}
		index++
		options[name] = argv[index]
	}
	if options["pinned"] != "true" || strings.TrimSpace(options["signature"]) == "" || strings.TrimSpace(options["bytes"]) == "" {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: confine-reserve requires --bytes N --pinned --signature S")
	}
	reserve, err := runner.ParseMemorySize(options["bytes"])
	if err != nil || reserve <= 0 {
		if err == nil {
			err = errors.New("must be positive")
		}
		return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --bytes: %w", err)
	}
	if raw := options["max-wait"]; raw != "" {
		wait, waitErr := time.ParseDuration(raw)
		if waitErr != nil || wait <= 0 || wait > 30*time.Minute {
			if waitErr == nil {
				waitErr = errors.New("must be in (0, 30m]")
			}
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --max-wait: %w", waitErr)
		}
	}
	return nil, options, nil
}

func parseAitestBootstrapArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	for i := 0; i < len(argv); i++ {
		name := strings.TrimPrefix(argv[i], "--")
		if argv[i] != "--supervisor-pid" {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s is not valid for aitest-bootstrap", name)
		}
		if i+1 >= len(argv) {
			return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: option --supervisor-pid requires a value")
		}
		i++
		options["supervisor-pid"] = argv[i]
	}
	if _, present := options["supervisor-pid"]; !present {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: --supervisor-pid is required")
	}
	return nil, options, nil
}

func parseWorkerAdmitArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	valid := map[string]bool{"job-id": true, "outer-scope": true, "estimated-bytes": true, "signature": true, "max-wait": true}
	for i := 0; i < len(argv); i++ {
		name := strings.TrimPrefix(argv[i], "--")
		if !valid[name] {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s is not valid for worker-admit", name)
		}
		if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s requires a value", name)
		}
		i++
		options[name] = argv[i]
	}
	for _, required := range []string{"job-id", "outer-scope", "estimated-bytes"} {
		if _, present := options[required]; !present {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --%s is required for worker-admit", required)
		}
	}
	return nil, options, nil
}

// parseWorkerPeakArgs parses the aitest supervisor's ONE end-of-run pool sample.
//
// CLI-only, like worker-admit, and for the same reason: its caller is the aitest
// supervisor relaying to the daemon, not an agent. It is deliberately not a
// dispatch-table verb — there is nothing an agent would ever ask it, and adding
// an MCP tool for a machine-to-machine report would be surface with no reader.
func parseWorkerPeakArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	valued := map[string]bool{"signature": true, "peak-rss": true, "budget": true, "budget-basis": true}
	for i := 0; i < len(argv); i++ {
		name := strings.TrimPrefix(argv[i], "--")
		if name == "oom" {
			options["oom"] = "true"
			continue
		}
		if !valued[name] {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s is not valid for worker-peak", name)
		}
		// A value may legitimately begin with "--" only if it is a signature,
		// and a pytest argument genuinely can (`--aitest-workers=auto`). So the
		// look-ahead guard that worker-admit uses is deliberately NOT copied
		// here: it would truncate exactly the keys this verb exists to carry.
		if i+1 >= len(argv) {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s requires a value", name)
		}
		i++
		options[name] = argv[i]
	}
	if options["signature"] == "" {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: --signature is required for worker-peak")
	}
	if (options["budget"] == "") != (options["budget-basis"] == "") {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: --budget and --budget-basis must be given together")
	}
	return nil, options, nil
}

// runWorkerPeakCommand relays one aitest pool sample to the daemon.
//
// It is best-effort by design and says so on stderr rather than failing loudly:
// the caller is a pytest run that has already finished its real work, and a
// suite must never be reported differently because AIRA could not record how
// much memory it used. Nothing is fabricated to fill a gap — an unparseable or
// absent term is simply not sent, and the store records it as unevaluated.
func runWorkerPeakCommand(ctx context.Context, options map[string]string, stderr io.Writer) int {
	report := runner.ConfinePeakReport{
		Kind:        string(store.ResourcePeakKindPytestWorker),
		Signature:   options["signature"],
		OOM:         options["oom"] == "true",
		BudgetBasis: options["budget-basis"],
	}
	if raw := options["peak-rss"]; raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --peak-rss must be a positive byte count, got %q\n", raw)
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		report.Peak = &value
	}
	if raw := options["budget"]; raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --budget must be a positive byte count, got %q\n", raw)
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		report.Budget = &value
	}
	if report.Budget != nil && runner.ConfineBudgetFamilyOf(report.BudgetBasis) == "" {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --budget-basis %q names no cap:/reserve: family\n", report.BudgetBasis)
		return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: daemon paths unavailable: %v\n", err)
		return codes.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := runner.ReportPeakSample(reportCtx, paths.SocketPath, report); err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: report pool peak: %v\n", err)
		return codes.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	return 0
}

func parseConfineManagementArgs(argv []string) ([]string, map[string]string, error) {
	options := map[string]string{}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: unexpected management argument %q", arg)
		}
		name, inline, hasInline := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if _, exists := options[name]; exists {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s may occur once", name)
		}
		switch name {
		case "detach":
			// Targeted rather than falling through to the generic "requires exactly
			// one of ..." message below: `--detach` is a LAUNCH option, and telling
			// an operator who forgot the delimiter that they must pick a management
			// verb sends them the wrong way entirely.
			return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: --detach requires a launch target after --, e.g. aira confine --detach --name gate -- make merge-gate")
		case "list", "steal", "budget":
			if hasInline {
				return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s does not take a value", name)
			}
			options[name] = "true"
		case "status":
			// The selector is OPTIONAL: with one, `--status` reports that job; with
			// none it lists the caller's own detached jobs, which is the natural
			// companion to refusing an ambiguous name.
			options["status"] = "true"
			if hasInline {
				if inline == "" {
					return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: option --status requires a value when written as --status=<selector>")
				}
				options["status-selector"] = inline
				continue
			}
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
				i++
				options["status-selector"] = argv[i]
			}
		case "kill", "slice", "owner":
			value := inline
			if !hasInline {
				if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
					return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s requires a value", name)
				}
				i++
				value = argv[i]
			}
			if value == "" {
				return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s requires a value", name)
			}
			options[name] = value
		default:
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: option --%s is not valid for confine management", name)
		}
	}
	list := options["list"] == "true"
	kill := options["kill"] != ""
	status := options["status"] == "true"
	budget := options["budget"] == "true"
	selected := 0
	for _, chosen := range []bool{list, kill, status, budget} {
		if chosen {
			selected++
		}
	}
	if selected != 1 {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: confine management requires exactly one of --list, --budget, --kill <selector>, or --status [<selector>]")
	}
	if !kill && options["steal"] == "true" {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: --steal is valid only with --kill")
	}
	// --status reads a durable filesystem record and never touches a cgroup
	// slice, so an accepted-and-ignored --slice would be exactly the silently
	// discarded scope AIRA-82 exists to refuse.
	if status && options["slice"] != "" {
		return nil, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: --slice is not valid with --status: a detached job's record is keyed by owner and scope id, not by slice")
	}
	if owner := options["owner"]; owner != "" {
		if err := runner.ValidateConfineIdentity(owner); err != nil {
			return nil, nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --owner: %w", err)
		}
	}
	return nil, options, nil
}

func runConfineCommand(ctx context.Context, target []string, options map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	maximum, high, err := parseScopeMemoryOptions(options, "E_CONFINE_ARGUMENT_INVALID")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	reserveRaw, reservePinned := options["memory-reserve"]
	if !reservePinned {
		reserveRaw = os.Getenv("AIRA_CONFINE_RESERVE")
		reservePinned = reserveRaw != ""
	}
	var reserve int64
	if reservePinned {
		reserve, err = runner.ParseMemorySize(reserveRaw)
		if err != nil || reserve < 1<<20 {
			if err == nil {
				err = errors.New("must be at least 1MiB")
			}
			_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --memory-reserve: %v\n", err)
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
	}
	// AIRA-62: the CLI TRANSCRIBES, it does not resolve. This used to be
	// `if maximum > 0 { reserve, reservePinned = maximum, true }` — unconditional,
	// with no delegate-ram guard, and running after --memory-reserve was parsed.
	// Since this is the only non-test producer of a runner.ConfineRequest, it made
	// the runner's own correct `!DelegateRAM && ScopeMemoryMax > 0` carve-out dead
	// code, so `--delegate-ram --memory-max 32G --memory-reserve 512M` charged the
	// shared ledger 32G rather than the 512M asked for. runner.ResolveConfineReserve
	// is now the single decision site; the non-delegate up-charge lives there and is
	// unchanged.
	admitTimeout := time.Duration(0)
	if raw := options["admit-timeout"]; raw != "" {
		admitTimeout, err = time.ParseDuration(raw)
		// AIRA-58: bound it HERE, synchronously, so the caller learns before any
		// daemon round-trip — the same already-honest shape as
		// `confine-reserve --max-wait`. The daemon enforces the same shared
		// runner.AdmitWaitCeiling independently, since a non-CLI caller reaches
		// the runner directly and an operator may run an older client.
		if err != nil || admitTimeout < time.Millisecond || admitTimeout > runner.AdmitWaitCeiling {
			if err == nil {
				err = fmt.Errorf("must be in [1ms,%s]", runner.AdmitWaitCeiling)
			}
			_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --admit-timeout: %v\n", err)
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
	}
	// AIRA-138. The CLI TRANSCRIBES both job bounds; parseConfineArgs has already
	// refused anything non-positive or unparseable, and this re-parse goes through
	// the same one helper so the two can never accept different languages.
	var jobTimeout, jobCPUTimeout time.Duration
	for _, bound := range []struct {
		name  string
		value *time.Duration
	}{{"timeout", &jobTimeout}, {"cpu-timeout", &jobCPUTimeout}} {
		raw := options[bound.name]
		if raw == "" {
			continue
		}
		parsed, boundErr := parseConfineJobBound(raw)
		if boundErr != nil {
			_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --%s: %v\n", bound.name, boundErr)
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		*bound.value = parsed
	}
	owner, err := resolveConfineOwner(ctx, options["owner"])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --owner: %v\n", err)
		return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	request := runner.ConfineRequest{
		Slice: options["slice"], Name: options["name"], Argv: append([]string(nil), target...),
		Owner:         owner,
		MemoryReserve: reserve, MemoryReservePinned: reservePinned,
		DelegateRAM:      options["delegate-ram"] == "true",
		Exclusive:        options["exclusive"] == "true",
		RequireAdmission: options["require-admission"] == "true",
		// AIRA-196. Transcribed, never inferred: with the flag absent this stays
		// false and the detached job's stdin is /dev/null, exactly as before.
		StdinConnect:   options["stdin-connect"] == "true",
		ScopeMemoryMax: maximum, ScopeMemoryHigh: high,
		AdmissionMaxWait: admitTimeout,
		Timeout:          jobTimeout, CPUTimeout: jobCPUTimeout,
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	}
	if paths, err := daemon.PathsFromEnv(); err == nil {
		request.RuntimeDir = paths.RuntimeDir
		request.AdmitSocketPath = paths.SocketPath
	} else if stderr != nil {
		_, _ = fmt.Fprintf(stderr, "confine: daemon paths unavailable; admission will use flock and no aitest coordinates are exported: %v\n", err)
	}
	// AIRA-187. Say the one true thing about a nested launch that nothing said
	// before. Printed for the detached form too, below, because a detached
	// supervisor requests admission on exactly the same terms.
	if warning := nestedConfineWarning(options, inheritedConfineScopeID()); warning != "" && stderr != nil {
		_, _ = fmt.Fprintln(stderr, warning)
	}
	if options["detach"] == "true" {
		return runConfineDetachCommand(ctx, request, stdout, stderr)
	}
	result, err := runConfined(ctx, request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return codes.ExitForCode(store.ErrorCode(err))
	}
	return result.Exit
}

// drainHoldSelfPath is the binary `aira drain wait` launches as its placeholder.
//
// `/proc/self/exe` rather than os.Executable() DELIBERATELY, and it matters for
// exactly the use this verb exists for: a drain window is when the aira binary
// on $PATH is most likely to be REPLACED underneath us, and /proc/self/exe names
// the running inode rather than a path whose contents may have changed by the
// time the setup shim execs it. It is the same default LaunchConfineDetached
// already uses for its own supervisor.
//
// A var, not a const, so a test can substitute a real helper binary without
// launching the CLI's own process tree.
var drainHoldSelfPath = "/proc/self/exe"

// runDrainCommand routes `aira drain <operation>`.
//
// The switch is not redundant with parseDrainArgs's own refusal, and that is the
// point: adding a second operation to the parser (or to the dispatch table's
// enum) without adding it here would otherwise run a WAIT for it — silently
// doing the wrong, slice-holding thing. The default arm makes that
// unrepresentable instead of something to remember.
func runDrainCommand(ctx context.Context, positional []string, options map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	operation := ""
	if len(positional) > 0 {
		operation = positional[0]
	}
	switch operation {
	case drainWaitOperation:
		return runDrainWaitCommand(ctx, options, stdin, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: unknown drain operation %q; the only one is `%s`\n", operation, drainWaitOperation)
		return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
}

// runDrainWaitCommand implements `aira drain wait` (AIRA-185).
//
// It is DELIBERATELY thin: it issues the exact same admission request `aira
// confine --exclusive -- <argv>` already makes, where <argv> is the internal
// `aira drain-hold` placeholder, launched through the real runner.Confine
// scope-creation path — real scope, real admission, real charging. Nothing here
// blocks on its own; the hold is a genuine exclusive confine job, and it
// therefore inherits exclusive mode's already-tested safety properties whole:
// connection-bound release (the daemon's deferred release fires on every
// connection-close path, so a Ctrl-C'd or SIGKILLed drain unwedges the slice
// instantly), at most one holder per slice, and no persisted state that could
// outlive the process.
//
// What it does NOT do, and must not be sold as doing: it does not protect
// already-running jobs from a deploy (they were never at risk — every confine
// job is its own cgroup scope launched by its own client, never a daemon child),
// and it is best-effort contention reduction rather than an absolute guarantee
// (a slice queue at its 256-waiter cap falls back to flock, outside the gate).
func runDrainWaitCommand(ctx context.Context, options map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Re-parsed through the SAME helpers parseDrainArgs validated with, so the
	// refusal and the transcription can never accept different languages.
	var holdFor time.Duration
	if raw := options["timeout"]; raw != "" {
		parsed, err := parseConfineJobBound(raw)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --timeout: %v\n", err)
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		holdFor = parsed
	}
	var admitTimeout time.Duration
	if raw := options["admit-timeout"]; raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < time.Millisecond || parsed > runner.AdmitWaitCeiling {
			if err == nil {
				err = fmt.Errorf("must be in [1ms,%s]", runner.AdmitWaitCeiling)
			}
			_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --admit-timeout: %v\n", err)
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		admitTimeout = parsed
	}
	owner, err := resolveConfineOwner(ctx, "")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: owner: %v\n", err)
		return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	request := runner.ConfineRequest{
		Name:            drainHoldName,
		Owner:           owner,
		Argv:            []string{drainHoldSelfPath, "drain-hold"},
		Exclusive:       true,
		ExclusiveReason: strings.TrimSpace(options["reason"]),
		// The ONE place the two clocks are separated, and the reason the help text
		// says so out loud: Timeout bounds the HELD duration only (it starts at the
		// release write, after admission and setup), while AdmissionMaxWait bounds
		// the wait to be admitted at all. Zero means "confine's own default" for
		// each, which for admission is 30 minutes — never "no bound" and never
		// "give up immediately".
		Timeout:          holdFor,
		AdmissionMaxWait: admitTimeout,
		Stdin:            stdin, Stdout: stdout, Stderr: stderr,
	}
	if paths, pathErr := daemon.PathsFromEnv(); pathErr == nil {
		request.RuntimeDir = paths.RuntimeDir
		request.AdmitSocketPath = paths.SocketPath
	} else if stderr != nil {
		// Stated, not swallowed: without daemon paths the admission attempt cannot
		// reach the daemon, and an exclusive request REFUSES rather than falling
		// back to flock, so the launch below will fail loudly. Saying why here turns
		// that refusal from a puzzle into an install problem.
		_, _ = fmt.Fprintf(stderr, "drain: daemon paths unavailable, so an exclusive admission cannot be established: %v\n", pathErr)
	}
	_, _ = fmt.Fprintln(stderr, drainWaitBanner(request, runner.DefaultConfineSlice))
	result, err := runConfined(ctx, request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return codes.ExitForCode(store.ErrorCode(err))
	}
	return result.Exit
}

// drainWaitBanner states BOTH clocks at the point of use, before anything
// blocks.
//
// It exists because `--timeout 10s` reads as "give up after 10 seconds" and is
// not: admission is a separate, already-existing budget that defaults to 30
// minutes, so a drain can legitimately sit unadmitted far longer than its own
// --timeout before the hold it bounds has even begun. The help text says this
// too; saying it again here means an operator who never reads --help still
// cannot be surprised by it.
func drainWaitBanner(request runner.ConfineRequest, defaultSlice string) string {
	slice := strings.TrimSpace(request.Slice)
	if slice == "" {
		slice = defaultSlice
	}
	// The EFFECTIVE budget, read from the one constant the runner actually applies
	// when no --admit-timeout is given, rather than a number restated here that
	// could drift away from it.
	admission := "up to " + runner.DefaultConfineAdmissionWait.String() + " (the default; --admit-timeout changes it)"
	if request.AdmissionMaxWait > 0 {
		admission = "up to " + request.AdmissionMaxWait.String()
	}
	hold := "until you interrupt it (Ctrl-C or SIGTERM)"
	if request.Timeout > 0 {
		hold = "for " + request.Timeout.String() + ", or until you interrupt it (Ctrl-C or SIGTERM)"
	}
	line := fmt.Sprintf("drain: asking to hold %s exclusively; new jobs stop being admitted and already-running ones finish untouched.", slice)
	line += fmt.Sprintf("\ndrain: waiting %s to be admitted, THEN holding %s. These are two separate budgets.", admission, hold)
	if reason := strings.TrimSpace(request.ExclusiveReason); reason != "" {
		line += "\ndrain: reason " + strconv.Quote(confineReasonForDisplay(reason))
	}
	return line
}

// runDrainHoldCommand is the INTERNAL placeholder `aira drain wait` launches
// inside its confine scope. It is not a verb an operator runs directly, and it
// is deliberately the simplest possible program: announce that the hold is real,
// then block.
//
// Announcing matters. The line below is written only once the process is
// RUNNING, which under confine means admission was granted and the scope was
// created — so it is a positive attestation that the slice is now held, not a
// claim made in advance of one. Before it appears the state is "draining", which
// `confine --list` reports as such.
//
// Release is by signal or by the confine job's own --timeout, both of which
// arrive as a cgroup.kill from the supervisor above it: a confined job has no
// graceful shutdown by design, so the handler installed here is for the
// unconfined case only and never runs in production.
func runDrainHoldCommand(stdout io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// It names every way the hold ends, not just Ctrl-C: --timeout is enforced by
	// the supervisor ABOVE this process, which knows nothing about it, and a line
	// that offered only Ctrl-C would quietly contradict an operator who set one.
	_, _ = fmt.Fprintln(stdout, "drain: the slice is now HELD — no new jobs are being admitted. It is released when this process ends: Ctrl-C, SIGTERM, or --timeout expiring.")
	<-ctx.Done()
	return 0
}

// runConfineDetachCommand launches a session-independent confine supervisor.
//
// Its exit code means "the supervisor started and every synchronous precondition
// passed" — never "the job succeeded", which cannot be known yet. That is the
// single largest false-pass risk in this verb, so the wording below states it
// outright rather than leaving an automated caller to infer success from 0.
func runConfineDetachCommand(ctx context.Context, request runner.ConfineRequest, stdout, stderr io.Writer) int {
	if paths, err := daemon.PathsFromEnv(); err == nil {
		request.DetachStateDir = paths.ConfineDetachDir
	} else {
		// Fail closed. A detached launch with nowhere durable to record its
		// outcome would lose the very result the operator detached in order to
		// keep, so it must never silently degrade to a foreground run.
		_, _ = fmt.Fprintf(stderr, "%s: cannot resolve the durable record directory: %v\n", runner.CodeConfineDetachFailed, err)
		return codes.ExitForCode(runner.CodeConfineDetachFailed)
	}
	// A detached job's stdio belongs to its capture files, never to the launching
	// terminal, which is about to go away.
	request.Stdin, request.Stdout, request.Stderr = nil, nil, nil
	launch, err := launchConfineDetached(ctx, request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return codes.ExitForCode(store.ErrorCode(err))
	}
	if launch == nil || launch.Acknowledge == nil {
		// A launch with no acknowledgement channel cannot be confirmed, and an
		// unconfirmed supervisor abandons the job — so reporting success here
		// would be a fabricated one.
		_, _ = fmt.Fprintf(stderr, "%s: the detached launch returned no acknowledgement channel\n", runner.CodeConfineDetachFailed)
		return codes.ExitForCode(runner.CodeConfineDetachFailed)
	}
	delivered := true
	for _, line := range []string{
		fmt.Sprintf("confine: detached scope %s on %s (supervisor pid %d)", launch.ScopeID, launch.Slice, launch.SupervisorPID),
		"confine:   stdout " + launch.StdoutPath,
		"confine:   stderr " + launch.StderrPath,
		"confine:   record " + launch.RecordPath,
		"confine: the job's exit code is NOT known yet — this exit 0 means the supervisor started, not that the job succeeded.",
		"confine: poll it with: aira confine --status " + launch.ScopeID,
	} {
		if _, writeErr := fmt.Fprintln(stdout, line); writeErr != nil {
			delivered = false
			break
		}
	}
	if ackErr := launch.Acknowledge(delivered); ackErr != nil || !delivered {
		// The handle did not reach anyone, so the supervisor abandons the launch
		// and this must not report success: a job nobody holds a handle to would
		// consume the shared slice invisibly.
		_, _ = fmt.Fprintf(stderr, "%s: the detached handle could not be delivered; the supervisor was told to abandon the launch\n", runner.CodeConfineDetachFailed)
		return codes.ExitForCode(runner.CodeConfineDetachFailed)
	}
	return 0
}

// runConfineStatusCommand answers `aira confine --status [<selector>]` from the
// durable record store, with no daemon involvement.
//
// Its exit code reports the STATUS QUERY, never the job: 0 when the state was
// established, 3 when it could not be, 2 for a miss or an ambiguous selector.
// The job's own exit code is printed instead. Overloading one channel with two
// meanings is exactly the kind of dishonesty this project's error contract
// exists to prevent.
func runConfineStatusCommand(ctx context.Context, options map[string]string, jsonOutput bool, stdout, stderr io.Writer) int {
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: cannot resolve the durable record directory: %v\n", runner.CodeConfineOutcomeUnknown, err)
		return codes.ExitForCode(runner.CodeConfineOutcomeUnknown)
	}
	owner, ownerErr := resolveConfineOwner(ctx, options["owner"])
	if ownerErr != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --owner: %v\n", ownerErr)
		return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	selector := strings.TrimSpace(options["status-selector"])
	if selector == "" {
		statuses, listErr := confineDetachStatusList(paths.ConfineDetachDir, owner)
		if listErr != nil {
			_, _ = fmt.Fprintln(stderr, listErr)
			return codes.ExitForCode(store.ErrorCode(listErr))
		}
		if jsonOutput {
			return writeConfineStatusJSON(statuses, stdout, stderr)
		}
		if len(statuses) == 0 {
			_, _ = fmt.Fprintf(stdout, "confine: no detached confine records for owner %s\n", owner)
			return 0
		}
		for _, status := range statuses {
			_, _ = fmt.Fprintln(stdout, runner.FormatConfineDetachStatus(status))
		}
		return 0
	}
	status, statusErr := confineDetachStatusFor(paths.ConfineDetachDir, selector, owner)
	if statusErr != nil {
		_, _ = fmt.Fprintln(stderr, statusErr)
		return codes.ExitForCode(store.ErrorCode(statusErr))
	}
	if jsonOutput {
		return writeConfineStatusJSON(status, stdout, stderr)
	}
	_, _ = fmt.Fprintln(stdout, runner.FormatConfineDetachStatus(status))
	if status.State == runner.ConfineDetachOutcomeUnknown {
		return codes.ExitForCode(runner.CodeConfineOutcomeUnknown)
	}
	return 0
}

func writeConfineStatusJSON(payload any, stdout, stderr io.Writer) int {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", runner.CodeConfineOutcomeUnknown, err)
		return codes.ExitForCode(runner.CodeConfineOutcomeUnknown)
	}
	_, _ = stdout.Write(append(encoded, '\n'))
	if status, ok := payload.(runner.ConfineDetachStatus); ok && status.State == runner.ConfineDetachOutcomeUnknown {
		return codes.ExitForCode(runner.CodeConfineOutcomeUnknown)
	}
	return 0
}

// runConfineSupervisor is the hidden `__confine-supervise` verb: the setsid'd
// process that owns a detached confine job for its whole life. It mirrors
// runSupervisor's argument handling, including reporting a malformed invocation
// down the ready channel so the launcher learns rather than waiting out its
// timeout.
func runConfineSupervisor(argv []string) int {
	values := map[string]string{}
	for i := 0; i < len(argv); i += 2 {
		if i+1 >= len(argv) || !strings.HasPrefix(argv[i], "--") {
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		name := strings.TrimPrefix(argv[i], "--")
		if name != "control" && name != "ready-fd" && name != "ack-fd" {
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		if _, exists := values[name]; exists {
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		values[name] = argv[i+1]
	}
	readyFD, readyErr := strconv.Atoi(values["ready-fd"])
	ackFD, ackErr := strconv.Atoi(values["ack-fd"])
	if values["control"] == "" || readyErr != nil || ackErr != nil || readyFD < 0 || ackFD < 0 {
		return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	if err := superviseConfineDetached(context.Background(), values["control"], readyFD, ackFD); err != nil {
		// fd 2 is the job's supervisor.log by the time the supervisor has a record
		// store, so this is the one place an operator can later read WHY a
		// detached job ended the way it did. Dropping it would leave an empty log
		// beside a record that points at it.
		_, _ = fmt.Fprintf(os.Stderr, "confine supervisor: %v\n", err)
		return codes.ExitForCode(store.ErrorCode(err))
	}
	return 0
}

func runConfineReserveCommand(ctx context.Context, options map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	reserve, err := runner.ParseMemorySize(options["bytes"])
	if err != nil || reserve <= 0 {
		if err == nil {
			err = errors.New("must be positive")
		}
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: --bytes: %v\n", err)
		return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	maxWait := runner.DefaultConfineReserveMaxWait
	if raw := options["max-wait"]; raw != "" {
		maxWait, err = time.ParseDuration(raw)
		if err != nil || maxWait <= 0 || maxWait > 30*time.Minute {
			_, _ = fmt.Fprintln(stderr, "E_CONFINE_ARGUMENT_INVALID: invalid --max-wait")
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
	}
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: daemon paths unavailable: %v\n", err)
		return codes.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	request := runner.ConfineReserveRequest{
		Slice: options["slice"], AdmitSocketPath: paths.SocketPath,
		Bytes: reserve, Pinned: options["pinned"] == "true",
		Signature: options["signature"], MaxWait: maxWait,
	}
	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	reservation, err := reserveConfined(signalCtx, request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return codes.ExitForCode(store.ErrorCode(err))
	}
	defer reservation.Close()
	if reservation.ClampedFrom > 0 {
		_, _ = fmt.Fprintf(stderr, "aira: RAM reservation clamped from %d to daemon ceiling %d\n", reservation.ClampedFrom, reservation.Reserve)
	}
	if _, err := fmt.Fprintf(stdout, "granted reserve=%d basis=%s\n", reservation.Reserve, reservation.Basis); err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: write grant: %v\n", err)
		return codes.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	// THE HOLD. Everything above was bounded by --max-wait; NOTHING below is, and
	// that is the contract (AIRA-69 design spec §4): the reservation must last as
	// long as the test it was granted for, so the helper blocks here until its
	// stdin reaches EOF or it is signalled. `--max-wait` bounds the ADMISSION
	// WAIT and nothing else.
	//
	// AIRA-108 was filed as a P0 against this, on the reasoning that a helper
	// alive 52 minutes past its own `--max-wait 300s` must have blown its bound.
	// It had not: it was here, holding a valid reservation for a caller whose test
	// had stopped making progress. Two states of this process are byte-identical
	// in `ps`, and only /proc tells them apart — waiting shows goroutine 1 in
	// `[IO wait]` under net.(*conn).Read with a thread on epoll_pwait; holding
	// shows goroutine 1 in `[select]` right here, with a thread in read(2) on
	// fd 0. `aira confine --list` now names every granted reservation and its age
	// so nobody has to go to /proc again for this.
	//
	// Consequence worth knowing before changing anything here: the release signal
	// is the WRITE END of this pipe being closed everywhere, so any descendant
	// that inherited it keeps the reservation alive after the original parent
	// exits. That is why the governor plugin closes it in an at-fork hook.
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdin)
		close(done)
	}()
	select {
	case <-done:
	case <-signalCtx.Done():
	}
	return 0
}

func runAitestBootstrapCommand(ctx context.Context, options map[string]string, stdout, stderr io.Writer) int {
	pid, err := strconv.Atoi(options["supervisor-pid"])
	if err != nil || pid <= 0 {
		_, _ = fmt.Fprintln(stderr, "E_CONFINE_ARGUMENT_INVALID: --supervisor-pid must be a positive integer")
		return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
	}
	// AIRA-121 gate condition C1, as AMENDED by AIRA-123. aitest-bootstrap
	// relocates the supervisor into a child scope of the job's OUTER cgroup scope
	// so that scope can delegate controllers to worker children. In ci-shim mode
	// there is no outer scope and no cgroup to relocate within, so this must not
	// reach CurrentCgroupPath's self-discovery below: that would nominate whatever
	// cgroup the container happens to live in as "outer" and then fail far later,
	// in a place that reads as a broken install rather than a designed
	// degradation. AIRA-121 shipped that guard as a clean, immediate FAILURE
	// (supervisor.py's bootstrap() calls _disable_daemon on a non-zero exit,
	// dropping the suite to its one-warning bare-fork pool); AIRA-123 replaces the
	// failure with an honest degraded SUCCESS, for the reason stated inside the
	// branch. Only the disposition changed -- the "do not self-discover" reasoning
	// is unchanged and still the reason this branch exists at all.
	if runner.ResolveConfineMode() == runner.ConfineModeShim {
		// AIRA-123. There is still nothing to relocate into -- the reasoning
		// above stands -- but a clean FAILURE is no longer the right answer,
		// because worker-admit can now make a real ledger-only admission decision
		// with no cgroup at all. Failing here would call _disable_daemon and drop
		// the whole suite to its ungoverned bare-fork pool, which is exactly the
		// value AIRA-123 exists to recover.
		//
		// So this reports SUCCESS with two honest facts and no third: the outer
		// "scope" is the ci-shim sentinel (not a path, and the daemon refuses to
		// treat it as one), and the admission grade is ledger-only. NO
		// supervisor_scope token is emitted, deliberately -- there is no such
		// cgroup, and supervisor.py's _cleanup_supervisor_scope correctly does
		// nothing when it is absent rather than rmdir'ing something invented.
		_, _ = fmt.Fprintf(stdout, "outer=%s admission=%s\n", runner.ShimConfineSlice, runner.AitestAdmissionLedgerOnly)
		_, _ = fmt.Fprintln(stderr, "aira aitest: ci-shim mode -- per-worker admission is LEDGER-ONLY (advisory): workers are admitted against the container's RAM budget, but there is no cgroup sub-scope, no memory.max and no kill backstop")
		return 0
	}
	// AIRA_AITEST_OUTER_SCOPE is the launcher's own scope.Reference(), injected
	// by AppendAitestChildEnvironment. Prefer it over self-discovery (AIRA-44):
	// a second aitest-enabled pytest run inside one confine job is, by the time
	// it bootstraps, already living in <outer>/.aira-supervisor — the first run's
	// drain relocated `make`, its shell and everything else there — so
	// CurrentCgroupPath() would name the supervisor scope as "outer", nest a
	// second supervisor scope inside it, and leave every worker-admit call
	// answering "unevaluated: unbounded" against a deliberately-uncapped cgroup.
	// The env value is not trusted blindly: BootstrapAitestSupervisor's
	// membership guard still refuses any scope the supervisor is not actually
	// inside.
	outer := strings.TrimSpace(os.Getenv("AIRA_AITEST_OUTER_SCOPE"))
	if outer != "" {
		// Refuse a relative path rather than resolving it against whatever
		// working directory pytest happened to have: bootstrap would mutate one
		// cgroup and then report an `outer=` the daemon later resolves from a
		// different directory, which is silently wrong accounting instead of a
		// clean error. Clean() also normalises a trailing slash so the reported
		// path and the daemon's are byte-identical.
		if !filepath.IsAbs(outer) {
			_, _ = fmt.Fprintf(stderr, "E_CONFINE_ARGUMENT_INVALID: AIRA_AITEST_OUTER_SCOPE must be an absolute cgroup path, got %q\n", outer)
			return codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")
		}
		outer = filepath.Clean(outer)
	}
	if outer == "" {
		// Unset means a launcher that predates this coordinate, or a hand
		// invocation. Self-discovery is still correct for the single-run case,
		// so fall back rather than refuse.
		discovered, err := runner.CurrentCgroupPath()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: discover outer scope: %v\n", err)
			return codes.ExitForCode("E_CONFINE_UNAVAILABLE")
		}
		outer = discovered
	}
	supervisorScope, err := runner.BootstrapAitestSupervisor(ctx, outer, pid)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return codes.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	// AIRA-123: `admission=` is stated on BOTH bootstrap paths. Emitting it only
	// on the degraded one would make its ABSENCE the claim that per-worker cgroup
	// sub-scopes are in play, which is the same "absence reads as the strong
	// guarantee" shape the containment token on the grant line exists to close.
	_, _ = fmt.Fprintf(stdout, "bootstrapped outer=%s supervisor_scope=%s admission=%s\n",
		outer, supervisorScope, runner.AitestAdmissionSubScope)
	return 0
}

// writeWorkerAdmitOutcome emits the ONE machine-readable line the aitest
// supervisor parses, on stdout, plus a human diagnostic on stderr that nothing
// parses. Every exit from the worker-admit verb goes through here — including
// the pre-dispatch argument failures in main() — so "there is always exactly
// one structured outcome" is a property of the verb, not a hope. Before this,
// non-grants were free text on stderr and the supervisor re-derived their
// meaning with eleven substring probes whose default was to run the rest of
// the suite unconfined (AIRA-42).
//
// errorCode is the store code whose exit status the verb returns; "" means the
// granted path (exit 0). A render failure is itself reported, but there is by
// definition no channel left to report it ON, so it only reaches stderr.
func writeWorkerAdmitOutcome(stdout, stderr io.Writer, outcome runner.WorkerAdmitOutcome, grant *runner.WorkerAdmitGrantFields, errorCode string) int {
	line, err := runner.WorkerAdmitOutcomeLine(outcome, grant)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: worker-admit could not render its outcome: %v\n", err)
		return codes.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	if _, err := fmt.Fprintln(stdout, line); err != nil {
		_, _ = fmt.Fprintf(stderr, "E_CONFINE_UNAVAILABLE: write worker-admit outcome: %v\n", err)
		return codes.ExitForCode("E_CONFINE_UNAVAILABLE")
	}
	if errorCode == "" {
		return 0
	}
	detail := outcome.Detail
	if detail == "" {
		detail = outcome.Reason
	}
	_, _ = fmt.Fprintf(stderr, "%s: worker-admit %s (%s): %s\n", errorCode, outcome.State, outcome.Reason, detail)
	return codes.ExitForCode(errorCode)
}

func runWorkerAdmitCommand(ctx context.Context, options map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Mirrors --memory-reserve's identical 1MiB floor (parseConfineArgs,
	// above) -- this used to only reject <=0, so an out-of-range value below
	// the daemon's own workerAdmitEstimatedBytesMin floor reached a live,
	// healthy daemon, got rejected at the protocol level, and was wrapped by
	// RequestWorkerAdmit as a generic "request rejected" message matching
	// none of supervisor.py's denied/timeout/unevaluated substrings --
	// misclassified as total daemon unavailability instead of a client
	// argument mistake (found by Sol build-review, AIRA-38 review wave).
	// The 1<<50 (1 PiB) top end mirrors the daemon's own admitMaxReserve
	// (internal/daemon/admit.go, unexported -- hardcoded here exactly as
	// the floor already is, matching --memory-reserve's own precedent): an
	// oversized value used to sail through this check and hit the daemon's
	// protocol-level rejection (E_DAEMON_PROTOCOL) instead, which ALSO
	// matched none of the classifier's substrings -- the identical
	// misclassification bug at the opposite extreme (found by Fable
	// re-gate).
	estimatedBytes, err := runner.ParseMemorySize(options["estimated-bytes"])
	if err != nil || estimatedBytes < 1<<20 || estimatedBytes > 1<<50 {
		if err == nil {
			err = errors.New("must be at least 1MiB and no larger than 1PiB")
		}
		return writeWorkerAdmitOutcome(stdout, stderr, runner.WorkerAdmitOutcome{
			State: runner.WorkerAdmitStateArgumentInvalid, Class: runner.WorkerAdmitClassRequestInvalid,
			Reason: runner.WorkerAdmitReasonEstimatedBytesOutOfRange,
			Detail: fmt.Sprintf("--estimated-bytes: %v", err),
		}, nil, "E_CONFINE_ARGUMENT_INVALID")
	}
	maxWait := runner.DefaultConfineReserveMaxWait
	if raw := options["max-wait"]; raw != "" {
		// AIRA-64: zero is VALID and means "speculative" -- answer from what can
		// be obtained without waiting. It used to be refused here as
		// argument-invalid/request-invalid, which the aitest supervisor treats
		// as TERMINAL and responds to by draining its remaining queue to
		// `unevaluated`. The daemon's own validator has always accepted zero
		// (validateWorkerAdmitArgs); only this side disagreed, so a speculative
		// pool-growth probe would have destroyed the run it was trying to help
		// (found by Sol plan-review round 2). Negatives are still refused.
		if maxWait, err = time.ParseDuration(raw); err != nil || maxWait < 0 {
			return writeWorkerAdmitOutcome(stdout, stderr, runner.WorkerAdmitOutcome{
				State: runner.WorkerAdmitStateArgumentInvalid, Class: runner.WorkerAdmitClassRequestInvalid,
				Reason: runner.WorkerAdmitReasonMaxWaitInvalid, Detail: "invalid --max-wait",
			}, nil, "E_CONFINE_ARGUMENT_INVALID")
		}
	}
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		return writeWorkerAdmitOutcome(stdout, stderr, runner.WorkerAdmitOutcome{
			State: runner.WorkerAdmitStateUnavailable, Class: runner.WorkerAdmitClassAdmissionUnusable,
			Reason: runner.WorkerAdmitReasonDaemonPathsUnavailable,
			Detail: fmt.Sprintf("daemon paths unavailable: %v", err),
		}, nil, "E_CONFINE_UNAVAILABLE")
	}
	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	outcome := runner.RequestWorkerAdmit(signalCtx, runner.WorkerAdmitClientRequest{
		SocketPath: paths.SocketPath, JobID: options["job-id"], OuterScope: options["outer-scope"],
		Signature: options["signature"], EstimatedBytes: estimatedBytes, MaxWait: maxWait,
	})
	if !outcome.Granted() {
		// The daemon's (or the transport's) own classification is relayed
		// VERBATIM. Nothing here re-derives it, and nothing downstream
		// re-derives it from prose either — that round trip through a
		// human sentence is the AIRA-42 defect this whole channel removes.
		return writeWorkerAdmitOutcome(stdout, stderr, outcome, nil, "E_CONFINE_UNAVAILABLE")
	}
	// AIRA-39: the DAEMON creates the worker scope now, inside the same
	// critical section that decided to grant, so there is no local
	// CreateWorkerScope call here any more and no grant->creation window for a
	// dying client to leave open. The scope named below already exists, with
	// its memory.max, memory.swap.max and memory.oom.group verified daemon-side
	// (AIRA-35 retired the memory.high this used to name).
	//
	// AIRA-42 merge note: this fix was built against the pre-AIRA-39 shape,
	// where a LOCAL CreateWorkerScope failure here produced the
	// `placement-failed` class. That call site is gone, so the CLI has no
	// placement path left to classify; a scope-creation failure is now the
	// daemon's own `worker-scope-create-failed` denial, classified daemon-side
	// like every other verdict. `placement-failed` survives in the catalogue
	// because supervisor.py still raises WorkerPlacementFailed from its own
	// fork/ack path -- it is simply no longer produced by this relay.
	//
	// A failed write here therefore leaves the daemon-created scope on the
	// tree, charging the AIRA-39 ledger until the outer confine job's teardown
	// removes the subtree. Not removing it is deliberate and matches the
	// symmetric decision the daemon itself makes on its own failed response
	// write (worker_admit.go: "Removing it here is NOT safe"). The CLI does not
	// own this scope, and a relay that unilaterally rmdir'd a cgroup the daemon
	// created and the supervisor may already have been told about would trade a
	// loud, retriable over-charge for a silent unconfined run.
	lease := outcome.Lease
	// AIRA-123: the grade and its coordinates are relayed together and
	// unchanged. This relay does not derive containment, does not default it,
	// and does not decide which coordinates a grade may carry -- the daemon
	// stated it and WorkerAdmitOutcomeLine refuses any combination that
	// contradicts itself, so a malformed grant fails loudly here rather than
	// reaching a supervisor that would read it as enforced.
	grantFields := &runner.WorkerAdmitGrantFields{
		ScopePath: lease.ScopePath, WorkerID: lease.WorkerID,
		MemoryMax: lease.MemoryMax, SwapCap: lease.SwapCap,
		CPUSlots: lease.CPUSlots, Containment: lease.Containment,
		Reserved: lease.Reserved,
	}
	if exit := writeWorkerAdmitOutcome(stdout, stderr, outcome, grantFields, ""); exit != 0 {
		_ = lease.Close()
		return exit
	}
	// Hold stdin open as the release signal, exactly mirroring confine-reserve.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stdin); close(done) }()
	select {
	case <-done:
	case <-signalCtx.Done():
	}
	_ = lease.Close()
	return 0
}

func resolveConfineOwner(ctx context.Context, explicit string) (string, error) {
	return resolveOwnerIn(ctx, explicit, "")
}

// resolveOwnerIn is resolveConfineOwner's chain rooted at an explicit
// directory. dir == "" preserves the historical behaviour exactly: discover
// from ".", and infer from the process cwd.
//
// AIRA-176 needs the parameterised form because `worktree register` must
// resolve identity for the WORKTREE BEING REGISTERED. Over MCP the server
// process's cwd is wherever the host launched it — not the caller's checkout —
// so a cwd-rooted resolution would attribute the binding to the wrong place
// (and, via the @cwd- inference, to a directory that is not even in the repo).
func resolveOwnerIn(ctx context.Context, explicit, dir string) (string, error) {
	if explicit != "" {
		if err := runner.ValidateConfineIdentity(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	if environment := strings.TrimSpace(os.Getenv("AIRA_CONFINE_OWNER")); environment != "" {
		if err := runner.ValidateConfineIdentity(environment); err != nil {
			return "", err
		}
		return environment, nil
	}
	discoverDir := dir
	if discoverDir == "" {
		discoverDir = "."
	}
	if project, err := app.Discover(ctx, discoverDir); err == nil && project.WorktreeID != "" {
		if err := runner.ValidateConfineIdentity(project.WorktreeID); err == nil {
			return project.WorktreeID, nil
		}
	}
	if dir != "" {
		return runner.InferConfineOwner(dir), nil
	}
	// Last resort: INFER from the launch directory rather than reporting the
	// literal "unknown" (AIRA-23). The reported hazard was a session about to
	// pgrep-kill two sibling sessions' jobs, all of them showing OWNER "unknown",
	// and being saved only by inspecting each process's cwd by hand — so cwd is
	// precisely the discriminator that was missing from `--list`.
	//
	// The inferred value is marked (runner.ConfineInferredOwnerPrefix) and is
	// therefore NEVER accepted as ownership proof by the kill guard: two sessions
	// in one directory infer the same string, so honouring it would let either
	// kill the other's job without --steal. It makes `--list` actionable; it does
	// not make the guard weaker, which AIRA-23 explicitly forbids.
	cwd, err := os.Getwd()
	if err != nil {
		return runner.ConfineUnknownOwner, nil
	}
	return runner.InferConfineOwner(cwd), nil
}

func runConfineManagementCommand(ctx context.Context, options map[string]string, jsonOutput bool, stdout, stderr io.Writer, injected Dispatcher) int {
	owner, err := resolveConfineOwner(ctx, options["owner"])
	if err != nil {
		return render(core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: --owner: " + err.Error(), Exit: codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}, jsonOutput, stdout, stderr)
	}
	verb := "confine-list"
	args := map[string]any{"slice": options["slice"], "owner": owner}
	if options["budget"] == "true" {
		verb = "confine-budget"
	}
	if selector := options["kill"]; selector != "" {
		verb = "confine-kill"
		args["selector"] = selector
		args["steal"] = options["steal"] == "true"
	}
	return dispatchConfineManagementRequest(ctx, core.Request{Verb: verb, Args: args}, jsonOutput, stdout, stderr, injected)
}

func dispatchConfineManagementRequest(ctx context.Context, request core.Request, jsonOutput bool, stdout, stderr io.Writer, injected Dispatcher) int {
	if request.Args == nil {
		request.Args = map[string]any{}
	}
	slice, _ := request.Args["slice"].(string)
	request.Args["slice"] = runner.ResolveConfineSlice(slice)
	dispatcher := injected
	var err error
	if dispatcher == nil {
		dispatcher, err = newDaemonDispatcher(nil, stdout, stderr, jsonOutput)
		if err != nil {
			return render(transportErrorResponse(err), jsonOutput, stdout, stderr)
		}
	}
	response := dispatcher.Dispatch(ctx, daemon.WorktreeScope{}, request)
	if request.Verb == "confine-list" && !jsonOutput && response.OK {
		return renderConfineListResponse(response, stdout, stderr)
	}
	if request.Verb == "confine-budget" && !jsonOutput && response.OK {
		return renderConfineBudgetResponse(response, stdout, stderr)
	}
	return render(response, jsonOutput, stdout, stderr)
}

// dispatchConfineJobIORequest runs `confine-log` / `confine-input` (AIRA-196).
//
// It is separate from dispatchConfineManagementRequest for two reasons that are
// not cosmetic: neither verb takes a --slice to resolve, and confine-input needs
// the caller's REAL stdin (management passes nil), which is where the bytes come
// from when no --data was given.
func dispatchConfineJobIORequest(ctx context.Context, request core.Request, jsonOutput bool, stdin io.Reader, stdout, stderr io.Writer, injected Dispatcher) int {
	dispatcher := injected
	if dispatcher == nil {
		production, err := newDaemonDispatcher(stdin, stdout, stderr, jsonOutput)
		if err != nil {
			return render(transportErrorResponse(err), jsonOutput, stdout, stderr)
		}
		// The CLI's stdin IS the operator's bytes, so confine-input may forward
		// it. Set here rather than in the constructor because the MCP face shares
		// that constructor and hands it the JSON-RPC protocol stream instead.
		production.stdinCarriesJobInput = true
		dispatcher = production
	}
	response := dispatcher.Dispatch(ctx, daemon.WorktreeScope{}, request)
	// Byte-transparent by default, exactly like run-log: the captured bytes go to
	// stdout unaltered and the metadata to stderr as one JSON line, which is what
	// a piping consumer wants. --json switches to the enveloped form.
	if request.Verb == "confine-log" && !jsonOutput {
		return renderConfineLog(response, stdout, stderr)
	}
	return render(response, jsonOutput, stdout, stderr)
}

// renderConfineLog mirrors renderRunLog. The metadata line is written even for a
// filtered or truncated read -- especially then: `filtered` and `truncated` are
// how a caller knows the bytes on stdout are not the whole story.
func renderConfineLog(response core.Response, stdout, stderr io.Writer) int {
	if chunk, ok := response.Data.(*runner.ConfineLogChunk); ok && chunk != nil {
		_, _ = stdout.Write(chunk.Bytes)
		metadata := map[string]any{
			"scope_id": chunk.ScopeID, "name": chunk.Name, "owner": chunk.Owner,
			"stream": chunk.Stream, "path": chunk.Path, "offset": chunk.Offset,
			"next_offset": chunk.NextOffset, "total_bytes": chunk.TotalBytes,
			"complete": chunk.Complete, "truncated": chunk.Truncated,
			"filtered": chunk.Filtered, "grep": chunk.Grep,
			"state": chunk.State, "reason": chunk.Reason,
			"exit": chunk.Exit, "error_code": chunk.ErrorCode,
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
		case "cwd", "stdin", "timeout", "cpu-timeout", "ticket", "phase", "label", "tool", "report", "report-stream", "suite", "shard", "retry", "usage", "provider", "memory-max", "memory-high":
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
	if _, _, err := parseScopeMemoryOptions(options, "E_RUN_ARGUMENT_INVALID"); err != nil {
		return nil, nil, err
	}
	return target, options, nil
}

func parseScopeMemoryOptions(options map[string]string, code string) (int64, int64, error) {
	parse := func(name string) (int64, error) {
		raw, present := options[name]
		if !present {
			return 0, nil
		}
		value, err := runner.ParseMemorySize(raw)
		if err != nil {
			return 0, fmt.Errorf("%s: --%s: %w", code, name, err)
		}
		return value, nil
	}
	maximum, err := parse("memory-max")
	if err != nil {
		return 0, 0, err
	}
	high, err := parse("memory-high")
	if err != nil {
		return 0, 0, err
	}
	_, maximumRequested := options["memory-max"]
	_, highRequested := options["memory-high"]
	if highRequested && !maximumRequested {
		return 0, 0, fmt.Errorf("%s: --memory-high requires --memory-max", code)
	}
	if maximumRequested && maximum == 0 {
		return 0, 0, fmt.Errorf("%s: --memory-max must be at least 1MiB", code)
	}
	if err := runner.ValidateScopeMemoryCap(maximum, high); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", code, err)
	}
	return maximum, high, nil
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
	case "eject":
		if len(positional) > 1 {
			return core.Request{}, errors.New("E_SELECTOR_AMBIGUOUS: eject accepts at most one project selector")
		}
		project := options["project"]
		if len(positional) == 1 {
			if project != "" || options["prefix"] != "" {
				return core.Request{}, errors.New("E_SELECTOR_AMBIGUOUS: choose one eject selector")
			}
			project = positional[0]
		}
		if project != "" && options["prefix"] != "" {
			return core.Request{}, errors.New("E_SELECTOR_AMBIGUOUS: choose --project or --prefix")
		}
		args["project"], args["prefix"] = project, options["prefix"]
		args["purge"], args["force"] = options["purge"] == "true", options["force"] == "true"
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
		if value, ok := options["memory-max"]; ok {
			args["memory_max"] = value
		}
		if value, ok := options["memory-high"]; ok {
			args["memory_high"] = value
		}
		args["timeout"] = options["timeout"]
		args["cpu_timeout"] = options["cpu-timeout"]
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
	case "confine":
		if len(positional) == 0 {
			return core.Request{}, errors.New("E_CONFINE_ARGUMENT_INVALID: confine target argv is empty")
		}
		args["argv"] = append([]string(nil), positional...)
		args["slice"], args["name"] = options["slice"], options["name"]
		if value, ok := options["memory-reserve"]; ok {
			args["memory_reserve"] = value
		}
		if value, ok := options["memory-max"]; ok {
			args["memory_max"] = value
		}
		if value, ok := options["memory-high"]; ok {
			args["memory_high"] = value
		}
	case "confine-log":
		if len(positional) != 1 || positional[0] == "" {
			return core.Request{}, errors.New("E_CONFINE_ARGUMENT_INVALID: confine-log requires one selector: a detached confine name, supervisor pid, or scope id")
		}
		args["selector"] = positional[0]
		args["stream"], args["from"], args["tail"] = options["stream"], options["from"], options["tail"]
		args["follow"] = options["follow"] == "true"
		args["full"] = options["full"] == "true"
		args["grep"], args["owner"] = options["grep"], options["owner"]
	case "confine-input":
		if len(positional) != 1 || positional[0] == "" {
			return core.Request{}, errors.New("E_CONFINE_ARGUMENT_INVALID: confine-input requires one selector: a detached confine name, supervisor pid, or scope id")
		}
		args["selector"] = positional[0]
		args["close"] = options["close"] == "true"
		args["steal"] = options["steal"] == "true"
		args["owner"] = options["owner"]
	case "confine-list":
		if len(positional) != 0 {
			return core.Request{}, errors.New("E_CONFINE_ARGUMENT_INVALID: confine-list accepts no selector")
		}
		args["slice"], args["owner"] = options["slice"], options["owner"]
	case "confine-budget":
		// No selector by design. A subject key is a NUL- or unit-separator-joined
		// argv, which is not a thing anyone can type; v1 answers for every subject
		// at once, worst-first, and the operator reads their own command off the
		// SUBJECT column.
		if len(positional) != 0 {
			return core.Request{}, errors.New("E_CONFINE_ARGUMENT_INVALID: confine-budget accepts no selector")
		}
		args["slice"], args["owner"] = options["slice"], options["owner"]
	case "confine-kill":
		if len(positional) != 1 || positional[0] == "" {
			return core.Request{}, errors.New("E_CONFINE_ARGUMENT_INVALID: confine-kill requires one selector")
		}
		args["selector"] = positional[0]
		args["steal"] = options["steal"] == "true"
		args["slice"], args["owner"] = options["slice"], options["owner"]
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
		args["grep"] = options["grep"]
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
		// The target selector names the project the operation applies to and is
		// carried on EVERY rant sub-verb, not just capture: a rant filed into a
		// shared tool's project is read, reviewed and redacted there too
		// (AIRA-179). Both selectors together are refused by the one resolver
		// that owns the vocabulary, not re-checked here.
		args["project"], args["prefix"] = options["project"], options["prefix"]
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
	case "worktree-register":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("E_SELECTOR_INVALID: worktree register requires exactly one ticket selector")
		}
		args["selector"], args["base"] = positional[0], options["base"]
		// owner/owner_attested are stamped after scope resolution
		// (stampWorktreeOwner); an --owner given here is only the chain's first
		// leg, never the final value.
		args["owner"] = options["owner"]
		args["owner_attested"] = false
	case "worktree-audit":
		if len(positional) > 1 {
			return core.Request{}, errors.New("E_SELECTOR_AMBIGUOUS: worktree audit accepts at most one selector")
		}
		if len(positional) == 1 {
			args["selector"] = positional[0]
		}
		args["base"] = options["base"]
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
	case "intent-retire":
		if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
			return core.Request{}, errors.New("E_SELECTOR_INVALID: intent-retire requires exactly one selector")
		}
		args["selector"] = positional[0]
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
		default:
			return core.Request{}, fmt.Errorf("unknown gate operation %q", args["subverb"])
		}
		for option, argument := range map[string]string{"checker": "checker", "predicate": "predicate", "cwd": "cwd", "timeout-ms": "timeout_ms", "output-cap-bytes": "output_cap_bytes", "parser": "parser", "mutation-kind": "mutation_kind", "mutation-file": "mutation_file", "mutation-test": "mutation_test", "mutation-occurrence": "mutation_occurrence", "mutation-pkgdir": "mutation_pkgdir", "mutation-testname": "mutation_testname", "mutation-content": "mutation_content", "mutation-seed": "mutation_seed", "mutation-expected-result": "mutation_expected_result"} {
			if value := options[option]; value != "" {
				args[argument] = value
			}
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

// isImportRequest reports whether this request carries an import `file`
// argument that prepareImportContent will read from disk.
func isImportRequest(request core.Request) bool {
	canonical := core.CanonicalVerb(request.Verb)
	if canonical == "req" {
		subverb, _ := request.Args["subverb"].(string)
		return strings.EqualFold(subverb, "import")
	}
	return canonical == "import"
}

func prepareImportContent(request *core.Request) error {
	if request == nil || !isImportRequest(*request) {
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
	encoded, err := marshalNoEscape(result)
	if err != nil {
		return
	}
	response.Data = result
	response.RawData = encoded
}

// marshalNoEscape mirrors json.Marshal but disables Go's default HTML
// escaping of '<', '>', and '&'. There's no HTML context on a terminal or in
// a JSON pipe, so that escaping only makes selector placeholders like "<id>"
// harder to read (AIRA-57).
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// marshalIndentNoEscape mirrors json.MarshalIndent without HTML escaping.
func marshalIndentNoEscape(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// renderHelp is the "help" verb's dedicated human formatter: a git-help-style
// listing of verb, usage, and (when the dispatch table carries one) a
// one-line summary — never the raw dispatch-table JSON dump (AIRA-57). It is
// only reached in human mode (a real terminal, --json not requested); the
// TTY-aware default in runWithInputDispatcher decides WHEN to use it.
func renderHelp(response core.Response, stdout, stderr io.Writer) int {
	entries, ok := response.Data.([]map[string]string)
	if !ok {
		// Shape surprise: fall back to the generic renderer rather than
		// fabricating a listing.
		return render(response, false, stdout, stderr)
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	for _, entry := range entries {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\n", entry["verb"], entry["usage"], entry["summary"])
	}
	if err := table.Flush(); err != nil {
		return exitForError("E_RUN_DETACH_FAILED")
	}
	// Face-level globals belong to this adapter, not to the core dispatch table
	// the listing above is generated from, so they are named here (AIRA-82).
	_, _ = fmt.Fprintf(stdout, "\nglobal options\n  %s DIR  resolve this call's project/worktree scope from DIR instead of the current directory\n  --json         render the structured response\n", scopeDirFlag)
	for _, warning := range response.Warnings {
		_, _ = fmt.Fprintf(stdout, "warning: %s\n", warning)
	}
	if response.Exit != 0 {
		return response.Exit
	}
	return 0
}

func render(response core.Response, jsonOutput bool, stdout, stderr io.Writer) int {
	var writeErr error
	if jsonOutput {
		data, _ := marshalNoEscape(response)
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

func renderConfineListResponse(response core.Response, stdout, stderr io.Writer) int {
	var result runner.ConfineListResult
	data := response.RawData
	if len(data) == 0 {
		data, _ = json.Marshal(response.Data)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return render(core.Response{Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": invalid confine-list response", Exit: codes.ExitForCode(daemon.CodeProtocol)}, false, stdout, stderr)
	}
	if result.Verdict == "unevaluated" {
		_, _ = fmt.Fprintf(stdout, "confine list: unevaluated: %s\n", result.Reason)
		if response.Exit != 0 {
			return response.Exit
		}
		return 3
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	// AIRA-102: LIVE + LEAF-PROCS, not a single "POPULATED" column.
	//
	// `Populated` is the scope's OWN cgroup.procs count, and a job that relocates
	// its processes into a child cgroup reads 0 there while very much running --
	// aitest drains into `<scope>/.aira-supervisor`, and `podman run
	// --cgroups=split` moves everything into `<scope>/runtime` plus the container
	// payload. Printing that 0 under a column named POPULATED told an operator the
	// job was not running. `SubtreePopulated` is the kernel's own subtree-aware
	// `cgroup.events populated` signal (already collected since AIRA-101, and
	// already trusted by the kill path and the exclusive gate) and is the honest
	// answer to "is this alive"; it was simply never rendered here.
	//
	// The DATA fields are deliberately left alone: the orphan reaper and the
	// daemon's reserve reconstruction consume Populated's leaf semantics on
	// purpose, so this is a FACE-only change.
	// AIRA-191. RESERVE sits beside CAP, and both are printed, because the whole
	// defect was that only one of them existed and it was the misleading one. A
	// --delegate-ram scope's CAP is an AIRA-15 containment ceiling sized for a
	// whole framework's workers; its RESERVE is what the admission ledger charges
	// it, and only the reserves sum toward the `slice reserve: <granted>` line
	// below. The reporter's own incident is exactly that confusion: a 45 GiB cap
	// read as another session's held reserve, when the reserve was 512M.
	//
	// An unestablished reserve prints "unevaluated" like every other facet here —
	// never the cap standing in for it.
	_, _ = fmt.Fprintln(table, "NAME\tOWNER\tSUPERVISOR-PID\tSCOPE-ID\tLIVE\tLEAF-PROCS\tRSS\tAGE\tRESERVE\tCAP")
	for _, record := range result.Scopes {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			record.Name, record.Owner, confineInt(record.SupervisorPID), record.ScopeID,
			confineLiveStatus(record),
			confineInt(record.Populated), confineInt64(record.RSSBytes), confineAge(record.AgeSeconds),
			confineInt64(record.ReserveBytes), confineString(record.Cap))
	}
	if err := table.Flush(); err != nil {
		return exitForError("E_RUN_DETACH_FAILED")
	}
	// AIRA-183. The legend for the two states AIRA-102's "no" used to conflate,
	// printed ONLY when a row actually shows one of them and naming only the
	// words that are on screen. An unconditional legend would be noise on the
	// listing an operator reads most (every row running), and a legend for a word
	// that is not present would be worse than noise.
	if legend := confineLiveLegend(result.Scopes); legend != "" {
		_, _ = fmt.Fprintln(stdout, legend)
	}
	if result.SliceReserve != nil {
		jobLabel := "jobs"
		if result.SliceReserve.Jobs == 1 {
			jobLabel = "job"
		}
		// AIRA-121: the containment qualifier is part of THIS line, not a
		// separate one below it, because the number and the strength of the
		// guarantee behind it must never be readable apart.
		containment := ""
		if result.SliceReserve.Containment != "" {
			containment = fmt.Sprintf(" [containment: %s; advisory budget from %s]",
				result.SliceReserve.Containment,
				runner.DescribeShimBudgetSource(result.SliceReserve.BudgetSource))
		}
		if result.SliceReserve.GrantedEstablished {
			_, _ = fmt.Fprintf(stdout, "slice reserve: %s granted / %s ceiling across %d admitted %s%s\n",
				formatReserveBytes(result.SliceReserve.GrantedBytes),
				formatReserveBytes(result.SliceReserve.CeilingBytes),
				result.SliceReserve.Jobs, jobLabel, containment)
		} else {
			// AIRA-220. The daemon holds no ledger for this slice yet, so the
			// granted total and job count are unestablished; report them as such
			// rather than as a fabricated empty slice. The ceiling is an
			// independent read and is still shown when it was established.
			_, _ = fmt.Fprintf(stdout, "slice reserve: unevaluated (no admission ledger) / %s ceiling%s\n",
				formatReserveBytes(result.SliceReserve.CeilingBytes), containment)
		}
		// Why a job is WAITING, which the admitted-jobs table above cannot show.
		// Printed unconditionally (including the zero) so "nothing is queued" is a
		// stated fact rather than an absence the reader has to interpret.
		if result.SliceReserve.FreezePhase != "" {
			waiterLabel := "waiters"
			if result.SliceReserve.Queued == 1 {
				waiterLabel = "waiter"
			}
			note := ""
			switch result.SliceReserve.FreezePhase {
			case "hold":
				note = " (fairness freeze holding capacity for the head waiter; it yields shortly)"
			case "yield":
				note = " (fairness freeze yielding; fitting waiters are being admitted)"
			}
			_, _ = fmt.Fprintf(stdout, "slice queue: %d queued %s, freeze %s%s\n",
				result.SliceReserve.Queued, waiterLabel, result.SliceReserve.FreezePhase, note)
		}
		// AIRA-101. Why a job is waiting when the slice looks far from full: a
		// benchmark has asked to run alone. Printed UNCONDITIONALLY, including the
		// "none" case, on the same reasoning as the lines around it — a line that
		// vanished when no exclusivity was active would be indistinguishable from a
		// line that vanished because the daemon predates the feature, so an
		// operator could not use its absence to rule a benchmark out.
		//
		// The whole block is already inside `SliceReserve != nil`, which is what
		// keeps this honest when nothing can be established: the daemon-down local
		// fallback produces no SliceReserve at all, and printing "none" there would
		// state a fact nobody checked.
		if exclusive := result.SliceReserve.Exclusive; exclusive == nil {
			_, _ = fmt.Fprintln(stdout, "slice exclusive: none")
		} else {
			_, _ = fmt.Fprintln(stdout, renderConfineExclusiveLine(exclusive))
		}
		// AIRA-68. The job count above spans three populations and the table above
		// THAT lists only scopes, so the two are not comparable — reading them
		// against each other is what produced a P0 that did not exist. Printed
		// with the zeros when the ledger IS established, so "no scope-less
		// reservations" is a stated fact rather than an absence the reader has to
		// interpret. AIRA-220: when the ledger is NOT established the split is the
		// SAME fabricated zeros as the headline (the "0 adopted scopes" the ticket
		// names as the AIRA-105 misreading), so it reads unevaluated in lockstep.
		if result.SliceReserve.GrantedEstablished {
			_, _ = fmt.Fprintf(stdout, "  of which: %d confine %s %s, %d scope-less %s %s, %d adopted %s %s\n",
				result.SliceReserve.ScopeJobs, confinePlural(result.SliceReserve.ScopeJobs, "scope", "scopes"),
				formatReserveBytes(result.SliceReserve.ScopeBytes),
				result.SliceReserve.ReservationJobs, confinePlural(result.SliceReserve.ReservationJobs, "reservation", "reservations"),
				formatReserveBytes(result.SliceReserve.ReservationBytes),
				result.SliceReserve.AdoptedJobs, confinePlural(result.SliceReserve.AdoptedJobs, "scope", "scopes"),
				formatReserveBytes(result.SliceReserve.AdoptedBytes))
		} else {
			_, _ = fmt.Fprintln(stdout, "  of which: unevaluated (no admission ledger)")
		}
		if result.SliceReserve.ReservationJobs > 0 {
			_, _ = fmt.Fprintln(stdout, "  (a scope-less reservation has no cgroup scope, so it never appears in the table above)")
			// AIRA-108. NAME them. The aggregate line above was AIRA-68's answer to
			// the first false P0 this blind spot produced; AIRA-108 is the second,
			// and it cost two sessions hours at /proc level because nothing could
			// say WHAT was holding 5.5G of a shared 62G machine-wide ceiling.
			//
			// `state=holding` is printed on every row deliberately. It is the fact
			// that settles the question these rows exist for — a granted
			// `confine-reserve` helper and one still waiting for admission are
			// byte-identical in `ps`, and only the granted one appears here at all.
			// Saying it beats leaving the reader to infer it from the heading, which
			// is precisely the inference that went wrong.
			//
			// Rows are already longest-held-first from the daemon.
			for _, hold := range result.SliceReserve.Reservations {
				_, _ = fmt.Fprintf(stdout, "    state=holding held=%s reserve=%s signature=%s\n",
					confineHeldDuration(hold.HeldMS), formatReserveBytes(hold.Reserve),
					confineSignatureForDisplay(hold.Signature))
			}
			// Never silently truncate: an elided row could be the oldest hold — the
			// one an operator is looking for — so say how many were dropped. This is
			// also the honest path when a daemon predates the field and sends NO
			// rows at all: it then reports every reservation as unlisted rather than
			// implying there were none.
			if unlisted := result.SliceReserve.ReservationJobs - len(result.SliceReserve.Reservations); unlisted > 0 {
				_, _ = fmt.Fprintf(stdout, "    … and %d further %s not listed\n",
					unlisted, confinePlural(unlisted, "reservation", "reservations"))
			}
		}
		if result.SliceReserve.VanishedJobs > 0 {
			// An observation, never a verdict, and stated in the PAST TENSE about
			// what the scan saw. A scope can be absent while the job's leader lives
			// on, having migrated into a sibling cgroup; and the newest sighting
			// here is up to one scan old, so "is now gone" would assert present
			// state the daemon cannot establish at the moment it prints it.
			_, _ = fmt.Fprintf(stdout, "  %d %s %s whose scope the confine scan observed and then observed absent; reclaimed at the stale-lease TTL\n",
				result.SliceReserve.VanishedJobs, confinePlural(result.SliceReserve.VanishedJobs, "lease", "leases"),
				formatReserveBytes(result.SliceReserve.VanishedBytes))
		}
		// AIRA-103. WHY the ceiling is what it is. Printed only when the subsystem
		// is running (CeilingMode == "" means off), and never claiming anything it
		// cannot establish: an unevaluated ceiling prints its reason, not a number,
		// and observe mode says plainly that nothing was applied.
		if mode := result.SliceReserve.CeilingMode; mode != "" {
			state := result.SliceReserve.CeilingState
			switch {
			case state == "unevaluated":
				_, _ = fmt.Fprintf(stdout, "slice ceiling: unevaluated (%s)\n",
					confineCeilingReason(result.SliceReserve.CeilingReason))
			case state != "throttled" && state != "unthrottled":
				// AIRA-106. The unknown-state fallback is checked BEFORE the mode
				// branch. It used to sit last, so an observe-mode snapshot carrying an
				// empty or newer-vocabulary state fell into the numeric "would be
				// effective" branch and rendered a figure for a state this binary does
				// not understand -- the one thing the fallback exists to prevent.
				_, _ = fmt.Fprintf(stdout, "slice ceiling: unevaluated (unrecognised state %q)\n", state)
			case mode == "observe":
				// CeilingBytes is the UNTOUCHED static capacity in observe mode --
				// observe applies nothing -- so the counterfactual must come from
				// CeilingWouldBeBytes. Printing CeilingBytes here reported the
				// static figure as though it were the observed decision, which
				// would have made the observe-then-enforce rollout blind on the
				// one surface an operator watches.
				//
				// AIRA-106: the cause clause is CHOSEN from the basis, never
				// appended. The line used to state "under system memory pressure"
				// unconditionally; with the static machine-reserve term in the
				// policy that is often not the cause, and appending a second
				// clause would have the line assert two. An UNTHROTTLED observe
				// snapshot has no basis at all and must not borrow either cause:
				// nothing reduced the ceiling, so the line says exactly that.
				if result.SliceReserve.CeilingState == "unthrottled" {
					_, _ = fmt.Fprintf(stdout, "slice ceiling: %s configured; not reduced (observe mode, not applied)%s\n",
						formatReserveBytes(result.SliceReserve.CeilingStaticBytes),
						confineCeilingSourceNote(result.SliceReserve))
					break
				}
				_, _ = fmt.Fprintf(stdout, "slice ceiling: %s configured; %s would be effective%s (observe mode, not applied)%s\n",
					formatReserveBytes(result.SliceReserve.CeilingStaticBytes),
					formatReserveBytes(result.SliceReserve.CeilingWouldBeBytes),
					confineCeilingCause(result.SliceReserve.CeilingBasis),
					confineCeilingSourceNote(result.SliceReserve))
			case state == "throttled":
				_, _ = fmt.Fprintf(stdout, "slice ceiling: reduced below the %s configured ceiling%s%s; new admissions wait, running jobs are untouched\n",
					formatReserveBytes(result.SliceReserve.CeilingStaticBytes),
					confineCeilingCause(result.SliceReserve.CeilingBasis),
					confineCeilingSourceNote(result.SliceReserve))
			default: // "unthrottled" -- every other state was answered above.
				_, _ = fmt.Fprintf(stdout, "slice ceiling: at its %s configured ceiling; not reduced by either policy term%s\n",
					formatReserveBytes(result.SliceReserve.CeilingStaticBytes),
					confineCeilingSourceNote(result.SliceReserve))
			}
			// Jobs admitted before the ceiling fell still hold their grants, so
			// granted CAN exceed the ceiling while throttled. That is a DRAIN
			// state, not the ledger inconsistency the residual line below reports,
			// and saying so is the difference between an operator reading this as
			// "working as designed" and as "the ledger has lost a discharge".
			if result.SliceReserve.CeilingState == "throttled" && result.SliceReserve.GrantedBytes > result.SliceReserve.CeilingBytes {
				_, _ = fmt.Fprintf(stdout, "  granted exceeds the effective ceiling by %s: draining -- admitted before the ceiling fell, and never preempted\n",
					formatReserveBytes(result.SliceReserve.GrantedBytes-result.SliceReserve.CeilingBytes))
			}
		}
		if result.SliceReserve.ResidualJobs != 0 || result.SliceReserve.ResidualBytes != 0 {
			// The split is derived from the ledger's own waiter list and the totals
			// come from its incremental counters; they are equal by construction, so
			// a residual is a real lost or double discharge. Printed signed and NOT
			// through formatReserveBytes, which floors a negative to 0B and would
			// hide exactly half of the defect.
			_, _ = fmt.Fprintf(stdout, "  LEDGER INCONSISTENCY: jobs %+d, bytes %+d unattributable to any population\n",
				result.SliceReserve.ResidualJobs, result.SliceReserve.ResidualBytes)
		}
	}
	if response.Exit != 0 {
		return response.Exit
	}
	return 0
}

func confineInt(value *int) string {
	if value == nil {
		return "unevaluated"
	}
	return strconv.Itoa(*value)
}

// confineBoolYesNo renders a subtree-population reading (AIRA-102). A nil is
// "unevaluated", never "no": a population that could not be read is not evidence
// of a dead job, and rendering it as one is the exact class of fabricated zero
// this repository forbids.
func confineBoolYesNo(value *bool) string {
	if value == nil {
		return "unevaluated"
	}
	if *value {
		return "yes"
	}
	return "no"
}

// The two LIVE values AIRA-183 splits out of AIRA-102's bare "no". They are
// constants because the renderer, the legend and the tests must all mean the
// same word by them; a legend that named a word the table did not print would be
// the same class of defect as the ambiguity it exists to remove.
const (
	confineLiveIdle     = "idle"
	confineLiveOrphaned = "orphaned"
)

// confineLiveStatus renders the LIVE column.
//
// AIRA-102 made this column subtree-aware, which fixed reading a busy job as
// dead. AIRA-183 fixes what it left: an empty subtree still rendered a bare
// "no", and "no" was two different situations at once — a supervisor that has
// DIED (the scope is orphaned and will be reaped) and a supervisor that is very
// much alive between processes. An operator read one as the other, concluded a
// kill had failed, and killed a second, unrelated job on that basis.
//
// The ordering is deliberate. SubtreePopulated is asked FIRST and answers
// outright when it is true or unknown, because supervisor liveness cannot
// improve either of those answers: a scope with live processes is running
// whoever launched it, and a population that could not be read is unevaluated no
// matter what the supervisor is doing.
//
// The supervisor reading only refines the EMPTY case, and when it is itself
// unestablished the column falls back to exactly the "no" AIRA-102 printed —
// the same true statement about the subtree, with no claim about the supervisor
// attached to it.
func confineLiveStatus(record runner.ConfineRecord) string {
	if record.SubtreePopulated == nil {
		return "unevaluated"
	}
	if *record.SubtreePopulated {
		return "yes"
	}
	if record.SupervisorLive == nil {
		return "no"
	}
	if *record.SupervisorLive {
		return confineLiveIdle
	}
	return confineLiveOrphaned
}

// confineLiveLegend explains the LIVE values actually present in this listing,
// or says nothing. It is derived from confineLiveStatus rather than from the
// record fields a second time, so the legend cannot describe a state the table
// did not render.
func confineLiveLegend(scopes []runner.ConfineRecord) string {
	idle, orphaned := false, false
	for _, record := range scopes {
		switch confineLiveStatus(record) {
		case confineLiveIdle:
			idle = true
		case confineLiveOrphaned:
			orphaned = true
		}
	}
	clauses := make([]string, 0, 2)
	if idle {
		clauses = append(clauses, confineLiveIdle+" = the scope subtree has no process right now but its supervisor is alive (mid fork/exec, or a genuinely idle moment) — nothing to investigate")
	}
	if orphaned {
		clauses = append(clauses, confineLiveOrphaned+" = the supervisor PID is gone and the scope is empty — the daemon's reaper removes it once it is past the reap grace, so there is nothing left to kill")
	}
	if len(clauses) == 0 {
		return ""
	}
	return "LIVE: " + strings.Join(clauses, "; ")
}

// confinePlural keeps the AIRA-68 breakdown line grammatical for a single-member
// population, matching the singular/plural handling the two lines above it
// already do inline.
// confineCeilingReason and confineMemAvailableNote keep an ABSENCE rendering as
// an absence: a zero MemAvailable is "not established" (a live /proc read never
// returns exactly 0 before the box is dead), never a measured zero, so it prints
// nothing rather than "MemAvailable 0B".
func confineCeilingReason(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "reason unavailable"
	}
	return reason
}

// confineCeilingCause renders WHICH policy term reduced the ceiling (AIRA-106).
//
// It names the TERM, not a cause in the world. That distinction is the point:
// the dynamic term is `MemAvailable + sliceAnon - freeMin`, and on a large,
// perfectly IDLE machine with a large freeMin it can bind with nothing at all
// consuming memory outside the slice. The pre-AIRA-106 wording ("reduced by
// memory used OUTSIDE the slice") asserted a fact about the machine that the
// comparison does not establish; this says what the daemon actually knows, and
// the MemAvailable figure printed beside it is what lets an operator tell the
// two situations apart.
//
// An unset or unrecognised basis renders NOTHING rather than defaulting to
// either term. Saying less is the honest failure mode.
func confineCeilingCause(basis string) string {
	switch basis {
	case "system-pressure":
		return " to keep the configured system free-memory reserve"
	case "machine-reserve":
		return " to keep the configured share of this machine outside the slice"
	default:
		return ""
	}
}

// confineCeilingSourceNote renders the system reading behind the ceiling AND
// whether it is current. A HELD ceiling's numbers are the last established ones,
// up to the hold TTL old; presenting them unmarked would state a current fact the
// daemon cannot establish, which is the same class of dishonesty as a fabricated
// zero. A non-positive MemAvailable is an absence and prints nothing at all.
func confineCeilingSourceNote(reserve *runner.ConfineSliceReserve) string {
	note := ""
	if reserve.MemAvailableBytes > 0 {
		note = " (system MemAvailable " + formatReserveBytes(reserve.MemAvailableBytes)
		if reserve.CeilingHeld {
			note += ", last established"
		}
		note += ")"
	} else if reserve.CeilingHeld {
		note = " (last established; no current system reading)"
	}
	if reserve.CeilingHeld && strings.TrimSpace(reserve.CeilingReason) != "" {
		note += " — holding: " + reserve.CeilingReason
	}
	return note
}

// renderConfineExclusiveLine renders the `slice exclusive:` line of
// `confine --list` for an ACTIVE exclusive state (the "none" case is its
// caller's, because that one must print even when this struct is absent).
//
// AIRA-119. The identity alone is not something an operator can act on, and that
// is not a cosmetic complaint — it is the defect that ticket records. Three facts
// were missing, each of which the daemon already knew:
//
//   - WHETHER THE NAMED JOB IS RUNNING. `held` means it is running alone;
//     `draining` means it has NOT STARTED — `aira confine` creates its cgroup
//     scope and launches its target only after admission is granted, so a
//     draining job owns no process and no scope, and it has no row in the table
//     above either. "draining for X" reads as though X were running, so an
//     operator greps for X, finds nothing, and concludes the daemon is naming a
//     job that already released. That misreading is AIRA-119.
//   - THE SCOPE ID. The one unique, greppable handle, and the selector
//     `aira confine --kill` takes. The daemon has always sent it and no face has
//     ever printed it, leaving `Name` — which defaults to "job" for every unnamed
//     confine — as the only identifier, while `Owner` can come from
//     AIRA_CONFINE_OWNER and so appears in no argv at all.
//   - HOW LONG. The discriminator between a normal drain and a stuck one. A
//     30-second drain is routine; an 18-minute one is the thing to escalate.
//
// An unestablished age (SinceMS == 0) prints NO age clause rather than "0s",
// which would state that a state just began when nothing established that.
func renderConfineExclusiveLine(exclusive *runner.ConfineExclusiveState) string {
	draining := exclusive.State == "draining"
	verb, since := "held by", "running alone for "
	if draining {
		verb, since = "draining for", "draining for "
	}
	owner := exclusive.Owner
	if strings.TrimSpace(owner) == "" {
		owner = "unknown owner"
	}
	line := fmt.Sprintf("slice exclusive: %s %q (%s)", verb, exclusive.Name, owner)
	// AIRA-185. The holder's own answer to "why", appended rather than substituted
	// for the identity: every existing token of this line keeps its exact spelling
	// and position, and the clause is absent entirely when no reason was given —
	// which is every `aira confine --exclusive`, whose Name IS the answer. The one
	// case that needed this is `aira drain wait`, whose Name is a fixed
	// placeholder ("drain") that says nothing about the deploy it is holding the
	// slice for.
	//
	// UNTRUSTED text from another session, printed straight into this operator's
	// shell: escaped, length-bounded, and then quoted so it cannot be mistaken for
	// a field of AIRA's own.
	if reason := strings.TrimSpace(exclusive.Reason); reason != "" {
		line += " reason=" + strconv.Quote(confineReasonForDisplay(reason))
	}
	if scope := strings.TrimSpace(exclusive.ScopeID); scope != "" {
		line += " scope=" + scope
	}
	if draining {
		// Stated INDEPENDENTLY of the age below, because it is a different fact and
		// it is the load-bearing one: the state is established even when the age is
		// not, and "draining for X" on its own is precisely what was read as X
		// running.
		line += ", not started yet"
	}
	if exclusive.SinceMS > 0 {
		line += ", " + since + (time.Duration(exclusive.SinceMS) * time.Millisecond).Round(time.Second).String()
	}
	return fmt.Sprintf("%s, %d %s waiting", line,
		exclusive.WaitingJobs, confinePlural(exclusive.WaitingJobs, "job", "jobs"))
}

func confinePlural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func confineInt64(value *int64) string {
	if value == nil {
		return "unevaluated"
	}
	return strconv.FormatInt(*value, 10)
}

// formatReserveBytes renders a KNOWN reserve/ceiling byte count. Unlike
// FormatConfineBytes (which maps 0 to "unknown" for the optional trailer facets),
// a slice-reserve summary value of 0 is a genuine zero (an idle slice, or a cap
// fully consumed by headroom), so render it as "0B" rather than "unknown".
func formatReserveBytes(value int64) string {
	if value <= 0 {
		return "0B"
	}
	return runner.FormatConfineBytes(value)
}

// confineHeldDuration renders a reservation's age. Rounded to the second: this
// is an operator's "is that stuck?" signal, where 52m is the whole story and the
// milliseconds are noise. A non-positive value is an age the daemon could not
// establish (an unset grant instant) and says so rather than printing "0s",
// which would read as a brand-new hold — the opposite of the truth.
func confineHeldDuration(heldMS int64) string {
	if heldMS <= 0 {
		return "unevaluated"
	}
	return (time.Duration(heldMS) * time.Millisecond).Round(time.Second).String()
}

// confineSignatureForDisplay makes a client-supplied signature safe for a
// terminal. It is arbitrary bytes chosen by whatever spawned the helper, and it
// is printed straight into an operator's shell, so control characters (which can
// rewrite the line, hide rows, or forge output) are escaped and the length is
// bounded. Truncation is MARKED with an ellipsis, never silent.
//
// An empty signature is a legitimate state — `signature` is optional on the
// admit wire — and renders as an explicit "(unnamed)": the absence is stated,
// never filled in with a guess.
func confineSignatureForDisplay(signature string) string {
	if strings.TrimSpace(signature) == "" {
		return "(unnamed)"
	}
	return confineTextForDisplay(signature, runner.ConfineReservationSignatureLimit)
}

// confineReasonForDisplay makes a client-supplied exclusive hold reason safe for
// a terminal (AIRA-185), on exactly the terms confineSignatureForDisplay makes a
// signature safe.
//
// Unlike a signature it has NO "(unnamed)" substitute: an absent reason is not a
// thing to name, and every caller omits the whole clause instead, so this is
// only ever reached with text the holder actually supplied.
func confineReasonForDisplay(reason string) string {
	return confineTextForDisplay(reason, runner.ConfineExclusiveReasonLimit)
}

// confineTextForDisplay is the ONE escaping and bounding rule for
// client-supplied text that reaches an operator's terminal, shared so a second
// such field cannot arrive with a second, looser rule beside it.
func confineTextForDisplay(text string, limit int) string {
	var builder strings.Builder
	runes := 0
	for _, r := range text {
		if runes >= limit {
			builder.WriteString("…")
			break
		}
		if r == unicode.ReplacementChar || !unicode.IsPrint(r) {
			builder.WriteString(strconv.QuoteRune(r))
		} else {
			builder.WriteRune(r)
		}
		runes++
	}
	return builder.String()
}

func confineAge(value *int64) string {
	if value == nil {
		return "unevaluated"
	}
	return (time.Duration(*value) * time.Second).String()
}

func confineString(value *string) string {
	if value == nil {
		return "unevaluated"
	}
	return *value
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
				data, _ = marshalNoEscape(report)
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
		data, _ = marshalIndentNoEscape(response.Data, "  ")
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
	return codes.ExitForCode(code)
}
