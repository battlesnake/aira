package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/domain"
	"aira/internal/gate"
	"aira/internal/runner"
)

// --- shared ledger helpers --------------------------------------------------

func gateLedgerRecords(t *testing.T, s *Store) []GateAuditRecord {
	t.Helper()
	audit, err := OpenGateAudit(s.commonDir, false)
	if err != nil {
		t.Fatal(err)
	}
	records, err := audit.Read()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func latestGateRecord(t *testing.T, records []GateAuditRecord, kind, gateID string) GateAuditRecord {
	t.Helper()
	var latest GateAuditRecord
	for _, record := range records {
		if record.Type == kind && record.Fields["gate_id"] == gateID && record.Seq > latest.Seq {
			latest = record
		}
	}
	if latest.Seq == 0 {
		t.Fatalf("no %s record for gate %s", kind, gateID)
	}
	return latest
}

// appendGateRecord forges a ledger record through the audit's own Append, so
// it carries a valid HMAC chain exactly as a real writer's would: the only
// thing a forged row differs in is the field the test chose to change.
func appendGateRecord(t *testing.T, s *Store, kind string, fields map[string]string) GateAuditRecord {
	t.Helper()
	audit, err := OpenGateAudit(s.commonDir, true)
	if err != nil {
		t.Fatal(err)
	}
	record, err := audit.Append(kind, fields)
	if err != nil {
		t.Fatalf("append forged %s: %v", kind, err)
	}
	return record
}

func onlyGateResult(t *testing.T, s *Store) GateCheckResult {
	t.Helper()
	report, err := s.GateCheck(context.Background())
	if err != nil {
		t.Fatalf("gate check: %v", err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("want exactly one gate result, got %#v", report)
	}
	return report.Results[0]
}

// forgeProvenPass appends a proof-of-fire and a linked trusted-pass result for
// def whose every binding matches the current gate and canary files, keyed on
// subject. edit, when non-nil, adjusts the two field sets before they are
// written.
func forgeProvenPass(t *testing.T, s *Store, def gate.GateDefinition, canary gate.CanaryDeclaration, subject, envDigest string, edit func(proof, result map[string]string)) {
	t.Helper()
	definitionDigest, err := gate.DigestGate(def)
	if err != nil {
		t.Fatal(err)
	}
	declarationDigest, err := canary.DeclarationDigest()
	if err != nil {
		t.Fatal(err)
	}
	proof := map[string]string{"gate_id": def.ID, "canary_id": canary.ID, "definition_digest": definitionDigest, "declaration_digest": declarationDigest,
		"canary_tree_digest": "forged-canary-tree", "subject_scope": subject, "lane": def.Lane.Name, "evaluator_version": def.Lane.EvaluatorVersion}
	result := gateResultFields(def, definitionDigest, declarationDigest, subject, "forged-canary-tree", "")
	result["gate_id"], result["subject"] = def.ID, subject
	result["verdict"], result["code"], result["trusted"], result["suspect"] = gate.VerdictPass, "", "true", "false"
	result["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if envDigest != "" {
		proof["env_digest"], result["env_digest"] = envDigest, envDigest
	}
	if edit != nil {
		edit(proof, result)
	}
	proofRecord := appendGateRecord(t, s, "proof-of-fire", proof)
	result["proof_seq"] = strconv.FormatUint(proofRecord.Seq, 10)
	appendGateRecord(t, s, "result", result)
}

// --- AIRA-285 ---------------------------------------------------------------

// verifies: AIRA-285 -- AIRA's own ticket and finding files are not part of
// the subject digest, so the edits `aira mv`, `aira create` and `aira find`
// leave behind do not orphan a stored gate result. The code edit at the end is
// the control: without it a digest that ignored everything would pass.
func TestSubjectDigestIgnoresAiraBookkeeping(t *testing.T) {
	root, original := subjectFixture(t, map[string]string{
		"main.go":                 "package main\n",
		".aira/tickets/AIRA-1.md": "---\nstatus: planned\n---\n",
		".aira/findings/f-1.md":   "finding one\n",
	})
	digest := func() string {
		t.Helper()
		value, err := subjectTreeDigest(root)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "tickets", "AIRA-1.md"), []byte("---\nstatus: done\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := digest(); got != original {
		t.Fatal("an unstaged ticket edit moved the subject digest")
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "tickets", "AIRA-2.md"), []byte("---\nstatus: draft\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	if got := digest(); got != original {
		t.Fatal("a new tracked ticket moved the subject digest")
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "findings", "f-1.md"), []byte("finding one, resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := digest(); got != original {
		t.Fatal("a finding edit moved the subject digest")
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := digest(); got == original {
		t.Fatal("control: a code edit left the subject digest unchanged")
	}
}

// verifies: AIRA-285 invariant I1 -- one filter, applied at the capture both
// the digest and the materialised snapshot are built from. A filter applied
// only to the digest would let a command read ticket bytes the proof does not
// bind (the false-pass direction); a filter applied only to subjectTreeDigest
// would make RunGate and GateCheck compute different digests for one tree.
func TestExcludedBookkeepingIsAbsentFromEveryLane(t *testing.T) {
	kept := []string{"main.go", ".aira/config", ".aira/requirements/AR-1.md", ".aira/gates/g.json", ".aira/gates/canaries/g-canary.json", ".aira/tickets-archive/x.md", "docs/.aira/tickets/x.md"}
	excluded := []string{".aira/tickets/AIRA-1.md", ".aira/findings/f-1.md"}
	files := map[string]string{}
	for _, path := range append(append([]string{}, kept...), excluded...) {
		files[path] = "body of " + path + "\n"
	}
	root, digest := subjectFixture(t, files)
	captured, err := captureSubject(root)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, entry := range captured.entries {
		present[entry.path] = true
	}
	for _, path := range excluded {
		if present[path] {
			t.Errorf("capture holds excluded path %s", path)
		}
	}
	for _, path := range kept {
		if !present[path] {
			t.Errorf("capture lost subject path %s", path)
		}
	}
	if captured.digest != digest {
		t.Fatalf("subjectTreeDigest %s != captureSubject digest %s", digest, captured.digest)
	}
	dir, cleanup, err := materializeSubject(captured)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, path := range []string{".aira/tickets", ".aira/findings"} {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(path))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("materialised snapshot has %s (err=%v)", path, err)
		}
	}
	for _, path := range kept {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); err != nil {
			t.Errorf("materialised snapshot lost %s: %v", path, err)
		}
	}
}

