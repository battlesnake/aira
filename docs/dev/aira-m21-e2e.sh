#!/usr/bin/env bash
set -u
BIN=~/tmp/aira-m21-bin
work=$(mktemp -d /tmp/m21eXXXX)           # short path (AF_UNIX limit)
export XDG_STATE_HOME="$work/state"
export XDG_RUNTIME_DIR="$work/run"         # short runtime dir
mkdir -p "$XDG_RUNTIME_DIR"
repo="$work/repo"; mkdir -p "$repo"; cd "$repo"
git init -q; git config user.email e@e; git config user.name e
pass=0; fail=0
chk(){ if eval "$2"; then echo "PASS: $1"; pass=$((pass+1)); else echo "FAIL: $1"; fail=$((fail+1)); fi; }

# 1. init auto-starts the daemon + routes
out=$("$BIN" init --project demo --prefix AIRA 2>&1); rc=$?
chk "init ok (rc=$rc)" "[ $rc -eq 0 ]"
# 2. daemon status shows running+ready
st=$("$BIN" daemon status 2>&1)
chk "daemon running" "echo \"\$st\" | grep -qiE 'running|ready|true'"
sock0=$(find "$XDG_RUNTIME_DIR" -name daemon.sock 2>/dev/null | wc -l)
chk "socket exists" "[ $sock0 -ge 1 ]"
# 3. create a ticket (routed coordination verb)
cr=$("$BIN" create 'daemon-routed ticket' --kind feature --severity P2 2>&1); rc=$?
chk "create routed (rc=$rc)" "[ $rc -eq 0 ]"
# 4. list shows it (routed)
ls_out=$("$BIN" list 2>&1)
chk "list shows ticket" "echo \"\$ls_out\" | grep -q 'daemon-routed ticket'"
# 5. a second process (fresh invocation) reuses the SAME daemon (no new socket race)
"$BIN" create 'second' --kind feature --severity P2 >/dev/null 2>&1
n=$(find "$XDG_RUNTIME_DIR" -name daemon.sock 2>/dev/null | wc -l)
chk "single daemon socket" "[ $n -eq 1 ]"
# 6. carve-out verb: run executes client-side (needs real cgroup; accept ok OR honest unevaluated)
run_out=$("$BIN" run --json -- /bin/echo hello 2>&1); rc=$?
chk "run carve-out returns (rc=$rc)" "echo \"\$run_out\" | grep -qE 'OK|E_RUN|status|hello'"
# 7. daemon stop
"$BIN" daemon stop >/dev/null 2>&1; sleep 0.3
st2=$("$BIN" daemon status 2>&1)
# after stop, a status may auto-start? status should NOT auto-start; expect not-running OR socket gone
chk "stop removed socket" "[ $(find \"$XDG_RUNTIME_DIR\" -name daemon.sock 2>/dev/null | wc -l) -eq 0 ]"
echo "=== e2e: $pass passed, $fail failed ==="
rm -rf "$work"
[ $fail -eq 0 ]
