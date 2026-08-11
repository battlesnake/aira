package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gate"
)

func TestGateAuditAuthenticatesChainAndDetectsSuffixTruncation(t *testing.T) {
	common := t.TempDir()
	a, err := OpenGateAudit(common, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Append("result", map[string]string{"gate_id": "traceability", "subject": "subject", "verdict": "pass", "at": "later"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Append("result", map[string]string{"gate_id": "traceability", "subject": "subject", "verdict": "fail", "at": "earlier"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	pos := 0
	frameStart := 0
	for frame := 0; frame < 3; frame++ {
		frameStart = pos
		n, count := binary.Uvarint(data[pos:])
		if count <= 0 {
			t.Fatalf("bad test frame at %d", pos)
		}
		pos += count + int(n) + sha256Size
	}
	_ = frameStart
	// Keep genesis and the first result; HEAD still names the removed second result.
	if err := os.WriteFile(a.LedgerPath, data[:frameStart], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err == nil || ErrorCode(err) != "E_JOURNAL_CORRUPT" {
		t.Fatalf("truncation error=%v", err)
	}
	if _, err := os.Stat(a.KeyPath); err != nil {
		t.Fatalf("key missing after write: %v", err)
	}
}

const sha256Size = 32

func TestGateAuditReadDoesNotCreateKey(t *testing.T) {
	a, err := OpenGateAudit(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); !errors.Is(err, errGateAuditEmpty) {
		t.Fatalf("read error=%v", err)
	}
	if _, err := os.Stat(a.KeyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created key: %v", err)
	}
}

func TestRebuildMissingGateKeyIsJournalCorruption(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	gitRun(t, root, "init", "-q")
	def, _ := testTraceGate(t, root)
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "caller", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation.go"), []byte("package caller\n// covers: AR-1\nfunc Caller() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "implementation_test.go"), []byte("package caller\n// verifies: AR-1\nfunc TestCaller(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, err := s.RunGate(context.Background(), def.ID); err != nil {
		t.Fatal(err)
	}
	audit, err := OpenGateAudit(filepath.Join(base, "common"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(audit.KeyPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err == nil || ErrorCode(err) != "E_JOURNAL_CORRUPT" {
		t.Fatalf("rebuild error=%v", err)
	}
}

func TestGateAuditAppendRejectsInvalidSemantics(t *testing.T) {
	a, err := OpenGateAudit(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"gate_id": "traceability", "subject": "subject"}
	invalid := cloneFields(base)
	invalid["verdict"] = "zero"
	if _, err := a.Append("result", invalid); err == nil || ErrorCode(err) != "E_GATE_INVALID" {
		t.Fatalf("invalid verdict error=%v", err)
	}
	if _, err := a.Append("unknown", base); err == nil || ErrorCode(err) != "E_GATE_INVALID" {
		t.Fatalf("invalid type error=%v", err)
	}
	if _, err := a.Append("result", base); err == nil || ErrorCode(err) != "E_GATE_INVALID" {
		t.Fatalf("missing verdict error=%v", err)
	}
}

func auditFrameBytes(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var frames [][]byte
	pos := 0
	for pos < len(data) {
		n, count := binary.Uvarint(data[pos:])
		if count <= 0 || pos+count+int(n)+sha256Size > len(data) {
			t.Fatal("invalid audit frame in test")
		}
		end := pos + count + int(n) + sha256Size
		frames = append(frames, append([]byte(nil), data[pos:end]...))
		pos = end
	}
	return frames
}

func rewriteAuditRecordFrame(t *testing.T, a *GateAudit, seq uint64, mutate func(*GateAuditRecord)) {
	t.Helper()
	data, err := os.ReadFile(a.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	frames := auditFrameBytes(t, data)
	for i, frame := range frames {
		n, count := binary.Uvarint(frame)
		var record GateAuditRecord
		if err := json.Unmarshal(frame[count:count+int(n)], &record); err != nil {
			t.Fatal(err)
		}
		if record.Seq != seq {
			continue
		}
		mutate(&record)
		payload, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		frames[i] = gateFrame(payload)
		if err := os.WriteFile(a.LedgerPath, bytes.Join(frames, nil), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("audit sequence %d not found", seq)
}

func TestGateAuditRejectsTamperReorderDuplicateNonceAndChangedSubject(t *testing.T) {
	t.Run("authentication-tag", func(t *testing.T) {
		a, err := OpenGateAudit(t.TempDir(), true)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = a.Append("result", map[string]string{"gate_id": "g", "subject": "s", "verdict": gate.VerdictPass})
		rewriteAuditRecordFrame(t, a, 1, func(record *GateAuditRecord) {
			record.Tag = strings.Repeat("0", len(record.Tag))
		})
		if _, err := a.Read(); err == nil || ErrorCode(err) != "E_JOURNAL_CORRUPT" {
			t.Fatalf("tag error=%v", err)
		}
	})
	t.Run("reordered-records", func(t *testing.T) {
		a, err := OpenGateAudit(t.TempDir(), true)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = a.Append("result", map[string]string{"gate_id": "g", "subject": "s", "verdict": gate.VerdictPass})
		_, _ = a.Append("result", map[string]string{"gate_id": "g", "subject": "s", "verdict": gate.VerdictFail})
		data, err := os.ReadFile(a.LedgerPath)
		if err != nil {
			t.Fatal(err)
		}
		frames := auditFrameBytes(t, data)
		frames[1], frames[2] = frames[2], frames[1]
		if err := os.WriteFile(a.LedgerPath, bytes.Join(frames, nil), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Read(); err == nil || ErrorCode(err) != "E_JOURNAL_CORRUPT" {
			t.Fatalf("reorder error=%v", err)
		}
	})
	t.Run("duplicate-nonce", func(t *testing.T) {
		a, err := OpenGateAudit(t.TempDir(), true)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = a.Append("result", map[string]string{"gate_id": "g", "subject": "s", "verdict": gate.VerdictPass})
		second, _ := a.Append("result", map[string]string{"gate_id": "g", "subject": "s", "verdict": gate.VerdictFail})
		firstRecords, err := a.Read()
		if err != nil {
			t.Fatal(err)
		}
		rewriteAuditRecordFrame(t, a, second.Seq, func(record *GateAuditRecord) {
			record.Nonce = firstRecords[1].Nonce
		})
		if _, err := a.Read(); err == nil || ErrorCode(err) != "E_JOURNAL_CORRUPT" {
			t.Fatalf("duplicate nonce error=%v", err)
		}
	})
	t.Run("changed-subject", func(t *testing.T) {
		a, err := OpenGateAudit(t.TempDir(), true)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = a.Append("result", map[string]string{"gate_id": "g", "subject": "s", "verdict": gate.VerdictPass})
		rewriteAuditRecordFrame(t, a, 1, func(record *GateAuditRecord) {
			record.Fields["subject"] = "other"
		})
		if _, err := a.Read(); err == nil || ErrorCode(err) != "E_JOURNAL_CORRUPT" {
			t.Fatalf("subject error=%v", err)
		}
	})
}
