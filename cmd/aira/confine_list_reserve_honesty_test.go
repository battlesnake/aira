package main

import (
	"bytes"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
)

// TestConfineListReserveRenderHonoursEstablished is AIRA-220's client-side
// regression across the `confine --list` text face. An admission ledger the
// daemon has not established (a fresh/restarted daemon before its first
// admission, or a long-idle slice) must render the granted total as
// `unevaluated`, never as a fabricated `0B granted / … across 0 admitted jobs`
// that reads "the slice is empty, launch freely" while jobs are live. The
// ceiling is an independent read and is still shown.
func TestConfineListReserveRenderHonoursEstablished(t *testing.T) {
	const ceiling = int64(52 << 30)
	render := func(res runner.ConfineSliceReserve) string {
		var stdout, stderr bytes.Buffer
		result := runner.ConfineListResult{SliceReserve: &res}
		if exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
			t.Fatalf("render exit=%d stderr=%q", exit, stderr.String())
		}
		return stdout.String()
	}

	t.Run("established renders the granted pair", func(t *testing.T) {
		out := render(runner.ConfineSliceReserve{GrantedEstablished: true, GrantedBytes: 3 << 30, CeilingBytes: ceiling, Jobs: 5, ScopeJobs: 5, ScopeBytes: 3 << 30})
		if !strings.Contains(out, "granted /") || !strings.Contains(out, "across 5 admitted jobs") {
			t.Fatalf("established summary missing its granted clause:\n%s", out)
		}
		if strings.Contains(out, "slice reserve: unevaluated") {
			t.Fatalf("established summary wrongly read unevaluated:\n%s", out)
		}
		// The population split is a fact when the ledger is established.
		if !strings.Contains(out, "of which: 5 confine scopes") {
			t.Fatalf("established summary dropped the population split:\n%s", out)
		}
	})

	t.Run("absent ledger reads unevaluated, keeps ceiling", func(t *testing.T) {
		out := render(runner.ConfineSliceReserve{GrantedEstablished: false, CeilingBytes: ceiling})
		if !strings.Contains(out, "slice reserve: unevaluated") {
			t.Fatalf("absent-ledger summary must read unevaluated for granted:\n%s", out)
		}
		if !strings.Contains(out, "ceiling") {
			t.Fatalf("absent-ledger summary dropped the independently-read ceiling:\n%s", out)
		}
		// The precise fabrications this ticket exists to end — including the
		// population-split line the ticket quotes as its symptom (the "0 adopted
		// scopes" AIRA-105 misreading), which is derived from the same absent
		// snapshot and must go unevaluated in lockstep, not print fabricated zeros.
		if strings.Contains(out, "0B granted") || strings.Contains(out, "across 0 admitted") {
			t.Fatalf("absent-ledger summary fabricated an empty slice:\n%s", out)
		}
		if strings.Contains(out, "of which: 0 confine") || strings.Contains(out, "0 adopted scopes") {
			t.Fatalf("absent-ledger summary fabricated a population split:\n%s", out)
		}
	})
}
