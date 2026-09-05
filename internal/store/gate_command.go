package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aira/internal/gate"
	"aira/internal/runner"
)

// evaluateChecker dispatches on the lane's checker. Every checker receives an
// already-captured subject rather than a root path: no evaluator re-reads the
// tree it is evaluating (AIRA-80).
func (s *Store) evaluateChecker(ctx context.Context, def gate.GateDefinition, subject capturedSubject) (DimensionEvaluation, error) {
	// A subject with no digest was never captured. captureSubject always sets one
	// -- even an empty tracked tree digests to the hash of no entries -- so an
	// empty digest can only be the zero value. No lane may evaluate against it:
	// the command lane would materialise an EMPTY tree and could pass a checker
	// that only fails on real content, and any verdict would be bound to the
	// empty-string digest. One guard at the dispatch point covers all three lanes.
	if subject.digest == "" {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_EVIDENCE_UNAVAILABLE"}, errors.New("U_GATE_EVIDENCE_UNAVAILABLE: subject was not captured")
	}
	switch def.Lane.Checker {
	case string(gate.CheckerDimension):
		if def.Checkable == nil {
			return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_EVIDENCE_UNAVAILABLE"}, errors.New("U_GATE_EVIDENCE_UNAVAILABLE: checkable payload is missing")
		}
		return evaluateDimension(subject, def.Checkable.Dimension)
	case string(gate.CheckerCommand):
		return s.runCommandChecker(ctx, def, subject)
	default:
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_COMMAND_RUN_UNEVALUATED"}, fmt.Errorf("E_GATE_INVALID: unsupported checker %q", def.Lane.Checker)
	}
}

func (s *Store) runCommandChecker(ctx context.Context, def gate.GateDefinition, subject capturedSubject) (DimensionEvaluation, error) {
	if def.Command == nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_COMMAND_RUN_UNEVALUATED"}, errors.New("U_GATE_COMMAND_RUN_UNEVALUATED: command payload is missing")
	}
	if s.runner == nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_COMMAND_RUN_UNEVALUATED"}, errors.New("U_GATE_COMMAND_RUN_UNEVALUATED: runner is unavailable")
	}
	command := def.Command.Normalized()
	if err := command.Validate(); err != nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_COMMAND_RUN_UNEVALUATED"}, err
	}
	snapshot, cleanup, err := materializeSubject(subject)
	if err != nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_COMMAND_RUN_UNEVALUATED"}, fmt.Errorf("U_GATE_COMMAND_RUN_UNEVALUATED: materialize subject: %w", err)
	}
	defer cleanup()
	rootDigest := subject.digest
	cwd, err := resolveCommandCwd(snapshot, command.Cwd)
	if err != nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_COMMAND_RUN_UNEVALUATED", Root: EvaluationRoot{Path: snapshot, Digest: rootDigest}}, err
	}
	env, entries, err := allowListedEnvironment(command.EnvAllow)
	if err != nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_COMMAND_RUN_UNEVALUATED", Root: EvaluationRoot{Path: snapshot, Digest: rootDigest}}, err
	}
	envDigest, err := runner.EnvDigest(entries)
	if err != nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_COMMAND_RUN_UNEVALUATED", Root: EvaluationRoot{Path: snapshot, Digest: rootDigest}}, err
	}
	record, launchErr := s.runner.Launch(ctx, runner.Request{Argv: command.Argv, Cwd: cwd, Env: env, ExplicitEnv: true, Timeout: time.Duration(command.TimeoutMS) * time.Millisecond})
	base := DimensionEvaluation{Root: EvaluationRoot{Path: snapshot, Digest: rootDigest}}
	if launchErr != nil || record == nil {
		base.Predicate, base.Evidence, base.Code = gate.PredicateUnevaluated, false, "U_GATE_COMMAND_RUN_UNEVALUATED"
		if record != nil {
			base.RunID = record.ID
		}
		return base, fmt.Errorf("%s: %w", base.Code, launchErr)
	}
	base.RunID = record.ID
	authoritativeEnvDigest, err := commandEnvDigestForRecord(envDigest, *record)
	if err != nil {
		base.Predicate, base.Evidence, base.Code = gate.PredicateUnevaluated, false, "U_GATE_COMMAND_RUN_UNEVALUATED"
		return base, fmt.Errorf("%s: %w", base.Code, err)
	}
	base.EnvDigest = authoritativeEnvDigest
	if hasRunnerCode(record, "E_RUN_TIMEOUT") {
		base.Predicate, base.Evidence, base.Code = gate.PredicateUnevaluated, false, "U_GATE_COMMAND_TIMEOUT"
		return base, nil
	}
	admissible, cleanExit, reason := admissibleCommandRun(*record)
	if !admissible {
		base.Predicate, base.Evidence, base.Code = gate.PredicateUnevaluated, false, "U_GATE_COMMAND_RUN_UNEVALUATED"
		if reason != "" {
			base.Code = reason
		}
		return base, nil
	}
	if hasRunnerCodeExcept(record, "E_RUN_FAILED") {
		base.Predicate, base.Evidence, base.Code = gate.PredicateUnevaluated, false, "U_GATE_COMMAND_RUN_UNEVALUATED"
		return base, nil
	}
	out, err := readCompleteOutput(ctx, s.runner, *record, "out")
	if err != nil {
		base.Predicate, base.Evidence, base.Code = gate.PredicateUnevaluated, false, "U_GATE_COMMAND_RUN_UNEVALUATED"
		return base, err
	}
	stderr, err := readCompleteOutput(ctx, s.runner, *record, "err")
	if err != nil {
		base.Predicate, base.Evidence, base.Code = gate.PredicateUnevaluated, false, "U_GATE_COMMAND_RUN_UNEVALUATED"
		return base, err
	}
	if int64(len(out))+int64(len(stderr)) > command.OutputCapBytes {
		base.Predicate, base.Evidence, base.Code = gate.PredicateUnevaluated, false, "U_GATE_OUTPUT_OVERFLOW"
		return base, nil
	}
	if command.Predicate == gate.CommandPredicateTestsGreen {
		predicate, code := classifyTestsGreen(cleanExit, out)
		if code != "" {
			base.Predicate, base.Evidence, base.Code = predicate, predicate != gate.PredicateUnevaluated, code
			return base, nil
		}
	}
	if !cleanExit {
		base.Predicate, base.Evidence, base.Code = gate.PredicateFail, true, "E_GATE_COMMAND_FAILED"
		return base, nil
	}
	base.Predicate, base.Evidence = gate.PredicatePass, true
	return base, nil
}

