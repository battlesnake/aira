package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func watchTestStore(t *testing.T, base string) *Store {
	t.Helper()
	root := filepath.Join(base, "root")
	common := filepath.Join(base, "common")
	for _, path := range []string{root, common} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return testStore(t, root, common, filepath.Join(base, "state"))
}

func appendWatchEvent(t *testing.T, s *Store, actor, verb, target string) int64 {
	t.Helper()
	var seq int64
	err := s.withImmediate(context.Background(), func(conn *sql.Conn) error {
		var err error
		seq, err = nextSequence(context.Background(), conn, s.projectID)
		if err != nil {
			return err
		}
		return insertEventActor(context.Background(), conn, s.projectID, seq, actor, verb, target)
	})
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestEventsSinceAndCurrentMaxSeq(t *testing.T) {
	base := t.TempDir()
	s := watchTestStore(t, base)
	appendWatchEvent(t, s, "one", "excluded", "AIRA-1")
	appendWatchEvent(t, s, "two", "wanted", "AIRA-2")
	appendWatchEvent(t, s, "three", "wanted", "AIRA-3")

	events, next, err := s.EventsSince(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if next != 2 || len(events) != 1 || events[0].Seq != 2 || events[0].Actor != "two" || events[0].Verb != "wanted" || events[0].Target != "AIRA-2" {
		t.Fatalf("events=%+v next=%d", events, next)
	}
	max, err := s.CurrentMaxSeq(context.Background())
	if err != nil || max != 3 {
		t.Fatalf("max=%d err=%v", max, err)
	}

	emptyBase := t.TempDir()
	empty := watchTestStore(t, emptyBase)
	if max, err := empty.CurrentMaxSeq(context.Background()); err != nil || max != 0 {
		t.Fatalf("empty max=%d err=%v", max, err)
	}
	if events, next, err := empty.EventsSince(context.Background(), 9, 10); err != nil || len(events) != 0 || next != 9 {
		t.Fatalf("empty events=%+v next=%d err=%v", events, next, err)
	}
}

func TestEventsSinceBacklogDrainsContiguously(t *testing.T) {
	base := t.TempDir()
	s := watchTestStore(t, base)
	for index := 0; index < 23; index++ {
		appendWatchEvent(t, s, "aira", "ticket.change", "AIRA-1")
	}
	var got []int64
	cursor := int64(0)
	for {
		events, next, err := s.EventsSince(context.Background(), cursor, 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 0 {
			if next != cursor {
				t.Fatalf("empty scan advanced %d -> %d", cursor, next)
			}
			break
		}
		if next <= cursor {
			t.Fatalf("cursor did not advance %d -> %d", cursor, next)
		}
		for _, event := range events {
			got = append(got, event.Seq)
		}
		cursor = next
	}
	want := make([]int64, 23)
	for index := range want {
		want[index] = int64(index + 1)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sequences=%v want=%v", got, want)
	}
}

func TestEventSequenceCommitOrderAcrossIndependentHandles(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "state", "state.db")
	registry := filepath.Join(base, "state", "registry.jsonl")
	firstDB, err := OpenDB(dbPath, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer firstDB.Close()
	secondDB, err := OpenDB(dbPath, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	readerDB, err := OpenDB(dbPath, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer readerDB.Close()
	if _, err := firstDB.db.Exec(`INSERT INTO projects(project_id,slug,common_dir,config_digest,created_at) VALUES ('commit-order','commit-order','/commit-order','','now')`); err != nil {
		t.Fatal(err)
	}

	first := &Store{db: firstDB.db, projectID: "commit-order"}
	second := &Store{db: secondDB.db, projectID: "commit-order"}
	firstAtCommit := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondAtCommit := make(chan struct{})
	releaseSecond := make(chan struct{})
	first.beforeCommit = func() { close(firstAtCommit); <-releaseFirst }
	second.beforeCommit = func() { close(secondAtCommit); <-releaseSecond }

	write := func(s *Store, target string, done chan<- error) {
		done <- s.withImmediate(context.Background(), func(conn *sql.Conn) error {
			seq, err := nextSequence(context.Background(), conn, s.projectID)
			if err != nil {
				return err
			}
			return insertEvent(context.Background(), conn, s.projectID, seq, "test", target)
		})
	}
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go write(first, "N", firstDone)
	<-firstAtCommit
	go write(second, "N+1", secondDone)

	select {
	case <-secondAtCommit:
		t.Fatal("second independent writer passed BEGIN IMMEDIATE before the first committed")
	case <-time.After(100 * time.Millisecond):
	}
	reader := &Store{db: readerDB.db, projectID: "commit-order"}
	if max, err := reader.CurrentMaxSeq(context.Background()); err != nil || max != 0 {
		t.Fatalf("reader observed uncommitted sequence: max=%d err=%v", max, err)
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondAtCommit:
	case <-time.After(2 * time.Second):
		t.Fatal("second writer did not proceed after first commit")
	}
	events, next, err := reader.EventsSince(context.Background(), 0, 10)
	if err != nil || next != 1 || len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("reader between commits events=%+v next=%d err=%v", events, next, err)
	}
	close(releaseSecond)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	events, next, err = reader.EventsSince(context.Background(), 0, 10)
	if err != nil || next != 2 || len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("committed events=%+v next=%d err=%v", events, next, err)
	}
}
