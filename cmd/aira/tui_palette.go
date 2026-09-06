package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"aira/internal/core"
)

// AIRA-127. The SLOT COLOUR palette, added to this module because the ticket
// directs the top view to reuse "the existing colour palette module,
// cmd/aira/tui_palette.go". A note for whoever reads this next: everything below
// the colour block is a COMMAND palette (the `:` verb picker) and there was no
// pre-existing colour-allocation mechanism anywhere in the TUI to extend — the
// dashboard's only colours are three literal tcell constants keyed off
// tableRow.Style. So this is the module's first colour table rather than an
// extension of one, and it is placed here so a second palette never appears.
//
// The colours are chosen to stay distinguishable on both light and dark
// terminals and to avoid the greys the bar reserves for out-of-slice usage.
// A slot beyond the table's length WRAPS, so two rows can share a colour on a
// slice running more than len(topSlotColours) scopes at once. That is a stated
// limit, not a defect to be hidden: the SLOT is the identity the bar and the row
// agree on, and the colour is a lookup from it, so a wrap costs legibility and
// never correctness.
var topSlotColours = []string{
	"#5fafff", // blue
	"#5fd75f", // green
	"#ff875f", // orange
	"#af87ff", // violet
	"#5fd7d7", // cyan
	"#d7d75f", // yellow
	"#ff5faf", // magenta
	"#87af5f", // olive
}

// topSlotColour is the ONE mapping from a stable slot index to a colour. The bar
// region and the process-list row for the same reservation both call it, which
// is what makes requirement 6 — identical colour in both places — structural
// rather than a convention two call sites have to remember.
func topSlotColour(slot int) string {
	if slot < 0 {
		return topColourScopeless
	}
	return topSlotColours[slot%len(topSlotColours)]
}

// topShadeNumerator/topShadeDenominator scale every channel of a slot colour to
// produce its DARKENED variant. Chosen to stay unmistakably the same hue — the
// slot identity requirement 6 makes structural must survive the split — while
// being obviously dimmer than the full-intensity used portion beside it.
const (
	topShadeNumerator   = 45
	topShadeDenominator = 100
)

// topShadeColour is AIRA-135's second shade of a slot colour: the part of a
// reservation that is held but NOT currently in use.
//
// It returns "" for anything it cannot darken (a colour that is not the
// `#rrggbb` form this palette uses). That is deliberate and is what the bar
// relies on: with no shade colour the region is drawn as ONE undivided span,
// which is today's behaviour and states nothing that was not established. It
// never invents a fallback shade, because a shade an operator cannot tell from
// the bright one would present a used/unused split that is not being drawn.
func topShadeColour(colour string) string {
	if len(colour) != 7 || colour[0] != '#' {
		return ""
	}
	channels := [3]int64{}
	for index := range channels {
		value, err := strconv.ParseInt(colour[1+index*2:3+index*2], 16, 32)
		if err != nil {
			return ""
		}
		channels[index] = value * topShadeNumerator / topShadeDenominator
	}
	return fmt.Sprintf("#%02x%02x%02x", channels[0], channels[1], channels[2])
}

const (
	// topColourScopeless marks the aggregate scope-less-reservation region. It is
	// deliberately outside the slot table: that region has no slot, no row, and no
	// stable identity to hold one.
	topColourScopeless = "#d7af00"
	// topColourOutside is the grey of requirement 5 — memory used by the rest of
	// the system, anchored to the bar's right edge.
	topColourOutside = "#6c6c6c"
	// topColourMarker draws the soft/hard/ceiling limit ticks.
	topColourMarker = "#ffffff"
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
