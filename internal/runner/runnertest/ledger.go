// Package runnertest builds synthetic run-ledger fixtures for tests in other
// packages.
//
// The runner's own framing function is unexported, and the packages that need a
// deliberately corrupt ledger — internal/store and internal/core, which grade
// the run-ledger check dimension (AIRA-172) — sit above it. Without one shared
// definition each of them would hand-roll the framing, and a drift in the real
// format would silently turn those tests into ones that pass for a
// torn-frame reason instead of the corrupt-record reason they assert.
// TestLedgerFramingRecipeIsStable in package runner pins Frame against the real
// frame(), so a drift fails there, once, with a message that says what to fix.
//
// Nothing in the product imports this package.
//
// SAFETY: every helper here writes inside a caller-supplied temp directory.
// None of them touches the machine's real common-directory run ledger, which is
// shared live by other sessions.
package runnertest

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Frame renders one ledger record exactly as the runner writes it: a uvarint
// payload length, the payload, then the payload's SHA-256.
func Frame(payload []byte) []byte {
	var prefix [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(prefix[:], uint64(len(payload)))
	digest := sha256.Sum256(payload)
	framed := append([]byte{}, prefix[:n]...)
	framed = append(framed, payload...)
	return append(framed, digest[:]...)
}

// LedgerPath is the run ledger's location under a common directory.
func LedgerPath(commonDir string) string {
	return filepath.Join(commonDir, "aira", "runs", "ledger.bin")
}

// FutureField is the field name the corrupt fixture carries. The ledger decoder
// runs with DisallowUnknownFields, so a field no current writer emits is a hard
// decode failure — the realistic corruption of an older binary reading a newer
// writer's shared ledger, and specific enough in the diagnostic that a test
// asserting on it cannot be satisfied by some other defect.
const FutureField = "field_from_the_future"

// HealthyRecords are two records that decode and replay cleanly.
func HealthyRecords() [][]byte {
	return [][]byte{
		[]byte(`{"schema_version":1,"sequence":1,"kind":"starting","run":{"id":"RUN-1"}}`),
		[]byte(`{"schema_version":1,"sequence":2,"kind":"running","run":{"id":"RUN-1"}}`),
	}
}

// CorruptRecords are one good record followed by one the decoder refuses.
func CorruptRecords() [][]byte {
	return [][]byte{
		[]byte(`{"schema_version":1,"sequence":1,"kind":"starting","run":{"id":"RUN-1"}}`),
		[]byte(fmt.Sprintf(`{"schema_version":1,"sequence":2,"kind":"starting","run":{"id":"RUN-2"},%q:true}`, FutureField)),
	}
}

// WriteLedger writes framed records to commonDir's run ledger, creating the run
// directories, and returns the ledger path plus the byte offset at which each
// record starts so a diagnostic that names the offset can be asserted exactly.
func WriteLedger(t *testing.T, commonDir string, records ...[]byte) (path string, offsets []int64) {
	t.Helper()
	path = LedgerPath(commonDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var data []byte
	offsets = make([]int64, 0, len(records))
	for _, record := range records {
		offsets = append(offsets, int64(len(data)))
		data = append(data, Frame(record)...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, offsets
}

// WriteCorruptLedger writes CorruptRecords and returns the ledger path and the
// byte offset of the bad record.
func WriteCorruptLedger(t *testing.T, commonDir string) (path string, badOffset int64) {
	t.Helper()
	records := CorruptRecords()
	path, offsets := WriteLedger(t, commonDir, records...)
	return path, offsets[len(offsets)-1]
}

// WriteHealthyLedger writes HealthyRecords and returns the ledger path.
func WriteHealthyLedger(t *testing.T, commonDir string) string {
	t.Helper()
	path, _ := WriteLedger(t, commonDir, HealthyRecords()...)
	return path
}
