package runner

import "testing"

func vramScopeHasFacet(facets []string, facet string) bool {
	for _, f := range facets {
		if f == facet {
			return true
		}
	}
	return false
}

// verifies: AIRA-269 — ApplyConfineScopeVRAM stamps each scope's declared --vram by
// scope id: a value in the map (INCLUDING 0) is an established VRAMBytes; a scope
// absent from the map is nil (unevaluated) and named in UnevaluatedFields. The
// 0-vs-nil distinction is load-bearing: 0 is "declared no VRAM / not a GPU job" (a
// positive fact the renderer shows as "—"), nil is "the daemon holds no record".
func TestApplyConfineScopeVRAM(t *testing.T) {
	G := int64(1) << 30
	scopes := []ConfineRecord{
		{ScopeID: "gpu"},
		{ScopeID: "nongpu"},
		{ScopeID: "lost"},
	}
	ApplyConfineScopeVRAM(scopes, map[string]int64{"gpu": 4 * G, "nongpu": 0})

	if scopes[0].VRAMBytes == nil || *scopes[0].VRAMBytes != 4*G {
		t.Fatalf("gpu VRAMBytes = %v, want an established 4G", scopes[0].VRAMBytes)
	}
	if vramScopeHasFacet(scopes[0].UnevaluatedFields, ConfineVRAMFacet) {
		t.Fatal("an established reservation must not carry the vram unevaluated facet")
	}
	if scopes[1].VRAMBytes == nil || *scopes[1].VRAMBytes != 0 {
		t.Fatalf("nongpu VRAMBytes = %v, want an ESTABLISHED 0 (declared no VRAM), never nil", scopes[1].VRAMBytes)
	}
	if vramScopeHasFacet(scopes[1].UnevaluatedFields, ConfineVRAMFacet) {
		t.Fatal("an established 0 must clear the vram unevaluated facet")
	}
	if scopes[2].VRAMBytes != nil {
		t.Fatalf("a scope absent from the ledger map must be nil (unevaluated), got %v", scopes[2].VRAMBytes)
	}
	if !vramScopeHasFacet(scopes[2].UnevaluatedFields, ConfineVRAMFacet) {
		t.Fatal("a scope absent from the map must be marked vram-unevaluated")
	}
}
