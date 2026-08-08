package core

import (
	"context"
	"path/filepath"
	"testing"

	"aira/internal/store"
)

func TestCheckExitCodesForPassFailUnevaluatedAndStoreError(t *testing.T) {
	passStore := coreTestStore(t)
	if response := New(passStore).Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": "healthy"}}); !response.OK {
		t.Fatalf("create healthy ticket: %#v", response)
	}
	pass := New(passStore).Do(context.Background(), Request{Verb: "check"})
	if pass.Code != "PASS" || pass.Exit != 0 {
		t.Fatalf("pass check = %#v", pass)
	}
	var passReport store.CheckReport
	marshalRoundTrip(t, pass.Data, &passReport)
	if passReport.Dimensions["relation-integrity"] != "unevaluated" {
		t.Fatalf("relation dimension = %#v", passReport.Dimensions)
	}

	failStore := coreTestStore(t)
	if _, err := failStore.AllocateID(context.Background(), "AIRA"); err != nil {
		t.Fatal(err)
	}
	fail := New(failStore).Do(context.Background(), Request{Verb: "check"})
	if fail.Code != "FAIL" || fail.Exit != 1 {
		t.Fatalf("fail check = %#v", fail)
	}

	orphanStore := coreTestStore(t)
	if err := orphanStore.RegisterWorktree(context.Background(), "gone", filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatal(err)
	}
	unevaluated := New(orphanStore).Do(context.Background(), Request{Verb: "check"})
	if unevaluated.Code != "PASS" || unevaluated.Exit != 0 {
		t.Fatalf("orphan check = %#v", unevaluated)
	}
	var orphanReport store.CheckReport
	marshalRoundTrip(t, unevaluated.Data, &orphanReport)
	if len(orphanReport.Warnings) != 1 || orphanReport.Warnings[0].Code != "W_ORPHAN_WORKTREE" {
		t.Fatalf("orphan warning = %#v", orphanReport.Warnings)
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
