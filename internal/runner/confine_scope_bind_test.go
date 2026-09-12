package runner

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestBindConfineScopeIDRefusesAForeignPIDNameOrOwner is the guard that makes
// AIRA-22's supervisor-minted scope id safe. The scope-id GRAMMAR is happy with
// any canonical pid/stamp/owner combination, so a syntax check alone would let a
// pre-minted id name a scope after a foreign pid (breaking `confine --kill <pid>`,
// the --list SupervisorPID column, and the orphan reaper's liveness predicate) or
// after a foreign owner (which the daemon binds separately at admit, so the
// mismatch degrades to an UNACCOUNTED flock-fallback launch).
//
// Each direction is asserted separately: a single "any mismatch is refused" case
// would still pass against an implementation that checked only one of them.
//
// verifies: AIRA-22
func TestBindConfineScopeIDRefusesAForeignPIDNameOrOwner(t *testing.T) {
	self := os.Getpid()
	good := confineScopeID("gate", "session-a")
	if err := bindConfineScopeID(good, "gate", "session-a"); err != nil {
		t.Fatalf("a scope id this process just minted for this request was refused: %v", err)
	}
	foreignPID := strings.Replace(good, "-"+strconv.Itoa(self)+"-", "-"+strconv.Itoa(self+1)+"-", 1)
	if foreignPID == good {
		t.Fatalf("test setup failed to rewrite the pid in %q", good)
	}
	for _, test := range []struct {
		name    string
		scopeID string
		reqName string
		owner   string
		want    string
	}{
		{name: "foreign pid", scopeID: foreignPID, reqName: "gate", owner: "session-a", want: "supervisor pid"},
		{name: "foreign name", scopeID: good, reqName: "other", owner: "session-a", want: "name"},
		{name: "foreign owner", scopeID: good, reqName: "gate", owner: "session-b", want: "owner"},
		{name: "malformed", scopeID: "not-a-scope-id", reqName: "gate", owner: "session-a", want: "malformed"},
		{name: "path separator", scopeID: "CONFINE-a/b-1-1", reqName: "gate", owner: "session-a", want: "malformed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := bindConfineScopeID(test.scopeID, test.reqName, test.owner)
			if err == nil {
				t.Fatalf("scope id %q was accepted for name=%q owner=%q", test.scopeID, test.reqName, test.owner)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not name the mismatched facet %q", err, test.want)
			}
		})
	}
}

// An unknown owner must bind against the ABSENCE of an owner suffix -- the
// encoding AIRA-52 chose so "nobody claimed this" can never be confused with a
// claim.
//
// verifies: AIRA-22
func TestBindConfineScopeIDAcceptsUnknownOwnerEncoding(t *testing.T) {
	unknown := confineScopeID("job", ConfineUnknownOwner)
	if strings.Contains(strings.TrimPrefix(unknown, "CONFINE-"), "@") {
		t.Fatalf("setup: unknown owner must be encoded as an absent suffix, got %q", unknown)
	}
	if err := bindConfineScopeID(unknown, "job", ConfineUnknownOwner); err != nil {
		t.Fatalf("unknown-owner id refused: %v", err)
	}
	if err := bindConfineScopeID(unknown, "job", "session-a"); err == nil {
		t.Fatal("an ownerless id was accepted for an owned request")
	}
}

// normalizeConfineIdentity is the SINGLE place name/owner defaulting happens, so
// the supervisor's mint and confineWithDeps cannot disagree about what a scope id
// should contain. This pins the defaults themselves.
//
// verifies: AIRA-22
func TestNormalizeConfineIdentityDefaultsAndValidates(t *testing.T) {
	name, owner, err := normalizeConfineIdentity(ConfineRequest{})
	if err != nil || name != "job" || owner != ConfineUnknownOwner {
		t.Fatalf("empty request normalised to name=%q owner=%q err=%v; want job/unknown/nil", name, owner, err)
	}
	if _, _, err := normalizeConfineIdentity(ConfineRequest{Name: "has space"}); err == nil {
		t.Fatal("an invalid name was normalised rather than refused")
	}
	if _, _, err := normalizeConfineIdentity(ConfineRequest{Owner: "has space"}); err == nil {
		t.Fatal("an invalid owner was normalised rather than refused")
	}
	name, owner, err = normalizeConfineIdentity(ConfineRequest{Name: "gate", Owner: "@cwd-wt"})
	if err != nil || name != "gate" || owner != "@cwd-wt" {
		t.Fatalf("an inferred owner must survive verbatim: name=%q owner=%q err=%v", name, owner, err)
	}
}
