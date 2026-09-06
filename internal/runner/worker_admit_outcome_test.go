package runner

import (
	"strings"
	"testing"
)

// verifies: AIRA-42 — the outcome line is a structured record, and free text
// inside it can never be read as a field.
func TestWorkerAdmitOutcomeLineRoundTrips(t *testing.T) {
	tests := []struct {
		name    string
		outcome WorkerAdmitOutcome
		grant   *WorkerAdmitGrantFields
	}{
		{
			name: "declined with a plain reason",
			outcome: WorkerAdmitOutcome{
				State: WorkerAdmitStateDenied, Class: WorkerAdmitClassContended,
				Reason: WorkerAdmitReasonInsufficientHeadroom,
			},
		},
		{
			name: "detail carrying spaces, newlines and its own key=value pairs",
			outcome: WorkerAdmitOutcome{
				State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
				Reason: WorkerAdmitReasonUnknownDaemonOutcome,
				// A hostile detail: if the renderer did not escape it, this
				// would forge a grant on the far side.
				Detail: "state=granted class=granted containment=enforced scope=/evil worker_id=9\nmemory_max=1 memory_high=1 100% =",
			},
		},
		{
			// AIRA-42's merge with AIRA-39 DELETED a defensive hack that
			// mangled the token "unbounded" into "un-bounded" in exactly this
			// kind of diagnostic, because the retired substring classifier
			// read any "unevaluated" message containing it as an uncapped
			// outer scope and ran the whole suite unconfined. Both strings
			// below are the real shapes that triggered it: an operator's
			// `aira confine --name unbounded-suite` echoed into a cgroup path,
			// and raw memory.max bytes quoted back through %q. They must now
			// survive VERBATIM while changing nothing about the verdict.
			name: "detail carrying the token the retired classifier keyed on",
			outcome: WorkerAdmitOutcome{
				State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContended,
				Reason: WorkerAdmitReasonWorkerScopesUnreadable,
				Detail: `worker scope /sys/fs/cgroup/.aira-CONFINE-unbounded-suite-1/.aira-worker-2: ` +
					`memory.max is not a finite byte count ("unbounded")`,
			},
		},
		{
			name: "granted with placement fields",
			outcome: WorkerAdmitOutcome{
				State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted,
			},
			grant: &WorkerAdmitGrantFields{
				ScopePath: "/sys/fs/cgroup/a b/.aira-worker-1", WorkerID: "1",
				MemoryMax: 400 << 20, Containment: WorkerAdmitContainmentEnforced,
			},
		},
	}
	// AIRA-123: the advisory grade round-trips too, and carries a DIFFERENT set
	// of keys. Its token count is asserted separately below because the shared
	// wantTokens arithmetic above is written for the enforced shape.
	t.Run("granted ledger-only with no placement fields", func(t *testing.T) {
		line, err := WorkerAdmitOutcomeLine(
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			&WorkerAdmitGrantFields{
				WorkerID: "7", Containment: WorkerAdmitContainmentAdvisory, Reserved: 400 << 20,
			})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		fields, err := ParseWorkerAdmitOutcomeLine(line)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if fields["containment"] != WorkerAdmitContainmentAdvisory || fields["reserved"] != "419430400" {
			t.Fatalf("advisory grade or reservation lost: %v", fields)
		}
		// ABSENCE, not emptiness. `scope=` with an empty value would still be a
		// scope key to every reader that tests for presence -- which is how a
		// ledger-only grant would come to be read as a placed one.
		if _, present := fields["scope"]; present {
			t.Fatalf("a ledger-only grant must carry NO scope key at all: %q", line)
		}
		if _, present := fields["memory_max"]; present {
			t.Fatalf("a ledger-only grant must carry NO memory_max key at all: %q", line)
		}
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line, err := WorkerAdmitOutcomeLine(test.outcome, test.grant)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.ContainsAny(line, "\n\r") {
				t.Fatalf("outcome line must stay on one line: %q", line)
			}
			// Token COUNT, not just field values: this is the direct check
			// that escaped free text cannot split into extra tokens. Without
			// it, a renderer that stopped escaping `detail` could still pass
			// every assertion below as long as the forged keys happened to
			// land on values equal to the real ones — and the whole reason
			// AIRA-39's "unbounded" mangling could be deleted is that
			// tokenisation is structurally safe, so it is worth asserting
			// structurally.
			wantTokens := 1 // the frame marker
			for _, present := range []bool{
				true, true, // state, class — always rendered
				test.outcome.Reason != "",
				test.outcome.Detail != "",
			} {
				if present {
					wantTokens++
				}
			}
			if test.grant != nil {
				// containment, worker_id, scope, memory_max — the four keys an
				// ENFORCED grant carries. AIRA-35 removed memory_high from the
				// set; AIRA-123 added containment (required on every grade) and
				// `reserved` (advisory only, so absent here). swap_cap and
				// cpu_slots are optional and empty in this fixture, so a renderer
				// that started emitting either unconditionally would break this
				// count rather than slipping through.
				wantTokens += 4
			}
			if got := len(strings.Fields(line)); got != wantTokens {
				t.Fatalf("line split into %d tokens, want %d — free text broke tokenisation: %q",
					got, wantTokens, line)
			}
			fields, err := ParseWorkerAdmitOutcomeLine(line)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			if fields["state"] != test.outcome.State || fields["class"] != test.outcome.Class {
				t.Fatalf("state/class round trip lost: %v from %q", fields, line)
			}
			if fields["reason"] != test.outcome.Reason || fields["detail"] != test.outcome.Detail {
				t.Fatalf("reason/detail round trip lost: %v from %q", fields, line)
			}
			if test.grant == nil {
				if _, present := fields["scope"]; present {
					t.Fatalf("a declined outcome must not carry placement fields: %v", fields)
				}
				return
			}
			if fields["scope"] != test.grant.ScopePath || fields["worker_id"] != test.grant.WorkerID {
				t.Fatalf("placement fields round trip lost: %v", fields)
			}
		})
	}
}

