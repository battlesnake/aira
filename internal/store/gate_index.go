package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aira/internal/gate"
)

type GateCheckResult struct {
	GateID  string
	Kind    string
	Subject string
	Verdict string
	Code    string
	Trusted bool
	Suspect bool
	Seq     uint64
}

// GateCheckReport carries the folded gate verdict. Code is the report-level
// reason and is set when the verdict cannot be established from the gate set
// itself, for example when no gate is defined at all.
type GateCheckReport struct {
	Verdict     string
	Code        string
	Results     []GateCheckResult
	Failed      int
	Unevaluated int
	Passed      int
}

// GateSetEmptyCode marks a gate report whose verdict could not be established
// because no gate definition was discovered. An unpopulated gate set evaluates
// nothing, so it is never a pass.
const GateSetEmptyCode = "U_GATE_SET_EMPTY"

type discoveredGate struct {
	Definition gate.GateDefinition
	Digest     string
}

func (s *Store) gateRoot() string { return filepath.Join(s.root, ".aira", "gates") }

func (s *Store) discoverGates() ([]discoveredGate, error) {
	root := s.gateRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("E_GATE_INVALID: read gate directory: %w", err)
	}
	var result []discoveredGate
	seen := map[string]bool{}
	firstData := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".canary.json") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("E_GATE_INVALID: gate file %s is a symlink", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		firstData[entry.Name()] = append([]byte(nil), data...)
		definition, err := gate.ParseGate(data, entry.Name())
		if err != nil {
			return nil, err
		}
		if seen[definition.ID] {
			return nil, fmt.Errorf("E_GATE_INVALID: duplicate gate id %s", definition.ID)
		}
		seen[definition.ID] = true
		digest, err := gate.DigestGate(definition)
		if err != nil {
			return nil, err
		}
		result = append(result, discoveredGate{Definition: definition, Digest: digest})
	}
	secondEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("U_GATE_EVIDENCE_UNAVAILABLE: gate directory changed during scan: %w", err)
	}
	secondNames := map[string]bool{}
	for _, entry := range secondEntries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") && !strings.HasSuffix(entry.Name(), ".canary.json") {
			secondNames[entry.Name()] = true
		}
	}
	if len(secondNames) != len(firstData) {
		return nil, errors.New("U_GATE_EVIDENCE_UNAVAILABLE: gate file set changed during scan")
	}
	for name, first := range firstData {
		second, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil || !bytes.Equal(first, second) {
			return nil, fmt.Errorf("U_GATE_EVIDENCE_UNAVAILABLE: gate file %s changed during scan", name)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Definition.ID < result[j].Definition.ID })
	return result, nil
}

func (s *Store) ListGates() ([]gate.GateDefinition, error) {
	discovered, err := s.discoverGates()
	if err != nil {
		return nil, err
	}
	result := make([]gate.GateDefinition, len(discovered))
	for i := range discovered {
		result[i] = discovered[i].Definition
	}
	return result, nil
}

func (s *Store) canaryFor(def gate.GateDefinition) (gate.CanaryDeclaration, error) {
	candidates := make([]string, 0, len(def.CanaryIDs)*2)
	for _, id := range def.CanaryIDs {
		candidates = append(candidates, filepath.Join(s.gateRoot(), "canaries", id+".json"), filepath.Join(s.gateRoot(), id+".canary.json"))
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			var declaration gate.CanaryDeclaration
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&declaration); err != nil {
				return gate.CanaryDeclaration{}, fmt.Errorf("E_GATE_CANARY_INVALID: %w", err)
			}
			if err := gate.ValidateCanary(declaration); err != nil {
				return gate.CanaryDeclaration{}, err
			}
			if declaration.GateID != def.ID {
				return gate.CanaryDeclaration{}, errors.New("E_GATE_CANARY_INVALID: canary references another gate")
			}
			if declaration.LaneBinding != def.Lane.Name {
				return gate.CanaryDeclaration{}, errors.New("E_GATE_CANARY_INVALID: canary lane does not match gate lane")
			}
			for _, id := range def.CanaryIDs {
				if id == declaration.ID {
					return declaration, nil
				}
			}
			return gate.CanaryDeclaration{}, errors.New("E_GATE_CANARY_INVALID: declared canary is not referenced by gate")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return gate.CanaryDeclaration{}, err
		}
	}
	return gate.CanaryDeclaration{}, errors.New("E_GATE_CANARY_INVALID: referenced canary declaration is missing")
}

func gateProjectionRows(records []GateAuditRecord) map[string]GateAuditRecord {
	latest := map[string]GateAuditRecord{}
	for _, record := range records {
		if record.Type != "result" {
			continue
		}
		key := record.Fields["gate_id"] + "\x00" + record.Fields["subject"]
		if prior, ok := latest[key]; !ok || record.Seq > prior.Seq {
			latest[key] = record
		}
	}
	return latest
}

func (s *Store) rebuildGateProjection(ctx context.Context) error {
	discovered, err := s.discoverGates()
	if err != nil {
		return err
	}
	audit, err := OpenGateAudit(s.commonDir, false)
	if err != nil {
		return err
	}
	records, err := audit.Read()
	if err != nil {
		if errors.Is(err, errGateAuditEmpty) {
			records = nil
		} else {
			return err
		}
	}
	latest := gateProjectionRows(records)
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		for _, table := range []string{"gates", "gate_results", "gate_proofs", "gate_attestations"} {
			if _, err := conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE project_id=?", s.projectID); err != nil {
				return err
			}
		}
		for _, item := range discovered {
			data, err := gate.RenderGate(item.Definition)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO gates(project_id,gate_id,definition_digest,definition_json) VALUES(?,?,?,?)`, s.projectID, item.Definition.ID, item.Digest, string(data)); err != nil {
				return err
			}
		}
		for _, record := range latest {
			data, _ := json.Marshal(record)
			trusted, suspect := 0, 0
			if record.Fields["trusted"] == "true" {
				trusted = 1
			}
			if record.Fields["suspect"] == "true" {
				suspect = 1
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO gate_results(project_id,gate_id,subject,seq,verdict,code,trusted,suspect,record_json) VALUES(?,?,?,?,?,?,?,?,?)`, s.projectID, record.Fields["gate_id"], record.Fields["subject"], record.Seq, record.Fields["verdict"], record.Fields["code"], trusted, suspect, string(data)); err != nil {
				return err
			}
		}
		for _, record := range records {
			if record.Type != "proof-of-fire" && record.Type != "attestation" {
				continue
			}
			data, _ := json.Marshal(record)
			table := "gate_proofs"
			if record.Type == "attestation" {
				table = "gate_attestations"
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO "+table+"(project_id,seq,gate_id,record_json) VALUES(?,?,?,?)", s.projectID, record.Seq, record.Fields["gate_id"], string(data)); err != nil {
				return err
			}
		}
		return nil
	})
}
