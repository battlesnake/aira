#!/usr/bin/env python3
"""Mutation check for the AIRA-323 command-key golden table.

The golden table (docs/dev/aira-history-charge-keys.golden.json) is the sole authority between
the Go CommandKeys and the replay script's keys() (spec section 5.1.4). A boundary of the key
function that no case pins could differ between the two without either test noticing. This
script applies each named one-line mutation to the replay script's source IN MEMORY (no file is
written), runs its --self-test on the committed table, and prints the mismatch count. It also
feeds deliberately invalid tables to the fixture validator.

Exit 0 only if the unmutated script is green, every mutation is red, and every invalid table is
refused. Run:  python3 -I docs/dev/aira-history-charge-key-mutations.py
"""
import contextlib
import io
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
REPLAY = os.path.join(HERE, "aira-history-charge-replay.py")

# (name, exact source text, replacement). Each source text must occur exactly once.
MUTATIONS = [
    ("hex run 7+ -> 8+", "[0-9a-f]{7,}|", "[0-9a-f]{8,}|"),
    ("hex run 7+ -> 6+", "[0-9a-f]{7,}|", "[0-9a-f]{6,}|"),
    ("all-digits rule dropped", "[0-9a-f]{7,}|[0-9]+", "[0-9a-f]{7,}"),
    ("digit run 3+ -> 2+", 're.search(r"[0-9]{3,}", t)', 're.search(r"[0-9]{2,}", t)'),
    ("digit-run rule dropped", 'if re.search(r"[0-9]{3,}", t):', "if False:"),
    ("'/' rule dropped", 'if "/" in t or t.endswith', "if t.endswith"),
    ("suffix .py dropped", '(".py", ".rs"', '(".rs"'),
    ("suffix .rs dropped", '".rs", ".sh"', '".sh"'),
    ("suffix .sh dropped", '".sh", ".txt"', '".txt"'),
    ("suffix .txt dropped", '".txt", ".json"', '".json"'),
    ("suffix .json dropped", '".json", ".toml"', '".toml"'),
    ("suffix .toml dropped", '".json", ".toml")', '".json")'),
    ("suffix .yaml added", '".json", ".toml")', '".json", ".toml", ".yaml")'),
    ("env NAME= upper case only", 're.match(r"[A-Za-z_][A-Za-z0-9_]*=", t)', 're.match(r"[A-Z_][A-Z0-9_]*=", t)'),
    ("/usr/bin/env not unwrapped", '("env", "/usr/bin/env")', '("env",)'),
    ("time -o dropped", 'TIME_ARG = {"-o", ', "TIME_ARG = {"),
    ("time -f dropped", '"-f", "--output"', '"--output"'),
    ("time --output dropped", '"--output", "--format"', '"--format"'),
    ("time --format dropped", '"--output", "--format"}', '"--output"}'),
    ("time -p dropped", 'TIME_FLAG = {"-p", ', "TIME_FLAG = {"),
    ("time -v dropped", '"-v", "-a"', '"-a"'),
    ("time -a dropped", '"-a", "--portability"', '"--portability"'),
    ("time --portability dropped", '"--portability", "--verbose"', '"--verbose"'),
    ("time --verbose dropped", '"--verbose", "--append"', '"--append"'),
    ("time --append dropped", '"--verbose", "--append"}', '"--verbose"}'),
    ("unknown time option guessed past", "return tk  # unknown option: do not guess", "i = j\n            continue"),
    ("no basename on argv[0]", "a0 = base(u[0])", "a0 = u[0]"),
    ("13 arguments instead of 14", "rest[:14]", "rest[:13]"),
    ("first4 = rest[:4]", "rest[:3]", "rest[:4]"),
    ("key_cmd from rest[0], flags included", '(pos[0] if pos else "")', '(rest[0] if rest else "")'),
    ("no repo prefix", "r = (LEGACY_REPO if repo is None else repo) + US", 'r = ""'),
    ("OOM veto dropped (coverage counterexample)", "if any(o for _p, o in h):", "if False:"),
]


def run_self_test(src):
    """Exec the (possibly mutated) replay source in a fresh namespace; return (exit, output)."""
    ns = {"__name__": "aira_replay_mutant", "__file__": REPLAY}
    exec(compile(src, REPLAY, "exec"), ns)
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        code = ns["self_test"]()
    return code, out.getvalue(), ns


def mismatches(output):
    m = re.search(r"(\d+) mismatches", output)
    return m.group(1) if m else "n/a"


def main():
    with open(REPLAY) as f:
        src = f.read()
    ok = True
    code, out, ns = run_self_test(src)
    print("%-44s exit %d  mismatches %s" % ("(unmutated)", code, mismatches(out)))
    ok &= code == 0
    for name, old, new in MUTATIONS:
        n = src.count(old)
        if n != 1:
            print("%-44s SOURCE TEXT FOUND %d TIMES, not applied" % (name, n))
            ok = False
            continue
        code, out, _ = run_self_test(src.replace(old, new))
        red = code != 0
        ok &= red
        print("%-44s exit %d  mismatches %s  %s" % (name, code, mismatches(out), "red" if red else "GREEN (not pinned)"))
    # The validator must refuse tables whose inputs break the wire rules.
    with open(os.path.join(HERE, "aira-history-charge-keys.golden.json")) as f:
        golden = json.load(f)
    bad_inputs = [
        ("repo of 64 hex digits", lambda c: c.__setitem__("repo", "a" * 64)),
        ("repo of 15 hex digits", lambda c: c.__setitem__("repo", "a" * 15)),
        ("repo of 17 hex digits", lambda c: c.__setitem__("repo", "a" * 17)),
        ("repo upper-case hex", lambda c: c.__setitem__("repo", "A" * 16)),
        ("repo NONE", lambda c: c.__setitem__("repo", "NONE")),
        ("repo empty", lambda c: c.__setitem__("repo", "")),
        ("empty argv", lambda c: c.__setitem__("argv", [])),
        ("argv token with NUL", lambda c: c.__setitem__("argv", ["a\x00b"])),
        ("missing want key", lambda c: c["want"].pop("key_cmd")),
    ]
    for name, corrupt in bad_inputs:
        table = json.loads(json.dumps(golden))
        corrupt(table["cases"][0])
        errs = ns["fixture_errors"](table)
        ok &= bool(errs)
        print("%-44s %s" % ("invalid table: " + name, "refused" if errs else "ACCEPTED (validator porous)"))
    print("mutation check: %s" % ("all red" if ok else "FAILED"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
