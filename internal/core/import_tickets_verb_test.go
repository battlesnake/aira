package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/store"
)

// AIRA-237 Task 2 — the id-accepting import verb + requirement display strip
// through the shared Core.Do (so the daemon and in-process faces emit identical
// bytes).

func namespacedReqCore(t *testing.T) (*Core, string) {
	t.Helper()
	base := t.TempDir()
	if err := exec.Command("git", "-C", base, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), store.Options{
		Root: base, CommonDir: base + "/common", DBPath: base + "/state/state.db",
		RegistryPath: base + "/state/registry.jsonl", ProjectID: "project-fee",
		WorktreeID: "main", ProjectSlug: "fee", Prefixes: []string{"BL"},
		RequirementPrefixes: []string{"VR"}, IDPrefix: "FEE",
		LeaseStateDir: filepath.Join(base, "lease-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s), base
}

// verifies: `aira import --tickets` routes to the id-accepting ticket importer
// and reports a pass verdict + created ids for a clean batch.
func TestImportTicketsVerbThroughCore(t *testing.T) {
	ctx := context.Background()
	c, base := namespacedReqCore(t)

	file := filepath.Join(base, "tickets.jsonl")
	body := strings.Join([]string{
		`{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b1"}`,
		`{"id":"BL-2","title":"two","status":"in-progress","kind":"chore","severity":"P1","body":"b2"}`,
	}, "\n")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := c.Do(ctx, Request{Verb: "import", Args: map[string]any{"file": file, "tickets": true, "strict": false}, Content: []byte(body), HasContent: true})
	if !resp.OK || resp.Code != "PASS" {
		t.Fatalf("import --tickets: ok=%v code=%q err=%q", resp.OK, resp.Code, resp.Error)
	}
	// Summary ids are the stored (compound) keys — the operator record of what was
	// written. (Import is not on the display-strip whitelist by design, so RawData
	// is nil and the typed Data carries the summary.)
	summary, ok := resp.Data.(store.ImportTicketsSummary)
	if !ok {
		t.Fatalf("import Data is not an ImportTicketsSummary: %T", resp.Data)
	}
	if len(summary.Created) != 2 || summary.Created[0] != "FEE-BL-1" {
		t.Fatalf("import summary created = %v; want 2 compound ids", summary.Created)
	}

	// A dangling endpoint makes the import verdict fail (honest partial import).
	bad := `{"id":"BL-3","title":"three","status":"planned","kind":"chore","severity":"P2","body":"b","links":[{"kind":"blocks","to":"NF-9"}]}`
	badResp := c.Do(ctx, Request{Verb: "import", Args: map[string]any{"file": file, "tickets": true, "strict": false}, Content: []byte(bad), HasContent: true})
	if badResp.Code != "FAIL" {
		t.Fatalf("import with a bad link should have verdict FAIL; code=%q", badResp.Code)
	}
}

// verifies: a requirement id displays bare through Core.Do (req show/ls), the
// symmetric output side of the requirement-verb canonicalization.
func TestRequirementDisplayStripThroughCore(t *testing.T) {
	ctx := context.Background()
	c, _ := namespacedReqCore(t)

	addResp := c.Do(ctx, Request{Verb: "req", Args: map[string]any{"subverb": "add", "text": "must hold", "status": "planned"}})
	if !addResp.OK {
		t.Fatalf("req add: %s", addResp.Error)
	}
	if raw := string(addResp.RawData); strings.Contains(raw, `"id":"FEE-VR-1"`) || !strings.Contains(raw, `"id":"VR-1"`) {
		t.Fatalf("req add did not display bare requirement id: %s", raw)
	}

	// req show resolves a BARE selector (input canonicalization) and displays the
	// id bare, while the file path stays compound (FEE-VR-1.md).
	showResp := c.Do(ctx, Request{Verb: "req", Args: map[string]any{"subverb": "show", "selector": "VR-1"}})
	if !showResp.OK {
		t.Fatalf("req show VR-1 (bare): %s", showResp.Error)
	}
	raw := string(showResp.RawData)
	if strings.Contains(raw, `"id":"FEE-VR-1"`) || !strings.Contains(raw, `"id":"VR-1"`) {
		t.Fatalf("req show did not resolve/display bare id: %s", raw)
	}
	if !strings.Contains(raw, "FEE-VR-1.md") {
		t.Fatalf("req show path must stay compound: %s", raw)
	}
}

// verifies (Task 4): `aira import --tickets --allocated-max BL=1217` seeds the
// forward allocator from the LAST-allocated number, so `aira id BL` mints
// EXACTLY N+1 (1218) then N+2 (1219) — the fencepost — and the minted id
// displays bare (the make-id wrapper contract). This exercises the whole CLI/
// core parse path (string -> map) + the display strip in one.
func TestImportAllocatedMaxFencepostThroughCore(t *testing.T) {
	ctx := context.Background()
	c, base := namespacedReqCore(t)

	file := filepath.Join(base, "tickets.jsonl")
	body := `{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b"}`
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := c.Do(ctx, Request{Verb: "import", Args: map[string]any{"file": file, "tickets": true, "strict": false, "allocated_max": []string{"BL=1217"}}, Content: []byte(body), HasContent: true})
	if !resp.OK || resp.Code != "PASS" {
		t.Fatalf("import --tickets --allocated-max: ok=%v code=%q err=%q", resp.OK, resp.Code, resp.Error)
	}

	first := c.Do(ctx, Request{Verb: "id", Args: map[string]any{"prefix": "BL"}})
	if !first.OK {
		t.Fatalf("aira id BL #1: %s", first.Error)
	}
	if raw := string(first.RawData); !strings.Contains(raw, `"id":"BL-1218"`) || strings.Contains(raw, `"id":"FEE-BL-1218"`) {
		t.Fatalf("first mint after --allocated-max BL=1217 = %s; want bare BL-1218 (N+1 fencepost, display-stripped)", raw)
	}
	second := c.Do(ctx, Request{Verb: "id", Args: map[string]any{"prefix": "BL"}})
	if !second.OK {
		t.Fatalf("aira id BL #2: %s", second.Error)
	}
	if raw := string(second.RawData); !strings.Contains(raw, `"id":"BL-1219"`) {
		t.Fatalf("second mint = %s; want bare BL-1219", raw)
	}
}

// verifies (Task 4): --allocated-max outside --tickets is refused with a stable
// error, never silently dropped (the AIRA-82 discarded-scope failure mode).
func TestImportAllocatedMaxRequiresTickets(t *testing.T) {
	ctx := context.Background()
	c, base := namespacedReqCore(t)
	file := filepath.Join(base, "findings.jsonl")
	if err := os.WriteFile(file, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := c.Do(ctx, Request{Verb: "import", Args: map[string]any{"file": file, "strict": false, "allocated_max": []string{"BL=10"}}})
	if resp.OK || !strings.Contains(resp.Error, "requires --tickets") {
		t.Fatalf("--allocated-max without --tickets should be refused; ok=%v err=%q", resp.OK, resp.Error)
	}
}
