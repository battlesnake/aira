package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// verifies: AIRA-210 -- every (verb, subverb, option) triple parseArgs lets
// through is either refused by name or CHANGES the built request. An option that
// is accepted and then ignored reads as applied (the exit-0 accept-and-discard
// class: `gate add --canary_id X` bound the default canary id, `gate show
// --gate_id` was dropped). The matrix is built from parseArgsAllowedOptions
// itself, so a newly admitted option with no consumer fails here without anyone
// remembering to list it.

// optionBaseArgvs are minimal valid invocations, per verb, one per subverb that
// takes options. The probe appends `--<option> <value>` to each.
var optionBaseArgvs = map[string][][]string{
	"init":              {{"init"}},
	"eject":             {{"eject"}},
	"create":            {{"create", "T"}},
	"new":               {{"new", "T"}},
	"rant":              {{"rant", "text"}, {"rant", "capture", "text"}, {"rant", "ls"}, {"rant", "get", "RANT-1"}, {"rant", "review", "RANT-1"}, {"rant", "redact", "RANT-1"}},
	"show":              {{"show", "AIRA-1"}},
	"get":               {{"get", "AIRA-1"}},
	"review":            {{"review", "AIRA-1"}},
	"list":              {{"list"}},
	"ls":                {{"ls"}},
	"grep":              {{"grep", "x"}},
	"import":            {{"import", "f.md"}},
	"count":             {{"count", "--by", "kind"}},
	"reconcile":         {{"reconcile"}},
	"claim":             {{"claim", "AIRA-1"}},
	"release":           {{"release", "AIRA-1"}},
	"heartbeat":         {{"heartbeat", "AIRA-1"}},
	"touch":             {{"touch", "AIRA-1", "a.go"}},
	"ready":             {{"ready"}},
	"worktree-register": {{"worktree-register", "AIRA-1"}},
	"worktree-audit":    {{"worktree-audit"}},
	"find":              {{"find", "add", "AIRA-1"}, {"find", "ls"}, {"find", "show", "F-1"}, {"find", "set", "F-1"}},
	"req":               {{"req", "add", "The system must X"}, {"req", "ls"}, {"req", "show", "AR-1"}, {"req", "set", "AR-1"}, {"req", "import", "REQUIREMENTS.md"}},
	"test-report":       {{"test-report", "add", "r.json", "--format", "go-json"}, {"test-report", "ls"}, {"test-report", "show", "TR-1"}, {"test-report", "flaky"}},
	"spend":             {{"spend", "add"}, {"spend", "ls"}},
	"quota":             {{"quota", "add"}, {"quota", "ls"}},
	"insights":          {{"insights"}},
	"lease":             {{"lease", "ls"}},
	"commands":          {{"commands", "ls"}, {"commands", "count", "--by", "key"}},
	"run-kill":          {{"run-kill", "RUN-1"}},
	"run-input":         {{"run-input", "RUN-1"}},
	"run-log":           {{"run-log", "RUN-1"}},
	"confine-list":      {{"confine-list"}},
	"confine-budget":    {{"confine-budget"}},
	// confine-dump is not a CLI verb (buildRequest: E_UNKNOWN_VERB); --dump is a
	// confine management flag parsed by parseConfineArgs.
	"confine-kill":  {{"confine-kill", "x"}},
	"confine-log":   {{"confine-log", "x"}},
	"confine-input": {{"confine-input", "x"}},
	"watch":         {{"watch"}},
	"gate": {
		{"gate", "add", "G"}, {"gate", "set", "G"}, {"gate", "show", "G"}, {"gate", "run", "G"}, {"gate", "attest", "G", "--verdict", "pass", "--actor", "a"},
		{"gate", "prove", "G"}, {"gate", "review", "G"}, {"gate", "ls"}, {"gate", "check"}, {"gate", "canary-run", "C"}, {"gate", "canary-show", "C"},
	},
}

// optionProbeValue is the value given to an option; a bare flag is probed with
// none. Anything absent gets "x", which the builder either consumes or refuses.
var optionProbeValue = map[string]string{
	"timeout-ms": "5", "output-cap-bytes": "5", "mutation-seed": "1", "mutation-occurrence": "1",
	"total": "5", "since": "5", "retry": "1", "shard": "1/2", "tail": "5", "from": "1",
	"severity": "blocker", "format": "go-json", "verdict": "pass", "kind": "bug", "status": "built",
	"at": "2026-01-01T00:00:00Z", "reset-at": "2026-01-01T05:00:00Z", "cost-usd": "0.1",
	"used": "1", "limit": "2", "remaining": "1", "window": "5h", "bucket": "output=5",
	"config-env": "A=B", "by": "tag", "outcome": "planned", "disposition": "waived",
	"checker": "command", "predicate": "exit-zero", "parser": "go-test-json-v1",
	"mutation-kind": "inject-file", "mutation-expected-result": "fail", "stream": "stdout",
	"allocated-max": "AIRA=3", "file": "x.go:3", "resolved-by": "ticket:AIRA-1", "cwd": "root",
}

