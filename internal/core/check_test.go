package core

import (
	"context"
	"path/filepath"
	"testing"
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
	if unevaluated.Code != "UNEVALUATED" || unevaluated.Exit != 3 {
		t.Fatalf("unevaluated check = %#v", unevaluated)
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
