package gitcontext

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveHeadFormsAndRemoteRedaction(t *testing.T) {
	tests := []struct {
		name     string
		head     string
		loose    map[string]string
		packed   string
		wantHash Field
		wantRef  Field
	}{
		{name: "detached", head: strings.Repeat("a", 40) + "\n", wantHash: valueField(strings.Repeat("a", 40)), wantRef: noneField("detached")},
		{name: "symbolic-chain", head: "ref: refs/heads/current\n", loose: map[string]string{"refs/heads/current": "ref: refs/heads/main\n", "refs/heads/main": strings.Repeat("b", 40) + "\n"}, wantHash: valueField(strings.Repeat("b", 40)), wantRef: valueField("refs/heads/main")},
		{name: "packed-ref", head: "ref: refs/heads/main\n", packed: strings.Repeat("c", 40) + " refs/heads/main\n", wantHash: valueField(strings.Repeat("c", 40)), wantRef: valueField("refs/heads/main")},
		{name: "unborn", head: "ref: refs/heads/new\n", wantHash: noneField("unborn"), wantRef: valueField("refs/heads/new")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := repositoryFixture(t, tt.head, tt.loose, tt.packed)
			if err := os.WriteFile(filepath.Join(opts.CommonDir, "config"), []byte("[remote \"origin\"]\n  url = https://user:secret@example.test/repo.git?token=x#frag\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got := NewResolver().Resolve(opts)
			assertField(t, got.HeadHash, tt.wantHash)
			assertField(t, got.HeadRef, tt.wantRef)
			assertField(t, got.RemoteURL, valueField("https://example.test/repo.git"))
		})
	}
}

func TestResolveAbsentUnreadableAndReftableAreHonest(t *testing.T) {
	opts := repositoryFixture(t, "ref: refs/heads/main\n", nil, "")
	if err := os.Remove(filepath.Join(opts.GitDir, "HEAD")); err != nil {
		t.Fatal(err)
	}
	got := NewResolver().Resolve(opts)
	assertField(t, got.HeadHash, noneField("absent"))
	assertField(t, got.HeadRef, noneField("absent"))

	opts = repositoryFixture(t, "ref: refs/heads/main\n", nil, "")
	resolver := NewResolver()
	resolver.ReadFile = func(path string) ([]byte, error) {
		if filepath.Base(path) == "HEAD" {
			return nil, errors.New("permission denied")
		}
		return os.ReadFile(path)
	}
	got = resolver.Resolve(opts)
	assertField(t, got.HeadHash, unevaluatedField("unreadable"))
	assertField(t, got.HeadRef, unevaluatedField("unreadable"))

	opts = repositoryFixture(t, "ref: refs/heads/main\n", map[string]string{"refs/heads/main": strings.Repeat("d", 40)}, "")
	if err := os.WriteFile(filepath.Join(opts.CommonDir, "config"), []byte("[extensions]\n refStorage = reftable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = NewResolver().Resolve(opts)
	assertField(t, got.HeadHash, unevaluatedField("reftable"))
	assertField(t, got.HeadRef, unevaluatedField("reftable"))
}

func TestResolveRequiresTwoConsecutiveSnapshots(t *testing.T) {
	opts := repositoryFixture(t, strings.Repeat("a", 40)+"\n", nil, "")
	resolver := NewResolver()
	resolver.MaxAttempts = 5
	headReads := 0
	resolver.ReadFile = func(path string) ([]byte, error) {
		if filepath.Base(path) == "HEAD" {
			headReads++
			switch headReads {
			case 1:
				return []byte(strings.Repeat("a", 40) + "\n"), nil
			case 2, 3:
				return []byte(strings.Repeat("b", 40) + "\n"), nil
			}
		}
		return os.ReadFile(path)
	}
	got := resolver.Resolve(opts)
	assertField(t, got.HeadHash, valueField(strings.Repeat("b", 40)))
	if headReads != 3 {
		t.Fatalf("HEAD reads = %d, want 3 (A, B, B)", headReads)
	}
}

func TestConfigIncludesAreUnevaluatedAndParsingHandlesCaseDuplicatesAndContinuation(t *testing.T) {
	opts := repositoryFixture(t, strings.Repeat("a", 40)+"\n", nil, "")
	config := "[REMOTE \"origin\"]\n URL = https://first.test/old\\\n path\n url = git@example.test:org/repo.git\n"
	if err := os.WriteFile(filepath.Join(opts.CommonDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewResolver().Resolve(opts)
	assertField(t, got.RemoteURL, valueField("git@example.test:org/repo.git"))

	if err := os.WriteFile(filepath.Join(opts.CommonDir, "config"), []byte("[include]\n path = ../secret.cfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = NewResolver().Resolve(opts)
	assertField(t, got.RemoteURL, unevaluatedField("config-include"))

	if err := os.WriteFile(filepath.Join(opts.CommonDir, "config"), []byte("include.path = ../secret.cfg\n[remote \"origin\"]\n url = https://example.test/repo.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = NewResolver().Resolve(opts)
	assertField(t, got.RemoteURL, unevaluatedField("config-include"))
}

func TestRemoteRedactionRemovesSCPCredentials(t *testing.T) {
	opts := repositoryFixture(t, strings.Repeat("a", 40)+"\n", nil, "")
	if err := os.WriteFile(filepath.Join(opts.CommonDir, "config"), []byte("[remote \"origin\"]\n url = user:secret@example.test:org/repo.git?token=x#frag\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewResolver().Resolve(opts)
	assertField(t, got.RemoteURL, valueField("***@example.test:org/repo.git"))
}

func repositoryFixture(t *testing.T, head string, loose map[string]string, packed string) Options {
	t.Helper()
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	if err := os.MkdirAll(common, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(common, "HEAD"), []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, value := range loose {
		path := filepath.Join(common, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if packed != "" {
		if err := os.WriteFile(filepath.Join(common, "packed-refs"), []byte(packed), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Options{RepoRoot: root, WorktreePath: root, CommonDir: common, GitDir: common, WorktreeID: "wt-1", Now: func() time.Time { return time.Unix(123, 0).UTC() }}
}

func valueField(value string) Field        { return Field{Value: value, Status: StatusValue} }
func noneField(reason string) Field        { return Field{Status: StatusNone, Reason: reason} }
func unevaluatedField(reason string) Field { return Field{Status: StatusUnevaluated, Reason: reason} }

func assertField(t *testing.T, got, want Field) {
	t.Helper()
	if got != want {
		t.Fatalf("field = %#v, want %#v", got, want)
	}
}