// optionConsumedLater are options the builder does not put in Request.Args
// because the CLI face consumes them after buildRequest. Each is a documented
// exception, not a default.
var optionConsumedLater = map[string]string{
	"spend add --usage-file": "read by the CLI after buildRequest and folded into the request's content",
	"init --id-prefix":       "consumed by the init fast path in runCLI before buildRequest runs",
	"ready --list":           "the documented spelling of the bare listing (core usage: ready [<selector>] | ready --list); its only effect, refusing a selector, is a builder check",
}

// optionVerbsNotBuilt are parseArgsAllowedOptions keys that never reach a
// builder.
var optionVerbsNotBuilt = map[string]string{
	"confine-dump": "not a CLI verb (buildRequest: E_UNKNOWN_VERB); --dump is a confine management flag parsed by parseConfineArgs",
}

func TestEveryAcceptedOptionIsConsumedOrRefused(t *testing.T) {
	var failures []string
	covered := map[string]bool{}
	verbs := make([]string, 0, len(parseArgsAllowedOptions))
	for verb := range parseArgsAllowedOptions {
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	for _, verb := range verbs {
		options := parseArgsAllowedOptions[verb]
		if len(options) == 0 {
			continue
		}
		if _, skip := optionVerbsNotBuilt[verb]; skip {
			continue
		}
		bases, ok := optionBaseArgvs[verb]
		if !ok {
			failures = append(failures, "verb "+verb+" admits options but optionBaseArgvs has no invocation for it")
			continue
		}
		names := make([]string, 0, len(options))
		for name := range options {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, base := range bases {
			baseRequest, err := buildFromArgv(base...)
			if err != nil {
				failures = append(failures, "base "+strings.Join(base, " ")+" does not build: "+err.Error())
				continue
			}
			for _, name := range names {
				if containsString(base, "--"+name) {
					continue
				}
				argv := append(append([]string(nil), base...), "--"+name)
				if !optionIsBareFlag(verb, name) {
					value := optionProbeValue[name]
					if value == "" {
						value = "x"
					}
					argv = append(argv, value)
				}
				label := strings.Join(base[:minInt(len(base), subverbDepth(base))], " ") + " --" + name
				request, err := buildFromArgv(argv...)
				switch {
				case err != nil && strings.HasPrefix(err.Error(), "E_SELECTOR_INVALID: option --"+name+" is not valid for"):
					covered[label] = true
				case err != nil:
					failures = append(failures, label+": built with the option failed for another reason: "+err.Error())
				case !reflect.DeepEqual(request.Args, baseRequest.Args):
					covered[label] = true
				default:
					if _, ok := optionConsumedLater[label]; ok {
						covered[label] = true
						continue
					}
					failures = append(failures, label+": accepted but request unchanged (accepted-and-discarded)")
				}
			}
		}
	}
	for _, failure := range failures {
		t.Error(failure)
	}
	if len(covered) < 100 {
		t.Fatalf("only %d (verb, subverb, option) pairs were exercised; the matrix is not walking the table", len(covered))
	}
	for label := range optionConsumedLater {
		if !covered[label] {
			t.Errorf("optionConsumedLater lists %q but no such pair was exercised (stale entry)", label)
		}
	}
}

// optionIsBareFlag mirrors parseArgs' value-less options.
func optionIsBareFlag(verb, name string) bool {
	switch name {
	case "rebuild", "steal", "strict":
		return true
	}
	switch {
	case name == "tickets" && verb == "import",
		(name == "purge" || name == "force") && verb == "eject",
		name == "close" && (verb == "run-input" || verb == "confine-input"),
		name == "from-start" && verb == "watch",
		name == "list" && verb == "ready",
		(name == "follow" || name == "full") && (verb == "run-log" || verb == "confine-log"),
		(name == "reasoning-subset" || name == "resolve-ticket") && verb == "spend",
		name == "all" && verb == "test-report",
		name == "unreviewed" && verb == "rant",
		name == "hold" && (verb == "create" || verb == "new"):
		return true
	}
	return false
}

// subverbDepth is how many leading argv words name the operation in a label:
// the verb, plus the subverb for the multi-operation verbs.
func subverbDepth(base []string) int {
	switch base[0] {
	case "rant", "find", "req", "test-report", "spend", "quota", "gate", "lease", "commands":
		if len(base) > 1 {
			return 2
		}
	}
	return 1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
