# AIRA #30 — `aira git`: non-interactive git network ops with SSH→gh-credential auth fallback

Status: PLAN (v1, for Sol adversarial plan-review)
Date: 2026-08-15
Milestone: #30 — Runner git-op command with SSH→gh-credential auth fallback
Depends on: nothing unlanded (master `1e84128`)
Pillar: third and final cut of "AIRA = safe operational primitives replacing raw
ops agents do badly" (peer of #27 run-kill guard and #29 admission gate).

## 1. Motivation and the honest boundary

Agents perform git **network** operations badly and non-deterministically. The two
recurring failure modes on this machine:

1. **Hanging.** A bare `git push`/`git fetch` blocks indefinitely when SSH wants a
   passphrase or an unknown-host confirmation, or when git falls through to an
   interactive HTTPS username/password prompt. A hung network op wastes wall-clock
   exactly like the admission-gate incident (#29).
2. **Auth ambiguity.** On a host with no usable SSH key, the agent does not know to
   fall back to the `gh`-provided HTTPS credential, and flails.

`aira git` makes the machine's auth capability a **primitive**: it runs the requested
network op **non-interactively** (never hangs), and when an SSH remote to github.com
fails to authenticate and `gh` can serve a credential, it falls back to HTTPS —
**without mutating the repository's stored config** and **without ever faking success**.

### The deliberate scope boundary (honesty-first)

AIRA does **not** do authorship. The agent stages and commits; AIRA even treats an
agent's uncommitted tickets as live state (#8-part2). `aira git` therefore covers
**only network/auth-bearing operations**, and only these four in v1:

| verb        | network | mutates local tree | notes |
|-------------|---------|--------------------|-------|
| `clone`     | yes     | creates a new dir  | URL is an argument |
| `fetch`     | yes     | no (updates refs)  | read-mostly |
| `push`      | yes     | no                 | the high-value case |
| `ls-remote` | yes     | no                 | pure probe / listing |

**`pull` is DEFERRED (D1)**, not because auth differs (it does not) but because pull =
fetch + a *local merge* that can create merge commits or a half-merged working tree —
authorship territory. The clean substitute is `aira git fetch` followed by the agent's
own merge/rebase. Adding `pull` later is a trivial allow-list extension; §11 records it.

**Everything else is out of scope by construction**: no `add`/`commit`/`merge`/
`rebase`/`checkout`/`reset`. The allow-list is closed; an unknown verb is refused with
a stable code, never passed through to `git`.

## 2. What "success" and "failure" mean here (no `unevaluated` theatre)

Unlike a gate check, a git network op has a **directly observable** result: the child
process exits 0 or not. So the AIRA honesty principle here is **not** "report
unevaluated when unsure whether it passed" — we always know whether it passed. The
honesty burden is instead:

- **Never silently fall back.** Fall back to HTTPS/gh **only** on a *positively
  classified* SSH-auth failure. If the SSH attempt failed for any other or an
  unclassifiable reason, surface that failure verbatim — do **not** retry the op over
  a second transport (a blind retry of a mutating `push` is a double-side-effect
  hazard).
- **Distinct codes for every distinguishable outcome** (§7), so the caller learns
  *why* — ssh-unusable vs gh-unusable vs both-auth-failed vs a non-auth git error vs
  timeout.
- **Record which transport actually carried the op** on success (`auth: ssh` |
  `auth: https-gh`), so the result is auditable.

## 3. Design principle: probe-then-commit, never op-then-scrape

The naive design ("run the real op over SSH, scrape stderr, retry over HTTPS on auth
error") is **rejected**: it risks running a *mutating* op twice and forces brittle
stderr classification onto the mutation path. Instead:

> **Decide the transport with a read-only probe, then run the real op exactly once
> over the chosen transport.**

The probe is a read-only `git ls-remote --heads <url>` (or `<url> HEAD`) under
BatchMode SSH against the resolved remote URL. It authenticates without any
side-effect. Outcomes:

- Probe exits 0 → SSH auth works → run the real op over SSH.
- Probe fails with a **positively-classified auth failure** (§6) AND remote host is
  github.com AND `gh` is available+authenticated → run the real op over HTTPS with the
  gh credential helper injected ephemerally (§5).
- Probe fails auth but no fallback is available (non-github host, or gh missing/not
  authed) → fail with the precise code; the op never runs.
- Probe fails for a **non-auth** reason (DNS, network down, repo-not-found, or an
  *unclassifiable* failure) → fail `E_GIT_FAILED` with the probe's stderr; **no
  fallback, op never runs.**

`clone` has no pre-existing remote, so the probe uses the clone URL argument directly.
`ls-remote` **is** the probe — it runs once as the real op (no separate probe round).
For `ls-remote` there is no mutation risk, so on a classified SSH-auth failure it may
fall back and re-run over HTTPS directly.

### 3.1 TOCTOU note (accepted)

There is a negligible window between probe and op where auth could change. Accepted:
auth state does not flip mid-second in practice, and the failure mode (op fails after a
passing probe) is surfaced honestly as `E_GIT_FAILED`, not masked.

## 4. Transport decision table (by resolved remote URL scheme + host)

Resolve the operation's target URL first (§8). Then:

| remote scheme | host        | plan |
|---------------|-------------|------|
| ssh (`git@…:` or `ssh://`) | github.com | probe SSH; on classified-auth-fail → gh-HTTPS fallback |
| ssh          | other host  | run over SSH under BatchMode; on auth-fail → `E_GIT_AUTH_FAILED` (no fallback exists) |
| https        | github.com  | run directly with gh credential helper injected (no SSH involved); `GIT_TERMINAL_PROMPT=0` |
| https        | other host  | run as-is, non-interactive (`GIT_TERMINAL_PROMPT=0`, cleared then default helpers); on auth-fail → `E_GIT_AUTH_FAILED` |
| `file://` / local path | — | reject `E_GIT_REMOTE_UNSUPPORTED` (not a network op; nothing to authenticate — out of this command's remit) |
| other scheme (`git://`, unknown) | — | reject `E_GIT_REMOTE_UNSUPPORTED` |

Only **github.com** gets the gh fallback in v1 (gh's default host). GitHub Enterprise
(`GH_HOST`) is **deferred (D2)** — documented, not silently pretended.

## 5. The HTTPS/gh fallback mechanism — ephemeral, never mutates `.git/config`

When falling back (or for a native-HTTPS github.com remote), invoke git with **`-c`
overrides only** so nothing is written to the repo config:

```
git \
  -c url."https://github.com/".insteadOf="git@github.com:" \
  -c url."https://github.com/".insteadOf="ssh://git@github.com/" \
  -c credential.helper= \
  -c credential.helper="!gh auth git-credential" \
  <op-args>
```

- The two `insteadOf` rewrites turn a github.com **SSH** URL into its HTTPS form *for
  this invocation only*. (For a remote that is already HTTPS they are inert.)
- `credential.helper=` (empty) **first clears** any inherited/system helper so a stray
  interactive helper cannot hang or leak; the second line sets **only** gh.
- `!gh auth git-credential` is git's documented shell-helper form.

`gh` availability is verified **before** the fallback so the failure is cleanly coded,
not a confusing git error: run `gh auth status` (non-interactive) — non-zero or a
missing binary → `E_GIT_GH_UNAVAILABLE`. Only when gh is present+authed do we run the
op over the fallback.

## 6. Non-interactive guarantees (the anti-hang contract) + the SSH-auth classifier

Every child inherits an environment that makes hanging impossible:

- `GIT_SSH_COMMAND="ssh -o BatchMode=yes -o ConnectTimeout=<n>"` — BatchMode disables
  every SSH prompt (passphrase, unknown-host confirm), failing closed instead of
  blocking; ConnectTimeout bounds the TCP connect.
- `GIT_TERMINAL_PROMPT=0` — git never prompts for HTTPS username/password.
- `GCM_INTERACTIVE=never` (harmless if git-credential-manager is absent) — belt-and-braces.
- The whole op runs under a **context deadline** (`git.op_timeout_seconds`, default
  120s). On deadline or ctx-cancel we kill the child's **process group** (Setpgid at
  launch; `kill(-pgid, SIGKILL)` after SIGTERM) so ssh and its children die too, then
  report `E_GIT_TIMEOUT`.

**StrictHostKeyChecking is left at its default under BatchMode**: BatchMode makes an
unknown host key a *failure* (no prompt), which we classify as a connection failure
(not a credential-auth failure) → it does **not** trigger the gh fallback on its own;
it surfaces as `E_GIT_FAILED`. (We do not silently `accept-new` unknown host keys.)

### 6.1 The conservative SSH-auth-failure classifier

The fallback trigger is a **positive** classification, defaulting to *not-auth-failure*
when uncertain. An SSH attempt is classified `auth-failure` only when **both**:

1. the process exited non-zero (ssh transport failures are exit 255), **and**
2. stderr matches one of the closed, well-known credential-rejection signatures:
   - `Permission denied (publickey` (with or without more methods listed)
   - `git@github.com: Permission denied`
   - `remote: Invalid username or password` / `remote: Support for password authentication`
   - `fatal: Authentication failed`
   - `Could not read from remote repository` **only when** preceded by a
     `Permission denied` line in the same stderr (git prints this generic tail after a
     publickey rejection; alone it is ambiguous → not classified).

Anything else — `Host key verification failed`, `Connection timed out`, `Could not
resolve hostname`, `Repository not found`, `does not exist` — is **not** an auth
failure. Uncertain ⇒ not-auth ⇒ **no fallback**, surface `E_GIT_FAILED`. This
conservatism is the crux Sol should attack: the danger is a **false-positive** (falling
back and, for `push`, risking a second side-effect) — but note the probe-then-commit
design (§3) means the *mutating* op only ever runs once over exactly one transport, so
even a misclassification cannot double-push; it can only pick the wrong transport for a
single attempt, which then fails honestly. The classifier signatures live in one
table with a `verifies:`-tagged unit test per signature (and per non-signature).

## 7. Response contract and stable error codes

Success (`ok: true`) data payload:

```json
{ "op": "push", "auth": "ssh" | "https-gh",
  "remote": "origin", "url": "git@github.com:owner/repo.git",
  "host": "github.com", "fell_back": false,
  "exit_code": 0 }
```

(`url` is the *resolved* remote URL, never a rewritten one; `fell_back` records whether
the gh path carried it.) The captured git stdout/stderr are returned to the face as
live output (CLI tees; JSON/MCP put a bounded tail in the payload — see §9), exactly
like the runner's model.

New stable codes (registered in `internal/store/check.go` exit-code map):

| code | exit | meaning |
|------|------|---------|
| `E_GIT_SSH_UNAVAILABLE`   | 1 | ssh binary absent, or `GIT_SSH_COMMAND`/ssh cannot be invoked at all |
| `E_GIT_GH_UNAVAILABLE`    | 1 | fallback needed but `gh` missing or `gh auth status` non-zero |
| `E_GIT_AUTH_FAILED`       | 1 | SSH auth failed and no usable fallback (non-github host, or gh unavailable) |
| `E_GIT_REMOTE_UNSUPPORTED`| 1 | remote scheme/host not a supported network target (local/file/git:// ) |
| `E_GIT_REMOTE_UNRESOLVED` | 1 | could not resolve the target URL (no such remote / no upstream) |
| `E_GIT_TIMEOUT`           | 3 | op exceeded `git.op_timeout_seconds`; child process-group killed |
| `E_GIT_FAILED`            | 1 | git itself failed for a non-auth reason (non-ff, conflict, not-found, unclassifiable) — raw stderr surfaced |
| `E_GIT_ARG_INVALID`       | 2 | unknown/disallowed sub-verb, or malformed args |
| `E_CONFIG_INVALID`        | 2 | (reused) malformed `git` config block |

Exit-code choice mirrors the runner family (auth/failed = 1, timeout = 3, arg/config = 2).

## 8. Target-URL resolution

- `clone <url> [dir]` — URL is the first positional. Host/scheme parsed from it.
- `push`/`fetch`/`ls-remote [remote]` — remote name defaults to `origin`; resolve via
  `git -C <root> remote get-url <remote>`. Empty/no-such-remote → `E_GIT_REMOTE_UNRESOLVED`.
  (v1 does **not** consult the branch's configured upstream remote; `origin` default +
  explicit `[remote]` arg covers the cases. Upstream-derivation is D3, documented.)
- `push` extra args: `push [remote] [refspec…]`; `fetch [remote] [refspec…]`. The
  refspecs pass through **after** an `--` delimiter in the argv (mirroring `aira run`),
  so no refspec can be mistaken for a flag or inject an option. No git *option* flags
  are accepted in v1 (a closed argv: `remote` + refspecs only) — this keeps the surface
  small and un-injectable; richer flags are D4.

URL parsing handles the two github SSH forms (`git@github.com:owner/repo(.git)` scp-like
and `ssh://git@github.com/owner/repo`) plus `https://github.com/owner/repo`. The host is
extracted structurally, not by substring-contains (`github.com.evil.example` must not
match — Sol should attack this).

## 9. Layering, seam, and faces

- **New package `internal/gitremote`** — pure git-network+auth logic, no store/domain
  deps. Exports `Client` with `Run(ctx, Request) (*Result, error)` and the config it
  needs (timeouts, gh-fallback toggle, ssh connect-timeout). It owns: URL resolution,
  scheme/host parse, the probe, the classifier, transport selection, the hardened
  process-group exec, and the gh-availability check. It shells `git`/`ssh`/`gh`; it is
  Linux-and-POSIX (Setpgid) — consistent with the runner being Linux-only.
- **Seam on Core** — a `GitOps` interface (mirrors the existing `Runner` seam):
  `Run(context.Context, gitremote.Request) (*gitremote.Result, error)`. Core's new
  `git` verb handler builds a `gitremote.Request` from args and calls `c.gitops.Run`.
  Nil seam → `E_GIT_SSH_UNAVAILABLE`-class "git ops unavailable on this face" (only the
  runner-bearing faces wire it).
- **Injection** — add a chainable `(c *Core) WithGitOps(GitOps) *Core` setter
  (least-invasive; avoids a sixth `NewWith…` permutation). `app.Open` builds
  `project.GitOps` from resolved config (like `project.Runner`); CLI (`main.go`) and MCP
  (`mcp_project.go`) call `.WithGitOps(project.GitOps)` on the constructed Core.
- **Dispatch descriptor** — one grouped verb `git` with `subverb` ∈
  {clone,fetch,push,ls-remote}, `MCPTool: "aira_git"`, `MCPOperation: "subverb"`,
  Safety `execute` (it runs a subprocess and touches the network). Summary/example
  table entry + golden-list updates (`dispatch_metadata_test.go` verb list,
  `skill_test.go` safety map).
- **Face output** — reuse the M17 `FaceOutput` model: CLI tees git's stdout/stderr live
  and prints the summary; `--json`/MCP suppress the live tee and return a bounded
  stderr/stdout tail in the payload so protocol streams stay clean.

## 10. Config

New top-level block on `app.Config`:

```json
"git": {
  "gh_fallback": true,
  "ssh_connect_timeout_seconds": 10,
  "op_timeout_seconds": 120
}
```

- `gh_fallback` is a **presence-pointer `*bool`** (absent ⇒ enabled; explicit `false`
  disables the fallback — then a github SSH-auth failure is `E_GIT_AUTH_FAILED`, never
  silently HTTPS).
- Timeouts absent ⇒ defaults (10s connect, 120s op). Zero or negative ⇒
  `E_CONFIG_INVALID` at Open (fail-closed, eager — same discipline as the admission
  block). The whole `git` block absent ⇒ all defaults, fallback enabled.

`git init`-generated config does **not** need the block (defaults suffice); existing
configs without it keep working.

## 11. Tests (TDD; every classifier signature + transport branch is discriminating)

Pure-unit (no network, `internal/gitremote`):
- **URL parse**: both SSH forms + HTTPS → correct host/scheme; `github.com.evil.example`
  and `git@github.com-x:` → **not** github.com; local/file/git:// → unsupported.
- **Classifier**: one case per §6.1 signature returns auth-failure; and
  `Host key verification failed`, `Connection timed out`, `Repository not found`,
  bare `Could not read from remote repository`, exit-0 → **not** auth-failure. (These
  are the load-bearing discriminators — a "classify everything as auth" impl must fail
  them.)
- **Transport selection** (table-driven over §4 with a fake exec seam): each row picks
  the expected transport / error code. The exec is injected (a `runFn` seam) so the
  probe/op/gh-status calls are simulated deterministically — no real git/ssh/gh.
- **Ephemeral-config**: assert the fallback argv contains the four `-c` overrides in
  order and that no `git config` write is ever issued (the seam records argv).
- **Anti-hang env**: assert every child request carries `BatchMode=yes`,
  `GIT_TERMINAL_PROMPT=0`, and a bounded deadline.
- **Timeout**: a fake exec that blocks past the deadline → `E_GIT_TIMEOUT` and a
  kill-group call recorded.
- **No-fallback matrix**: github SSH-auth-fail + gh-absent → `E_GIT_GH_UNAVAILABLE`;
  + `gh_fallback:false` → `E_GIT_AUTH_FAILED`; non-github SSH-auth-fail →
  `E_GIT_AUTH_FAILED`; probe non-auth-fail → `E_GIT_FAILED` with op **never invoked**
  (the seam asserts the mutating op call count is 0).

Core-level:
- `git` verb dispatch: unknown sub-verb → `E_GIT_ARG_INVALID`; nil seam → unavailable
  code; success payload shape; `--json` suppresses live tee.
- Config: malformed `git` block → `E_CONFIG_INVALID` at Open; presence-pointer
  `gh_fallback:false` threads through.

Golden/metadata: verb-list, safety-map, MCP-tool, skill-artifact goldens updated.

**Real-binary e2e** (`~/tmp/aira-gitauth-e2e.sh`, committed reproduction): against a
**local bare repo over `file://` is not applicable** (we reject local), so the e2e uses
two mechanisms — (a) a fake `git`/`ssh`/`gh` on `PATH` (shell shims) to drive the
transport-selection + fallback + classifier branches deterministically end-to-end
through the real CLI `--json` face (the load-bearing check the Go seam-tests cannot: it
exercises `.aira/config git.*` → app → gitremote → face); and (b) an **opt-in live
smoke** (`AIRA_GIT_LIVE=1`, skipped by default) that does a real `ls-remote` against a
public github repo to prove the real ssh/https paths, gated so CI/offline never depends
on the network.

## 12. Non-goals / deferrals (written down, not pretended)

- **D1 `pull`** — fetch + local merge (authorship-adjacent). Substitute: `aira git
  fetch` + agent merge. Trivial allow-list addition later.
- **D2 GitHub Enterprise (`GH_HOST`)** — v1 fallback is github.com only.
- **D3 upstream-remote derivation** — v1 uses `origin` default + explicit `[remote]`.
- **D4 arbitrary git flags** — v1 argv is closed (remote + refspecs after `--`).
- **D5 cgroup-scoping git ops** — v1 uses a plain process-group child with
  context-timeout, not the M12 cgroup runner. Git network ops are short; resource-
  scoping them is a possible future.
- **D6 credential caching / ssh-agent management** — out of scope; we consume whatever
  ssh/gh already provide, non-interactively.

## 13. Invariants (for the build review to attack in both directions)

1. A **mutating** op (`push`) runs **at most once**, over exactly one transport
   (probe-then-commit). No stderr-scrape-and-retry on the mutation path.
2. The gh fallback fires **only** on a positively-classified SSH-auth failure to a
   github.com host with `gh_fallback` enabled and gh present+authed; every other
   failure surfaces its own distinct code and does **not** fall back.
3. No path can hang: BatchMode + `GIT_TERMINAL_PROMPT=0` + cleared credential helper +
   context deadline with process-group kill, on every child.
4. The repo's stored `.git/config` is never mutated (fallback is `-c`-only).
5. Success records the transport that actually carried the op; failure never reports
   success (no fake pass), and every distinguishable cause has its own code.
6. The verb allow-list is closed; an unknown sub-verb is refused, never passed to git.
7. Host detection is structural (parsed authority), never substring-contains.
