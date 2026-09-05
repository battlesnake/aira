package core

import (
	"context"
	"path/filepath"
	"testing"

	"aira/internal/codes"
	"aira/internal/store"
)

func TestCheckExitCodesForPassFailUnevaluatedAndStoreError(t *testing.T) {
	passStore := coreTestStore(t)
	if response := New(passStore).Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": "healthy"}}); !response.OK {
		t.Fatalf("create healthy ticket: %#v", response)
	}
	pass := New(passStore).Do(context.Background(), Request{Verb: "check"})
	if pass.Code != "UNEVALUATED" || pass.Exit != 3 {
		t.Fatalf("empty requirement registry check = %#v", pass)
	}
	var passReport store.CheckReport
	marshalRoundTrip(t, pass.Data, &passReport)
	if passReport.Dimensions["relation-integrity"] != "pass" {
		t.Fatalf("relation dimension = %#v", passReport.Dimensions)
	}

	failStore := coreTestStore(t)
	if _, err := failStore.AllocateID(context.Background(), "AIRA"); err != nil {
		t.Fatal(err)
	}
	fail := New(failStore).Do(context.Background(), Request{Verb: "check"})
	if fail.Code != "UNEVALUATED" || fail.Exit != 1 {
		t.Fatalf("fail check = %#v", fail)
	}

	orphanStore := coreTestStore(t)
	if err := orphanStore.RegisterWorktree(context.Background(), "gone", filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatal(err)
	}
	unevaluated := New(orphanStore).Do(context.Background(), Request{Verb: "check"})
	if unevaluated.Code != "UNEVALUATED" || unevaluated.Exit != 3 {
		t.Fatalf("orphan check = %#v", unevaluated)
	}
	var orphanReport store.CheckReport
	marshalRoundTrip(t, unevaluated.Data, &orphanReport)
	if len(orphanReport.Warnings) != 1 || orphanReport.Warnings[0].Code != "W_ORPHAN_WORKTREE" {
		t.Fatalf("orphan warning = %#v", orphanReport.Warnings)
	}
	if orphanReport.Dimensions["orphan-worktree"] != "warning" {
		t.Fatalf("orphan dimension = %#v", orphanReport.Dimensions)
	}

	errorStore := coreTestStore(t)
	if err := errorStore.Close(); err != nil {
		t.Fatal(err)
	}
	storeError := New(errorStore).Do(context.Background(), Request{Verb: "check"})
	if storeError.Exit != 4 || storeError.OK || storeError.Code == "PASS" {
		t.Fatalf("store-error check = %#v", storeError)
	}
}

func TestCancelledCheckProducesRuntimeUnevaluatedExitThree(t *testing.T) {
	s := coreTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := New(s).Do(ctx, Request{Verb: "check"})
	if response.Code != "UNEVALUATED" || response.Exit != 3 || !response.OK {
		t.Fatalf("cancelled check = %#v", response)
	}
	var report store.CheckReport
	marshalRoundTrip(t, response.Data, &report)
	if !report.Unevaluated || report.Dimensions["relation-integrity"] != "unevaluated" {
		t.Fatalf("runtime unevaluated report = %#v", report)
	}
	for _, dimension := range []string{
		"allocated-id-file", "duplicate-id", "stale-index", "orphan-worktree",
		"ticket-file-integrity", "reconcile-integrity", "rebuild-integrity",
	} {
		if report.Dimensions[dimension] != "unevaluated" {
			t.Fatalf("cancelled dimension %q = %q, want unevaluated", dimension, report.Dimensions[dimension])
		}
	}
}

func TestAlreadyInitializedUsesInvalidInvocationExitCode(t *testing.T) {
	if got := codes.ExitForCode("E_ALREADY_INITIALIZED"); got != 2 {
		t.Fatalf("already initialized exit=%d, want 2", got)
	}
}