// verifies: AIRA-285 -- the peer's flow end to end: gate run, then a ticket
// moved through the store's own transition (the path `aira mv` uses), then a
// read. Before the change the read was U_GATE_NO_RESULT. The requirement twin
// pins the other direction: requirements are read by the traceability checker,
// so an edit to one must still orphan the pass.
func TestTicketMoveKeepsProvenGate(t *testing.T) {
	s, definition := newProvenGateStore(t)
	ctx := context.Background()
	ticket, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "bookkeeping", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, s.root, "add", "-A")
	if result, err := s.RunGate(ctx, definition.ID); err != nil || result.Verdict != gate.VerdictPass || !result.Trusted {
		t.Fatalf("run with a tracked ticket: %#v err=%v", result, err)
	}
	ticketFile := filepath.Join(s.root, ".aira", "tickets", ticket.ID+".md")
	before, err := os.ReadFile(ticketFile)
	if err != nil {
		t.Fatalf("ticket file: %v", err)
	}
	if _, err := s.MoveTicket(ctx, ticket.ID, domain.StatusInProgress); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(ticketFile)
	if err != nil || bytes.Equal(before, after) {
		t.Fatalf("the move did not edit the tracked ticket file (err=%v)", err)
	}
	if result := onlyGateResult(t, s); result.Verdict != gate.VerdictPass || !result.Trusted || result.Code != "" {
		t.Fatalf("a ticket move cost the stored pass: %#v", result)
	}
	requirement := filepath.Join(s.root, ".aira", "requirements", "AR-1.md")
	data, err := os.ReadFile(requirement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requirement, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if result := onlyGateResult(t, s); result.Verdict != gate.VerdictUnevaluated || result.Code != "U_GATE_NO_RESULT" {
		t.Fatalf("a requirement edit kept the stored pass: %#v", result)
	}
}