func commandEnvDigestForRecord(constructed string, record runner.RunRecord) (string, error) {
	if record.EnvDigest != constructed {
		return "", fmt.Errorf("runner env digest %q differs from constructed child environment %q", record.EnvDigest, constructed)
	}
	return record.EnvDigest, nil
}

func classifyTestsGreen(cleanExit bool, output []byte) (gate.PredicateState, string) {
	if !cleanExit {
		return gate.PredicateFail, "E_GATE_COMMAND_FAILED"
	}
	parsed, parseErr := gate.ParseGoTestJSONV1(output)
	if parseErr != nil {
		return gate.PredicateUnevaluated, "U_GATE_PARSER_INCOMPLETE"
	}
	if parsed.FailedCount != 0 {
		return gate.PredicateFail, "E_GATE_COMMAND_FAILED"
	}
	if parsed.DiscoveredCount == 0 {
		return gate.PredicateUnevaluated, "U_GATE_PARSER_INCOMPLETE"
	}
	return gate.PredicatePass, ""
}

func allowListedEnvironment(names []string) ([]string, []runner.EnvEntry, error) {
	entries := make([]runner.EnvEntry, 0, len(names))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			entries = append(entries, runner.EnvEntry{Key: []byte(name), Value: []byte(value)})
		}
	}
	entries = runner.StripCoordinationEnv(entries)
	sort.Slice(entries, func(i, j int) bool { return string(entries[i].Key) < string(entries[j].Key) })
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		values = append(values, string(entry.Key)+"="+string(entry.Value))
	}
	return values, entries, nil
}

func currentCommandEnvDigest(command gate.Command) (string, error) {
	if err := command.Validate(); err != nil {
		return "", err
	}
	_, entries, err := allowListedEnvironment(command.EnvAllow)
	if err != nil {
		return "", err
	}
	return runner.EnvDigest(entries)
}

