package runner

import (
	"strings"
	"testing"
)

const testSessionID = "b6b9daa7-4e9b-4de2-b1be-fff8ea5fb6d2"

// TestConfineSessionOwnerShape pins the exact shape ConfineSessionOwner accepts
// (AIRA-313): a canonical lowercase UUID, optionally padded with surrounding
// whitespace, and nothing else. Every refused value must report ok=false without
// panicking, so the owner chain falls through to its old behaviour; every
// accepted value must survive both owner validators so it can be embedded in a
// scope id.
//
// verifies: AIRA-313
func TestConfineSessionOwnerShape(t *testing.T) {
	want := "cc-" + testSessionID
	for _, raw := range []string{
		testSessionID,
		" " + testSessionID,
		testSessionID + " ",
		"\t" + testSessionID + "\t",
		testSessionID + "\n",
		"\n " + testSessionID + " \r\n",
	} {
		assertAcceptedSessionOwner(t, raw, want)
	}
	// The contract is any canonical lowercase 8-4-4-4-12 UUID, not only v4: a
	// v7 id (version nibble '7', variant 'b') must be accepted too, so a
	// recogniser that pins the v4 version nibble fails here.
	const v7SessionID = "01890a5d-ac96-774b-bcce-b302099a8057"
	assertAcceptedSessionOwner(t, v7SessionID, "cc-"+v7SessionID)

	refused := []string{
		"",
		" ",
		"\t\n",
		strings.ToUpper(testSessionID),
		"B6b9daa7-4e9b-4de2-b1be-fff8ea5fb6d2",
		testSessionID[:35],
		testSessionID + "0",
		testSessionID + "x",
		"x" + testSessionID,
		strings.Replace(testSessionID, "-", "", 1) + "0",
		"b6b9daa74-e9b-4de2-b1be-fff8ea5fb6d2",
		"b6b9daa7-4e9b-4de2-b1be-fff8ea5fb6dg",
		"../x",
		"cc-" + testSessionID,
		"b6b9daa7@4e9b-4de2-b1be-fff8ea5fb6d2",
		"b6b9daa7/4e9b-4de2-b1be-fff8ea5fb6d2",
		"b6b9daa7 4e9b-4de2-b1be-fff8ea5fb6d2",
		"b6b9daa7\t4e9b-4de2-b1be-fff8ea5fb6d2",
		testSessionID[:20] + "\x00" + testSessionID[21:],
		testSessionID[:20] + "\n" + testSessionID[21:],
		testSessionID[:8] + " " + testSessionID[8:],
		testSessionID[:8] + "\t" + testSessionID[8:],
		strings.Repeat("a", 200),
	}
	for _, raw := range refused {
		owner, ok := ConfineSessionOwner(raw)
		if ok || owner != "" {
			t.Fatalf("ConfineSessionOwner(%q) = %q, %v; want \"\", false", raw, owner, ok)
		}
	}

	for _, owner := range []string{
		"cc-deploy",
		"cc-" + strings.ToUpper(testSessionID),
		"cc-" + testSessionID + "x",
		"cc-" + testSessionID[:35],
		testSessionID,
		"cc--" + testSessionID[1:],
		"@cwd-x",
		"",
		ConfineUnknownOwner,
		strings.Repeat("a", 64),
	} {
		if IsConfineSessionOwner(owner) {
			t.Fatalf("IsConfineSessionOwner(%q) = true", owner)
		}
	}
}

// TestConfineScopeIDRoundTripsSessionOwner pins that a Claude Code session owner
// (cc-<uuid>, 39 bytes) is carried through every scope-id path unchanged: the
// job mint, the aitest worker mint (which copies the parent's owner), the shared
// parser, and the supervisor's bind check; and that the worst-case scope
// directory name stays under NAME_MAX.
//
// verifies: AIRA-313
func TestConfineScopeIDRoundTripsSessionOwner(t *testing.T) {
	owner, ok := ConfineSessionOwner(testSessionID)
	if !ok {
		t.Fatal("setup: session owner refused")
	}
	if err := ValidateConfineIdentity(owner); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfineOwner(owner); err != nil {
		t.Fatal(err)
	}
	longName := strings.Repeat("n", 100)
	scopeID, err := MintConfineScopeID(ConfineRequest{Name: longName, Owner: owner})
	if err != nil {
		t.Fatalf("MintConfineScopeID: %v", err)
	}
	name, _, _, parsedOwner, ok := ParseConfineScopeID(scopeID)
	if !ok || name != longName || parsedOwner != owner {
		t.Fatalf("ParseConfineScopeID(%q) = name %q owner %q ok %v", scopeID, name, parsedOwner, ok)
	}
	if err := bindConfineScopeID(scopeID, longName, owner); err != nil {
		t.Fatalf("bindConfineScopeID: %v", err)
	}
	if dir := ".aira-" + scopeID; len(dir) > 255 {
		t.Fatalf("worst-case scope directory is %d bytes > 255: %q", len(dir), dir)
	}
	worker := MintWorkerScopeID(7, 4194304, owner)
	_, _, _, workerOwner, ok := ParseConfineScopeID(worker)
	if !ok || workerOwner != owner {
		t.Fatalf("worker scope %q owner %q ok %v; want %q", worker, workerOwner, ok, owner)
	}
}

func assertAcceptedSessionOwner(t *testing.T, raw, want string) {
	t.Helper()
	owner, ok := ConfineSessionOwner(raw)
	if !ok || owner != want {
		t.Fatalf("ConfineSessionOwner(%q) = %q, %v; want %q, true", raw, owner, ok, want)
	}
	if err := ValidateConfineIdentity(owner); err != nil {
		t.Fatalf("accepted owner %q fails ValidateConfineIdentity: %v", owner, err)
	}
	if err := ValidateConfineOwner(owner); err != nil {
		t.Fatalf("accepted owner %q fails ValidateConfineOwner: %v", owner, err)
	}
	if !ConfineOwnerIsAttested(owner) {
		t.Fatalf("accepted owner %q is not attested", owner)
	}
	if !IsConfineSessionOwner(owner) {
		t.Fatalf("IsConfineSessionOwner(%q) = false for an accepted owner", owner)
	}
}
