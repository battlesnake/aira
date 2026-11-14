#!/usr/bin/env python3
"""AIRA-323 mutation check: every named mutation must turn its tests red.

Each mutation replaces one exact snippet in one production file, runs the
focused tests that are meant to catch it, and restores the file. A mutation
whose tests still pass is a porous test and fails this script.

Run from anywhere, under aira confine (it builds and runs Go tests), with a
short TMPDIR (the daemon tests bind unix sockets):

    TMPDIR=~/tmp/t aira confine --name <unique> -- python3 docs/dev/aira-323-mutation-check.py

Optional arguments run only the mutations whose name contains one of them.

Exit 0: every mutation was caught. Exit 1: at least one survived (or a snippet
no longer matches the source, which also fails: the list must track the code).
"""
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]

STORE = "internal/store/confine_peak_history.go"
SCHEMA = "internal/store/store.go"
HANDLER = "internal/daemon/confine_report.go"
SENDER = "internal/runner/admission_linux.go"
LAUNCH = "internal/runner/confine_linux.go"
IDENT = "internal/runner/repo_identity.go"

T_STORE = ("./internal/store/", "Telemetry|RecordConfinePeakStoresTheTelemetryAsGiven")
T_T1 = ("./internal/daemon/", "TestConfineReportCompletionFactsEndToEnd")
T_T2 = ("./internal/daemon/", "TestConfineReportCompletionAbsentIsNullForEveryKind")
T_T3 = ("./internal/daemon/", "TestConfineReportCompletionValidation")
T_T9 = ("./internal/daemon/", "TestConfineReportVersionSkewResend")
T_T4 = ("./internal/runner/", "TestConfineCompletionTimingComposite|TestConfineCompletionWaitOnlyForADaemonGrant")
T_T4B = ("./internal/runner/", "TestConfineCompletionProductionSeams")
T_T5 = ("./internal/runner/", "TestRepoIdentityOnRealRepositories")
T_T6 = ("./internal/runner/", "TestConfineCompletionRepoIdentity")

WAIT_RULE = """	if admission.release != nil {
		waited := (admitReturned - admitEntered).Milliseconds()
		waitMS = &waited
	}"""
WALL_END = "		wallMS := (deps.monoNow() - admitReturned).Milliseconds()"
REPO_CALL = """	repoID, repoOK := deps.repoIdentity()
	if !repoOK {
		repoID = ""
	}"""
ADMIT_CALL = """	admitEntered := deps.monoNow()
	admission, err := deps.admit(ctx, path, request, reserve)"""
STARTED = "	startedAtMS := deps.wallNow().UnixMilli()\n"
RESEND = """	if errors.Is(err, errReportCompletionFactsRefused) {
		return reportPeakExchangeOnce(ctx, socketPath, confinePeakReportArgs(report, false))
	}"""
RETENTION = "WHERE kind=? AND signature=? AND rowid NOT IN (\n SELECT rowid FROM confine_peak_history WHERE kind=? AND signature=? ORDER BY at DESC,rowid DESC LIMIT ?\n)`, string(observation.Kind), observation.Signature, string(observation.Kind), observation.Signature, confinePeakHistoryLimit)"