func resolveCommandCwd(root, cwd string) (string, error) {
	path := root
	if cwd != "root" {
		path = filepath.Join(root, filepath.FromSlash(cwd))
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("U_GATE_COMMAND_RUN_UNEVALUATED: evaluation root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("E_GATE_INVALID: command cwd: %w", err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("E_GATE_INVALID: command cwd escapes evaluation root")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("E_GATE_INVALID: command cwd is not a directory")
	}
	return resolved, nil
}

func allOutputsComplete(record runner.RunRecord) bool {
	if len(record.OutputRefs) == 0 {
		return false
	}
	for _, ref := range record.OutputRefs {
		if ref.State != runner.OutputComplete || ref.Path == "" || ref.Bytes < 0 {
			return false
		}
	}
	return true
}

// admissibleScopeIntegrity reports whether a run's scope-integrity verdict is
// trustworthy enough to derive a gate verdict from. The command gate's verdict
// rests on the leader's exit code and the completely-captured output, not on
// descendant memory containment, so ScopeUnverified — placement proven but
// whole-subtree containment not attestable, the normal outcome for any command
// that forks children (e.g. `go test`) — is admissible alongside ScopeContained.
// States that evidence a real integrity failure (a descendant killed at teardown
// or witnessed escaping, leader migration, a handoff never verified) stay
// inadmissible: the run was not clean.
func admissibleScopeIntegrity(integrity runner.ScopeIntegrity) bool {
	return integrity == runner.ScopeContained || integrity == runner.ScopeUnverified
}

// admissibleCommandRun deliberately does not delegate to CleanSuccess: command
// gates also consume each output reference and must reject forced capture
// closure before any parser sees bytes.
func admissibleCommandRun(record runner.RunRecord) (admissible, cleanExit bool, reason string) {
	if record.CaptureForcedClosed || record.Status != runner.StatusExited || record.ExitCode == nil || !admissibleScopeIntegrity(record.ScopeIntegrity) || !record.CaptureComplete || !record.TerminalComplete || !allOutputsComplete(record) {
		return false, false, "U_GATE_COMMAND_RUN_UNEVALUATED"
	}
	for _, code := range record.ErrorCodes {
		if code != "E_RUN_FAILED" {
			return false, false, "U_GATE_COMMAND_RUN_UNEVALUATED"
		}
	}
	return true, *record.ExitCode == 0, ""
}

func readCompleteOutput(ctx context.Context, execution Execution, record runner.RunRecord, stream string) ([]byte, error) {
	ref, ok := record.OutputRefs[stream]
	if !ok || ref.State != runner.OutputComplete {
		return nil, errors.New("U_GATE_COMMAND_RUN_UNEVALUATED: output is incomplete")
	}
	chunk, err := execution.ReadOutput(ctx, runner.OutputRequest{RunID: record.ID, Stream: stream, Full: true})
	if err != nil || chunk == nil || chunk.Truncated || !chunk.Complete || chunk.OutputState != runner.OutputComplete || chunk.TotalBytes != ref.Bytes || chunk.NextOffset != ref.Bytes {
		if err == nil {
			err = errors.New("output byte count or completion evidence did not match")
		}
		return nil, fmt.Errorf("U_GATE_COMMAND_RUN_UNEVALUATED: %w", err)
	}
	return chunk.Bytes, nil
}

func hasRunnerCode(record *runner.RunRecord, code string) bool {
	for _, existing := range record.ErrorCodes {
		if existing == code {
			return true
		}
	}
	return false
}

func hasRunnerCodeExcept(record *runner.RunRecord, allowed string) bool {
	for _, code := range record.ErrorCodes {
		if code != allowed {
			return true
		}
	}
	return false
}

// materializeSubject copies an already-captured subject's tracked files into an
// isolated tree a command may execute in.
//
// It takes the capture rather than a root path so the bytes that run and the
// bytes the verdict is bound to (capturedSubject.digest) come from one read by
// construction (AIRA-72 invariant I3, generalised to every lane by AIRA-80).
// Re-reading a root here would instead open a window in which the tree changes
// between the bytes that ran and the bytes that were bound -- a small false
// pass.
//
// The stage is `add -A -f`, not `add -A`. The materialised directory holds
// exactly the captured entries and nothing else -- we wrote every one of them
// and `git init` adds only .git -- so forcing makes the resulting index exactly
// the captured set. Without the force, `git add -A` drops any entry matched by
// the copied .gitignore or the user's core.excludesFile, which is legal for a
// tracked file (`git add -f` in the source) and left the materialised tree's
// tracked set a strict subset of the source's. The mutation canary
// re-materialises from this tree, so that loss made a canary able to fire
// because a file DISAPPEARED rather than because the declared mutation
// perturbed anything -- and a fire mints proof-of-fire, which licenses a
// trusted pass (AIRA-81). Pinned by
// TestMaterializationPreservesTrackedButIgnoredFiles and, end to end, by
// TestIgnoredTrackedFileDropDoesNotMintProofOfFire.
func materializeSubject(subject capturedSubject) (string, func(), error) {
	dir, err := os.MkdirTemp("", "aira-gate-subject-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	for _, entry := range subject.entries {
		// A command runs in a real working tree, so this refuses what it cannot
		// reproduce faithfully rather than dereferencing it. The digest is broader
		// on purpose: it must describe entries the materialiser rejects.
		if !entry.regular() {
			cleanup()
			return "", func() {}, fmt.Errorf("tracked path %s is not a regular file", entry.path)
		}
		path := filepath.Join(dir, filepath.FromSlash(entry.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			cleanup()
			return "", func() {}, err
		}
		if err := os.WriteFile(path, entry.payload, entry.perm.Perm()); err != nil {
			cleanup()
			return "", func() {}, err
		}
		if err := os.Chmod(path, entry.perm.Perm()); err != nil {
			cleanup()
			return "", func() {}, err
		}
	}
	if _, stderr, err := runGit(dir, "init", "-q"); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(stderr))
	}
	if _, stderr, err := runGit(dir, "add", "-A", "-f"); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(stderr))
	}
	return dir, cleanup, nil
}

