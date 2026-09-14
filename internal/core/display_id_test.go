package core

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/store"
)

// AIRA-237 Task 1 — display-side id_prefix strip.

// verifies: stripDisplayIDs rewrites id-bearing fields (incl. nested, relation
// endpoints, and string elements of a strip-key array) while preserving object
// KEY ORDER, numeric SPELLING (no float64 rounding of a >2^53 int), and leaving
// `path` compound.
func TestStripDisplayIDsRewritesFieldsAndPreservesShape(t *testing.T) {
	strip := func(s string) string { return strings.TrimPrefix(s, "FEE-") }

	// A big int beyond float64's exact range: a decode-to-map re-marshal would
	// corrupt it; the token/json.Number path must keep it verbatim.
	in := `{"id":"FEE-BL-1","path":".aira/tickets/FEE-BL-1.md","generation":9007199254740993,` +
		`"ticket":{"id":"FEE-BL-1","title":"x"},"relations":[{"kind":"blocks","from":"FEE-BL-1","to":"FEE-BL-2"}],` +
		`"blocked_by":["FEE-BL-3","FEE-BL-4"],"ready":true,"note":null}`
	got, err := stripDisplayIDs([]byte(in), strip)
	if err != nil {
		t.Fatalf("stripDisplayIDs: %v", err)
	}
	want := `{"id":"BL-1","path":".aira/tickets/FEE-BL-1.md","generation":9007199254740993,` +
		`"ticket":{"id":"BL-1","title":"x"},"relations":[{"kind":"blocks","from":"BL-1","to":"BL-2"}],` +
		`"blocked_by":["BL-3","BL-4"],"ready":true,"note":null}`
	if string(got) != want {
		t.Fatalf("stripDisplayIDs mismatch:\n got=%s\nwant=%s", got, want)
	}

	// Identity strip reproduces the input byte for byte (cross-face invariant).
	identity, err := stripDisplayIDs([]byte(in), func(s string) string { return s })
	if err != nil {
		t.Fatalf("identity strip: %v", err)
	}
	if string(identity) != in {
		t.Fatalf("identity strip changed bytes:\n got=%s\nwant=%s", identity, in)
	}
}

// verifies: bareTicketPrefixes strips the id_prefix so the audit matches
// externally-authored branches/subjects, and is identity when non-namespaced.
func TestBareTicketPrefixes(t *testing.T) {
	got := bareTicketPrefixes([]string{"FEE-BL", "FEE-NF"}, "FEE")
	if len(got) != 2 || got[0] != "BL" || got[1] != "NF" {
		t.Fatalf("bareTicketPrefixes = %v; want [BL NF]", got)
	}
	same := []string{"BL", "NF"}
	if out := bareTicketPrefixes(same, ""); &out[0] != &same[0] {
		t.Fatalf("non-namespaced bareTicketPrefixes must return the input slice unchanged")
	}
}

func namespacedCore(t *testing.T) *Core {
	t.Helper()
	base := t.TempDir()
	if err := exec.Command("git", "-C", base, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), store.Options{
		Root: base, CommonDir: base + "/common", DBPath: base + "/state/state.db",
		RegistryPath: base + "/state/registry.jsonl", ProjectID: "project-fee",
		WorktreeID: "main", ProjectSlug: "fee", Prefixes: []string{"BL"}, IDPrefix: "FEE",
		LeaseStateDir: filepath.Join(base, "lease-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s)
}

// verifies: `aira id BL` prints {"id":"BL-101"} (the make-id wrapper contract),
// and create/show display the ticket id bare while the compound stays in `path`.
func TestDisplayIDProjectionThroughCore(t *testing.T) {
	ctx := context.Background()
	c := namespacedCore(t)

	// `aira id BL` must display bare.
	idResp := c.Do(ctx, Request{Verb: "id", Args: map[string]any{"prefix": "BL"}})
	if !idResp.OK {
		t.Fatalf("id verb failed: %s", idResp.Error)
	}
	var idData struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(idResp.RawData, &idData); err != nil {
		t.Fatalf("id RawData not set/parseable (%q): %v", idResp.RawData, err)
	}
	if idData.ID != "BL-1" {
		t.Fatalf("aira id BL displayed %q; want BL-1", idData.ID)
	}

	// create displays bare id, keeps compound path.
	createResp := c.Do(ctx, Request{Verb: "create", Args: map[string]any{"title": "t", "kind": "chore", "severity": "P2"}})
	if !createResp.OK {
		t.Fatalf("create failed: %s", createResp.Error)
	}
	raw := string(createResp.RawData)
	if !strings.Contains(raw, `"id":"BL-2"`) {
		t.Fatalf("create did not display bare id: %s", raw)
	}
	if !strings.Contains(raw, `.aira/tickets/FEE-BL-2.md`) {
		t.Fatalf("create path must stay compound: %s", raw)
	}

	// show resolves a bare selector and displays bare.
	showResp := c.Do(ctx, Request{Verb: "show", Args: map[string]any{"selector": "BL-2"}})
	if !showResp.OK {
		t.Fatalf("show BL-2 failed: %s", showResp.Error)
	}
	showRaw := string(showResp.RawData)
	if !strings.Contains(showRaw, `"id":"BL-2"`) || strings.Contains(showRaw, `"id":"FEE-BL-2"`) {
		t.Fatalf("show did not display bare id: %s", showRaw)
	}
	if !strings.Contains(showRaw, `.aira/tickets/FEE-BL-2.md`) {
		t.Fatalf("show path must stay compound: %s", showRaw)
	}

	// list displays bare ids for every row.
	listResp := c.Do(ctx, Request{Verb: "list", Args: map[string]any{}})
	if !listResp.OK {
		t.Fatalf("list failed: %s", listResp.Error)
	}
	if lr := string(listResp.RawData); strings.Contains(lr, `"id":"FEE-BL-2"`) || !strings.Contains(lr, `"id":"BL-2"`) {
		t.Fatalf("list did not display bare ids: %s", lr)
	}

	// check must pass under namespacing: the rebuildable-index identity invariant
	// (filename == frontmatter == store key) holds because ALL three are the
	// compound FEE-BL-2 — a divergent file would be dropped and check would fail.
	checkResp := c.Do(ctx, Request{Verb: "check"})
	if checkResp.Code == "FAIL" {
		t.Fatalf("check FAILED under namespacing (identity invariant broken): %s", string(checkResp.RawData))
	}
}
