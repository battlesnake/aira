package domain

import (
	"bytes"
	"strings"
	"testing"
)

// verifies: AIRA-216 -- front matter was written with json.Marshal, which turns
// <, > and & into \u003c, \u003e, \u0026, so a status-only move rewrote the title
// bytes. The three front-matter writers share one encoder with HTML escaping off.

func assertNoHTMLEscapes(t *testing.T, label string, data []byte) {
	t.Helper()
	for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if bytes.Contains(data, []byte(escape)) {
			t.Fatalf("%s carries %s:\n%s", label, escape, data)
		}
	}
}

// assertFrontmatterDelimiters pins the bytes around the header directly:
// ParseTicket tolerates JSON whitespace, so a round trip cannot catch an extra
// newline from the encoder.
func assertFrontmatterDelimiters(t *testing.T, label string, data []byte) {
	t.Helper()
	if !bytes.HasPrefix(data, []byte("---\n{")) {
		t.Fatalf("%s does not start with ---\\n{: %q", label, data)
	}
	rest := data[len("---\n"):]
	end := bytes.Index(rest, []byte("}\n---\n"))
	if end < 0 {
		t.Fatalf("%s header is not followed by exactly }\\n---\\n: %q", label, data)
	}
	header := rest[:end+1]
	if bytes.Contains(header, []byte("\n")) {
		t.Fatalf("%s header is not a single line: %q", label, header)
	}
	if bytes.Contains(rest, []byte("}\n\n---\n")) {
		t.Fatalf("%s has a blank line before the closing delimiter: %q", label, data)
	}
}

func TestRenderTicketDoesNotEscapeHTMLCharacters(t *testing.T) {
	ticket := Ticket{
		ID: "AIRA-42", Project: "aira", Title: "planned -> done & <id> handling",
		Status: StatusPlanned, Kind: KindFeature, Severity: SeverityP2,
	}
	data, err := RenderTicket(ticket, "Body\n")
	if err != nil {
		t.Fatal(err)
	}
	assertNoHTMLEscapes(t, "ticket", data)
	if !bytes.Contains(data, []byte(`"title":"planned -> done & <id> handling"`)) {
		t.Fatalf("literal title missing:\n%s", data)
	}
	assertFrontmatterDelimiters(t, "ticket", data)
	got, body, err := ParseTicket(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != ticket.Title || body != "Body\n" {
		t.Fatalf("round trip title=%q body=%q", got.Title, body)
	}
}

func TestRenderFindingDoesNotEscapeHTMLCharacters(t *testing.T) {
	input := validReviewInput()
	input.Disposition = DispositionWaived
	input.WaiverReason = "<a> & <b>"
	input.WaiverActor = "owner"
	finding, err := NewReviewFinding(input)
	if err != nil {
		t.Fatal(err)
	}
	data, err := RenderFinding(finding)
	if err != nil {
		t.Fatal(err)
	}
	assertNoHTMLEscapes(t, "finding", data)
	if !strings.Contains(string(data), `"waiver_reason":"<a> & <b>"`) {
		t.Fatalf("literal waiver reason missing:\n%s", data)
	}
	assertFrontmatterDelimiters(t, "finding", data)
	got, err := ParseFinding(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.WaiverReason != "<a> & <b>" {
		t.Fatalf("round trip waiver reason=%q", got.WaiverReason)
	}
}

func TestRenderRequirementKeepsDelimiterBytes(t *testing.T) {
	requirement, err := NewRequirement(RequirementInput{ID: "AR-3", Text: "A requirement", Status: RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	assertNoHTMLEscapes(t, "requirement", data)
	assertFrontmatterDelimiters(t, "requirement", data)
}

// For content with no <, > or &, the bytes are exactly what json.Marshal wrote.
func TestRenderFrontmatterIsByteIdenticalForPlainContent(t *testing.T) {
	ticket := Ticket{
		ID: "AIRA-42", Project: "aira", Title: "plain title",
		Status: StatusPlanned, Kind: KindFeature, Severity: SeverityP2,
	}
	data, err := RenderTicket(ticket, "Body\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "---\n" + `{"schema":1,"id":"AIRA-42","project":"aira","title":"plain title","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}` + "\n---\nBody\n"
	if string(data) != want {
		t.Fatalf("plain-content bytes changed:\n got: %q\nwant: %q", data, want)
	}
}
