// verifies: AIRA-290

package domain

import "testing"

// TestRelationKindForward pins the inverse -> forward mapping that the
// `aira link` refusal uses to name the command that stores the same relation.
func TestRelationKindForward(t *testing.T) {
	inverse := map[RelationKind]RelationKind{
		RelationBlockedBy:    RelationBlocks,
		RelationChild:        RelationParent,
		RelationDuplicatedBy: RelationDuplicates,
		RelationSupersededBy: RelationSupersedes,
		RelationResolvedBy:   RelationResolves,
	}
	for k, want := range inverse {
		got, ok := k.Forward()
		if !ok || got != want {
			t.Fatalf("%q.Forward() = %q,%v; want %q,true", k, got, ok, want)
		}
		// The pair must round-trip through Inverse, so a wrong table entry
		// (child -> blocks) cannot hide behind a shape check.
		if want.Inverse() != k {
			t.Fatalf("%q.Inverse() = %q, want %q", want, want.Inverse(), k)
		}
	}
	for _, k := range []RelationKind{RelationBlocks, RelationParent, RelationRelates, RelationDuplicates, RelationSupersedes, RelationResolves, "", "bogus"} {
		if got, ok := k.Forward(); ok || got != "" {
			t.Fatalf("%q.Forward() = %q,%v; want \"\",false", k, got, ok)
		}
	}
}
