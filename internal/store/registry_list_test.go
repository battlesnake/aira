package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestListRegistryEntriesMissingAndEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	entries, err := ListRegistryEntries(missing)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing registry entries=%#v err=%v, want empty nil", entries, err)
	}
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err = ListRegistryEntries(empty)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty registry entries=%#v err=%v, want empty nil", entries, err)
	}
}

func TestListRegistryEntriesReturnsAllExportedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.jsonl")
	data := `{"project_id":"project","common_dir":"/common","worktree_id":"worktree","root":"/root","prefixes":["AIRA"],"requirement_prefixes":["REQ"],"at":"ignored"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ListRegistryEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []RegistryEntry{{
		ProjectID: "project", WorktreeID: "worktree", CommonDir: "/common", Root: "/root",
		Prefixes: []string{"AIRA"}, RequirementPrefixes: []string{"REQ"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries=%#v, want %#v", got, want)
	}
}

func TestListRegistryEntriesToleratesOnlyCrashTornTail(t *testing.T) {
	valid := `{"project_id":"project","common_dir":"/common","worktree_id":"worktree","root":"/root","prefixes":["AIRA"]}`
	tests := []struct {
		name      string
		data      string
		wantCount int
		wantErr   bool
	}{
		{name: "torn final record", data: valid + "\n" + `{"project_id":"crash`, wantCount: 1},
		{name: "valid final record without newline", data: valid, wantCount: 1},
		{name: "malformed newline terminated record", data: valid + "\n" + "not-json\n", wantErr: true},
		{name: "malformed complete record before final tail", data: valid + "\nnot-json\n" + valid, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.jsonl")
			if err := os.WriteFile(path, []byte(test.data), 0o644); err != nil {
				t.Fatal(err)
			}
			entries, err := ListRegistryEntries(path)
			if test.wantErr {
				if err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
					t.Fatalf("entries=%#v err=%v, want E_CONFIG_INVALID", entries, err)
				}
				return
			}
			if err != nil || len(entries) != test.wantCount {
				t.Fatalf("entries=%#v err=%v, want count %d", entries, err, test.wantCount)
			}
		})
	}
}