func trackedSnapshotPaths(root string) ([]string, error) {
	out, stderr, err := runGit(root, "ls-files", "-z", "--cached", "--")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(stderr))
	}
	paths := make([]string, 0)
	for _, raw := range bytes.Split([]byte(out), []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "" {
			continue
		}
		if !gate.SafeRelativePath(path) {
			return nil, fmt.Errorf("invalid tracked path %q", path)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	// `git ls-files --cached` lists an unmerged path once per STAGE, so a
	// conflicted index yields the same path two or three times. Left as-is the
	// capture reads and digests that one file two or three times, while
	// materializeSubject writes it once and stages one entry -- so
	// capture -> materialise -> capture stops being the identity exactly when a
	// repository is mid-conflict, and a command gate's proof-time subject stops
	// agreeing with GateCheck's check-time subject.
	//
	// The fix is to collapse the duplicates, not to refuse them. The subject is
	// defined as the WORKING TREE content of every tracked path (AIRA-72: the
	// index is not the witness, precisely because it can disagree with the
	// worktree), and the worktree has exactly one content per path however many
	// index stages exist. Refusing instead would make every gate hard-fail for the
	// whole duration of any merge conflict anywhere in the tree, including
	// conflicts in files a gate does not look at -- a blanket false fail. A
	// conflicted file still carries its conflict markers into the digest and into
	// whatever the checker runs, so a genuinely broken tree still fails on its own
	// merits, loudly. Pinned by TestCaptureOfAnUnmergedIndexCountsEachPathOnce.
	deduplicated := paths[:0]
	for i, path := range paths {
		if i > 0 && path == paths[i-1] {
			continue
		}
		deduplicated = append(deduplicated, path)
	}
	return deduplicated, nil
}

// Tracked-tree reading now lives in gate_subject.go as captureSubjectEntries,
// shared by the subject digest and this materialiser so the bytes that are
// bound and the bytes that are executed come from one read (AIRA-72).

// The three near-identical normalizing path predicates this file, gate_eval.go
// and gate/canary.go each carried are now the single gate.SafeRelativePath
// (AIRA-60), so the check a gate author sees at declaration time and the check
// evaluation applies are the same check.

// The tracked-tree digest lives in gate_subject.go as subjectTreeDigest, the
// single producer of every gate subject digest. It used to be duplicated here
// with a different scope from the one gate_eval.go used, which is how AIRA-72
// -- a Go-only subject digest on every non-command checker -- survived.

func applyMutation(root string, mutation gate.MutationSeed) error {
	switch mutation.Kind {
	case "go-inject-failing-test":
		return injectFailingTest(root, mutation)
	case "go-negate-assertion":
		return negateAssertion(root, mutation)
	case "inject-file":
		return injectFile(root, mutation)
	default:
		return errors.New("unsupported mutation kind")
	}
}

// injectFile is the language-agnostic mutation kind. It writes the declared
// literal body at the declared relative path inside the isolated mutation
// snapshot, so a project with no Go source can still drive a canary that fires:
// the Go kinds both require go/parser over .go files, which left a command gate
// in any other toolchain permanently unable to reach a canary-proven pass
// (E_GATE_CANARY_DID_NOT_FIRE is a hard fail, never unevaluated).
//
// It is deliberately create-only. O_EXCL refuses an existing target atomically,
// so the mutation is provably additive: it can neither destroy subject content
// nor report an injection it did not make.
//
// A target path matched by the subject's git excludes — its own .gitignore, or
// the user's core.excludesFile — is dropped by the `git add -A` that re-stages
// the mutated snapshot and never reaches the checker's `git ls-files --cached`
// view. That surfaces as E_GATE_CANARY_DID_NOT_FIRE: loud, never a false pass.
//
// The proof this kind yields is weaker than go-inject-failing-test's, and a
// declaration must be written knowing it. go-inject-failing-test injects a
// compiling, failing test, so its fire proves the whole test-failure to
// non-zero-exit pathway. inject-file proves only that the declared perturbation
// produces a non-zero exit. Inject a compiling, failing test in the subject's
// own language; a body that merely breaks the build proves only that the build
// breaks. The concrete honest-mistake false pass this admits: a `make test`
// recipe that aborts on a compile error but swallows real test failures fires
// the canary on a syntax-broken injection and would never fire on a failing
// test, so the lane earns a trusted pass it cannot actually back.
func injectFile(root string, mutation gate.MutationSeed) error {
	// The declaration validator already refuses an unsafe path, but this writes
	// a new file, so the check is repeated locally rather than trusted from a
	// caller. gate.SafeRelativePath is the same predicate the tracked snapshot
	// and the declaration validator use: it refuses an absolute path, any ..
	// traversal, and any .git segment. The last matters most here — a write into
	// the snapshot's own .git (a config carrying core.fsmonitor, say) would be
	// executed by the git add that re-stages the mutation.
	if !gate.SafeRelativePath(mutation.File) {
		return errors.New("mutation file escapes the snapshot root")
	}
	if mutation.Content == "" {
		return errors.New("mutation content is empty")
	}
	path := filepath.Join(root, filepath.FromSlash(mutation.File))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("mutation target is not injectable: %w", err)
	}
	if _, err := file.WriteString(mutation.Content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func injectFailingTest(root string, mutation gate.MutationSeed) error {
	packageDir := filepath.Join(root, filepath.FromSlash(mutation.PkgDir))
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		return err
	}
	packageName := ""
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || entry.Name() == "aira_m10b_mutation_test.go" {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), filepath.Join(packageDir, entry.Name()), nil, 0)
		if parseErr == nil {
			packageName = file.Name.Name
			break
		}
	}
	if packageName == "" {
		return errors.New("package source is unavailable")
	}
	path := filepath.Join(packageDir, "aira_m10b_mutation_test.go")
	content := fmt.Sprintf("package %s\n\nimport \"testing\"\n\nfunc %s(t *testing.T) {\n\tt.Fatal(\"AIRA mutation\")\n}\n", packageName, mutation.TestName)
	return os.WriteFile(path, []byte(content), 0o644)
}

func negateAssertion(root string, mutation gate.MutationSeed) error {
	path := filepath.Join(root, filepath.FromSlash(mutation.File))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("mutation file is unavailable")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return err
	}
	var target *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == mutation.Test {
			target = function
			break
		}
	}
	if target == nil || target.Body == nil {
		return errors.New("mutation test function is unavailable")
	}
	occurrence := 0
	var selected *ast.IfStmt
	ast.Inspect(target.Body, func(node ast.Node) bool {
		if statement, ok := node.(*ast.IfStmt); ok {
			occurrence++
			if occurrence == mutation.Occurrence {
				selected = statement
				return false
			}
		}
		return selected == nil
	})
	if selected == nil {
		return errors.New("assertion occurrence is unavailable")
	}
	selected.Cond = &ast.UnaryExpr{OpPos: selected.Cond.Pos(), Op: token.NOT, X: selected.Cond}
	var output bytes.Buffer
	if err := format.Node(&output, fset, file); err != nil {
		return err
	}
	return os.WriteFile(path, output.Bytes(), info.Mode().Perm())
}