# (name, [(file, old, new), ...], test)
MUTATIONS = [
    # --- store (writer, T7, T8)
    ("store: absent duration stored as 0", [(STORE, "	if value == nil {\n		return nil\n	}\n	return *value", "	if value == nil {\n		return int64(0)\n	}\n	return *value")], T_STORE),
    ("store: absent repo stored as empty string", [(STORE, "	if observation.Repo != \"\" {\n		repoValue = observation.Repo\n	}", "	repoValue = observation.Repo")], T_STORE),
    ("T7: retention per (signature, repo)", [(STORE, RETENTION, RETENTION.replace("signature=? AND rowid", "signature=? AND COALESCE(repo,'')=? AND rowid").replace("signature=? ORDER", "signature=? AND COALESCE(repo,'')=? ORDER").replace("observation.Signature, string(observation.Kind), observation.Signature, confine", "observation.Signature, observation.Repo, string(observation.Kind), observation.Signature, observation.Repo, confine"))], T_STORE),
    ("T8: one column omitted from the ALTER list", [(SCHEMA, "		{\"repo\", `ALTER TABLE confine_peak_history ADD COLUMN repo TEXT`},\n", "")], T_STORE),
    ("T8: ALTER with DEFAULT 0", [(SCHEMA, "ADD COLUMN wall_ms INTEGER`", "ADD COLUMN wall_ms INTEGER DEFAULT 0`")], T_STORE),
    # --- handler (T1, T2, T3)
    ("T1: wall_ms left out of the allowlist", [(HANDLER, "	\"wall_ms\": true, ", "	")], T_T1),
    ("T1: wait_ms left out of the allowlist", [(HANDLER, "\"wait_ms\": true, ", "")], T_T1),
    ("T1: started_at_ms left out of the allowlist", [(HANDLER, "\"started_at_ms\": true, ", "")], T_T1),
    ("T1: repo left out of the allowlist", [(HANDLER, "\"started_at_ms\": true, \"repo\": true,", "\"started_at_ms\": true,")], T_T1),
    ("T1: daemon derives started_at_ms as at - wall_ms", [(HANDLER, "WallMS: wallMS, WaitMS: waitMS, StartedAtMS: startedAtMS, Repo: repo,", "WallMS: wallMS, WaitMS: waitMS, StartedAtMS: func() *int64 { if wallMS == nil { return startedAtMS }; v := time.Now().UnixMilli() - *wallMS; return &v }(), Repo: repo,")], T_T1),
    ("T2: facts dropped for a kind other than confine", [(HANDLER, "	ctx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)", "	if kind != store.ResourcePeakKindConfine {\n		wallMS, waitMS, startedAtMS, repo = nil, nil, nil, \"\"\n	}\n	ctx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)")], T_T2),
    ("T3: duration lower bound removed (wall_ms and wait_ms)", [(HANDLER, "!valid || value < 0 || value > confineReportMaxDurationMS", "!valid || value > confineReportMaxDurationMS")], T_T3),
    ("T3: wall_ms lower bound removed alone", [(HANDLER, "!valid || value < 0 || value > confineReportMaxDurationMS", "!valid || (value < 0 && name != \"wall_ms\") || value > confineReportMaxDurationMS")], T_T3),
    ("T3: wait_ms lower bound removed alone", [(HANDLER, "!valid || value < 0 || value > confineReportMaxDurationMS", "!valid || (value < 0 && name != \"wait_ms\") || value > confineReportMaxDurationMS")], T_T3),
    ("T3: wall_ms upper bound removed alone", [(HANDLER, "!valid || value < 0 || value > confineReportMaxDurationMS", "!valid || value < 0 || (value > confineReportMaxDurationMS && name != \"wall_ms\")")], T_T3),
    ("T3: wait_ms upper bound removed alone", [(HANDLER, "!valid || value < 0 || value > confineReportMaxDurationMS", "!valid || value < 0 || (value > confineReportMaxDurationMS && name != \"wait_ms\")")], T_T3),
    ("T3: started_at_ms lower bound removed", [(HANDLER, "!valid || value < confineReportMinStartedAtMS || value >= confineReportEndStartedAtMS", "!valid || value >= confineReportEndStartedAtMS")], T_T3),
    ("T3: started_at_ms upper bound removed", [(HANDLER, "!valid || value < confineReportMinStartedAtMS || value >= confineReportEndStartedAtMS", "!valid || value < confineReportMinStartedAtMS")], T_T3),
    ("T3: repo pattern removed", [(HANDLER, "if !valid || !validConfineReportRepo(named) {", "if !valid {")], T_T3),
    ("T3: repo pattern accepts upper-case hex", [(HANDLER, "r >= 'a' && r <= 'f')", "r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F')")], T_T3),
    ("T3: duration upper bound moved by one", [(HANDLER, "value > confineReportMaxDurationMS", "value >= confineReportMaxDurationMS")], T_T3),
    ("T3: duration lower bound moved by one", [(HANDLER, "value < 0 ||", "value <= 0 ||")], T_T3),
    ("T3: started_at_ms lower bound moved by one", [(HANDLER, "value < confineReportMinStartedAtMS", "value <= confineReportMinStartedAtMS")], T_T3),
    ("T3: started_at_ms upper bound moved by one", [(HANDLER, "value >= confineReportEndStartedAtMS", "value > confineReportEndStartedAtMS")], T_T3),
    ("T3: started_at_ms later than the daemon clock refused", [(HANDLER, "value >= confineReportEndStartedAtMS {", "value >= confineReportEndStartedAtMS || value > time.Now().UnixMilli() {")], T_T3),
    # --- sender (T1, T9)
    ("T1: sender drops wall_ms", [(SENDER, "		args[\"wall_ms\"] = *report.WallMS\n", "")], T_T1),
    ("T1: sender copies the > 0 idiom for wait_ms", [(SENDER, "if report.WaitMS != nil {", "if report.WaitMS != nil && *report.WaitMS > 0 {")], T_T1),
    ("T9: no re-send", [(SENDER, RESEND, "")], T_T9),
    ("T9: re-send matched on the allowlist text only", [(SENDER, "if response.Code == \"E_DAEMON_PROTOCOL\" && carriesCompletionFacts(args) {", "if response.Code == \"E_DAEMON_PROTOCOL\" && carriesCompletionFacts(args) && strings.Contains(response.Error, \"unexpected confine-report field\") {")], T_T9),
    ("T9: re-send keeps the new fields", [(SENDER, "confinePeakReportArgs(report, false))\n	}", "confinePeakReportArgs(report, true))\n	}")], T_T9),
    ("T9: re-send drops peak and budget", [(SENDER, "confinePeakReportArgs(report, false))\n	}", "map[string]any{\"signature\": report.Signature, \"oom\": report.OOM})\n	}")], T_T9),
    ("T9: re-send with oom false", [(SENDER, "confinePeakReportArgs(report, false))\n	}", "func() map[string]any { a := confinePeakReportArgs(report, false); a[\"oom\"] = false; return a }())\n	}")], T_T9),
    ("T9: re-send with oom true", [(SENDER, "confinePeakReportArgs(report, false))\n	}", "func() map[string]any { a := confinePeakReportArgs(report, false); a[\"oom\"] = true; return a }())\n	}")], T_T9),
    ("T9: re-send when the frame carried no new field", [(SENDER, "if response.Code == \"E_DAEMON_PROTOCOL\" && carriesCompletionFacts(args) {", "if response.Code == \"E_DAEMON_PROTOCOL\" {")], T_T9),
    ("T9: re-send twice", [(SENDER, RESEND, "	if errors.Is(err, errReportCompletionFactsRefused) {\n		if again := reportPeakExchangeOnce(ctx, socketPath, confinePeakReportArgs(report, false)); again == nil {\n			return nil\n		}\n		return reportPeakExchangeOnce(ctx, socketPath, confinePeakReportArgs(report, false))\n	}")], T_T9),
    ("T9: re-send on a transport error", [(SENDER, RESEND, "	if err != nil {\n		return reportPeakExchangeOnce(ctx, socketPath, confinePeakReportArgs(report, false))\n	}")], T_T9),
    ("T9(g): re-send on any refusal code", [(SENDER, "if response.Code == \"E_DAEMON_PROTOCOL\" && carriesCompletionFacts(args) {", "if carriesCompletionFacts(args) {")], T_T9),
    # --- launch site (T4, T6)
    ("T4: wall timer started before deps.admit", [(LAUNCH, WALL_END, "		wallMS := (deps.monoNow() - admitEntered).Milliseconds()")], T_T4),
    ("T4: wall timer started before repoIdentity", [(LAUNCH, REPO_CALL, "	preRepo := deps.monoNow()\n" + REPO_CALL), (LAUNCH, WALL_END, "		wallMS := (deps.monoNow() - preRepo).Milliseconds()")], T_T4),
    ("T4: start time read before deps.admit", [(LAUNCH, STARTED, ""), (LAUNCH, ADMIT_CALL, "	startedAtMS := deps.wallNow().UnixMilli()\n" + ADMIT_CALL)], T_T4),
    ("T4: start time read before repoIdentity", [(LAUNCH, STARTED, ""), (LAUNCH, REPO_CALL, "	startedAtMS := deps.wallNow().UnixMilli()\n" + REPO_CALL)], T_T4),
    ("T4: wait timer started before repoIdentity", [(LAUNCH, REPO_CALL, "	preRepo := deps.monoNow()\n" + REPO_CALL), (LAUNCH, "waited := (admitReturned - admitEntered).Milliseconds()", "waited := (admitReturned - preRepo).Milliseconds()\n		_ = admitEntered")], T_T4),
    ("T4: wait timer ended at report time", [(LAUNCH, "WaitMS: waitMS, StartedAtMS", "WaitMS: func() *int64 { if waitMS == nil { return nil }; v := (deps.monoNow() - admitEntered).Milliseconds(); return &v }(), StartedAtMS")], T_T4),
    ("T4: the grant's waitedMS sent", [(LAUNCH, "waited := (admitReturned - admitEntered).Milliseconds()", "waited := admission.waitedMS\n		_ = admitEntered")], T_T4),
    ("T4: start time read at report time as wallNow - wall_ms", [(LAUNCH, STARTED, STARTED + "	_ = startedAtMS\n"), (LAUNCH, "StartedAtMS: &startedAtMS,", "StartedAtMS: func() *int64 { v := deps.wallNow().UnixMilli() - wallMS; return &v }(),")], T_T4),
    ("T4: wall_ms from wall-clock readings", [(LAUNCH, STARTED, "	startedAtMS := deps.wallNow().UnixMilli()\n	startedWall := deps.wallNow()\n"), (LAUNCH, WALL_END, "		wallMS := deps.wallNow().Sub(startedWall).Milliseconds()")], T_T4),
    ("T4: Go-zero waitedMS sent for a lease-less admission", [(LAUNCH, WAIT_RULE, WAIT_RULE + " else {\n		waited := admission.waitedMS\n		waitMS = &waited\n	}")], T_T4),
    ("T4: rule keyed on the state name, not release", [(LAUNCH, "	if admission.release != nil {\n		waited :=", "	if admission.state == \"immediate\" || admission.state == \"waited\" || admission.state == \"unevaluated\" {\n		waited :=")], T_T4),
    ("T4 (amendment 1): wall_ms snapshot when deps.start returns", [(LAUNCH, "	if err := deps.start(command); err != nil {", "	startReturned := new(time.Duration)\n	if err := func() error { e := deps.start(command); *startReturned = deps.monoNow(); return e }(); err != nil {"), (LAUNCH, WALL_END, "		wallMS := (*startReturned - admitReturned).Milliseconds()")], T_T4),
    ("T6: repo not copied into the report", [(LAUNCH, ", Repo: repoID,", ","), (LAUNCH, "	if !repoOK {\n		repoID = \"\"\n	}", "	if !repoOK {\n		repoID = \"\"\n	}\n	_ = repoID")], T_T6),
    ("T6: none sent when ok is false", [(LAUNCH, "	if !repoOK {\n		repoID = \"\"\n	}", "	if !repoOK {\n		repoID = \"none\"\n	}")], T_T6),
    ("T6: id kept when ok is false", [(LAUNCH, "	if !repoOK {\n		repoID = \"\"\n	}", "	_ = repoOK")], T_T6),
    ("T6: identity computed after admission", [(LAUNCH, REPO_CALL, "	var repoID string\n	var repoOK bool"), (LAUNCH, "	admitReturned := deps.monoNow()\n", "	admitReturned := deps.monoNow()\n" + REPO_CALL.replace(":=", "=") + "\n")], T_T6),
    ("T6: identity computed twice", [(LAUNCH, REPO_CALL, REPO_CALL + "\n	_, _ = deps.repoIdentity()")], T_T6),
    # --- production seams (T4b)
    ("T4b: processEpoch never assigned", [(LAUNCH, "var processEpoch = time.Now()", "var processEpoch time.Time")], T_T4B),
    ("T4b: default repoIdentity returns unevaluated", [(LAUNCH, "	return RepoIdentity(cwd)\n}", "	_ = cwd\n	return \"\", false\n}")], T_T4B),
    ("T4b: wallNow in whole seconds", [(LAUNCH, "return time.Now().Round(0) }", "return time.Now().Round(0).Truncate(time.Second) }")], T_T4B),
    ("T4b: wallNow keeps the monotonic reading", [(LAUNCH, "return time.Now().Round(0) }", "return time.Now() }")], T_T4B),
    # --- RepoIdentity (T5)
    ("T5: worktree's own gitdir hashed, not its commondir", [(IDENT, "		common = named\n", "		_ = named\n")], T_T5),
    ("T5: step 0 (symlink resolution) skipped", [(IDENT, "	resolved, err := filepath.EvalSymlinks(cwd)\n	if err != nil {\n		return \"\", false\n	}\n	resolved, err = filepath.Abs(resolved)", "	resolved, err := filepath.Abs(cwd)")], T_T5),
    ("T5: none returned on an error", [(IDENT, "	resolved, err := filepath.EvalSymlinks(cwd)\n	if err != nil {\n		return \"\", false\n	}", "	resolved, err := filepath.EvalSymlinks(cwd)\n	if err != nil {\n		return RepoIdentityNone, true\n	}")], T_T5),
]


