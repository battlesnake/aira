#!/usr/bin/env bash
set -euo pipefail

if [[ ! -t 0 || ! -t 1 ]]; then
  echo "This repro must run from a real interactive terminal." >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
repro_root="${AIRA_TUI_REPRO_DIR:-/home/user/tmp/aira-tui-v3-terminal-repro}"
go_tmp="${repro_root}/go-tmp"
binary="${repro_root}/aira"
mkdir -p "${go_tmp}"

echo "Building a static repro binary at ${binary}"
(
  cd "${repo_root}"
  TMPDIR="${go_tmp}" CGO_ENABLED=0 whale-run go build -o "${binary}" ./cmd/aira
)

cat <<'STEPS'

Real-terminal checklist (run from an initialized AIRA worktree):

1. Press x, choose run, and enter:
     -- sh -c 'trap "exit 130" INT; echo child=$$; while :; do sleep 1; done'
   Confirm, then press Ctrl-C. The child must stop, the TUI process must survive,
   and Enter must restore the dashboard.
   The dashboard is intentionally frozen while the foreground child owns the
   terminal; it force-refreshes every data panel after resume.

2. Launch twice, resize the terminal during and after each launch, and confirm
   that the alternate screen redraws without duplicated or corrupted content.

3. Launch a crashing child:
     -- sh -c 'echo child-crash >&2; kill -ABRT $$'
   Its failure must be reported and Enter must restore normal terminal mode.

4. Check child stdin ownership:
     --stdin - -- sh -c 'IFS= read -r line; printf "child-read=%s\n" "$line"'
   Type a line while suspended. The child, not the dashboard, must receive it.

5. Quit with q after returning to the dashboard.

Caveats intentionally preserved by v3:
- `run --pty` calls Setsid, so terminal Ctrl-C cannot reach that child; use
  `aira run-kill RUN-n` from another terminal.
- SIGTERM sent directly to the TUI while execute is running is swallowed until
  the child exits. Interrupt the foreground child first, then quit the TUI.

STEPS

exec "${binary}" tui
