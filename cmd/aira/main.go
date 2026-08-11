package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

func main() { os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr)) }

// Run is the deliberately small CLI adapter: argv parsing, core request
// construction, and rendering. It contains no ticket or consistency logic.
func Run(argv []string, stdout, stderr io.Writer) int {
	if len(argv) > 0 && strings.ToLower(argv[0]) == "mcp" {
		return runMCP(context.Background(), os.Stdin, stdout, stderr)
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

	if verb == "init" {
		requestArgs := map[string]any{}
		if value := options["project"]; value != "" {
			requestArgs["project"] = value
		}
		if value := options["prefixes"]; value != "" {
			requestArgs["prefixes"] = splitComma(value)
		}
		dispatcher := core.NewWithInitializer(nil, func(ctx context.Context, initArgs map[string]any) (any, error) {
			return app.Init(ctx, ".", initArgs)
		})
		return render(dispatcher.Do(context.Background(), core.Request{Verb: "init", Args: requestArgs}), jsonOutput, stdout, stderr)
	}

	request, err := buildRequest(verb, positional, options)
	if err != nil {
		code := store.ErrorCode(err)
		if code == "E_INTERNAL" {
			code = "E_SELECTOR_INVALID"
		}
		return render(core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
	}
	s, project, err := app.Open(context.Background(), ".")
	if err != nil {
		code := appErrorCode(err)
		return render(core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}, jsonOutput, stdout, stderr)
	}
	defer s.Close()
	dispatcher := core.NewWithRunnerInput(s, project.Runner, os.Stdin)
	response := dispatcher.Do(context.Background(), request)
	if verb == "run-log" && !jsonOutput {
		return renderRunLog(response, stdout, stderr)
	}
	return render(response, jsonOutput, stdout, stderr)
}

func removeJSON(argv []string) ([]string, bool) {
	result := make([]string, 0, len(argv))
	jsonOutput := false
	end := len(argv)
	if len(argv) > 0 && strings.EqualFold(argv[0], "run") {
		for i := 1; i < len(argv); i++ {
			if argv[i] == "--" {
				end = i
				break
			}
		}
	}
	for i, arg := range argv {
		if i >= end {
			if len(argv) > 0 && strings.EqualFold(argv[0], "run") && arg == "--json" {
				// The token remains in the child argv verbatim, but still acts as
				// the outer adapter's output selector for deterministic diagnostics.
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
	options := map[string]string{}
	var positional []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		if name == "rebuild" || name == "steal" || name == "strict" || (name == "list" && verb == "ready") || ((name == "follow" || name == "full") && verb == "run-log") {
			options[name] = "true"
			continue
		}
		if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
			if verb == "run-log" {
				return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s requires a value", name)
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
		} else {
			options[name] = argv[i]
		}
	}
	allowed := map[string]map[string]bool{
		"init":   {"project": true, "prefixes": true},
		"create": {"kind": true, "severity": true, "labels": true, "body": true},
		"new":    {"kind": true, "severity": true, "labels": true, "body": true},
		"show":   {"fields": true}, "get": {"fields": true},
		"list": {"by": true, "fields": true}, "ls": {"by": true, "fields": true},
		"grep":   {"kind": true, "by": true, "fields": true},
		"import": {"strict": true},
		"count":  {"by": true}, "reconcile": {"rebuild": true},
		"claim":   {"steal": true, "actor": true},
		"release": {"token": true}, "heartbeat": {"token": true},
		"touch":    {"token": true},
		"ready":    {"list": true},
		"find":     {"category": true, "severity": true, "verdict": true, "source": true, "message": true, "file": true, "requirement": true, "by": true, "fields": true, "disposition": true, "reason": true, "actor": true},
		"req":      {"status": true, "fields": true},
		"run-kill": {},
		"run-log":  {"stream": true, "from": true, "tail": true, "follow": true, "full": true},
	}
	for name := range options {
		if !allowed[verb][name] {
			if verb == "run-log" {
				return nil, nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: option --%s is not valid for %s", name, verb)
			}
			return nil, nil, fmt.Errorf("option --%s is not valid for %s", name, verb)
		}
	}
	return positional, options, nil
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
		if name == "merge" || name == "store-stdin" {
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
		case "cwd", "stdin":
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
		args["stdin"] = options["stdin"]
		args["store_stdin"] = options["store-stdin"] == "true"
	case "run-kill":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("E_RUN_ARGUMENT_INVALID: run-kill requires <run-id>")
		}
		args["run_id"] = positional[0]
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
	case "show", "get":
		if len(positional) != 1 {
			return core.Request{}, fmt.Errorf("show requires <selector>")
		}
		args["selector"] = positional[0]
		args["fields"] = splitComma(options["fields"])
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

func render(response core.Response, jsonOutput bool, stdout, stderr io.Writer) int {
	if jsonOutput {
		data, _ := json.Marshal(response)
		_, _ = fmt.Fprintln(stdout, string(data))
	} else if response.OK {
		renderHuman(response, stdout)
	} else if response.Error != "" {
		_, _ = fmt.Fprintln(stderr, response.Error)
	}
	if response.Exit != 0 {
		return response.Exit
	}
	if response.OK {
		return 0
	}
	return exitForError(response.Code)
}

func renderHuman(response core.Response, out io.Writer) {
	if response.Code == "PASS" || response.Code == "FAIL" || response.Code == "UNEVALUATED" {
		if report, ok := response.Data.(interface{}); ok {
			data, _ := json.Marshal(report)
			_, _ = fmt.Fprintf(out, "verdict: %s\n%s\n", strings.ToLower(response.Code), data)
		}
		for _, warning := range response.Warnings {
			_, _ = fmt.Fprintf(out, "warning: %s\n", warning)
		}
		return
	}
	data, _ := json.MarshalIndent(response.Data, "", "  ")
	_, _ = fmt.Fprintln(out, string(data))
	for _, warning := range response.Warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", warning)
	}
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

func appErrorCode(err error) string {
	return store.ErrorCode(err)
}

func exitForError(code string) int {
	return store.ExitForCode(code)
}
