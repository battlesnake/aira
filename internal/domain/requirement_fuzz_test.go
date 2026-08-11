package domain

import "testing"

func FuzzParseRequirement(f *testing.F) {
	seeds := []Requirement{
		{ID: "AR-1", Status: RequirementDesigned, Text: "The coordination contract is documented."},
		{ID: "AR-42", Status: RequirementBuilt, Text: "A multiline requirement statement.\nWith a second line."},
	}
	for _, requirement := range seeds {
		data, err := RenderRequirement(requirement)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(data))
	}
	f.Add("garbage")
	f.Add("---\n{}\n---\n")

	f.Fuzz(func(t *testing.T, data string) {
		requirement, err := ParseRequirement([]byte(data))
		if err != nil {
			return
		}
		canonical, err := RenderRequirement(requirement)
		if err != nil {
			t.Fatalf("render successful parse: %v", err)
		}
		reparsed, err := ParseRequirement(canonical)
		if err != nil {
			t.Fatalf("parse rendered requirement: %v", err)
		}
		recanonical, err := RenderRequirement(reparsed)
		if err != nil {
			t.Fatalf("re-render parsed requirement: %v", err)
		}
		if string(canonical) != string(recanonical) {
			t.Fatal("requirement did not round-trip through its canonical renderer")
		}
	})
}
