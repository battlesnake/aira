package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func d2Store(t *testing.T) *Store {
	t.Helper()
	base := persistentTemp(t, "d2-journal")
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return openTestStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"), "main", "AIRA")
}

func insertDeferredEvent(t *testing.T, s *Store, seq int64) eventRecord {
	t.Helper()
	event := eventRecord{
		ProjectID: s.projectID, Seq: seq, At: "2026-08-17T12:00:00Z", Actor: "aira-daemon",
		Verb: "lease.lapse", Target: "AIRA-" + strconv.FormatInt(seq, 10),
		PayloadDigest: digestBytes([]byte("event-" + strconv.FormatInt(seq, 10))),
	}
	if _, err := s.db.Exec(`INSERT INTO events(project_id,seq,at_wall,actor,verb,target,payload_digest,journaled)
		VALUES(?,?,?,?,?,?,?,0)`, event.ProjectID, event.Seq, event.At, event.Actor, event.Verb, event.Target, event.PayloadDigest); err != nil {
		t.Fatal(err)
	}
	insertDeferredOutbox(t, s, seq)
	return event
}

func insertDeferredOutbox(t *testing.T, s *Store, seq int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,
		intended_digest,intended_bytes,materialised,journaled) VALUES(?,?,'','','lease.lapse','','',NULL,1,0)`, s.projectID, seq); err != nil {
		t.Fatal(err)
	}
}

func outboxJournaled(t *testing.T, s *Store, seq int64) int {
	t.Helper()
	var journaled int
	if err := s.db.QueryRow(`SELECT journaled FROM outbox WHERE project_id=? AND seq=?`, s.projectID, seq).Scan(&journaled); err != nil {
		t.Fatal(err)
	}
	return journaled
}

func TestFlushDeferredJournalSnapshotsSingleConnectionAndDeduplicates(t *testing.T) {
	s := d2Store(t)
	insertDeferredEvent(t, s, 101)
	insertDeferredEvent(t, s, 102)
	done := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		count, err := s.FlushDeferredJournal(context.Background())
		done <- struct {
			count int
			err   error
		}{count, err}
	}()
	select {
	case result := <-done:
		if result.err != nil || result.count != 2 {
			t.Fatalf("flush count=%d err=%v", result.count, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("flush deadlocked with MaxOpenConns(1); outbox rows were not closed before journaling")
	}
	count, err := s.FlushDeferredJournal(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("idempotent flush count=%d err=%v", count, err)
	}
	data, err := os.ReadFile(filepath.Join(s.auditDir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{101, 102} {
		if got := strings.Count(string(data), `"seq":`+strconv.FormatInt(seq, 10)); got != 1 {
			t.Fatalf("journal occurrences for seq %d=%d, want exactly one", seq, got)
		}
		if outboxJournaled(t, s, seq) != 1 {
			t.Fatalf("seq %d was not marked journaled", seq)
		}
	}
	journal, err := readJournal(filepath.Join(s.auditDir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(journal) != 2 {
		t.Fatalf("journal records=%d, want exactly 2", len(journal))
	}
}

func TestFlushDeferredJournalBoundedLockDoesNotFakeSuccess(t *testing.T) {
	s := d2Store(t)
	insertDeferredEvent(t, s, 110)
	lock, err := acquireLock(filepath.Join(s.auditDir, "journal.lock"))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	count, err := s.FlushDeferredJournal(context.Background())
	if err == nil || count != 0 {
		t.Fatalf("bounded flush count=%d err=%v", count, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("bounded lock took %v, want below 3s", elapsed)
	}
	if outboxJournaled(t, s, 110) != 0 {
		t.Fatal("lock timeout falsely marked row journaled")
	}
	unlockFile(lock)
	count, err = s.FlushDeferredJournal(context.Background())
	if err != nil || count != 1 || outboxJournaled(t, s, 110) != 1 {
		t.Fatalf("retry after released lock count=%d err=%v", count, err)
	}
}

func TestFlushDeferredJournalKeyConflictSkipsToLaterSequence(t *testing.T) {
	s := d2Store(t)
	first := insertDeferredEvent(t, s, 120)
	insertDeferredEvent(t, s, 121)
	conflict := first
	conflict.Actor = "mallory"
	writeJSONL(t, filepath.Join(s.auditDir, "journal.jsonl"), conflict)
	count, err := s.FlushDeferredJournal(context.Background())
	if count != 1 || !errors.Is(err, errJournalKeyConflict) {
		t.Fatalf("flush count=%d err=%v, want one plus key-conflict poison", count, err)
	}
	if outboxJournaled(t, s, 120) != 0 || outboxJournaled(t, s, 121) != 1 {
		t.Fatal("key poison did not preserve its row and advance later rows")
	}
}

func TestFlushDeferredJournalMissingEventSkipsToLaterSequence(t *testing.T) {
	s := d2Store(t)
	insertDeferredOutbox(t, s, 130)
	insertDeferredEvent(t, s, 131)
	count, err := s.FlushDeferredJournal(context.Background())
	if count != 1 || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("flush count=%d err=%v, want one plus missing-event poison", count, err)
	}
	if outboxJournaled(t, s, 130) != 0 || outboxJournaled(t, s, 131) != 1 {
		t.Fatal("missing event stranded a later sequence or was falsely marked")
	}
}

func TestFlushDeferredJournalMalformedJournalAbortsPass(t *testing.T) {
	s := d2Store(t)
	insertDeferredEvent(t, s, 140)
	insertDeferredEvent(t, s, 141)
	if err := os.WriteFile(filepath.Join(s.auditDir, "journal.jsonl"), []byte("{not-json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	count, err := s.FlushDeferredJournal(context.Background())
	if count != 0 || !errors.Is(err, errJournalMalformed) {
		t.Fatalf("flush count=%d err=%v, want malformed global abort", count, err)
	}
	if outboxJournaled(t, s, 140) != 0 || outboxJournaled(t, s, 141) != 0 {
		t.Fatal("malformed global error marked an outbox row")
	}
}

func TestFlushDeferredJournalLaterGlobalOverridesEarlierPoison(t *testing.T) {
	s := d2Store(t)
	insertDeferredOutbox(t, s, 150)
	insertDeferredEvent(t, s, 151)
	planted := errors.New("planted directory sync failure")
	old := beforeDirSync
	beforeDirSync = func(f *os.File) error {
		if filepath.Clean(f.Name()) == filepath.Clean(s.auditDir) {
			return planted
		}
		return nil
	}
	defer func() { beforeDirSync = old }()
	count, err := s.FlushDeferredJournal(context.Background())
	if count != 0 || !errors.Is(err, planted) || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("flush count=%d err=%v, want later global error itself", count, err)
	}
	if outboxJournaled(t, s, 150) != 0 || outboxJournaled(t, s, 151) != 0 {
		t.Fatal("global failure falsely marked an outbox row")
	}
}

func TestFlushDeferredJournalCancellationBetweenEvents(t *testing.T) {
	s := d2Store(t)
	insertDeferredEvent(t, s, 160)
	insertDeferredEvent(t, s, 161)
	ctx, cancel := context.WithCancel(context.Background())
	s.afterJournalFlushEvent = func(EventKey) { cancel() }
	count, err := s.FlushDeferredJournal(ctx)
	if count != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("flush count=%d err=%v, want one-event prefix and cancellation", count, err)
	}
	if outboxJournaled(t, s, 160) != 1 || outboxJournaled(t, s, 161) != 0 {
		t.Fatal("cancellation did not leave an honestly journaled prefix")
	}
}

func TestAppendEventDedupSyncsFileAfterImmediateParent(t *testing.T) {
	base := persistentTemp(t, "d2-dedup-sync")
	path := filepath.Join(base, "common", "aira", "journal.jsonl")
	event := eventRecord{ProjectID: "p", Seq: 1, At: "at", Actor: "actor", Verb: "verb", Target: "target", PayloadDigest: "digest"}
	writeJSONL(t, path, event)
	dirSynced := false
	fileSynced := false
	oldDir, oldFile := beforeDirSync, beforeFileSync
	beforeDirSync = func(f *os.File) error {
		if filepath.Clean(f.Name()) == filepath.Clean(filepath.Dir(path)) {
			dirSynced = true
		}
		return nil
	}
	beforeFileSync = func(f *os.File) error {
		if filepath.Clean(f.Name()) == filepath.Clean(path) {
			if !dirSynced {
				t.Fatal("file sync preceded immediate-parent sync")
			}
			fileSynced = true
		}
		return nil
	}
	defer func() { beforeDirSync, beforeFileSync = oldDir, oldFile }()
	if err := appendEventIfMissing(path, event, path+".lock"); err != nil {
		t.Fatal(err)
	}
	if !dirSynced || !fileSynced {
		t.Fatalf("dedup durability dirSync=%v fileSync=%v", dirSynced, fileSynced)
	}
}

func TestFlushDeferredJournalFileSyncFailureIsRetryableWithoutFakeMark(t *testing.T) {
	s := d2Store(t)
	insertDeferredEvent(t, s, 170)
	planted := errors.New("planted file sync failure")
	old := beforeFileSync
	beforeFileSync = func(f *os.File) error {
		if filepath.Clean(f.Name()) == filepath.Clean(filepath.Join(s.auditDir, "journal.jsonl")) {
			return planted
		}
		return nil
	}
	count, err := s.FlushDeferredJournal(context.Background())
	beforeFileSync = old
	if count != 0 || !errors.Is(err, planted) {
		t.Fatalf("fsync failure count=%d err=%v", count, err)
	}
	if outboxJournaled(t, s, 170) != 0 {
		t.Fatal("fsync failure falsely marked row journaled")
	}
	count, err = s.FlushDeferredJournal(context.Background())
	if err != nil || count != 1 || outboxJournaled(t, s, 170) != 1 {
		t.Fatalf("fsync retry count=%d err=%v", count, err)
	}
}

func TestNewScopeSyncsExistingAuditGrandparent(t *testing.T) {
	base := persistentTemp(t, "d2-grandparent-sync")
	root := filepath.Join(base, "root")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(common, "aira", "locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	seen := false
	old := beforeDirSync
	beforeDirSync = func(f *os.File) error {
		if filepath.Clean(f.Name()) == filepath.Clean(common) {
			seen = true
		}
		return nil
	}
	defer func() { beforeDirSync = old }()
	_ = openTestStore(t, root, common, filepath.Join(base, "state"), "main", "AIRA")
	if !seen {
		t.Fatal("scope open did not sync common after provisioning pre-existing common/aira")
	}
}

func TestAppendReceiptSyncsImmediateParentForExistingFile(t *testing.T) {
	s := d2Store(t)
	receipt := AllocationReceipt{
		ProjectID: s.projectID, WorktreeID: s.worktreeID, ID: "AIRA-88",
		Path: "tickets/AIRA-88.md", Seq: 188, At: "2026-08-17T12:00:00Z", State: "allocated",
	}
	path := filepath.Join(s.auditDir, "receipts.jsonl")
	writeJSONL(t, path, receipt)
	seen := false
	old := beforeDirSync
	beforeDirSync = func(f *os.File) error {
		if filepath.Clean(f.Name()) == filepath.Clean(s.auditDir) {
			seen = true
		}
		return nil
	}
	defer func() { beforeDirSync = old }()
	if err := s.appendReceiptIfMissing(receipt); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("receipt dedup did not sync its immediate parent")
	}
}

func TestAppendEventTypedIdentityConflictAndFullFileScan(t *testing.T) {
	base := persistentTemp(t, "d2-full-scan")
	path := filepath.Join(base, "journal.jsonl")
	event := eventRecord{ProjectID: "p", Seq: 7, At: "at", Actor: "actor", Verb: "verb", Target: "target", PayloadDigest: "digest"}
	conflict := event
	conflict.At = "other-at"
	writeJSONL(t, path, event, conflict)
	err := appendEventIfMissing(path, event, path+".lock")
	if !errors.Is(err, errJournalKeyConflict) || !strings.HasPrefix(err.Error(), "E_JOURNAL_CORRUPT:") {
		t.Fatalf("conflicting duplicate after match err=%v", err)
	}
	writeJSONL(t, path, eventRecord{ProjectID: event.ProjectID, Seq: event.Seq, At: event.At, Actor: "other", Verb: event.Verb, Target: event.Target, PayloadDigest: event.PayloadDigest})
	err = appendEventIfMissing(path, event, path+".lock")
	if !errors.Is(err, errJournalKeyConflict) {
		t.Fatalf("actor identity mismatch err=%v, want typed key conflict", err)
	}
}
