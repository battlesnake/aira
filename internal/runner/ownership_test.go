//go:build linux

package runner

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type ownershipSpyScope struct {
	mu          sync.Mutex
	members     []int
	terminates  int
	kills       int
	removes     int
	signalGroup bool
	lastMembers []int
}

func (s *ownershipSpyScope) Reference() string { return "/ownership-spy" }
func (s *ownershipSpyScope) FD() int           { return -1 }
func (s *ownershipSpyScope) Members() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.members...), nil
}
func (s *ownershipSpyScope) Empty() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.members) == 0, nil
}
func (s *ownershipSpyScope) Terminate(pids []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminates++
	s.lastMembers = append([]int(nil), pids...)
	if s.signalGroup {
		for _, pid := range pids {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
	}
	s.members = nil
	return nil
}
func (s *ownershipSpyScope) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kills++
	if s.signalGroup {
		for _, pid := range s.lastMembers {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
	s.members = nil
	return nil
}
func (s *ownershipSpyScope) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removes++
	return nil
}

type ownershipSpyBackend struct {
	mu                     sync.Mutex
	scope                  *ownershipSpyScope
	opens                  int
	onOpen                 func()
	unavailableAfterRemove bool
}

func (b *ownershipSpyBackend) Probe(context.Context) error { return nil }
func (b *ownershipSpyBackend) Create(context.Context, string) (Scope, error) {
	return b.scope, nil
}
func (b *ownershipSpyBackend) Open(context.Context, string) (Scope, error) {
	b.mu.Lock()
	b.opens++
	onOpen := b.onOpen
	unavailableAfterRemove := b.unavailableAfterRemove
	b.mu.Unlock()
	if unavailableAfterRemove {
		b.scope.mu.Lock()
		removed := b.scope.removes > 0
		b.scope.mu.Unlock()
		if removed {
			return nil, errors.New("scope was removed")
		}
	}
	if onOpen != nil {
		onOpen()
	}
	return b.scope, nil
}

func (b *ownershipSpyBackend) counts() (open, terminate, kill, remove int) {
	b.mu.Lock()
	open = b.opens
	b.mu.Unlock()
	b.scope.mu.Lock()
	defer b.scope.mu.Unlock()
	return open, b.scope.terminates, b.scope.kills, b.scope.removes
}

func newOwnershipRunners(t *testing.T, members []int) (*Runner, *Runner, *ownershipSpyBackend) {
	t.Helper()
	common := t.TempDir()
	backend := &ownershipSpyBackend{scope: &ownershipSpyScope{members: append([]int(nil), members...)}}
	a, err := New(Config{CommonDir: common, Backend: backend, Owner: "A", TermGrace: time.Millisecond, Grace: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Config{CommonDir: common, Backend: backend, Owner: "B", TermGrace: time.Millisecond, Grace: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return a, b, backend
}

func appendOwnedActiveRun(t *testing.T, r *Runner, id, owner string) RunRecord {
	t.Helper()
	record := RunRecord{SchemaVersion: ledgerSchema, ID: id, Owner: owner, Status: StatusRunning, ScopeIntegrity: ScopeContained, CgroupScope: "/ownership-spy"}
	appendRunEvent(t, r, "starting", record)
	appendRunEvent(t, r, "scope-created", record)
	appendRunEvent(t, r, "running", record)
	return record
}

func assertRunEventsOwner(t *testing.T, r *Runner, id, owner string) {
	t.Helper()
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Run.ID != id {
			continue
		}
		found = true
		if event.Run.Owner != owner {
			t.Fatalf("%s event owner=%q, want %q: %+v", event.Kind, event.Run.Owner, owner, event.Run)
		}
	}
	if !found {
		t.Fatalf("no events for %s", id)
	}
}

func TestLaunchStampsOwnerBeforeStartingAndFailureEventsKeepIt(t *testing.T) {
	backend := &ownershipSpyBackend{scope: &ownershipSpyScope{}}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: backend, Owner: "A", TermGrace: time.Millisecond, Grace: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r.startFn = func(*exec.Cmd) error { return errors.New("injected launch failure") }
	if _, err := r.Launch(context.Background(), Request{Argv: []string{"ignored"}}); err == nil {
		t.Fatal("injected launch failure unexpectedly succeeded")
	}
	assertRunEventsOwner(t, r, "RUN-1", "A")
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Kind != "starting" || events[0].Run.Owner != "A" {
		t.Fatalf("first event=%+v", events)
	}
}

func TestLaunchOwnerSurvivesNormalExitAndTimeoutEvents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request Request
	}{
		{name: "normal exit", request: Request{Argv: []string{"/bin/sh", "-c", "sleep 0.03"}}},
		{name: "timeout", request: Request{Argv: []string{"/bin/sh", "-c", "sleep 30"}, Timeout: 10 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &ownershipSpyBackend{scope: &ownershipSpyScope{signalGroup: true}}
			r, err := New(Config{CommonDir: t.TempDir(), Backend: backend, Owner: "A", TermGrace: time.Millisecond, Grace: 20 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			r.startFn = func(cmd *exec.Cmd) error {
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if err := cmd.Start(); err != nil {
					return err
				}
				backend.scope.mu.Lock()
				backend.scope.members = []int{cmd.Process.Pid}
				backend.scope.mu.Unlock()
				return nil
			}
			record, err := r.Launch(context.Background(), tc.request)
			if err != nil {
				t.Fatal(err)
			}
			if !record.Status.Terminal() || record.Owner != "A" {
				t.Fatalf("terminal owner record=%+v", record)
			}
			assertRunEventsOwner(t, r, record.ID, "A")
		})
	}
}

func TestForeignKillRefusalIsAtomicAndNonDestructive(t *testing.T) {
	a, b, spy := newOwnershipRunners(t, []int{101})
	appendOwnedActiveRun(t, a, "RUN-1", "A")
	before, err := b.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := b.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	returned, err := b.Kill(context.Background(), "RUN-1", false)
	var foreign *ForeignOwnerError
	if !errors.As(err, &foreign) || foreign.RunID != "RUN-1" || foreign.Owner != "A" || foreign.CallerOwner != "B" {
		t.Fatalf("foreign refusal record=%+v err=%v", returned, err)
	}
	after, err := b.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	eventsAfter, err := b.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*before, *after) || !reflect.DeepEqual(*before, *returned) || len(eventsBefore) != len(eventsAfter) || !reflect.DeepEqual(eventsBefore, eventsAfter) {
		t.Fatalf("refusal mutated durable state\nbefore=%+v\nafter=%+v\nevents before=%d after=%d", before, after, len(eventsBefore), len(eventsAfter))
	}
	if open, terminate, kill, remove := spy.counts(); open != 0 || terminate != 0 || kill != 0 || remove != 0 {
		t.Fatalf("refusal touched backend open=%d terminate=%d kill=%d remove=%d", open, terminate, kill, remove)
	}
}

func TestStealIsDurableBeforeScopeForFreshIntent(t *testing.T) {
	a, b, spy := newOwnershipRunners(t, []int{102})
	appendOwnedActiveRun(t, a, "RUN-1", "A")
	spy.onOpen = func() {
		current, err := b.Get("RUN-1")
		if err != nil {
			t.Fatal(err)
		}
		if !current.KillIntent.Present || current.KillIntent.Sequence == 0 || current.StolenBy != "B" {
			t.Fatalf("scope opened before durable intent+steal evidence: %+v", current)
		}
	}
	record, err := b.Kill(context.Background(), "RUN-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusKilled || record.StolenBy != "B" || !containsString(record.ErrorCodes, "E_RUN_KILLED") {
		t.Fatalf("steal result=%+v", record)
	}
	assertRunEventsOwner(t, b, "RUN-1", "A")
	events, err := b.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "kill-intent" && event.Run.ID == "RUN-1" {
			if event.Run.StolenBy != "B" || event.Run.KillIntent.Sequence == 0 {
				t.Fatalf("fresh steal event=%+v", event)
			}
			return
		}
	}
	t.Fatal("fresh steal did not use the durable kill-intent event")
}

func TestExistingIntentStealSurvivesCrashAndReconcile(t *testing.T) {
	a, b, spy := newOwnershipRunners(t, []int{103})
	spy.unavailableAfterRemove = true
	record := appendOwnedActiveRun(t, a, "RUN-1", "A")
	record.KillIntent = KillIntent{Present: true}
	record.ScopeKill.Requested = true
	event, err := a.append(ledgerEvent{Kind: "kill-intent", Run: record})
	if err != nil {
		t.Fatal(err)
	}
	intentSequence := event.Run.KillIntent.Sequence
	spy.onOpen = func() {
		current, err := b.Get("RUN-1")
		if err != nil {
			t.Fatal(err)
		}
		if current.StolenBy != "B" || current.KillIntent.Sequence != intentSequence {
			t.Fatalf("scope opened before sequence-preserving steal evidence: %+v", current)
		}
	}
	attempt, err := b.killWithIntent(context.Background(), "RUN-1", "run-kill", killPolicy{Steal: true, CallerOwner: "B"})
	if err != nil || !attempt.Kill.Completed {
		t.Fatalf("pre-crash steal attempt=%+v err=%v", attempt, err)
	}
	_, terminatesBefore, killsBefore, removesBefore := spy.counts()
	// Simulate a crash before Kill publishes terminal state: construct a fresh
	// runner over the same durable ledger and let recovery arbitrate it.
	fresh, err := New(Config{CommonDir: a.ledger.root, Backend: spy, Owner: "A", TermGrace: time.Millisecond, Grace: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	results, err := fresh.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Status.Terminal() || results[0].StolenBy != "B" || results[0].KillIntent.Sequence != intentSequence {
		t.Fatalf("reconcile lost steal evidence: %+v", results)
	}
	if _, terminatesAfter, killsAfter, removesAfter := spy.counts(); terminatesAfter != terminatesBefore || killsAfter != killsBefore || removesAfter != removesBefore {
		t.Fatalf("reconcile repeated physical kill: terminate %d→%d kill %d→%d remove %d→%d", terminatesBefore, terminatesAfter, killsBefore, killsAfter, removesBefore, removesAfter)
	}
	assertRunEventsOwner(t, fresh, "RUN-1", "A")
	events, err := fresh.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	foundSteal := false
	for _, durable := range events {
		if durable.Kind == "kill-steal" {
			foundSteal = true
			if durable.Run.StolenBy != "B" || durable.Run.KillIntent.Sequence != intentSequence {
				t.Fatalf("sequence-regressing steal event=%+v", durable)
			}
		}
	}
	if !foundSteal {
		t.Fatal("existing intent did not append a dedicated durable steal event")
	}
}

func TestOwnershipEvidenceMergeIsMonotonic(t *testing.T) {
	base := RunRecord{ID: "RUN-1", Owner: "A", StolenBy: "B"}
	got := mergeEvidence(base, RunRecord{ID: "RUN-1"})
	if got.Owner != "A" || got.StolenBy != "B" {
		t.Fatalf("empty candidate cleared ownership evidence: %+v", got)
	}
}

func TestOwnerlessStealPreservesExistingStolenBy(t *testing.T) {
	for _, existingIntent := range []bool{false, true} {
		name := "fresh intent"
		if existingIntent {
			name = "existing intent"
		}
		t.Run(name, func(t *testing.T) {
			a, _, spy := newOwnershipRunners(t, []int{109})
			record := appendOwnedActiveRun(t, a, "RUN-1", "A")
			if existingIntent {
				record.KillIntent = KillIntent{Present: true}
				record.ScopeKill.Requested = true
				event, err := a.append(ledgerEvent{Kind: "kill-intent", Run: record})
				if err != nil {
					t.Fatal(err)
				}
				record = event.Run
			}
			record.StolenBy = "B"
			if _, err := a.append(ledgerEvent{Kind: "kill-steal", Run: record}); err != nil {
				t.Fatal(err)
			}
			ownerless, err := New(Config{CommonDir: a.ledger.root, Backend: spy, TermGrace: time.Millisecond, Grace: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			killed, err := ownerless.Kill(context.Background(), "RUN-1", true)
			if err != nil {
				t.Fatal(err)
			}
			if killed.StolenBy != "B" {
				t.Fatalf("ownerless steal cleared evidence after kill: %+v", killed)
			}
			fresh, err := New(Config{CommonDir: a.ledger.root, Backend: spy, Owner: "A", TermGrace: time.Millisecond, Grace: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			results, err := fresh.Reconcile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].StolenBy != "B" {
				t.Fatalf("ownerless steal evidence did not survive reconcile: %+v", results)
			}
		})
	}
}

func TestOwnershipGuardFailOpenAndLifecyclePolicies(t *testing.T) {
	t.Run("legacy owner", func(t *testing.T) {
		a, b, _ := newOwnershipRunners(t, []int{104})
		appendOwnedActiveRun(t, a, "RUN-1", "")
		if _, err := b.Kill(context.Background(), "RUN-1", false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("empty caller owner", func(t *testing.T) {
		a, _, spy := newOwnershipRunners(t, []int{105})
		appendOwnedActiveRun(t, a, "RUN-1", "A")
		caller, err := New(Config{CommonDir: a.ledger.root, Backend: spy, TermGrace: time.Millisecond, Grace: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := caller.Kill(context.Background(), "RUN-1", false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("same owner", func(t *testing.T) {
		a, _, _ := newOwnershipRunners(t, []int{106})
		appendOwnedActiveRun(t, a, "RUN-1", "A")
		if _, err := a.Kill(context.Background(), "RUN-1", false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("timeout bypasses foreign owner", func(t *testing.T) {
		a, b, spy := newOwnershipRunners(t, []int{107})
		appendOwnedActiveRun(t, a, "RUN-1", "A")
		attempt, err := b.killWithIntent(context.Background(), "RUN-1", "run-timeout", killPolicy{Enforce: false, CallerOwner: "B"})
		if err != nil || !attempt.Kill.Completed {
			t.Fatalf("timeout attempt=%+v err=%v", attempt, err)
		}
		if open, terminate, kill, _ := spy.counts(); open == 0 || terminate == 0 || kill == 0 {
			t.Fatalf("timeout did not execute scope action open=%d terminate=%d kill=%d", open, terminate, kill)
		}
	})
	t.Run("terminal foreign is idempotent", func(t *testing.T) {
		a, b, spy := newOwnershipRunners(t, nil)
		record := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Owner: "A", Status: StatusExited, ScopeIntegrity: ScopeContained, TerminalComplete: true}
		appendRunEvent(t, a, "terminal", record)
		want, err := a.Get("RUN-1")
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.Kill(context.Background(), "RUN-1", false)
		if err != nil || !reflect.DeepEqual(*got, *want) {
			t.Fatalf("terminal result=%+v err=%v", got, err)
		}
		if open, terminate, kill, remove := spy.counts(); open != 0 || terminate != 0 || kill != 0 || remove != 0 {
			t.Fatalf("terminal kill touched backend %d/%d/%d/%d", open, terminate, kill, remove)
		}
	})
}

func TestUnknownKillDoesNotAppendIntent(t *testing.T) {
	_, b, spy := newOwnershipRunners(t, []int{108})
	if _, err := b.Kill(context.Background(), "RUN-404", false); err == nil || !strings.Contains(err.Error(), "E_RUN_NOT_FOUND") {
		t.Fatalf("unknown kill error=%v", err)
	}
	events, err := b.ledger.read()
	if err != nil || len(events) != 0 {
		t.Fatalf("unknown kill events=%+v err=%v", events, err)
	}
	if open, terminate, kill, remove := spy.counts(); open != 0 || terminate != 0 || kill != 0 || remove != 0 {
		t.Fatalf("unknown kill touched backend %d/%d/%d/%d", open, terminate, kill, remove)
	}
}
