package gate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	CheckerCommand             Checker = "command"
	CommandPredicateExitZero           = "exit-zero"
	CommandPredicateTestsGreen         = "tests-green"
	CommandParserGoTestJSONV1          = "go-test-json-v1"
	DefaultOutputCapBytes      int64   = 8 * 1024 * 1024
)

// Command is the committed, serialized execution policy for command gates.
// Cwd is "root" or a relative path below the materialized evaluation root.
type Command struct {
	Argv           []string `json:"argv"`
	Cwd            string   `json:"cwd"`
	EnvAllow       []string `json:"env_allow"`
	TimeoutMS      int64    `json:"timeout_ms"`
	OutputCapBytes int64    `json:"output_cap_bytes"`
	Parser         string   `json:"parser,omitempty"`
	Predicate      string   `json:"predicate"`
}

func (c Command) Normalized() Command {
	if c.OutputCapBytes == 0 {
		c.OutputCapBytes = DefaultOutputCapBytes
	}
	c.Argv = append([]string(nil), c.Argv...)
	c.EnvAllow = append([]string(nil), c.EnvAllow...)
	return c
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c Command) Validate() error {
	c = c.Normalized()
	if len(c.Argv) == 0 || strings.TrimSpace(c.Argv[0]) == "" {
		return errors.New("E_GATE_INVALID: command argv must not be empty")
	}
	for _, token := range c.Argv {
		if token == "" || strings.ContainsRune(token, 0) {
			return errors.New("E_GATE_INVALID: command argv contains an invalid token")
		}
	}
	if c.Cwd != "root" {
		if c.Cwd == "" || filepath.IsAbs(filepath.FromSlash(c.Cwd)) || !safeRelativePath(c.Cwd) {
			return errors.New("E_GATE_INVALID: command cwd must be root or a safe relative path")
		}
	}
	if c.TimeoutMS <= 0 {
		return errors.New("E_GATE_INVALID: command timeout_ms must be positive")
	}
	if c.OutputCapBytes < 1024 || c.OutputCapBytes > 64*1024*1024 {
		return errors.New("E_GATE_INVALID: command output_cap_bytes must be between 1KiB and 64MiB")
	}
	if c.Predicate != CommandPredicateExitZero && c.Predicate != CommandPredicateTestsGreen {
		return fmt.Errorf("E_GATE_INVALID: unknown command predicate %q", c.Predicate)
	}
	if c.Parser != "" && c.Parser != CommandParserGoTestJSONV1 {
		return fmt.Errorf("E_GATE_INVALID: unknown command parser %q", c.Parser)
	}
	if c.Predicate == CommandPredicateTestsGreen && c.Parser != CommandParserGoTestJSONV1 {
		return errors.New("E_GATE_INVALID: tests-green requires go-test-json-v1")
	}
	if c.Predicate == CommandPredicateExitZero && c.Parser != "" {
		return errors.New("E_GATE_INVALID: exit-zero does not accept a parser")
	}
	previous := ""
	pathAllowed := false
	for _, name := range c.EnvAllow {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("E_GATE_INVALID: invalid environment name %q", name)
		}
		if previous != "" && name <= previous {
			return errors.New("E_GATE_INVALID: env_allow must be sorted and unique")
		}
		previous = name
		if name == "PATH" {
			pathAllowed = true
		}
	}
	if !filepath.IsAbs(filepath.FromSlash(c.Argv[0])) && !pathAllowed {
		return errors.New("E_GATE_INVALID: relative argv[0] requires PATH in env_allow")
	}
	return nil
}

func safeRelativePath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return false
		}
	}
	return true
}

type GoTestJSONResult struct {
	Complete        bool
	DiscoveredCount int
	FailedCount     int
}

// GoTestEvent is the strict line-level event shape shared by command gates
// and the test-report ingester. Keeping decoding here prevents the two
// consumers from accepting different go test -json grammars.
type GoTestEvent struct {
	Time    string  `json:"Time,omitempty"`
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test,omitempty"`
	Elapsed float64 `json:"Elapsed,omitempty"`
	Output  string  `json:"Output,omitempty"`
}

// DecodeGoTestJSONEvents decodes the strict JSONL envelope and event
// vocabulary. It intentionally does not decide whether an otherwise valid
// stream is complete; callers apply their own terminal-state policy.
func DecodeGoTestJSONEvents(data []byte) ([]GoTestEvent, error) {
	if !utf8Valid(data) {
		return nil, errors.New("parser malformed: output is not UTF-8")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var events []GoTestEvent
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			return nil, errors.New("parser malformed: blank event line")
		}
		var event GoTestEvent
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil || event.Action == "" || event.Package == "" {
			return nil, errors.New("parser malformed: malformed event")
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return nil, errors.New("parser malformed: multiple JSON values")
		}
		if event.Test != "" && (event.Action == "start" || event.Action == "package-terminal") {
			return nil, errors.New("parser malformed: package event has test")
		}
		switch event.Action {
		case "start", "run", "pause", "cont", "output", "bench", "pass", "fail", "skip":
		default:
			return nil, fmt.Errorf("parser malformed: unknown action %q", event.Action)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("parser malformed: partial or oversized final line")
	}
	if len(data) == 0 {
		return nil, errors.New("parser malformed: empty stream")
	}
	return events, nil
}

