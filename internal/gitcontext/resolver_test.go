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

func TestResolveRejectsSymlinkedAndMalformedRefs(t *testing.T) {
	// A loose ref that is itself a symlink must never be followed; the target's
	// contents must not be reported as a real hash.
	t.Run("symlinked-loose-ref", func(t *testing.T) {
		opts := repositoryFixture(t, "ref: refs/heads/main\n", nil, "")
		planted := filepath.Join(t.TempDir(), "planted")
		if err := os.WriteFile(planted, []byte(strings.Repeat("e", 40)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		refPath := filepath.Join(opts.GitDir, "refs", "heads", "main")
		if err := os.MkdirAll(filepath.Dir(refPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(planted, refPath); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		got := NewResolver().Resolve(opts)
		assertField(t, got.HeadHash, unevaluatedField("unreadable"))
	})
	// A symlinked intermediate directory in the ref path must be refused too.
	t.Run("symlinked-intermediate-dir", func(t *testing.T) {
		opts := repositoryFixture(t, "ref: refs/heads/main\n", nil, "")
		planted := t.TempDir()
		if err := os.WriteFile(filepath.Join(planted, "main"), []byte(strings.Repeat("e", 40)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(opts.GitDir, "refs"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(planted, filepath.Join(opts.GitDir, "refs", "heads")); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		got := NewResolver().Resolve(opts)
		assertField(t, got.HeadHash, unevaluatedField("unreadable"))
	})
	// A malformed refname component must be refused rather than steer a read.
	t.Run("malformed-ref-components", func(t *testing.T) {
		for _, ref := range []string{"refs/.hidden/main", "refs/heads/foo.lock/bar", "refs/heads/ma\x01in"} {
			opts := repositoryFixture(t, "ref: "+ref+"\n", nil, "")
			got := NewResolver().Resolve(opts)
			assertField(t, got.HeadHash, unevaluatedField("unusual-ref-storage"))
		}
	})
}

func TestResolveHeadUnevaluatedWhenStorageOrFormatUnknown(t *testing.T) {
	// An unreadable config makes the ref backend unknowable, so HEAD must not be
	// interpreted as a files-backend ref — even though the loose ref exists.
	t.Run("unreadable-config", func(t *testing.T) {
		opts := repositoryFixture(t, "ref: refs/heads/main\n", map[string]string{"refs/heads/main": strings.Repeat("d", 40)}, "")
		resolver := NewResolver()
		resolver.ReadFile = func(path string) ([]byte, error) {
			if filepath.Base(path) == "config" {
				return nil, errors.New("permission denied")
			}
			return readBoundedRegularFile(path)
		}
		got := resolver.Resolve(opts)
		assertField(t, got.HeadHash, unevaluatedField("ref-storage-unknown"))
		assertField(t, got.HeadRef, unevaluatedField("ref-storage-unknown"))
	})
	// An include can silently switch the backend, so HEAD is unevaluated too.
	t.Run("config-include-taints-head", func(t *testing.T) {
		opts := repositoryFixture(t, "ref: refs/heads/main\n", map[string]string{"refs/heads/main": strings.Repeat("d", 40)}, "")
		if err := os.WriteFile(filepath.Join(opts.CommonDir, "config"), []byte("[include]\n path = ../x.cfg\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := NewResolver().Resolve(opts)
		assertField(t, got.HeadHash, unevaluatedField("ref-storage-unknown"))
		assertField(t, got.HeadRef, unevaluatedField("ref-storage-unknown"))
	})
	// The configured object format bounds the accepted hash width both ways.
	t.Run("sha256-enforces-width", func(t *testing.T) {
		narrow := repositoryFixture(t, strings.Repeat("a", 40)+"\n", nil, "")
		if err := os.WriteFile(filepath.Join(narrow.CommonDir, "config"), []byte("[extensions]\n objectFormat = sha256\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertField(t, NewResolver().Resolve(narrow).HeadHash, unevaluatedField("unusual-head"))

		wide := repositoryFixture(t, strings.Repeat("a", 64)+"\n", nil, "")
		if err := os.WriteFile(filepath.Join(wide.CommonDir, "config"), []byte("[extensions]\n objectFormat = sha256\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertField(t, NewResolver().Resolve(wide).HeadHash, valueField(strings.Repeat("a", 64)))
	})
	t.Run("sha1-rejects-sha256-width", func(t *testing.T) {
		opts := repositoryFixture(t, strings.Repeat("a", 64)+"\n", nil, "")
		if err := os.WriteFile(filepath.Join(opts.CommonDir, "config"), []byte("[core]\n bare = false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertField(t, NewResolver().Resolve(opts).HeadHash, unevaluatedField("unusual-head"))
	})
	t.Run("unknown-object-format", func(t *testing.T) {
		opts := repositoryFixture(t, strings.Repeat("a", 40)+"\n", nil, "")
		if err := os.WriteFile(filepath.Join(opts.CommonDir, "config"), []byte("[extensions]\n objectFormat = sha512\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertField(t, NewResolver().Resolve(opts).HeadHash, unevaluatedField("unknown-object-format"))
	})
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