// verifies: AIRA-42 — a granted line always carries placement coordinates and
// a declined line never does, so a half-formed grant cannot be rendered at all.
// verifies: AIRA-64 §9.21 — the cpu_slots token survives rendering and
// parsing, and its ABSENCE is preserved as absence.
//
// The absence half is the load-bearing one: an older daemon emits no token, and
// the supervisor must be able to tell "this daemon said nothing" from "this
// daemon said ok". Rendering an empty value as `cpu_slots=` would collapse that
// distinction and turn silence into a claim.
func TestWorkerAdmitOutcomeLineCarriesCPUSlots(t *testing.T) {
	granted := WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted}
	base := WorkerAdmitGrantFields{
		ScopePath: "/outer/.aira-worker-1", WorkerID: "1", MemoryMax: 400,
		Containment: WorkerAdmitContainmentEnforced,
	}

	for _, state := range []string{WorkerAdmitCPUSlotsOK, WorkerAdmitCPUSlotsUnevaluated} {
		grant := base
		grant.CPUSlots = state
		line, err := WorkerAdmitOutcomeLine(granted, &grant)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := ParseWorkerAdmitOutcomeLine(line)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if fields["cpu_slots"] != state {
			t.Fatalf("cpu_slots=%q, want %q (line=%q)", fields["cpu_slots"], state, line)
		}
		// The three fields the supervisor REQUIRES must be untouched by the
		// addition, or an additive field became a breaking one.
		for key, want := range map[string]string{
			"scope": "/outer/.aira-worker-1", "worker_id": "1",
			"memory_max": "400",
		} {
			if fields[key] != want {
				t.Fatalf("%s=%q want %q", key, fields[key], want)
			}
		}
	}

	line, err := WorkerAdmitOutcomeLine(granted, &base)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := ParseWorkerAdmitOutcomeLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := fields["cpu_slots"]; present {
		t.Fatalf("an unset CPUSlots must emit NO token, so silence cannot be read as a claim: %q", line)
	}
}

