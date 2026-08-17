package core

import "strings"

// Route identifies whether an operation belongs in the pure-store daemon or
// must remain in the client process because its handler can reach execution or
// GitOps dependencies.
type Route uint8

const (
	RouteDaemon Route = iota
	RouteClient
)

// CanonicalVerb is the single alias map used by both Do and the client routing
// classifier.
func CanonicalVerb(verb string) string {
	verb = strings.ToLower(strings.TrimSpace(verb))
	switch verb {
	case "new":
		return "create"
	case "get":
		return "show"
	case "ls":
		return "list"
	case "watch":
		return "watch"
	default:
		return verb
	}
}

// Classify returns the canonical verb and its execution location. selector is
// the show selector, or the gate subverb for gate operations.
func Classify(verb, selector string) (string, Route) {
	canonical := CanonicalVerb(verb)
	operation := strings.ToLower(strings.TrimSpace(selector))
	switch {
	case canonical == "watch":
		return canonical, RouteDaemon
	case canonical == "show" && strings.HasPrefix(selector, "RUN-"):
		return canonical, RouteClient
	case canonical == "run" || strings.HasPrefix(canonical, "run-"):
		return canonical, RouteClient
	case canonical == "reconcile", canonical == "check", canonical == "git":
		return canonical, RouteClient
	case canonical == "gate" && (operation == "run" || operation == "canary-run"):
		return canonical, RouteClient
	default:
		return canonical, RouteDaemon
	}
}

// ClassifyRequest extracts the only operation-granular arguments used by the
// routing decision.
func ClassifyRequest(req Request) (string, Route) {
	selector := ""
	canonical := CanonicalVerb(req.Verb)
	if canonical == "show" {
		selector, _ = req.Args["selector"].(string)
	} else if canonical == "gate" {
		selector, _ = req.Args["subverb"].(string)
	}
	return Classify(canonical, selector)
}

// StoreFreeCarved reports whether a canonical client-routed request can run
// without touching the state store. Telemetry values, rather than key
// presence, split run because CLI request construction includes empty keys.
func StoreFreeCarved(verb string, args map[string]any) bool {
	canonical := CanonicalVerb(verb)
	switch canonical {
	case "run":
		for _, name := range []string{"report", "tool", "usage", "provider"} {
			value, present := args[name]
			if !present || value == nil {
				continue
			}
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) != "" {
				return false
			}
		}
		return true
	case "run-kill", "run-log", "git":
		return true
	case "show":
		selector, _ := args["selector"].(string)
		return strings.HasPrefix(selector, "RUN-")
	default:
		return false
	}
}
