package main

import (
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-185. A drain's Name is a fixed placeholder ("drain") that says nothing
// about the deploy it is holding the slice for, so the holder's own --reason is
// what makes the `confine --list` line actionable for a blocked session.
//
// verifies: AIRA-185
func TestRenderConfineListReportsTheExclusiveHoldReason(t *testing.T) {
	reserve := &runner.ConfineSliceReserve{GrantedBytes: 1 << 30, CeilingBytes: 61 << 30, Jobs: 1}
	reserve.Exclusive = &runner.ConfineExclusiveState{
		State: "held", Name: "drain", Owner: "mark", ScopeID: "CONFINE-drain-100-1@mark",
		WaitingJobs: 3, Reason: "deploy: slice-ceiling flip",
	}
	out := renderExclusiveList(t, reserve)
	want := `slice exclusive: held by "drain" (mark) reason="deploy: slice-ceiling flip" scope=CONFINE-drain-100-1@mark, 3 jobs waiting`
	if !strings.Contains(out, want) {
		t.Fatalf("held line missing the reason:\nwant %s\ngot\n%s", want, out)
	}

	// A DRAINING hold has not started yet, and the reason must be just as visible
	// then — that is the state an operator is most likely to be staring at while
	// their own job waits.
	reserve.Exclusive = &runner.ConfineExclusiveState{
		State: "draining", Name: "drain", Owner: "mark", WaitingJobs: 1, Reason: "deploy",
	}
	out = renderExclusiveList(t, reserve)
	if !strings.Contains(out, `slice exclusive: draining for "drain" (mark) reason="deploy", not started yet, 1 job waiting`) {
		t.Fatalf("draining line missing the reason:\n%s", out)
	}
}

// The clause is ADDITIVE: every `aira confine --exclusive` supplies no reason,
// and its line must be byte-identical to what it was before AIRA-185. A renderer
// that emitted `reason=""` would be inventing a fact nobody stated.
//
// verifies: AIRA-185
func TestRenderConfineListOmitsAnAbsentOrBlankReasonEntirely(t *testing.T) {
	for name, reason := range map[string]string{"absent": "", "whitespace": "   \t "} {
		t.Run(name, func(t *testing.T) {
			reserve := &runner.ConfineSliceReserve{GrantedBytes: 1 << 30, CeilingBytes: 61 << 30, Jobs: 1}
			reserve.Exclusive = &runner.ConfineExclusiveState{
				State: "held", Name: "bench-fft", Owner: "mark", ScopeID: "CONFINE-bench-fft-100-1@mark",
				WaitingJobs: 4, Reason: reason,
			}
			out := renderExclusiveList(t, reserve)
			if strings.Contains(out, "reason=") {
				t.Fatalf("a reason clause was invented for %s:\n%s", name, out)
			}
			if !strings.Contains(out, `slice exclusive: held by "bench-fft" (mark) scope=CONFINE-bench-fft-100-1@mark, 4 jobs waiting`) {
				t.Fatalf("the pre-AIRA-185 line changed shape:\n%s", out)
			}
		})
	}
}

// The reason is UNTRUSTED text chosen by another session and printed straight
// into this operator's shell. Control characters can rewrite the line, hide
// rows, or forge output, and an unbounded label can push the whole line off
// screen — both are escaped and bounded, and truncation is marked.
//
// verifies: AIRA-185
func TestRenderConfineListEscapesAndBoundsAHostileHoldReason(t *testing.T) {
	reserve := &runner.ConfineSliceReserve{GrantedBytes: 1 << 30, CeilingBytes: 61 << 30, Jobs: 1}
	reserve.Exclusive = &runner.ConfineExclusiveState{
		State: "held", Name: "drain", Owner: "mark", WaitingJobs: 0,
		Reason: "deploy\x1b[2K\r  slice exclusive: none" + strings.Repeat("x", runner.ConfineExclusiveReasonLimit),
	}
	out := renderExclusiveList(t, reserve)
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\r") {
		t.Fatalf("a control character reached the terminal:\n%q", out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("an over-long reason was truncated without marking it:\n%s", out)
	}
	// The forged text is still visible, but INSIDE the quoted reason field rather
	// than as a line of AIRA's own.
	lines := strings.Split(out, "\n")
	forged := 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "slice exclusive: none") {
			forged++
		}
	}
	if forged != 0 {
		t.Fatalf("a hostile reason forged an AIRA line:\n%s", out)
	}
}

// verifies: AIRA-185
func TestTopFooterCarriesTheExclusiveHoldReason(t *testing.T) {
	result := runner.ConfineListResult{
		Verdict: "pass",
		SliceReserve: &runner.ConfineSliceReserve{
			GrantedBytes: 1 << 30, CeilingBytes: 61 << 30, Jobs: 1, GrantedEstablished: true,
			Exclusive: &runner.ConfineExclusiveState{State: "held", Name: "drain", Reason: "deploy"},
		},
	}
	footer := topFooter(result)
	if !strings.Contains(footer, `EXCLUSIVE held "deploy"`) {
		t.Fatalf("footer=%q", footer)
	}

	// Without a reason the footer is exactly what it was before AIRA-185.
	result.SliceReserve.Exclusive = &runner.ConfineExclusiveState{State: "draining", Name: "bench"}
	footer = topFooter(result)
	if !strings.Contains(footer, "EXCLUSIVE draining") || strings.Contains(footer, `"`) {
		t.Fatalf("footer=%q", footer)
	}

	// And it escapes the same hostile text the list line does.
	result.SliceReserve.Exclusive = &runner.ConfineExclusiveState{State: "held", Reason: "deploy\x1b[2Kforged"}
	if footer = topFooter(result); strings.Contains(footer, "\x1b") {
		t.Fatalf("an escape sequence reached the footer: %q", footer)
	}
}

// verifies: AIRA-220
// TestTopFooterHonoursGrantedEstablished pins that `aira top`'s footer reports the
// granted total AND the population split as unevaluated when the daemon holds no
// admission ledger, and as facts when it does — so an inverted establishment gate
// (or a build that forgot to gate the footer's population clause) is caught.
func TestTopFooterHonoursGrantedEstablished(t *testing.T) {
	t.Run("established", func(t *testing.T) {
		footer := topFooter(runner.ConfineListResult{Verdict: "pass", SliceReserve: &runner.ConfineSliceReserve{
			GrantedEstablished: true, GrantedBytes: 3 << 30, CeilingBytes: 12 << 30, Jobs: 2, ScopeJobs: 2, ScopeBytes: 3 << 30,
		}})
		if strings.Contains(footer, "unevaluated") {
			t.Fatalf("established footer wrongly read unevaluated: %q", footer)
		}
		if !strings.Contains(footer, "granted") || !strings.Contains(footer, "2 scopes") {
			t.Fatalf("established footer missing granted or population clause: %q", footer)
		}
	})
	t.Run("absent-ledger", func(t *testing.T) {
		footer := topFooter(runner.ConfineListResult{Verdict: "pass", SliceReserve: &runner.ConfineSliceReserve{
			GrantedEstablished: false, CeilingBytes: 12 << 30,
		}})
		if !strings.Contains(footer, "granted unevaluated") {
			t.Fatalf("absent-ledger footer must read granted unevaluated: %q", footer)
		}
		if !strings.Contains(footer, "populations unevaluated") {
			t.Fatalf("absent-ledger footer must read populations unevaluated, not fabricated zeros: %q", footer)
		}
	})
}