type testEventState struct {
	paused   bool
	terminal bool
	outcome  string
}

// ParseGoTestJSONV1 parses the strict, line-oriented go test -json event
// grammar. An incomplete result is returned with an error; callers must never
// treat it as a predicate pass.
func ParseGoTestJSONV1(data []byte) (GoTestJSONResult, error) {
	result := GoTestJSONResult{}
	if !utf8Valid(data) {
		return result, errors.New("parser incomplete: output is not UTF-8")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Test output lines can be large, but a bounded parser must not silently
	// accept Scanner's default token limit as a valid stream.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	packages := map[string]string{}
	tests := map[string]testEventState{}
	starts := map[string]bool{}
	packageTerminal := map[string]bool{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			return result, errors.New("parser incomplete: blank event line")
		}
		// The shared decoder owns envelope strictness. This gate parser keeps
		// its existing completeness semantics and state-machine diagnostics.
		var event GoTestEvent
		decoded, decodeErr := DecodeGoTestJSONEvents(append(append([]byte(nil), line...), '\n'))
		if decodeErr != nil {
			return result, errors.New("parser incomplete: malformed event")
		}
		event = decoded[0]
		if _, ok := packages[event.Package]; !ok {
			packages[event.Package] = event.Package
		}
		if event.Test != "" && (event.Action == "start" || event.Action == "package-terminal") {
			return result, errors.New("parser incomplete: package event has test")
		}
		switch event.Action {
		case "start":
			if starts[event.Package] || event.Test != "" {
				return result, errors.New("parser incomplete: invalid package start")
			}
			starts[event.Package] = true
		case "run":
			if !starts[event.Package] || event.Test == "" || packageTerminal[event.Package] {
				return result, errors.New("parser incomplete: invalid test start")
			}
			key := event.Package + "\x00" + event.Test
			if _, exists := tests[key]; exists {
				return result, errors.New("parser incomplete: duplicate test start")
			}
			tests[key] = testEventState{}
			result.DiscoveredCount++
		case "pause":
			if !setPaused(tests, event.Package, event.Test, true) {
				return result, errors.New("parser incomplete: invalid pause")
			}
		case "cont":
			if !setPaused(tests, event.Package, event.Test, false) {
				return result, errors.New("parser incomplete: invalid continue")
			}
		case "output", "bench":
			// Output may be package-level or attached to a running test.
		case "pass", "fail", "skip":
			if event.Test != "" {
				key := event.Package + "\x00" + event.Test
				state, ok := tests[key]
				if !ok || state.terminal || state.paused {
					return result, errors.New("parser incomplete: invalid test terminal")
				}
				state.terminal = true
				state.outcome = event.Action
				tests[key] = state
				if event.Action == "fail" {
					result.FailedCount++
				}
				continue
			}
			if !starts[event.Package] || packageTerminal[event.Package] {
				return result, errors.New("parser incomplete: invalid package terminal")
			}
			for key, state := range tests {
				if strings.HasPrefix(key, event.Package+"\x00") && !state.terminal {
					return result, errors.New("parser incomplete: package ended with open test")
				}
			}
			packageTerminal[event.Package] = true
			packages[event.Package] = event.Action
			if event.Action == "fail" {
				result.FailedCount++
			}
		default:
			return result, fmt.Errorf("parser incomplete: unknown action %q", event.Action)
		}
	}
	if err := scanner.Err(); err != nil {
		return result, errors.New("parser incomplete: partial or oversized final line")
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return result, errors.New("parser incomplete: partial final line")
	}
	if len(starts) == 0 {
		return result, errors.New("parser incomplete: no package")
	}
	for packageName := range starts {
		if !packageTerminal[packageName] {
			return result, errors.New("parser incomplete: package has no terminal event")
		}
	}
	for _, state := range tests {
		if !state.terminal || state.paused {
			return result, errors.New("parser incomplete: discovered test has no terminal event")
		}
	}
	result.Complete = true
	return result, nil
}

func setPaused(tests map[string]testEventState, pkg, test string, paused bool) bool {
	if test == "" {
		return false
	}
	key := pkg + "\x00" + test
	state, ok := tests[key]
	if !ok || state.terminal || state.paused == paused {
		return false
	}
	state.paused = paused
	tests[key] = state
	return true
}

func utf8Valid(data []byte) bool {
	return utf8.Valid(data)
}

// CanonicalEnvAllow returns a detached sorted copy for callers building lane
// identities without changing the committed definition in place.
func CanonicalEnvAllow(names []string) []string {
	copy := append([]string(nil), names...)
	sort.Strings(copy)
	return copy
}