// verifies: AIRA-35 — swap_cap survives the render/parse round trip for EVERY
// catalogued value, and an unset SwapCap emits no token at all.
//
// The exact value is pinned per state rather than "one of the three", for the
// same reason worker_admit_cli_granted_linux_test.go pins cpu_slots exactly:
// mutation testing there proved that accepting any catalogued value lets a hop
// that hardcodes a constant survive the whole suite. swap_cap is a governance
// signal with the same failure mode -- "enforced" fabricated on a host where
// swap could not actually be bounded is precisely the silent lost guarantee
// this field exists to prevent -- so it gets the same treatment.
func TestWorkerAdmitOutcomeLineCarriesSwapCap(t *testing.T) {
	granted := WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted}
	base := WorkerAdmitGrantFields{
		ScopePath: "/outer/.aira-worker-1", WorkerID: "1", MemoryMax: 400,
		Containment: WorkerAdmitContainmentEnforced,
	}

	for _, state := range []string{
		WorkerAdmitSwapCapEnforced, WorkerAdmitSwapCapNotApplicable, WorkerAdmitSwapCapUnavailable,
	} {
		if !IsWorkerAdmitSwapCap(state) {
			t.Fatalf("%q is not catalogued by IsWorkerAdmitSwapCap", state)
		}
		grant := base
		grant.SwapCap = state
		line, err := WorkerAdmitOutcomeLine(granted, &grant)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := ParseWorkerAdmitOutcomeLine(line)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if fields["swap_cap"] != state {
			t.Fatalf("swap_cap=%q, want %q (line=%q)", fields["swap_cap"], state, line)
		}
		// The three required fields must be untouched by the addition.
		for key, want := range map[string]string{
			"scope": "/outer/.aira-worker-1", "worker_id": "1", "memory_max": "400",
		} {
			if fields[key] != want {
				t.Fatalf("%s=%q want %q", key, fields[key], want)
			}
		}
	}

	if IsWorkerAdmitSwapCap("") || IsWorkerAdmitSwapCap("ok") {
		t.Fatal("IsWorkerAdmitSwapCap must reject uncatalogued values")
	}

	line, err := WorkerAdmitOutcomeLine(granted, &base)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := ParseWorkerAdmitOutcomeLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := fields["swap_cap"]; present {
		t.Fatalf("an unset SwapCap must emit NO token, so silence cannot be read as a claim: %q", line)
	}
}

