package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// verifies: AIRA-350 §2.7 — the small-job lane reads the same on every face.
// The short keywords are on every face (confine help, install help, MCP
// instructions, skill, guide, the install option's description); the long-only
// keywords on the long faces (skill, guide, the install option's description).
// Mutation: drop either constant from any face, or a keyword from either text
// -> red.
func TestConfineSmallLaneGuidanceIsOnEveryFace(t *testing.T) {
	var confineHelp bytes.Buffer
	if exit := runConfineHelpCommand(false, &confineHelp, io.Discard); exit != 0 {
		t.Fatalf("confine help exit=%d", exit)
	}
	var installHelp bytes.Buffer
	if exit := Run([]string{"help", "install"}, &installHelp, io.Discard); exit != 0 {
		t.Fatalf("install help exit=%d", exit)
	}
	artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	installOption := ""
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name != "install" {
			continue
		}
		for _, arg := range descriptor.Args {
			if arg.Name == "small_lane" {
				installOption = arg.Description
			}
		}
	}
	short := []string{"1G", "8G", "ahead of larger", "ordinary", "larger job at the front"}
	long := append([]string{"small-job lane", "restart", "CI", "--front", "worker", "--delegate-ram"}, short...)
	for _, face := range []struct {
		name, text string
		want       []string
	}{
		{"confine help", confineHelp.String(), short},
		{"install help", installHelp.String(), short},
		{"mcp", mcpWireInstructions(t), short},
		{"skill", string(artifacts.SkillMD), long},
		{"guide", string(artifacts.Guide), long},
		{"install option", installOption, long},
	} {
		flat := strings.ToLower(strings.Join(strings.Fields(face.text), " "))
		for _, want := range face.want {
			if !strings.Contains(flat, strings.ToLower(want)) {
				t.Errorf("%s does not carry the small-job lane guidance %q", face.name, want)
			}
		}
	}
	if !strings.Contains(installHelp.String(), "[--small-lane SIZE|off] [--small-lane-job-max SIZE]") {
		t.Errorf("install usage does not name the lane flags:\n%s", installHelp.String())
	}
}

// verifies: AIRA-350 §2.6 — `confine --list` prints the lane line after `slice
// queue:` only while the lane is on, and marks a small row's reserve. The
// booked figure is unevaluated without an established ledger. Mutations: the
// line printed while off; the small mark dropped.
func TestConfineListSmallLaneLine(t *testing.T) {
	render := func(reserve *runner.ConfineSliceReserve, scopes []runner.ConfineRecord) string {
		result := runner.ConfineListResult{Verdict: "pass", Scopes: scopes, SliceReserve: reserve}
		dispatch := dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, _ core.Request) core.Response {
			return core.Response{OK: true, Code: "OK", Data: result}
		})
		var stdout terminalBuffer
		var stderr bytes.Buffer
		if exit := runWithInputDispatcher([]string{"confine", "--list"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 {
			t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
		}
		return stdout.String()
	}
	on := &runner.ConfineSliceReserve{
		GrantedEstablished: true, GrantedBytes: 10 << 30, CeilingBytes: 40 << 30, Jobs: 7, FreezePhase: "idle",
		LaneCapacityBytes: 8 << 30, LaneJobMaxBytes: 1 << 30, LaneUsedBytes: 3 << 30, LaneJobs: 5,
	}
	small := confineLiveRecord(t, "smalljob", 4242, nil, nil)
	reserve := int64(512 << 20)
	small.ReserveBytes, small.Small = &reserve, true
	out := render(on, []runner.ConfineRecord{small})
	if !strings.Contains(out, "small-job lane: 3.0G of 8G booked by 5 jobs (jobs booking at most 1G go first)\n") {
		t.Fatalf("lane line missing:\n%s", out)
	}
	if queue, lane := strings.Index(out, "slice queue:"), strings.Index(out, "small-job lane:"); queue < 0 || lane < queue {
		t.Fatalf("the lane line must follow `slice queue:`:\n%s", out)
	}
	if !strings.Contains(out, "536870912 small") {
		t.Fatalf("a small row's reserve is not marked:\n%s", out)
	}
	unestablished := *on
	unestablished.GrantedEstablished = false
	if out := render(&unestablished, nil); !strings.Contains(out, "small-job lane: unevaluated of 8G booked (jobs booking at most 1G go first)") {
		t.Fatalf("an unestablished ledger must not print a booked figure:\n%s", out)
	}
	off := *on
	off.LaneCapacityBytes, off.LaneJobMaxBytes, off.LaneUsedBytes, off.LaneJobs = 0, 0, 0, 0
	small.Small = false
	if out := render(&off, []runner.ConfineRecord{small}); strings.Contains(out, "small-job lane") || strings.Contains(out, " small") {
		t.Fatalf("lane off must print no lane line and no small mark:\n%s", out)
	}
}