// digestSubjectEntriesRule1 is a frozen copy of the 44ab844 framing (subject
// rule 1: every tracked file, no rule tag). It exists only so the upgrade
// regression below can construct the digest an old binary recorded.
func digestSubjectEntriesRule1(entries []subjectEntry) string {
	h := sha256.New()
	var length [8]byte
	write := func(data []byte) {
		binary.BigEndian.PutUint64(length[:], uint64(len(data)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(data)
	}
	for _, entry := range entries {
		write([]byte(entry.path))
		_, _ = h.Write([]byte{byte(entry.kind)})
		write(entry.payload)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// verifies: AIRA-285 invariant I2a -- a result and proof recorded under the old
// subject rule are never served after the upgrade. The tree tracks no ticket or
// finding, so its entry set is identical under both rules; only the rule tag in
// the framing can tell the two digests apart. GateCheck compares a result's
// canary_tree_digest with its linked proof's, never with a freshly derived
// canary tree, so without the tag an obsolete proof-of-fire would be accepted.
// The control forgery, keyed on the current digest, proves the forged rows are
// otherwise valid, so the miss is caused by the tag alone.
func TestOldSubjectRuleResultIsNotServed(t *testing.T) {
	s, definition, canary := provenGateFixture(t)
	entries, err := captureSubjectEntries(s.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if gate.ExcludedFromSubject(entry.path) {
			t.Fatalf("fixture tracks bookkeeping path %s; the test needs an entry set identical under both rules", entry.path)
		}
	}
	forgeProvenPass(t, s, definition, canary, digestSubjectEntriesRule1(entries), "", nil)
	if result := onlyGateResult(t, s); result.Verdict != gate.VerdictUnevaluated || result.Code != "U_GATE_NO_RESULT" || result.Trusted {
		t.Fatalf("a rule-1 result was served: %#v", result)
	}
	current, err := subjectTreeDigest(s.root)
	if err != nil {
		t.Fatal(err)
	}
	forgeProvenPass(t, s, definition, canary, current, "", nil)
	if result := onlyGateResult(t, s); result.Verdict != gate.VerdictPass || !result.Trusted {
		t.Fatalf("control: the same forgery on the current digest is not a pass: %#v", result)
	}
}

// verifies: AIRA-285 -- the store side of the canary refusal. A gate whose
// canary targets a ticket path fails before any launch and records nothing; a
// pass recorded while the canary was still valid reads stale once the canary
// is rewritten to target bookkeeping, naming the declaration.
func TestCanaryTargetingBookkeepingIsRefusedByTheStore(t *testing.T) {
	s, definition, canary := provenGateFixture(t)
	subject, err := subjectTreeDigest(s.root)
	if err != nil {
		t.Fatal(err)
	}
	forgeProvenPass(t, s, definition, canary, subject, "", nil)
	if result := onlyGateResult(t, s); result.Verdict != gate.VerdictPass {
		t.Fatalf("precondition: forged pass is not served: %#v", result)
	}
	canary.Seed.Files[".aira/tickets/zz.md"] = "seeded ticket\n"
	writeGateFixture(t, s.root, definition, canary)
	before := gateLedgerRecords(t, s)
	if _, err := s.RunGate(context.Background(), definition.ID); err == nil || ErrorCode(err) != "E_GATE_CANARY_INVALID" || !strings.Contains(err.Error(), "AIRA-285") {
		t.Fatalf("run gate with a bookkeeping canary: err=%v", err)
	}
	if after := gateLedgerRecords(t, s); len(after) != len(before) {
		t.Fatalf("a refused run appended %d ledger records", len(after)-len(before))
	}
	result := onlyGateResult(t, s)
	if result.Verdict != gate.VerdictUnevaluated || result.Code != "U_GATE_PROOF_STALE" || !reflect.DeepEqual(result.Mismatch, []string{"declaration"}) {
		t.Fatalf("pass behind a now-invalid canary: %#v", result)
	}
}

// --- AIRA-286 ---------------------------------------------------------------

func copyFields(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// verifies: AIRA-286 -- every binding comparison that refuses a stored pass
// names itself, and only itself. Each row starts from a real RunGate result R
// and its proof P and changes one thing: a field of a copy of R (site 1), a
// field of a copy of P re-linked from a copy of R (site 2), the result's age
// (site 4), or the current gate and canary files. Where both R and P carry a
// field, the forged P carries the same value, so only the comparison under test
// can trip. The two control rows prove the forging harness alone does not make
// a pass stale, so a stale row cannot pass for the wrong reason.
func TestStaleProofNamesEveryMismatchedBinding(t *testing.T) {
	type forge struct {
		result map[string]string // fields of R to override
		proof  map[string]string // fields of P to override (a new P is linked when non-nil)
	}
	rows := []struct {
		name  string
		forge *forge
		files func(t *testing.T, s *Store, def gate.GateDefinition, canary gate.CanaryDeclaration)
		want  []string // nil: control row, expects a trusted pass
	}{
		{name: "control/result copy", forge: &forge{result: map[string]string{}}},
		{name: "control/proof and result copy", forge: &forge{result: map[string]string{}, proof: map[string]string{}}},
		{name: "site1/definition_digest", forge: &forge{result: map[string]string{"definition_digest": "x"}, proof: map[string]string{"definition_digest": "x"}}, want: []string{"definition"}},
		{name: "site1/declaration_digest", forge: &forge{result: map[string]string{"declaration_digest": "x"}, proof: map[string]string{"declaration_digest": "x"}}, want: []string{"declaration"}},
		{name: "site1/subject_scope", forge: &forge{result: map[string]string{"subject_scope": "x"}}, want: []string{"subject"}},
		{name: "site1/lane", forge: &forge{result: map[string]string{"lane": "x"}}, want: []string{"lane"}},
		{name: "site1/evaluator_version", forge: &forge{result: map[string]string{"evaluator_version": "x"}}, want: []string{"lane"}},
		{name: "site1/lane and evaluator_version name lane once", forge: &forge{result: map[string]string{"lane": "x", "evaluator_version": "x"}}, want: []string{"lane"}},
		{name: "site1/empty canary_tree_digest", forge: &forge{result: map[string]string{"canary_tree_digest": ""}}, want: []string{"proof"}},
		{name: "site2/gate_id", forge: &forge{result: map[string]string{}, proof: map[string]string{"gate_id": "other-gate"}}, want: []string{"proof"}},
		{name: "site2/canary_id", forge: &forge{result: map[string]string{}, proof: map[string]string{"canary_id": "other-canary"}}, want: []string{"declaration"}},
		{name: "site2/definition_digest", forge: &forge{result: map[string]string{}, proof: map[string]string{"definition_digest": "x"}}, want: []string{"definition"}},
		{name: "site2/declaration_digest", forge: &forge{result: map[string]string{}, proof: map[string]string{"declaration_digest": "x"}}, want: []string{"declaration"}},
		{name: "site2/canary_tree_digest", forge: &forge{result: map[string]string{}, proof: map[string]string{"canary_tree_digest": "other-tree"}}, want: []string{"proof"}},
		{name: "site2/subject_scope", forge: &forge{result: map[string]string{}, proof: map[string]string{"subject_scope": "x"}}, want: []string{"subject"}},
		{name: "site2/lane", forge: &forge{result: map[string]string{}, proof: map[string]string{"lane": "x"}}, want: []string{"lane"}},
		{name: "site2/evaluator_version", forge: &forge{result: map[string]string{}, proof: map[string]string{"evaluator_version": "x"}}, want: []string{"lane"}},
		{name: "site4/age", forge: &forge{result: map[string]string{"at": time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)}}, want: []string{"age"}},
		{name: "site4/unparseable at", forge: &forge{result: map[string]string{"at": "garbage"}}, want: []string{"age"}},
		{name: "current/canary file deleted", files: func(t *testing.T, s *Store, def gate.GateDefinition, canary gate.CanaryDeclaration) {
			if err := os.Remove(filepath.Join(s.root, ".aira", "gates", "canaries", canary.ID+".json")); err != nil {
				t.Fatal(err)
			}
		}, want: []string{"declaration"}},
		{name: "current/definition and lane", files: func(t *testing.T, s *Store, def gate.GateDefinition, canary gate.CanaryDeclaration) {
			def.Lane.EvaluatorVersion = "changed"
			writeGateFixture(t, s.root, def, canary)
		}, want: []string{"definition", "lane"}},
		// Code order trips declaration (the missing canary) before definition;
		// the vocabulary order puts definition first.
		{name: "current/definition and canary deleted, vocabulary order", files: func(t *testing.T, s *Store, def gate.GateDefinition, canary gate.CanaryDeclaration) {
			def.Description = "edited"
			writeGateFixture(t, s.root, def, canary)
			if err := os.Remove(filepath.Join(s.root, ".aira", "gates", "canaries", canary.ID+".json")); err != nil {
				t.Fatal(err)
			}
		}, want: []string{"definition", "declaration"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s, definition := newProvenGateStore(t)
			canary, err := s.canaryFor(definition)
			if err != nil {
				t.Fatal(err)
			}
			if row.forge != nil {
				records := gateLedgerRecords(t, s)
				result := copyFields(latestGateRecord(t, records, "result", definition.ID).Fields)
				if row.forge.proof != nil {
					proof := copyFields(latestGateRecord(t, records, "proof-of-fire", definition.ID).Fields)
					for key, value := range row.forge.proof {
						proof[key] = value
					}
					forged := appendGateRecord(t, s, "proof-of-fire", proof)
					result["proof_seq"] = strconv.FormatUint(forged.Seq, 10)
				}
				for key, value := range row.forge.result {
					result[key] = value
				}
				appendGateRecord(t, s, "result", result)
			}
			if row.files != nil {
				row.files(t, s, definition, canary)
			}
			for pass := 0; pass < 2; pass++ {
				result := onlyGateResult(t, s)
				if row.want == nil {
					if result.Verdict != gate.VerdictPass || !result.Trusted || result.Code != "" || result.Mismatch != nil {
						t.Fatalf("control row is not a clean trusted pass: %#v", result)
					}
					continue
				}
				if result.Verdict != gate.VerdictUnevaluated || result.Code != "U_GATE_PROOF_STALE" || result.Trusted || !result.Suspect {
					t.Fatalf("not refused as stale: %#v", result)
				}
				if !reflect.DeepEqual(result.Mismatch, row.want) {
					t.Fatalf("Mismatch = %#v, want %#v", result.Mismatch, row.want)
				}
			}
		})
	}
}

// commandGateFiles writes a command gate and its inject-file canary as files.
// GateCheck never launches a process, so the env rows need no runner: the
// records are forged for this definition.
func commandGateFiles(t *testing.T, envName string) (*Store, gate.GateDefinition, gate.CanaryDeclaration) {
	t.Helper()
	base, root := t.TempDir(), filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", ".")
	definition := gate.GateDefinition{SchemaVersion: 2, ID: "env-command", Name: "Env command", Kind: gate.KindCheckable,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "shell", Checker: string(gate.CheckerCommand), EvaluatorVersion: "1"},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 3600, RequireCurrentCanary: true}, CanaryIDs: []string{"env-command-mutation"},
		Command: &gate.Command{Argv: []string{"/bin/true"}, Cwd: "root", EnvAllow: []string{envName}, TimeoutMS: 1000, Predicate: gate.CommandPredicateExitZero}, Enabled: true}
	canary := gate.CanaryDeclaration{SchemaVersion: 1, ID: "env-command-mutation", GateID: definition.ID, Mode: gate.CanaryMutation,
		Mutation:           &gate.MutationSeed{SchemaVersion: 1, Kind: "inject-file", Seed: 1, File: "tests/canary.txt", Content: "canary\n", ExpectedResult: gate.VerdictFail},
		ExpectedGateResult: gate.VerdictFail, LaneBinding: "shell", Isolation: gate.IsolationTempGit, Cadence: gate.CadenceOnDemand}
	writeGateFixture(t, root, definition, canary)
	return testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state")), definition, canary
}

// forgeCommandPass forges a current trusted pass for the command gate, its env
// binding computed the way GateCheck computes it.
func forgeCommandPass(t *testing.T, s *Store, definition gate.GateDefinition, canary gate.CanaryDeclaration, edit func(proof, result map[string]string)) {
	t.Helper()
	subject, err := subjectTreeDigest(s.root)
	if err != nil {
		t.Fatal(err)
	}
	envDigest, err := currentCommandEnvDigest(*definition.Command)
	if err != nil {
		t.Fatal(err)
	}
	forgeProvenPass(t, s, definition, canary, subject, envDigest, edit)
}

// verifies: AIRA-286 -- the env binding is named at both sites that compare
// it: the proof's env digest against the result's (site 2) and the current
// process's allow-listed environment against the result's (site 3, the
// daemon-versus-shell PATH trap the peer hit).
func TestStaleProofNamesTheEnvBinding(t *testing.T) {
	const name = "AIRA_GATE_FAMILY_ENV"
	t.Run("control", func(t *testing.T) {
		t.Setenv(name, "one")
		s, definition, canary := commandGateFiles(t, name)
		forgeCommandPass(t, s, definition, canary, nil)
		if result := onlyGateResult(t, s); result.Verdict != gate.VerdictPass || !result.Trusted || result.Mismatch != nil {
			t.Fatalf("forged command pass is not a clean trusted pass: %#v", result)
		}
	})
	t.Run("site2/proof env_digest", func(t *testing.T) {
		t.Setenv(name, "one")
		s, definition, canary := commandGateFiles(t, name)
		forgeCommandPass(t, s, definition, canary, func(proof, _ map[string]string) { proof["env_digest"] = "x" })
		if result := onlyGateResult(t, s); result.Code != "U_GATE_PROOF_STALE" || !reflect.DeepEqual(result.Mismatch, []string{"env"}) {
			t.Fatalf("proof env mismatch: %#v", result)
		}
	})
	t.Run("site3/current environment", func(t *testing.T) {
		t.Setenv(name, "one")
		s, definition, canary := commandGateFiles(t, name)
		forgeCommandPass(t, s, definition, canary, nil)
		t.Setenv(name, "two")
		if result := onlyGateResult(t, s); result.Code != "U_GATE_PROOF_STALE" || !reflect.DeepEqual(result.Mismatch, []string{"env"}) {
			t.Fatalf("current env mismatch: %#v", result)
		}
	})
}

// verifies: AIRA-286 -- the name reaches the two places a user reads it: the
// `ready` finding and the `aira check` gates finding. Codes and kinds stay as
// they were; only the message gains the binding.
func TestReadyAndCheckFindingsNameTheBinding(t *testing.T) {
	const name = "AIRA_GATE_FAMILY_READY_ENV"
	t.Setenv(name, "one")
	s, definition, canary := commandGateFiles(t, name)
	forgeCommandPass(t, s, definition, canary, nil)
	if err := os.MkdirAll(filepath.Join(s.root, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTicketFile(t, filepath.Join(s.root, ".aira", "tickets", "AIRA-1.md"), "AIRA-1")
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	t.Setenv(name, "two")
	want := "gate " + definition.ID + " is unevaluated (does not match: env)"
	rows, err := s.Ready("")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		for _, finding := range row.Findings {
			if finding.Code == "U_GATE_PROOF_STALE" && finding.Subject == definition.ID {
				if finding.Message != want || finding.Kind != gate.VerdictUnevaluated {
					t.Fatalf("ready finding = %#v, want message %q", finding, want)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("ready carries no stale-proof finding: %#v", rows)
	}
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, finding := range append(append([]CheckFinding{}, report.Findings...), report.UnevaluatedFindings...) {
		if finding.Code == "U_GATE_PROOF_STALE" && finding.Subject == definition.ID {
			if finding.Message != want || finding.Kind != "unevaluated" {
				t.Fatalf("check finding = %#v, want message %q", finding, want)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("check carries no stale-proof finding: %#v", report)
	}
}

// verifies: AIRA-286 -- the message helper names bindings only for a stale
// proof, and is the bare verdict otherwise.
func TestGateFindingMessage(t *testing.T) {
	for _, row := range []struct {
		result GateCheckResult
		want   string
	}{
		{GateCheckResult{GateID: "g", Verdict: "unevaluated", Code: "U_GATE_PROOF_STALE", Mismatch: []string{"definition", "env"}}, "gate g is unevaluated (does not match: definition, env)"},
		{GateCheckResult{GateID: "g", Verdict: "unevaluated", Code: "U_GATE_NO_RESULT"}, "gate g is unevaluated"},
		{GateCheckResult{GateID: "g", Verdict: "fail", Code: "E_GATE_FAILED"}, "gate g is fail"},
	} {
		if got := gateFindingMessage(row.result); got != row.want {
			t.Errorf("gateFindingMessage(%#v) = %q, want %q", row.result, got, row.want)
		}
	}
}

// --- AIRA-288 ---------------------------------------------------------------

// verifies: AIRA-288 invariant I5 -- a run id is a reference, never a binding.
// A forged otherwise-valid result naming a run that does not exist is still a
// trusted pass, and GateCheck hands the id back unchanged.
func TestRunIDIsNotABinding(t *testing.T) {
	s, definition, canary := provenGateFixture(t)
	subject, err := subjectTreeDigest(s.root)
	if err != nil {
		t.Fatal(err)
	}
	forgeProvenPass(t, s, definition, canary, subject, "", func(_, result map[string]string) {
		result["run_id"], result["canary_run_id"] = "RUN-404", "RUN-405"
	})
	result := onlyGateResult(t, s)
	if result.Verdict != gate.VerdictPass || !result.Trusted {
		t.Fatalf("a run id that names no run cost the pass: %#v", result)
	}
	if result.RunID != "RUN-404" || result.CanaryRunID != "RUN-405" {
		t.Fatalf("GateCheck did not return the recorded run ids: %#v", result)
	}
}

// verifies: AIRA-288 -- a dimension gate launches no process, so it records no
// run id and returns none; the absence is shown by the absent fields.
func TestTraceabilityGateRecordsNoRunID(t *testing.T) {
	s, definition := newProvenGateStore(t)
	result, err := s.RunGate(context.Background(), definition.ID)
	if err != nil || result.Verdict != gate.VerdictPass {
		t.Fatalf("run: %#v err=%v", result, err)
	}
	if result.RunID != "" || result.CanaryRunID != "" {
		t.Fatalf("dimension gate returned run ids: %#v", result)
	}
	record := latestGateRecord(t, gateLedgerRecords(t, s), "result", definition.ID)
	if _, ok := record.Fields["run_id"]; ok {
		t.Fatalf("dimension result recorded run_id: %#v", record.Fields)
	}
	if _, ok := record.Fields["canary_run_id"]; ok {
		t.Fatalf("dimension result recorded canary_run_id: %#v", record.Fields)
	}
}

// sequencedExecution hands out one canned record per launch, in order, and
// reports incomplete output for the run ids named in incomplete.
type sequencedExecution struct {
	records    []runner.RunRecord
	incomplete map[string]bool
	launches   int
}

func (e *sequencedExecution) Launch(context.Context, runner.Request) (*runner.RunRecord, error) {
	if e.launches >= len(e.records) {
		return nil, errors.New("unexpected launch")
	}
	record := e.records[e.launches]
	e.launches++
	return &record, nil
}

func (e *sequencedExecution) ReadOutput(_ context.Context, request runner.OutputRequest) (*runner.OutputChunk, error) {
	if e.incomplete[request.RunID] {
		return &runner.OutputChunk{RunID: request.RunID, Stream: request.Stream}, nil
	}
	return &runner.OutputChunk{RunID: request.RunID, Stream: request.Stream, Complete: true, OutputState: runner.OutputComplete}, nil
}

func cleanStubRecord(id, envDigest string) runner.RunRecord {
	zero := 0
	return runner.RunRecord{SchemaVersion: 1, ID: id, Status: runner.StatusExited, ExitCode: &zero, EnvDigest: envDigest,
		ScopeIntegrity: runner.ScopeContained, CaptureComplete: true, TerminalComplete: true,
		OutputRefs: map[string]runner.OutputRef{
			"out": {Path: "out", State: runner.OutputComplete},
			"err": {Path: "err", State: runner.OutputComplete},
		}}
}

// verifies: AIRA-288 -- when the canary run cannot be evaluated, the
// unevaluated result still records both run ids, and the returned error names
// them after the code, so the error still classifies by its code.
func TestCanaryErrorRecordsCanaryRunID(t *testing.T) {
	const name = "AIRA_GATE_FAMILY_STUB_ENV"
	t.Setenv(name, "one")
	s, definition, _ := commandGateFiles(t, name)
	envDigest, err := currentCommandEnvDigest(*definition.Command)
	if err != nil {
		t.Fatal(err)
	}
	s.SetRunner(&sequencedExecution{
		records:    []runner.RunRecord{cleanStubRecord("RUN-1", envDigest), cleanStubRecord("RUN-2", envDigest)},
		incomplete: map[string]bool{"RUN-2": true},
	})
	_, err = s.RunGate(context.Background(), definition.ID)
	if err == nil {
		t.Fatal("an unreadable canary run did not surface as an error")
	}
	if ErrorCode(err) != "U_GATE_COMMAND_RUN_UNEVALUATED" {
		t.Fatalf("error code = %q (%v)", ErrorCode(err), err)
	}
	if !strings.Contains(err.Error(), "subject run RUN-1") || !strings.Contains(err.Error(), "canary run RUN-2") {
		t.Fatalf("error does not name the runs: %v", err)
	}
	record := latestGateRecord(t, gateLedgerRecords(t, s), "result", definition.ID)
	if record.Fields["verdict"] != gate.VerdictUnevaluated || record.Fields["run_id"] != "RUN-1" || record.Fields["canary_run_id"] != "RUN-2" {
		t.Fatalf("unevaluated result record = %#v", record.Fields)
	}
	result := onlyGateResult(t, s)
	if result.RunID != "RUN-1" || result.CanaryRunID != "RUN-2" || result.Verdict == gate.VerdictPass {
		t.Fatalf("gate check = %#v", result)
	}
}

// --- AIRA-287 ---------------------------------------------------------------

func ledgerBytes(t *testing.T, s *Store) []byte {
	t.Helper()
	audit, err := OpenGateAudit(s.commonDir, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(audit.LedgerPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return data
}

// verifies: AIRA-287 -- canary-run reports the canary's own outcome (fired,
// did not fire, could not tell) beside the gate's result on the canary tree,
// and writes nothing to the gate ledger. `fired` is JSON null whenever the
// canary-tree result is not established.
func TestCanaryRunReportsFired(t *testing.T) {
	s, definition, canary := provenGateFixture(t)
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "fixture", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	requirementData, err := domain.RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		name      string
		seed      map[string]string
		verdict   string
		fired     string
		gateState gate.PredicateState
	}{
		{name: "fires", seed: canary.Seed.Files, verdict: "pass", fired: "true", gateState: gate.PredicateFail},
		{name: "does not fire", seed: map[string]string{
			".aira/requirements/AR-1.md": string(requirementData),
			"implementation.go":          "package fixture\n// covers: AR-1\nfunc Fixture() {}\n",
			"implementation_test.go":     "package fixture\n// verifies: AR-1\nfunc TestFixture(t *testing.T) {}\n",
		}, verdict: "fail", fired: "false", gateState: gate.PredicatePass},
		{name: "unevaluated", seed: map[string]string{
			"implementation.go": "package fixture\n// covers: AR-1\nfunc Fixture() {}\n",
		}, verdict: "unevaluated", fired: "null", gateState: gate.PredicateUnevaluated},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rewritten := canary
			rewritten.Seed = gate.Seed{Files: row.seed}
			writeGateFixture(t, s.root, definition, rewritten)
			before := ledgerBytes(t, s)
			value, err := s.GateAction(context.Background(), "canary-run", "", canary.ID)
			if err != nil {
				t.Fatal(err)
			}
			result, ok := value.(CanaryRunResult)
			if !ok {
				t.Fatalf("canary-run returned %T", value)
			}
			if result.Verdict != row.verdict || result.GateResult != row.gateState || result.CanaryID != canary.ID || result.GateID != definition.ID || result.TreeDigest == "" {
				t.Fatalf("result = %#v", result)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `"fired":`+row.fired) {
				t.Fatalf("encoded = %s, want fired %s", encoded, row.fired)
			}
			if after := ledgerBytes(t, s); !bytes.Equal(before, after) {
				t.Fatal("canary-run changed the gate ledger")
			}
		})
	}
}
