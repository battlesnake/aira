package core

import (
	"fmt"
	"strings"
)

// canonicalEnumArgs is the ONE enforcement point for declared closed enums
// (AIRA-210). Every core-routed request reaches spec.Run through (*Core).Do, so
// checking here makes the declarations (ArgSpec.Enum) mean the same thing on
// the CLI, MCP and TUI faces instead of each face keeping its own copy.
//
// For each declared enum argument that is present with a non-nil value:
//
//   - a non-string value is refused;
//   - an empty (or all-space) string is the ABSENT value when the argument is
//     optional (the CLI builders send "" for an unset flag) and is refused when
//     it is required;
//   - otherwise the first declared member that equals the trimmed value under
//     strings.EqualFold is written back in its DECLARED spelling, so a handler
//     only ever sees a canonical token; no member matches: the value is refused.
//
// A refusal names the canonical verb, the declared argument, the value exactly
// as received (never folded or trimmed, so the operator sees what they typed)
// and the whole declared set. The caller's map is never mutated: a rewrite is
// made on a shallow copy.
//
// Handlers keep their own domain-level validation as a backstop for callers
// that do not pass through Do; through Do they only ever see canonical values.
func canonicalEnumArgs(spec verbSpec, in map[string]any) (map[string]any, error) {
	out, copied := in, false
	for _, arg := range spec.Args {
		if len(arg.Enum) == 0 {
			continue
		}
		raw, present := in[arg.Name]
		if !present || raw == nil {
			continue
		}
		refuse := func(shown string) error {
			return fmt.Errorf("E_ARGUMENT_INVALID: %s %s %q is invalid; allowed: %s", spec.Name, arg.Name, shown, strings.Join(arg.Enum, ", "))
		}
		typed, isString := raw.(string)
		if !isString {
			return nil, refuse(fmt.Sprint(raw))
		}
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			if arg.Required {
				return nil, refuse(typed)
			}
			continue
		}
		member := ""
		for _, candidate := range arg.Enum {
			if strings.EqualFold(candidate, trimmed) {
				member = candidate
				break
			}
		}
		if member == "" {
			return nil, refuse(typed)
		}
		if member != typed {
			if !copied {
				copied = true
				out = make(map[string]any, len(in))
				for key, value := range in {
					out[key] = value
				}
			}
			out[arg.Name] = member
		}
	}
	return out, nil
}
