package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/codes"
	"aira/internal/daemon"
	"aira/internal/store"
)

// verifies: S17 — parseConfineReportArgs is the renamed parseWorkerPeakArgs
// (the CLI verb is now spelled the same as the wire verb it always sent).
// --signature is required, --budget/--budget-basis travel as a pair, and an
// unrecognised option (including --json, which is stripped before parseArgs
// ever sees this verb -- see the run-path dispatch guard) is refused.
func TestParseConfineReportArgsRequiresSignatureAndPairsBudgetTerms(t *testing.T) {
	if _, _, err := parseConfineReportArgs(nil); err == nil {
		t.Fatal("no --signature must error")
	}
	if _, _, err := parseConfineReportArgs([]string{"--signature", "sig", "--budget", "100"}); err == nil {
		t.Fatal("--budget without --budget-basis must error")
	}
	if _, _, err := parseConfineReportArgs([]string{"--signature", "sig", "--budget-basis", "cap:x"}); err == nil {
		t.Fatal("--budget-basis without --budget must error")
	}
	if _, _, err := parseConfineReportArgs([]string{"--signature", "sig", "--bogus", "x"}); err == nil {
		t.Fatal("an unknown option must error")
	}
	_, options, err := parseConfineReportArgs([]string{
		"--signature", "--aitest-workers=auto", "--peak-rss", "700", "--oom",
		"--budget", "104857600", "--budget-basis", "cap:aitest:env:set",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// A signature may legitimately begin with "--" (a pytest argument like
	// --aitest-workers=auto travels inside it): the look-ahead guard worker-admit
	// uses is deliberately absent here, so this must NOT be truncated or refused.
	if options["signature"] != "--aitest-workers=auto" || options["peak-rss"] != "700" ||
		options["oom"] != "true" || options["budget"] != "104857600" || options["budget-basis"] != "cap:aitest:env:set" {
		t.Fatalf("options=%v", options)
	}
}

// verifies: S17 — client-argument errors are refused BEFORE any daemon dial,
// exactly like the deleted worker-peak relay (no daemon runs in this test; a
// dial attempt would surface as E_CONFINE_UNAVAILABLE instead of the correct
// E_CONFINE_ARGUMENT_INVALID if these checks were missing or ordered late).
func TestRunConfineReportCommandRejectsBadTermsBeforeDial(t *testing.T) {
	cases := []struct {
		name    string
		options map[string]string
		want    string
	}{
		{"non-numeric peak-rss", map[string]string{"signature": "sig", "peak-rss": "not-a-number"}, "--peak-rss"},
		{"zero peak-rss", map[string]string{"signature": "sig", "peak-rss": "0"}, "--peak-rss"},
		{"non-numeric budget", map[string]string{"signature": "sig", "budget": "nope", "budget-basis": "cap:x"}, "--budget"},
		{"budget-basis names no family", map[string]string{"signature": "sig", "budget": "100", "budget-basis": "nonsense"}, "budget-basis"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var stderr bytes.Buffer
			exit := runConfineReportCommand(context.Background(), testCase.options, &stderr)
			if want := codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID"); exit != want {
				t.Fatalf("exit=%d want %d stderr=%q", exit, want, stderr.String())
			}
			if !strings.Contains(stderr.String(), testCase.want) {
				t.Fatalf("stderr=%q, want a mention of %q", stderr.String(), testCase.want)
			}
		})
	}
}

// verifies: S17 — a syntactically valid report fails closed (E_CONFINE_UNAVAILABLE)
// rather than silently succeeding when no daemon is reachable. Combined with the
// success-path test below, this bounds runConfineReportCommand's behaviour: it must
// actually dial the daemon, and a send-path mutation that skipped the dial (turning
// this command into a no-op that always returns 0) would flip this exit to 0.
func TestRunConfineReportCommandFailsClosedWhenDaemonUnreachable(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	var stderr bytes.Buffer
	exit := runConfineReportCommand(context.Background(), map[string]string{"signature": "sig"}, &stderr)
	if want := codes.ExitForCode("E_CONFINE_UNAVAILABLE"); exit != want {
		t.Fatalf("exit=%d want %d (E_CONFINE_UNAVAILABLE) stderr=%q", exit, want, stderr.String())
	}
}

// verifies: S17 — the mutation pin the plan calls for (§S17 tests): breaking the
// confine-report send path must red this test. It exercises the real wire path
// end to end -- a real daemon.Server on a real unix socket backed by a real
// sqlite store -- and then reopens that same store to confirm the sample landed
// under Kind=pytest-worker (never the generic "confine" kind a confine job's own
// auto-report would use), with every term (peak, oom, budget) intact. This is the
// estimate's feedback path (§9 of the admission-counter design): starving it here
// is exactly the regression a Go-side no-op or a wrong-kind report would cause.
func TestRunConfineReportCommandSendsPytestWorkerSampleToDaemon(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	server := daemon.NewServer(paths)
	startCommandDaemon(t, server)

	const signature = "/repo/one\x1ftests\x1f--aitest-workers=auto"
	options := map[string]string{
		"signature": signature, "peak-rss": "734003200", "oom": "true",
		"budget": "104857600", "budget-basis": "cap:aitest:env:set",
	}
	var stderr bytes.Buffer
	exit := runConfineReportCommand(context.Background(), options, &stderr)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}

	// A second handle onto the same WAL-mode sqlite file is a normal concurrent
	// reader -- no need to stop the daemon's own connection first.
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stats, err := db.ResourcePeakHistory(context.Background(), store.ResourcePeakKindPytestWorker, signature)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SampleCount != 1 || stats.PeakMax != 734003200 || stats.OOMCount != 1 || stats.MaxOOMPeak != 734003200 {
		t.Fatalf("pytest-worker stats=%+v, want one recorded sample carrying the reported peak and OOM", stats)
	}
	// The confine kind must stay untouched: an aitest sample must never enter
	// the estimate a plain `aira confine`/`aira run` command draws on.
	confineStats, err := db.ResourcePeakHistory(context.Background(), store.ResourcePeakKindConfine, signature)
	if err != nil {
		t.Fatal(err)
	}
	if confineStats.SampleCount != 0 {
		t.Fatalf("confine-kind stats=%+v, want no rows -- the pytest-worker sample must not leak into the confine kind", confineStats)
	}
}
