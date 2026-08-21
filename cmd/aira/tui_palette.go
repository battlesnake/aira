package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"aira/internal/core"
)

type paletteArg struct {
	Spec     core.ArgSpec
	Required bool
}

type paletteEntry struct {
	Verb        string
	Operation   string
	Summary     string
	Safety      core.SafetyClass
	Destructive bool
	Args        []paletteArg
}

func buildPalette(descriptors []core.DispatchDescriptor) []paletteEntry {
	entries := make([]paletteEntry, 0)
	for _, descriptor := range descriptors {
		if len(descriptor.Operations) == 0 {
			if !paletteOperationAdmitted(descriptor.Name, "", descriptor.Safety) {
				continue
			}
			entry := paletteEntry{Verb: descriptor.Name, Summary: descriptor.Summary, Safety: descriptor.Safety, Destructive: descriptor.Destructive}
			for _, arg := range descriptor.Args {
				if descriptor.Name == "run-log" && arg.Name == "follow" {
					continue
				}
				entry.Args = append(entry.Args, paletteArg{Spec: arg, Required: arg.Required})
			}
			entries = append(entries, entry)
			continue
		}
		byName := make(map[string]core.ArgSpec, len(descriptor.Args))
		for _, arg := range descriptor.Args {
			byName[arg.Name] = arg
		}
		for _, operation := range descriptor.Operations {
			if !paletteOperationAdmitted(descriptor.Name, operation.Name, operation.Safety) {
				continue
			}
			entry := paletteEntry{Verb: descriptor.Name, Operation: operation.Name, Summary: operation.Summary, Safety: operation.Safety, Destructive: operation.Destructive}
			for _, operationArg := range operation.Args {
				spec, ok := byName[operationArg.Name]
				if !ok {
					// Metadata validation catches this elsewhere. Retaining a string
					// field here keeps palette construction fail-closed at parse time.
					spec = core.ArgSpec{Name: operationArg.Name, Kind: core.ArgKindString}
				}
				entry.Args = append(entry.Args, paletteArg{Spec: spec, Required: operationArg.Required})
			}
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entryKey(entries[i]) < entryKey(entries[j]) })
	return entries
}

func paletteOperationAdmitted(verb, operation string, safety core.SafetyClass) bool {
	if safety != core.SafetyRead && safety != core.SafetyMutate && safety != core.SafetyLease {
		return false
	}
	if _, route := core.Classify(verb, operation); route != core.RouteDaemon {
		return false
	}
	return !isPaletteFileContentOperation(verb, operation)
}

func isPaletteFileContentOperation(verb, operation string) bool {
	return verb == "import" || verb == "req" && operation == "import" || verb == "test-report" && operation == "add"
}

func entryKey(entry paletteEntry) string {
	if entry.Operation == "" {
		return entry.Verb
	}
	return entry.Verb + " " + entry.Operation
}

func parsePaletteRequest(entry paletteEntry, values map[string]string) (core.Request, error) {
	allowed := make(map[string]paletteArg, len(entry.Args))
	for _, arg := range entry.Args {
		allowed[arg.Spec.Name] = arg
	}
	for name := range values {
		if _, ok := allowed[name]; !ok {
			return core.Request{}, fmt.Errorf("E_SELECTOR_INVALID: unknown argument %q", name)
		}
	}
	args := make(map[string]any)
	if entry.Operation != "" {
		switch {
		case entry.Verb == "link" && entry.Operation == "list":
			args["list"] = true
		case entry.Verb == "link" && entry.Operation == "link":
			// The link handler uses only the list discriminator. The mutation
			// operation deliberately has no synthetic subverb.
		default:
			args["subverb"] = entry.Operation
		}
	}
	for _, arg := range entry.Args {
		raw := strings.TrimSpace(values[arg.Spec.Name])
		if raw == "" {
			if arg.Required {
				return core.Request{}, fmt.Errorf("E_SELECTOR_INVALID: %s requires %s", entryKey(entry), arg.Spec.Name)
			}
			continue
		}
		if len(arg.Spec.Enum) > 0 && !containsString(arg.Spec.Enum, raw) {
			return core.Request{}, fmt.Errorf("E_SELECTOR_INVALID: %s must be one of %s", arg.Spec.Name, strings.Join(arg.Spec.Enum, ", "))
		}
		switch arg.Spec.Kind {
		case core.ArgKindBool:
			value, err := strconv.ParseBool(raw)
			if err != nil {
				return core.Request{}, fmt.Errorf("E_SELECTOR_INVALID: %s must be true or false", arg.Spec.Name)
			}
			args[arg.Spec.Name] = value
		case core.ArgKindStringList:
			parts := strings.Split(raw, ",")
			values := make([]string, 0, len(parts))
			for _, part := range parts {
				if value := strings.TrimSpace(part); value != "" {
					values = append(values, value)
				}
			}
			args[arg.Spec.Name] = values
		default:
			args[arg.Spec.Name] = raw
		}
	}
	if entry.Verb == "run-log" {
		args["follow"] = false
	}
	request := core.Request{Verb: entry.Verb, Args: args}
	if _, route := core.ClassifyRequest(request); route != core.RouteDaemon {
		return core.Request{}, fmt.Errorf("E_SELECTOR_INVALID: %s is unavailable in the daemon-routed palette", entryKey(entry))
	}
	return request, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