func TestWorkerAdmitOutcomeLineRefusesInconsistentOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		outcome WorkerAdmitOutcome
		grant   *WorkerAdmitGrantFields
	}{
		{"uncatalogued state", WorkerAdmitOutcome{State: "wat", Class: WorkerAdmitClassContended}, nil},
		{"uncatalogued class", WorkerAdmitOutcome{State: WorkerAdmitStateDenied, Class: "wat"}, nil},
		{
			"state granted but class not",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassContended},
			&WorkerAdmitGrantFields{},
		},
		// AIRA-123: the containment grade is required and its coordinates are
		// checked in BOTH directions. Each row below is a shape a producer
		// could plausibly emit and a consumer would then read as the wrong
		// grade -- which is the whole failure this ticket must make
		// unrepresentable.
		{
			"granted with no containment grade at all",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			&WorkerAdmitGrantFields{ScopePath: "/outer/.aira-worker-1", WorkerID: "1", MemoryMax: 400},
		},
		{
			"granted with an uncatalogued containment grade",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			&WorkerAdmitGrantFields{ScopePath: "/o/.aira-worker-1", WorkerID: "1", MemoryMax: 400, Containment: "sort-of"},
		},
		{
			"advisory grant naming a cgroup scope",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			&WorkerAdmitGrantFields{
				ScopePath: "/outer/.aira-worker-1", WorkerID: "1",
				Containment: WorkerAdmitContainmentAdvisory, Reserved: 400,
			},
		},
		{
			"advisory grant reporting a memory_max nothing enforces",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			&WorkerAdmitGrantFields{
				WorkerID: "1", MemoryMax: 400,
				Containment: WorkerAdmitContainmentAdvisory, Reserved: 400,
			},
		},
		{
			"advisory grant with no reservation",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			&WorkerAdmitGrantFields{WorkerID: "1", Containment: WorkerAdmitContainmentAdvisory},
		},
		{
			"enforced grant with no scope path",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			&WorkerAdmitGrantFields{WorkerID: "1", MemoryMax: 400, Containment: WorkerAdmitContainmentEnforced},
		},
		{
			"enforced grant also carrying an advisory reservation",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			&WorkerAdmitGrantFields{
				ScopePath: "/o/.aira-worker-1", WorkerID: "1", MemoryMax: 400,
				Containment: WorkerAdmitContainmentEnforced, Reserved: 400,
			},
		},
		{
			"class granted but state not",
			WorkerAdmitOutcome{State: WorkerAdmitStateDenied, Class: WorkerAdmitClassGranted},
			nil,
		},
		{
			"granted without placement fields",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			nil,
		},
		{
			"declined with placement fields",
			WorkerAdmitOutcome{State: WorkerAdmitStateDenied, Class: WorkerAdmitClassContended},
			&WorkerAdmitGrantFields{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if line, err := WorkerAdmitOutcomeLine(test.outcome, test.grant); err == nil {
				t.Fatalf("expected a refusal, rendered %q", line)
			}
		})
	}
}

// verifies: AIRA-42 — the parser refuses anything outside the catalogue rather
// than defaulting, which is what keeps an unrecognised shape from being
// resolved into "the daemon is gone".
func TestParseWorkerAdmitOutcomeLineRefusesUncataloguedInput(t *testing.T) {
	tests := []struct{ name, line string }{
		{"empty", ""},
		{"no marker", "state=denied class=contended"},
		{"wrong marker", "granted scope=/x worker_id=1 memory_max=1"},
		{"not key=value", WorkerAdmitOutcomeMarker + " state=denied contended"},
		{"missing state", WorkerAdmitOutcomeMarker + " class=contended"},
		{"missing class", WorkerAdmitOutcomeMarker + " state=denied"},
		{"uncatalogued state", WorkerAdmitOutcomeMarker + " state=wat class=contended"},
		{"uncatalogued class", WorkerAdmitOutcomeMarker + " state=denied class=wat"},
		{"grantedness disagreement", WorkerAdmitOutcomeMarker + " state=granted class=contended"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if fields, err := ParseWorkerAdmitOutcomeLine(test.line); err == nil {
				t.Fatalf("expected a refusal for %q, parsed %v", test.line, fields)
			}
		})
	}
}

// verifies: AIRA-42 — exactly two classes strip RAM containment, and both are
// named here so widening that set cannot happen silently.
func TestWorkerAdmitContainmentStrippingClassesAreExactlyTwo(t *testing.T) {
	stripping := map[string]bool{
		WorkerAdmitClassAdmissionUnusable: true,
		WorkerAdmitClassPlacementFailed:   true,
	}
	// The other four must not be in that set: granted uses the grant,
	// contended retries, and both terminal classes mark the queued work
	// unevaluated while leaving the daemon in use.
	for _, class := range []string{
		WorkerAdmitClassGranted, WorkerAdmitClassContended,
		WorkerAdmitClassRequestInvalid, WorkerAdmitClassContractViolation,
	} {
		if stripping[class] {
			t.Fatalf("class %q must not strip containment", class)
		}
	}
	if got := len(WorkerAdmitClasses()); got != len(stripping)+4 {
		t.Fatalf("the class catalogue has %d entries; this test enumerates %d — "+
			"a new class must declare whether it strips containment", got, len(stripping)+4)
	}
}