def main() -> int:
    survivors, broken = [], []
    # Optional arguments select mutations whose name contains any of them.
    selected = [m for m in MUTATIONS if not sys.argv[1:] or any(a in m[0] for a in sys.argv[1:])]
    for name, edits, (package, selector) in selected:
        originals = {}
        try:
            applied = True
            for path, old, new in edits:
                file = ROOT / path
                text = originals.get(path) or file.read_text()
                originals.setdefault(path, text)
                current = file.read_text()
                if current.count(old) != 1:
                    broken.append(f"{name}: snippet matched {current.count(old)} times in {path}")
                    applied = False
                    break
                file.write_text(current.replace(old, new, 1))
            if not applied:
                continue
            result = subprocess.run(
                ["go", "test", "-count=1", "-run", selector, package],
                cwd=ROOT, capture_output=True, text=True,
            )
            output = result.stdout + result.stderr
            if "build failed" in output or "setup failed" in output:
                # A mutation that does not compile proves nothing about the tests.
                broken.append(f"{name}: does not compile")
                print(f"BUILD {name}", flush=True)
                continue
            caught = result.returncode != 0
            print(f"{'RED ' if caught else 'PASS'}  {name}", flush=True)
            if not caught:
                survivors.append(name)
        finally:
            for path, text in originals.items():
                (ROOT / path).write_text(text)
    print(f"\n{len(selected)} mutations, {len(selected) - len(survivors) - len(broken)} red, "
          f"{len(survivors)} survived, {len(broken)} unapplied")
    for line in survivors:
        print("SURVIVED:", line)
    for line in broken:
        print("UNAPPLIED:", line)
    return 1 if survivors or broken else 0


if __name__ == "__main__":
    sys.exit(main())
