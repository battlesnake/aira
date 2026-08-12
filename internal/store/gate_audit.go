package store

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"aira/internal/gate"
)

const gateAuditSchema = 1

type GateAuditRecord struct {
	SchemaVersion int               `json:"schema_version"`
	Seq           uint64            `json:"seq"`
	Type          string            `json:"type"`
	Nonce         string            `json:"nonce"`
	PrevDigest    string            `json:"prev_digest"`
	Fields        map[string]string `json:"fields,omitempty"`
	Digest        string            `json:"digest"`
	Tag           string            `json:"tag"`
}

type gateAuditHead struct {
	SchemaVersion int    `json:"schema_version"`
	Seq           uint64 `json:"seq"`
	Digest        string `json:"digest"`
	Tag           string `json:"tag"`
}

type GateAudit struct {
	Dir, LedgerPath, HeadPath, KeyPath, LockPath string
}

func OpenGateAudit(commonDir string, writable bool) (*GateAudit, error) {
	if strings.TrimSpace(commonDir) == "" {
		return nil, errors.New("E_CONFIG_INVALID: common directory is required")
	}
	common, err := filepath.Abs(commonDir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(common, "aira", "gates")
	if writable {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &GateAudit{Dir: dir, LedgerPath: filepath.Join(dir, "audit.bin"), HeadPath: filepath.Join(dir, "HEAD"), KeyPath: filepath.Join(dir, "hmac.key"), LockPath: filepath.Join(dir, "audit.lock")}, nil
}

func (a *GateAudit) key(write bool) ([]byte, error) {
	data, err := os.ReadFile(a.KeyPath)
	if err == nil {
		if len(data) != 32 {
			return nil, errors.New("E_JOURNAL_CORRUPT: invalid gate HMAC key")
		}
		if info, statErr := os.Stat(a.KeyPath); statErr != nil || info.Mode().Perm() != 0o600 {
			return nil, errors.New("E_JOURNAL_CORRUPT: gate HMAC key permissions are not 0600")
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !write {
		return nil, errors.New("E_JOURNAL_CORRUPT: gate HMAC key is unavailable")
	}
	if _, ledgerErr := os.Stat(a.LedgerPath); ledgerErr == nil {
		return nil, errors.New("E_JOURNAL_CORRUPT: gate HMAC key is unavailable")
	}
	if _, headErr := os.Stat(a.HeadPath); headErr == nil {
		return nil, errors.New("E_JOURNAL_CORRUPT: gate HMAC key is unavailable")
	}
	data = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.KeyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return a.key(false)
		}
		return nil, err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err := syncGateDir(a.Dir); err != nil {
		return nil, err
	}
	return data, nil
}

func syncGateDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func gateFrame(payload []byte) []byte {
	var prefix [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(prefix[:], uint64(len(payload)))
	sum := sha256.Sum256(payload)
	out := append([]byte{}, prefix[:n]...)
	out = append(out, payload...)
	return append(out, sum[:]...)
}

func gateDigest(r GateAuditRecord) (string, []byte, error) {
	r.Digest = ""
	r.Tag = ""
	data, err := json.Marshal(r)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), data, nil
}

func gateTag(key, payload []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func gateRecordPayload(r GateAuditRecord) []byte {
	return gate.CanonicalPayload(r.Type, map[string]string{
		"fields":      canonicalFields(r.Fields),
		"nonce":       r.Nonce,
		"prev_digest": r.PrevDigest,
		"seq":         strconv.FormatUint(r.Seq, 10),
	})
}

func canonicalFields(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+fields[k])
	}
	return strings.Join(parts, "\x00")
}

func (a *GateAudit) Append(kind string, fields map[string]string) (GateAuditRecord, error) {
	if err := validateGateAuditFields(kind, fields); err != nil {
		return GateAuditRecord{}, err
	}
	lock, err := acquireLock(a.LockPath)
	if err != nil {
		return GateAuditRecord{}, fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
	defer unlockFile(lock)
	key, err := a.key(true)
	if err != nil {
		return GateAuditRecord{}, err
	}
	records, err := a.readWithKey(key)
	if err != nil && !errors.Is(err, errGateAuditEmpty) {
		return GateAuditRecord{}, err
	}
	var seq uint64
	var prev string
	if len(records) == 0 {
		genesis := GateAuditRecord{SchemaVersion: gateAuditSchema, Type: "genesis", Nonce: "genesis", Fields: map[string]string{"format": "aira-gate-audit-v1"}}
		digest, payload, e := gateDigest(genesis)
		if e != nil {
			return GateAuditRecord{}, e
		}
		genesis.Digest = digest
		genesis.Tag = gateTag(key, gateRecordPayload(genesis))
		payload, e = json.Marshal(genesis)
		if e != nil {
			return GateAuditRecord{}, e
		}
		if e = a.writeFrame(payload); e != nil {
			return GateAuditRecord{}, e
		}
		if e = a.writeHead(key, genesis); e != nil {
			return GateAuditRecord{}, e
		}
		records = []GateAuditRecord{genesis}
		seq, prev = 1, digest
	} else {
		seq, prev = records[len(records)-1].Seq+1, records[len(records)-1].Digest
	}
	nonceBytes := make([]byte, 16)
	if _, err = io.ReadFull(rand.Reader, nonceBytes); err != nil {
		return GateAuditRecord{}, err
	}
	nonce := hex.EncodeToString(nonceBytes)
	for _, r := range records {
		if r.Nonce == nonce {
			return GateAuditRecord{}, errors.New("E_JOURNAL_CORRUPT: duplicate nonce")
		}
	}
	record := GateAuditRecord{SchemaVersion: gateAuditSchema, Seq: seq, Type: kind, Nonce: nonce, PrevDigest: prev, Fields: cloneFields(fields)}
	digest, payload, err := gateDigest(record)
	if err != nil {
		return GateAuditRecord{}, err
	}
	record.Digest = digest
	record.Tag = gateTag(key, gateRecordPayload(record))
	payload, err = json.Marshal(record)
	if err != nil {
		return GateAuditRecord{}, err
	}
	if err := a.writeFrame(payload); err != nil {
		return GateAuditRecord{}, err
	}
	if err := a.writeHead(key, record); err != nil {
		return GateAuditRecord{}, err
	}
	return record, nil
}

func validateGateAuditFields(kind string, fields map[string]string) error {
	if kind != "result" && kind != "attestation" && kind != "proof-of-fire" && kind != "review" && kind != "baseline" && kind != "baseline-pointer" {
		return errors.New("E_GATE_INVALID: invalid audit record type")
	}
	required := func(names ...string) error {
		for _, name := range names {
			if strings.TrimSpace(fields[name]) == "" {
				return fmt.Errorf("E_GATE_INVALID: audit %s field %q is required", kind, name)
			}
		}
		return nil
	}
	switch kind {
	case "baseline":
		return required("gate_id", "comparator", "comparator_version", "lane", "comparison_key", "source_commit", "source_report_ids", "snapshot_digest", "snapshot_json", "pin_actor", "pin_at")
	case "baseline-pointer":
		if err := required("gate_id", "active_baseline_seq"); err != nil {
			return err
		}
		if _, err := strconv.ParseUint(fields["active_baseline_seq"], 10, 64); err != nil || fields["active_baseline_seq"] == "0" {
			return errors.New("E_GATE_INVALID: baseline pointer sequence is invalid")
		}
	case "result":
		if err := required("gate_id", "subject", "verdict"); err != nil {
			return err
		}
		switch fields["verdict"] {
		case gate.VerdictPass, gate.VerdictFail, gate.VerdictUnevaluated:
		default:
			return fmt.Errorf("E_GATE_INVALID: invalid result verdict %q", fields["verdict"])
		}
	case "attestation":
		if err := required("gate_id", "subject", "actor", "attested_result", "challenge_nonce"); err != nil {
			return err
		}
		if fields["attested_result"] != gate.VerdictPass && fields["attested_result"] != gate.VerdictFail {
			return fmt.Errorf("E_GATE_INVALID: invalid attested result %q", fields["attested_result"])
		}
	case "proof-of-fire":
		if err := required("gate_id", "canary_id", "definition_digest", "declaration_digest", "canary_tree_digest", "subject_scope", "lane"); err != nil {
			return err
		}
		if _, ok := fields["evaluator_version"]; !ok {
			return errors.New("E_GATE_INVALID: audit proof-of-fire field \"evaluator_version\" is required")
		}
	case "review":
		if err := required("gate_id", "subject", "challenge"); err != nil {
			return err
		}
	}
	return nil
}

var errGateAuditEmpty = errors.New("empty gate audit")

func cloneFields(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (a *GateAudit) writeFrame(payload []byte) error {
	f, err := os.OpenFile(a.LedgerPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	data := gateFrame(payload)
	for len(data) > 0 {
		n, writeErr := f.Write(data)
		if writeErr != nil {
			_ = f.Close()
			return writeErr
		}
		data = data[n:]
	}
	if err = f.Sync(); err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		return err
	}
	return syncGateDir(a.Dir)
}

func (a *GateAudit) writeHead(key []byte, record GateAuditRecord) error {
	head := gateAuditHead{SchemaVersion: gateAuditSchema, Seq: record.Seq, Digest: record.Digest}
	payload := []byte(fmt.Sprintf("head\x00seq=%d\x00digest=%s", head.Seq, head.Digest))
	head.Tag = gateTag(key, payload)
	data, err := json.Marshal(head)
	if err != nil {
		return err
	}
	tmp := a.HeadPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY, 0o600)
	if err == nil {
		err = f.Sync()
		_ = f.Close()
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, a.HeadPath); err != nil {
		return err
	}
	return syncGateDir(a.Dir)
}

func (a *GateAudit) Read() ([]GateAuditRecord, error) {
	if _, ledgerErr := os.Stat(a.LedgerPath); errors.Is(ledgerErr, os.ErrNotExist) {
		if _, headErr := os.Stat(a.HeadPath); headErr == nil {
			return nil, errors.New("E_JOURNAL_CORRUPT: head exists without ledger")
		}
		return nil, errGateAuditEmpty
	}
	key, err := a.key(false)
	if err != nil {
		return nil, err
	}
	return a.readWithKey(key)
}

func (a *GateAudit) Verify() error {
	_, err := a.Read()
	return err
}

func (a *GateAudit) readWithKey(key []byte) ([]GateAuditRecord, error) {
	f, err := os.Open(a.LedgerPath)
	if errors.Is(err, os.ErrNotExist) {
		if _, headErr := os.Stat(a.HeadPath); headErr == nil {
			return nil, errors.New("E_JOURNAL_CORRUPT: head exists without ledger")
		}
		return nil, errGateAuditEmpty
	}
	if err != nil {
		return nil, fmt.Errorf("E_JOURNAL_CORRUPT: %w", err)
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var records []GateAuditRecord
	nonces := map[string]bool{}
	var prevDigest string
	var expected uint64
	for {
		n, readErr := binary.ReadUvarint(reader)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, errors.New("E_JOURNAL_CORRUPT: malformed audit frame")
		}
		if n > 64*1024*1024 {
			return nil, errors.New("E_JOURNAL_CORRUPT: audit frame too large")
		}
		raw := make([]byte, n+32)
		if _, readErr = io.ReadFull(reader, raw); readErr != nil {
			return nil, errors.New("E_JOURNAL_CORRUPT: torn audit frame")
		}
		payload, want := raw[:n], raw[n:]
		sum := sha256.Sum256(payload)
		if !bytes.Equal(want, sum[:]) {
			return nil, errors.New("E_JOURNAL_CORRUPT: audit frame digest mismatch")
		}
		var record GateAuditRecord
		if readErr = json.Unmarshal(payload, &record); readErr != nil {
			return nil, errors.New("E_JOURNAL_CORRUPT: malformed audit record")
		}
		if record.SchemaVersion != gateAuditSchema || record.Seq != expected || nonces[record.Nonce] || record.PrevDigest != prevDigest {
			return nil, errors.New("E_JOURNAL_CORRUPT: invalid audit chain")
		}
		digest, _, digestErr := gateDigest(record)
		if digestErr != nil || digest != record.Digest {
			return nil, errors.New("E_JOURNAL_CORRUPT: record digest mismatch")
		}
		if !hmac.Equal([]byte(record.Tag), []byte(gateTag(key, gateRecordPayload(record)))) {
			return nil, errors.New("E_JOURNAL_CORRUPT: record authentication failed")
		}
		records = append(records, record)
		nonces[record.Nonce] = true
		prevDigest = record.Digest
		expected++
	}
	if len(records) == 0 {
		return nil, errors.New("E_JOURNAL_CORRUPT: empty audit ledger")
	}
	headData, readErr := os.ReadFile(a.HeadPath)
	if readErr != nil {
		return nil, errors.New("E_JOURNAL_CORRUPT: durable audit head is missing")
	}
	var head gateAuditHead
	if readErr = json.Unmarshal(headData, &head); readErr != nil || head.SchemaVersion != gateAuditSchema {
		return nil, errors.New("E_JOURNAL_CORRUPT: malformed audit head")
	}
	last := records[len(records)-1]
	headPayload := []byte(fmt.Sprintf("head\x00seq=%d\x00digest=%s", head.Seq, head.Digest))
	if head.Seq != last.Seq || head.Digest != last.Digest || !hmac.Equal([]byte(head.Tag), []byte(gateTag(key, headPayload))) {
		return nil, errors.New("E_JOURNAL_CORRUPT: durable audit head does not match ledger")
	}
	return records, nil
}

func GateAuditRecords(records []GateAuditRecord) []GateAuditRecord {
	out := append([]GateAuditRecord(nil), records...)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}
