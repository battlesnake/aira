package domain

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewRequirementValidates(t *testing.T) {
	if _, err := NewRequirement(RequirementInput{ID: "AR-1", Text: "A requirement", Status: RequirementBuilt}); err != nil {
		t.Fatalf("valid requirement rejected: %v", err)
	}
	bad := []struct {
		name  string
		input RequirementInput
	}{
		{"empty-id", RequirementInput{ID: "", Text: "t", Status: RequirementBuilt}},
		{"bad-id", RequirementInput{ID: "not an id", Text: "t", Status: RequirementBuilt}},
		{"empty-text", RequirementInput{ID: "AR-1", Text: "   ", Status: RequirementBuilt}},
		{"empty-status", RequirementInput{ID: "AR-1", Text: "t", Status: ""}},
		{"bad-status", RequirementInput{ID: "AR-1", Text: "t", Status: "in-progress"}},
	}
	for _, tc := range bad {
		if _, err := NewRequirement(tc.input); err == nil || !strings.Contains(err.Error(), "E_REQUIREMENT_INVALID") {
			t.Fatalf("%s: expected E_REQUIREMENT_INVALID, got %v", tc.name, err)
		}
	}
}

func TestRequirementRoundTrip(t *testing.T) {
	for _, status := range []RequirementStatus{
		RequirementBuilt, RequirementPartial, RequirementDesigned, RequirementPlanned,
		RequirementBoundary, RequirementRetired, RequirementSuperseded,
	} {
		req, err := NewRequirement(RequirementInput{ID: "AR-3", Text: "Multi\nline requirement.", Status: status})
		if err != nil {
			t.Fatalf("status %s: %v", status, err)
		}
		data, err := RenderRequirement(req)
		if err != nil {
			t.Fatalf("render %s: %v", status, err)
		}
		got, err := ParseRequirement(data)
		if err != nil {
			t.Fatalf("parse %s: %v", status, err)
		}
		if got.ID != req.ID || got.Status != req.Status || got.Text != req.Text || got.Schema != 1 {
			t.Fatalf("round-trip mismatch for %s: got %#v want %#v", status, got, req)
		}
	}
}

func TestParseRequirementRejectsTrailingFrontmatterContent(t *testing.T) {
	requirement, err := NewRequirement(RequirementInput{ID: "AR-3", Text: "A requirement", Status: RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("\n---\n")
	for _, trailing := range [][]byte{[]byte("  GARBAGE"), []byte(` {"schema":1}`)} {
		bad := bytes.Replace(data, marker, append(append([]byte{}, trailing...), marker...), 1)
		if _, err := ParseRequirement(bad); err == nil || !strings.Contains(err.Error(), "E_REQUIREMENT_INVALID") {
			t.Fatalf("ParseRequirement trailing frontmatter content %q: expected E_REQUIREMENT_INVALID, got %v", trailing, err)
		}
	}
}

func TestParseRequirementRejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"no-frontmatter":    []byte("just text\n"),
		"unterminated":      []byte("---\n{\"schema\":1,\"id\":\"AR-1\",\"status\":\"built\"}\nbody\n"),
		"unknown-field":     []byte("---\n{\"schema\":1,\"id\":\"AR-1\",\"status\":\"built\",\"extra\":1}\n---\nbody\n"),
		"bad-status":        []byte("---\n{\"schema\":1,\"id\":\"AR-1\",\"status\":\"nope\"}\n---\nbody\n"),
		"bad-id":            []byte("---\n{\"schema\":1,\"id\":\"nope id\",\"status\":\"built\"}\n---\nbody\n"),
		"no-trailing-newln": []byte("---\n{\"schema\":1,\"id\":\"AR-1\",\"status\":\"built\"}\n---\nbody"),
		"empty-text":        []byte("---\n{\"schema\":1,\"id\":\"AR-1\",\"status\":\"built\"}\n---\n\n"),
	}
	for name, data := range cases {
		if _, err := ParseRequirement(data); err == nil || !strings.Contains(err.Error(), "E_REQUIREMENT_INVALID") {
			t.Fatalf("%s: expected E_REQUIREMENT_INVALID, got %v", name, err)
		}
	}
}
