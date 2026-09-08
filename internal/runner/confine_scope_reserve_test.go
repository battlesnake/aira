package runner

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// AIRA-191 / AIRA-192. The per-scope reserve merge, in the ONE place a
// ConfineRecord's reserve is ever established.
//
// verifies: AIRA-191
// verifies: AIRA-192

func TestApplyConfineScopeReservesEstablishesOnlyWhatTheDaemonKnows(t *testing.T) {
	capText := "48318382080" // 45 GiB: a --delegate-ram scope's wide CEILING
	other := "2147483648"
	scopes := []ConfineRecord{
		{ScopeID: "CONFINE-suite-101-a", Cap: &capText},
		{ScopeID: "CONFINE-lost-102-b", Cap: &other},
	}
	ApplyConfineScopeReserves(scopes, map[string]int64{"CONFINE-suite-101-a": 536870912})

	if scopes[0].ReserveBytes == nil || *scopes[0].ReserveBytes != 536870912 {
		t.Fatalf("known scope reserve=%v, want the daemon's 512 MiB charge, never the 45 GiB cap beside it",
			scopes[0].ReserveBytes)
	}
	if contains(scopes[0].UnevaluatedFields, "reserve") {
		t.Fatalf("an ESTABLISHED reserve was also named unevaluated: %v", scopes[0].UnevaluatedFields)
	}
	// The false-pass direction, and the whole point of the field: a scope the
	// daemon holds no admission record for must stay ABSENT. Falling back to the
	// cap is exactly the AIRA-192 defect.
	if scopes[1].ReserveBytes != nil {
		t.Fatalf("an unknown scope was given a reserve of %d; a cap is not a reserve and absence is not a number",
			*scopes[1].ReserveBytes)
	}
	if !contains(scopes[1].UnevaluatedFields, "reserve") {
		t.Fatalf("an unestablished reserve was not named in UnevaluatedFields: %v", scopes[1].UnevaluatedFields)
	}
}

// A nil map is the DAEMON-DOWN listing: nothing about any scope's admission is
// establishable, and every record must say so rather than silently carrying no
// opinion at all.
func TestApplyConfineScopeReservesWithNoDaemonKnowledgeMarksEveryScope(t *testing.T) {
	capText := "8192"
	scopes := []ConfineRecord{{ScopeID: "CONFINE-a-101-a", Cap: &capText}, {ScopeID: "CONFINE-b-102-b"}}
	ApplyConfineScopeReserves(scopes, nil)
	for index, record := range scopes {
		if record.ReserveBytes != nil {
			t.Fatalf("scope %d got reserve %d from a daemon that knew nothing", index, *record.ReserveBytes)
		}
		if !contains(record.UnevaluatedFields, "reserve") {
			t.Fatalf("scope %d unevaluated fields=%v, want `reserve` named", index, record.UnevaluatedFields)
		}
	}
}

// Idempotence matters because the merge runs once per listing over records the
// scan rebuilds each time, and a second pass must not accumulate duplicate
// facet names or overwrite an established reading with an absence.
func TestApplyConfineScopeReservesIsIdempotent(t *testing.T) {
	scopes := []ConfineRecord{{ScopeID: "CONFINE-a-101-a"}, {ScopeID: "CONFINE-b-102-b"}}
	reserves := map[string]int64{"CONFINE-a-101-a": 4096}
	ApplyConfineScopeReserves(scopes, reserves)
	ApplyConfineScopeReserves(scopes, reserves)
	if scopes[0].ReserveBytes == nil || *scopes[0].ReserveBytes != 4096 {
		t.Fatalf("established reserve lost on a second pass: %v", scopes[0].ReserveBytes)
	}
	if got := countString(scopes[1].UnevaluatedFields, "reserve"); got != 1 {
		t.Fatalf("`reserve` named %d times in %v, want exactly once", got, scopes[1].UnevaluatedFields)
	}
}

// A zero reserve is a REAL charge (an admitted job whose ledger charge is zero),
// not an absence, and the pointer is what keeps the two apart on the wire.
func TestApplyConfineScopeReservesKeepsAZeroChargeDistinctFromAbsence(t *testing.T) {
	scopes := []ConfineRecord{{ScopeID: "CONFINE-zero-101-a"}}
	ApplyConfineScopeReserves(scopes, map[string]int64{"CONFINE-zero-101-a": 0})
	if scopes[0].ReserveBytes == nil {
		t.Fatal("a zero charge was dropped to an absence; a job charged nothing is not a job the daemon has lost")
	}
	if *scopes[0].ReserveBytes != 0 {
		t.Fatalf("reserve=%d, want the established 0", *scopes[0].ReserveBytes)
	}
	if contains(scopes[0].UnevaluatedFields, "reserve") {
		t.Fatal("an established zero was named unevaluated")
	}
}

// The wire shape itself: `reserve_bytes` is present as JSON null when absent,
// exactly as `cap` and `rss_bytes` beside it are. An `omitempty` here would make
// a scope with no established reserve indistinguishable, to a JSON consumer,
// from a daemon build that never had the field — which is the AIRA-191
// attribution gap in a new place.
func TestConfineRecordReserveBytesIsAlwaysOnTheWire(t *testing.T) {
	data, err := json.Marshal(ConfineRecord{ScopeID: "CONFINE-a-101-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"reserve_bytes":null`) {
		t.Fatalf("absent reserve encoded as %s, want an explicit null", data)
	}
	value := int64(4096)
	data, err = json.Marshal(ConfineRecord{ScopeID: "CONFINE-a-101-a", ReserveBytes: &value})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"reserve_bytes":4096`) {
		t.Fatalf("established reserve encoded as %s", data)
	}
	var decoded ConfineRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ReserveBytes == nil || *decoded.ReserveBytes != value {
		t.Fatalf("round trip lost the reserve: %+v", decoded)
	}
}

func TestApplyConfineScopeReservesLeavesAnEmptyListingAlone(t *testing.T) {
	var scopes []ConfineRecord
	ApplyConfineScopeReserves(scopes, map[string]int64{"CONFINE-a-101-a": 1})
	if !reflect.DeepEqual(scopes, []ConfineRecord(nil)) {
		t.Fatalf("scopes=%+v", scopes)
	}
}

func contains(values []string, want string) bool {
	return countString(values, want) > 0
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}
