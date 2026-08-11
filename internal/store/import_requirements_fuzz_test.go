package store

import (
	"os"
	"testing"

	"aira/internal/domain"
)

func FuzzParseRequirementsTable(f *testing.F) {
	requirements, err := os.ReadFile("../../REQUIREMENTS.md")
	if err != nil {
		f.Fatalf("read REQUIREMENTS.md seed: %v", err)
	}
	f.Add(string(requirements))
	f.Add("| ID | Requirement | Status | Implemented-by | Verified-by |\n| --- | --- | --- | --- | --- |\n| AR-1 | wrong width | designed | only-four |\n")
	f.Add("| ID | Requirement | Status | Implemented-by | Verified-by |\n| --- | --- | --- | --- | --- |\n| AR-1 | literal | pipe | designed | owner | check |\n")
	f.Add("| --- | :--- | ---: | :---: | --- |\n")
	f.Add("")

	f.Fuzz(func(t *testing.T, data string) {
		rows, err := parseRequirementTable([]byte(data))
		if err != nil {
			return
		}
		for _, row := range rows {
			requirement, err := domain.ParseRequirement(row.Data)
			if err != nil {
				t.Fatalf("parse imported requirement %s: %v", row.ID, err)
			}
			canonical, err := domain.RenderRequirement(requirement)
			if err != nil {
				t.Fatalf("render imported requirement %s: %v", row.ID, err)
			}
			if string(row.Data) != string(canonical) {
				t.Fatalf("imported requirement %s did not retain canonical rendering", row.ID)
			}
		}
	})
}
