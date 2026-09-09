package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"aira/internal/buildid"
	"aira/internal/core"
	"aira/internal/daemon"
)

// versionDaemonWait bounds the daemon half of `aira version`.
const versionDaemonWait = 5 * time.Second

// runVersionCommand answers `aira version` / `--version` / `-v`.
//
// AIRA-202. It is handled beside `help` and BEFORE project discovery, for the
// same reason: asking what binary you are running must work in any directory,
// with no project, and — above all — with the daemon down, because a wedged or
// stale daemon is exactly when the question gets asked.
//
// The verb never fails. The client half is always establishable locally, and an
// unreachable daemon yields an `unevaluated` daemon half with its reason rather
// than an error, so the answer is always at least half useful.
func runVersionCommand(ctx context.Context, dispatcher Dispatcher, dispatcherErr error, jsonOutput bool, stdout, stderr io.Writer) int {
	client := buildid.Current()
	daemonIdentity := buildid.Identity{Reason: "no dispatcher was available to ask the daemon"}
	if dispatcherErr != nil {
		daemonIdentity = buildid.Identity{Reason: "the daemon client could not be constructed: " + dispatcherErr.Error()}
	}
	if dispatcher != nil {
		// Bounded deliberately. The generic client wait is minutes long, which is
		// right for a mutation that must not be abandoned half-done -- and wrong
		// for a read-only "what is running", which an operator types precisely
		// BECAUSE something is wedged. A slow answer here is worth less than a
		// prompt `unevaluated`, so the wait is short and the timeout is reported
		// as the reason rather than as a hang.
		bounded, cancel := context.WithTimeout(ctx, versionDaemonWait)
		defer cancel()
		response := dispatcher.Dispatch(bounded, daemon.WorktreeScope{}, core.Request{Verb: "version"})
		daemonIdentity = daemonIdentityFrom(response)
	}
	report := buildid.NewReport(client, daemonIdentity)
	if jsonOutput {
		return render(core.Response{OK: true, Code: "OK", Data: report}, true, stdout, stderr)
	}
	return renderVersion(report, stdout)
}

// daemonIdentityFrom decodes the daemon's own answer, keeping every failure
// distinguishable from an established one. A transport error, a refusal and a
// malformed reply are all `unevaluated` with the cause named — never a silent
// zero Identity, which would render as an unexplained blank.
func daemonIdentityFrom(response core.Response) buildid.Identity {
	if !response.OK {
		reason := response.Error
		if reason == "" {
			reason = "the daemon did not answer the version request (" + response.Code + ")"
		}
		// A daemon that does not KNOW this verb identifies itself by that alone: it
		// predates the verb. Both shapes are covered because an older daemon does
		// not refuse an unknown verb outright -- it falls through to its own store
		// path and fails resolving the empty scope this request carries, which is
		// what E_CONFIG_INVALID means HERE specifically. Worded as an inference,
		// because these codes mean other things on other requests.
		if response.Code == "E_UNKNOWN_VERB" || response.Code == "E_CONFIG_INVALID" {
			reason += " — the running daemon does not implement the version verb, which itself indicates it is OLDER than this client; " +
				"run `aira install` and restart aira-daemon.service"
		}
		return buildid.Identity{Reason: reason}
	}
	if identity, ok := response.Data.(buildid.Identity); ok {
		return identity
	}
	// Over the wire the reply arrives as RawData, so both shapes are decoded the
	// way renderConfineBudgetResponse does it.
	data := response.RawData
	if len(data) == 0 {
		var err error
		if data, err = json.Marshal(response.Data); err != nil {
			return buildid.Identity{Reason: "the daemon's version reply could not be re-encoded: " + err.Error()}
		}
	}
	var identity buildid.Identity
	if err := json.Unmarshal(data, &identity); err != nil {
		return buildid.Identity{Reason: "the daemon's version reply could not be decoded: " + err.Error()}
	}
	if !identity.Established && identity.Reason == "" {
		identity.Reason = "the daemon reported no build identity"
	}
	return identity
}

func renderVersion(report buildid.Report, stdout io.Writer) int {
	_, _ = fmt.Fprintf(stdout, "client: %s\n", report.Client)
	if !report.Client.Established {
		_, _ = fmt.Fprintf(stdout, "        %s\n", report.Client.Reason)
	}
	_, _ = fmt.Fprintf(stdout, "daemon: %s\n", report.Daemon)
	if !report.Daemon.Established {
		_, _ = fmt.Fprintf(stdout, "        %s\n", report.Daemon.Reason)
	}
	if report.Diverged {
		// Stated rather than left to the reader: the daemon is the component whose
		// compiled-in domain decides what a mutation is allowed to do, so a client
		// newer than its daemon explains refusals the source says cannot happen.
		_, _ = fmt.Fprintln(stdout, "DIVERGED: the client and daemon were built from different commits. "+
			"Mutating verbs execute in the DAEMON, so its build decides what is legal; run `aira install` and restart it.")
	}
	return 0
}

// isVersionSpelling matches the verb and both flag spellings, case-insensitively.
//
// The fold is not cosmetic: every neighbouring dispatch in main.go lowercases
// (`verb := strings.ToLower(args[0])`), so without it `aira VERSION` would be
// the one spelling in the family reported as an unknown verb.
func isVersionSpelling(arg string) bool {
	switch strings.ToLower(arg) {
	case "version", "--version", "-v":
		return true
	}
	return false
}
