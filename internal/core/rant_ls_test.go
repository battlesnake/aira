package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/store"
)

// verifies: AIRA-327 — the rant ls face: envelope, compact rows, --full,
// bounds and the --by refusal.

func rantLs(c *Core, args map[string]any) Response {
	args["subverb"] = "ls"
	return c.Do(context.Background(), Request{Verb: "rant", Args: args})
}

// rantLsWire is the envelope as every face serialises it.
func rantLsWire(t *testing.T, response Response) map[string]any {
	t.Helper()
	if !response.OK {
		t.Fatalf("rant ls: %#v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.Data
}

func rantCapture(t *testing.T, c *Core, body string) string {
	t.Helper()
	response := c.Do(context.Background(), Request{Verb: "rant", Args: map[string]any{"subverb": "capture", "text": body}, Actor: "opus"})
	if !response.OK {
		t.Fatalf("capture: %#v", response)
	}
	return response.Data.(store.RantAddResult).ID
}

func TestRantLsEnvelopeAndCompactRows(t *testing.T) {
	c := New(coreTestStore(t))
	long := strings.Repeat("ß", domain.RantPreviewRunes+10)
	first := rantCapture(t, c, "first")
	second := rantCapture(t, c, long)
	third := rantCapture(t, c, "third")
	if r := c.Do(context.Background(), Request{Verb: "rant", Args: map[string]any{"subverb": "review", "selector": first, "outcome": "planned"}, Actor: "owner"}); !r.OK {
		t.Fatalf("review: %#v", r)
	}

	data := rantLsWire(t, rantLs(c, map[string]any{}))
	if data["total"] != float64(2) || data["returned"] != float64(2) || data["truncated"] != false {
		t.Fatalf("default envelope = %v", data)
	}
	if next, present := data["next_since"]; !present || next != nil {
		t.Fatalf("next_since = %v present=%v, want an explicit null", next, present)
	}
	rows := data["rows"].([]any)
	if rows[0].(map[string]any)["id"] != second || rows[1].(map[string]any)["id"] != third {
		t.Fatalf("default rows = %v, want the two unreviewed rants oldest first", rows)
	}
	compact := rows[0].(map[string]any)
	for _, absent := range []string{"git_context", "reviews", "session", "observed_at"} {
		if _, present := compact[absent]; present {
			t.Fatalf("compact row carries %q: %v", absent, compact)
		}
	}
	if compact["body"] != strings.Repeat("ß", domain.RantPreviewRunes) || compact["body_truncated"] != true || compact["body_bytes"] != float64(len(long)) {
		t.Fatalf("compact preview: body_truncated=%v body_bytes=%v", compact["body_truncated"], compact["body_bytes"])
	}
	if compact["reviewed"] != false || compact["outcome"] != "" || compact["actor"] != "opus" {
		t.Fatalf("compact metadata = %v", compact)
	}

	// Paging through the face: limit 1 is truncated with a cursor.
	page := rantLsWire(t, rantLs(c, map[string]any{"status": "all", "limit": "1"}))
	if page["total"] != float64(3) || page["returned"] != float64(1) || page["truncated"] != true || page["next_since"] != float64(1) {
		t.Fatalf("limit 1 envelope = %v", page)
	}
	reviewed := rantLsWire(t, rantLs(c, map[string]any{"status": "reviewed"}))
	if row := reviewed["rows"].([]any)[0].(map[string]any); row["id"] != first || row["reviewed"] != true || row["outcome"] != "planned" {
		t.Fatalf("reviewed row = %v", row)
	}
}

// --full returns exactly the row `rant get` returns (the pre-AIRA-327 ls row).
func TestRantLsFullRowsMatchGet(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	id := rantCapture(t, c, strings.Repeat("x", domain.RantPreviewRunes*3))
	response := rantLs(c, map[string]any{"full": true})
	if !response.OK {
		t.Fatalf("ls --full: %#v", response)
	}
	rows, ok := response.Data.(map[string]any)["rows"].([]domain.Rant)
	if !ok || len(rows) != 1 {
		t.Fatalf("ls --full rows = %#v", response.Data)
	}
	want, err := s.GetRant(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("ls --full row differs from rant get:\n got %#v\nwant %#v", rows[0], want)
	}
}

func TestRantLsRefusesOutOfRangeValuesNamingTheField(t *testing.T) {
	c := New(coreTestStore(t))
	for name, tc := range map[string]struct {
		args map[string]any
		code string
		want string
	}{
		"limit 0":          {map[string]any{"limit": "0"}, domain.CodeRantInvalid, `--limit "0"`},
		"limit 201":        {map[string]any{"limit": "201"}, domain.CodeRantInvalid, `--limit "201"`},
		"limit -1":         {map[string]any{"limit": "-1"}, domain.CodeRantInvalid, `--limit "-1"`},
		"limit text":       {map[string]any{"limit": "lots"}, domain.CodeRantInvalid, `--limit "lots"`},
		"since text":       {map[string]any{"since": "x"}, domain.CodeRantInvalid, `--since "x"`},
		"grep blank":       {map[string]any{"grep": "  "}, domain.CodeRantInvalid, "--grep"},
		"two tags":         {map[string]any{"tags": []string{"a", "b"}}, domain.CodeRantInvalid, "a, b"},
		"status bogus":     {map[string]any{"status": "open"}, "E_ARGUMENT_INVALID", `"open"`},
		"severity bogus":   {map[string]any{"severity_filter": "P1"}, "E_ARGUMENT_INVALID", `"P1"`},
		"capture unset":    {map[string]any{"subverb": "capture", "text": "x", "severity": "unset"}, "E_ARGUMENT_INVALID", `"unset"`},
		"empty tag filter": {map[string]any{"tags": []string{"---"}}, domain.CodeRantInvalid, `"---"`},
	} {
		args := tc.args
		var response Response
		if args["subverb"] == "capture" {
			response = c.Do(context.Background(), Request{Verb: "rant", Args: args})
		} else {
			response = rantLs(c, args)
		}
		if response.OK || response.Code != tc.code || !strings.Contains(response.Error, tc.want) {
			t.Fatalf("%s: ok=%v code=%q error=%q, want %s naming %s", name, response.OK, response.Code, response.Error, tc.code, tc.want)
		}
	}
	for _, limit := range []string{"1", "200"} {
		if response := rantLs(c, map[string]any{"limit": limit}); !response.OK {
			t.Fatalf("limit %s refused: %#v", limit, response)
		}
	}
	if response := rantLs(c, map[string]any{"severity_filter": "unset", "status": "ALL"}); !response.OK {
		t.Fatalf("severity unset / status ALL refused: %#v", response)
	}
}

// --by aggregates every rant; a filter or page option beside it would be
// silently discarded, so each is refused by name.
func TestRantLsByRefusesEveryFilterAndPageOption(t *testing.T) {
	c := New(coreTestStore(t))
	rantCapture(t, c, "x")
	for flag, args := range map[string]map[string]any{
		"--status":   {"status": "all"},
		"--severity": {"severity_filter": "blocker"},
		"--tag":      {"tags": []string{"infra"}},
		"--actor":    {"actor": "opus"},
		"--grep":     {"grep": "x"},
		"--since":    {"since": "0"},
		"--limit":    {"limit": "5"},
		"--newest":   {"newest": true},
		"--full":     {"full": true},
	} {
		args["by"] = "tag"
		response := rantLs(c, args)
		if response.OK || response.Code != domain.CodeRantInvalid || !strings.Contains(response.Error, "combined with "+flag+" ") {
			t.Fatalf("--by tag %s: ok=%v code=%q error=%q", flag, response.OK, response.Code, response.Error)
		}
	}
	// Alone, --by still aggregates every rant, its output unchanged.
	response := rantLs(c, map[string]any{"by": "tag"})
	if !response.OK {
		t.Fatalf("--by tag: %#v", response)
	}
	if _, ok := response.Data.(store.RantCountResult); !ok {
		t.Fatalf("--by tag data = %T, want store.RantCountResult", response.Data)
	}
}

// The dispatch table drives the generated guide, the palette and the CLI's
// declared-option check: ls declares every new argument and not the old flag.
func TestRantLsOperationDeclaresTheNewArguments(t *testing.T) {
	var ls *OperationSpec
	for _, descriptor := range New(nil).DispatchDescriptors() {
		if descriptor.Name != "rant" {
			continue
		}
		for i := range descriptor.Operations {
			if descriptor.Operations[i].Name == "ls" {
				ls = &descriptor.Operations[i]
			}
		}
	}
	if ls == nil {
		t.Fatal("no rant ls operation")
	}
	declared := map[string]bool{}
	for _, arg := range ls.Args {
		declared[arg.Name] = true
	}
	for _, name := range []string{"by", "status", "severity_filter", "tags", "actor", "grep", "since", "limit", "newest", "full", "project", "prefix"} {
		if !declared[name] {
			t.Fatalf("rant ls does not declare %q (declares %v)", name, declared)
		}
	}
	if declared["unreviewed"] || declared["severity"] {
		t.Fatalf("rant ls still declares a retired/capture argument: %v", declared)
	}
}
