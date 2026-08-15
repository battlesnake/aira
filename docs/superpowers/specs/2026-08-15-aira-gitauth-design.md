# AIRA #30 — `aira git`: non-interactive git network ops with SSH→gh-credential auth fallback

Status: PLAN (v2 — incorporates Sol round-1 findings: 4×P0 + 5×P1/P2)
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

## 3. Design principle: probe-then-commit over an EXPLICIT resolved URL

The naive design ("run the real op over SSH, scrape stderr, retry over HTTPS on auth
error") is **rejected**: it risks running a *mutating* op twice and forces brittle
stderr classification onto the mutation path. Instead:

> **Resolve one explicit endpoint URL, decide the transport with a read-only probe of
> that exact URL, then run the real op exactly once over the chosen transport — with
> the endpoint pinned as an explicit URL on the git command line, never a remote name.**

### 3.0 Endpoint is resolved and PINNED as an explicit URL (fixes probe≠op)

Resolve the *verb-specific effective* URL first (§8): **`push` uses the push URL**
(`git remote get-url --push <remote>`), fetch/ls-remote use the fetch URL, clone uses
the argument. **Both the probe and the committed op are then invoked against that
explicit URL string** (e.g. `git push https://github.com/owner/repo.git <refspec>`),
never against the bare remote name. This guarantees the probe and the op target the
*same* endpoint — a `pushurl` that differs from the fetch URL cannot make us probe one
place and push another, and a concurrent `remote set-url` cannot redirect the committed
op after validation. Inherited `url.*.insteadOf` rewrites apply *identically* to the
probe and the op (same URL, same config), so they stay consistent; a config mutation
landing *between* probe and op is the sole accepted TOCTOU (§3.1).

### 3.1 The probe and the classification

The probe is a read-only `git ls-remote --heads <explicit-ssh-URL>` under BatchMode SSH.
It authenticates with no side-effect. **Both the probe AND (for reporting) the committed
op are classified** by the conservative SSH-auth classifier (§6.1). Outcomes:

- Probe exits 0 → SSH auth works → run the real op over SSH (explicit ssh URL).
- Probe fails and is **positively classified as an SSH-auth failure** (§6.1) AND the
  host is exactly github.com AND `gh_fallback` is enabled AND `gh` is
  available+authed for github.com → run the real op over HTTPS against an **explicit
  canonical HTTPS URL we construct ourselves** (§5), with the gh credential helper.
- Probe classified auth-fail but no fallback available (non-github host, gh missing/not
  authed, or fallback disabled) → `E_GIT_AUTH_FAILED` / `E_GIT_GH_UNAVAILABLE`; op never runs.
- Probe fails for any **non-auth** reason (host-key, DNS, network, repo-not-found, or
  *unclassifiable*) → `E_GIT_FAILED` with redacted stderr; **no fallback, op never runs.**

If the committed op itself then fails with an SSH-auth signature (auth changed after a
passing probe — key expiry, agent swap, endpoint race), it is reported
**`E_GIT_AUTH_FAILED`** (the single real attempt is classified for reporting), **never
retried**, never mislabeled `E_GIT_FAILED`.

`clone` probes the clone URL argument. `ls-remote` **is** the probe (runs once as the
real op); with no mutation risk, on a classified SSH-auth failure it may fall back and
re-run over the explicit HTTPS URL directly.

### 3.2 TOCTOU note (accepted)

The only residual window is a config/auth change landing *between* the probe and the
single committed op. Accepted: auth does not flip mid-second in practice, both invoke
the identical explicit URL, and a post-probe op auth-failure is reported honestly as
`E_GIT_AUTH_FAILED` (never masked, never retried).

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

## 5. The HTTPS/gh fallback — EXPLICIT canonical URL, never `insteadOf`, never mutates config

The `insteadOf` rewrite trick is **rejected** (Sol P0): structural host-acceptance is
broader than any fixed textual `insteadOf` pattern, so an accepted-but-unrewritten SSH
form (alternate user, explicit port, host case, `ssh://` spelling) would run over SSH
while we *report* `https-gh` — a dishonest transport label. Instead we **parse
`owner/repo` from the resolved SSH URL and construct one explicit canonical HTTPS URL
ourselves**, then hand *that* to git:

```
git \
  -c credential.helper= \
  -c credential.helper="!gh auth git-credential" \
  <op> https://github.com/<owner>/<repo>.git [refspecs…]
```

- The op targets an **explicit `https://github.com/owner/repo.git`** we built — so the
  transport we report is *provably* the transport git uses (no reliance on rewrite rules
  firing). `<owner>/<repo>` come from the strict URL parse (§8), `.git` normalised.
- `credential.helper=` (empty) **first clears** any inherited/system helper so a stray
  interactive helper cannot hang or leak; the second sets **only** gh.
- `!gh auth git-credential` is git's documented shell-helper form.
- A native-HTTPS github.com remote uses its own resolved URL with the same two
  `credential.helper` `-c` lines (no reconstruction needed).

Nothing is written to `.git/config` (all `-c`, ephemeral).

`gh` availability is verified **before** the fallback, **scoped to github.com** (Sol
P1): `gh auth status --hostname github.com` (non-interactive, under the op deadline) —
missing binary or non-zero → `E_GIT_GH_UNAVAILABLE`. As a strengthening we additionally
confirm the helper actually serves a github.com credential by feeding
`protocol=https\nhost=github.com\n\n` to `gh auth git-credential get` and checking a
`password=` line comes back (token value never logged); no credential → still
`E_GIT_GH_UNAVAILABLE` (honest: the precondition the fallback depends on is unmet).
Only when both hold do we run the op over the fallback.

## 6. Non-interactive guarantees (the anti-hang contract) + the SSH-auth classifier

Every child inherits an environment that makes hanging impossible:

- `GIT_SSH_COMMAND="ssh -o BatchMode=yes -o ConnectTimeout=<n>"` — BatchMode disables
  every SSH prompt (passphrase, unknown-host confirm), failing closed instead of
  blocking; ConnectTimeout bounds the TCP connect.
- `GIT_TERMINAL_PROMPT=0` — git never prompts for HTTPS username/password.
- `GCM_INTERACTIVE=never` (harmless if git-credential-manager is absent) — belt-and-braces.
- **Every** child — the URL-resolution `git remote get-url`, the `gh` binary/status/
  credential checks, the SSH probe, and the committed op — is launched in its own
  **process group** (Setpgid) and shares the **single end-to-end op context**
  (`git.op_timeout_seconds`, default 120s). Their stdout/stderr pipes are drained
  concurrently (a full pipe can never wedge the child). On deadline or ctx-cancel each
  live child gets `SIGTERM`, a **bounded grace** (e.g. 2s), then `kill(-pgid, SIGKILL)`,
  then a **bounded reap** — the shutdown path itself cannot exceed the deadline by
  waiting on a stuck child. Report `E_GIT_TIMEOUT`. The OS-level guarantee is stated as
  **best-effort bounded cancellation**: uninterruptible kernel I/O (D-state) cannot be
  excluded, so we promise bounded *signalling*, not instantaneous death.

**StrictHostKeyChecking is left at its default under BatchMode**: BatchMode makes an
unknown host key a *failure* (no prompt), which we classify as a connection failure
(not a credential-auth failure) → it does **not** trigger the gh fallback on its own;
it surfaces as `E_GIT_FAILED`. (We do not silently `accept-new` unknown host keys.)

### 6.1 The conservative, SSH-ATTRIBUTABLE auth-failure classifier

The classifier runs over the **SSH-transport** probe/op, whose stderr is git passing
through ssh's own diagnostics. It is a **positive** classification defaulting to
*not-auth-failure* when uncertain. An attempt is classified `auth-failure` only when
**both**:

1. the process exited **non-zero** — note we do **not** require a specific code (Sol
   P0): `git ls-remote` exits 128 when its ssh child exits 255, so gating on `==255`
   would miss every real failure; and
2. stderr matches one of the closed, **ssh-emitted publickey-rejection** signatures:
   - `Permission denied (publickey` (with or without further methods listed)
   - `<user>@<host>: Permission denied` (e.g. `git@github.com: Permission denied`)
   - `Could not read from remote repository` **only when** preceded by a
     `Permission denied` line in the same stderr (git's generic tail after a publickey
     rejection; alone it is ambiguous → not classified).

**HTTPS/application-layer signatures are deliberately NOT in the SSH classifier** (Sol
P0): `remote: Invalid username or password`, `remote: Support for password
authentication`, and bare `fatal: Authentication failed` describe *repository
authorization / HTTPS credential* failures, not ssh-credential negotiation; accepting
them could trigger a forbidden fallback. Since the probe/op we classify runs over SSH
explicitly, such application-layer lines do not legitimately appear.

Anything else — `Host key verification failed`, `Connection timed out`, `Could not
resolve hostname`, `Connection refused`, `Repository not found`, `does not exist` — is
**not** an auth failure. Uncertain ⇒ not-auth ⇒ **no fallback**, surface `E_GIT_FAILED`.
The danger to guard is a **false-positive** (falling back wrongly); the probe-then-commit
design (§3) means even a misclassification only picks the wrong transport for the
*single* attempt, which then fails honestly — it can never double-push. The signatures
live in one table with a `verifies:`-tagged unit test per signature **and per
non-signature** (the load-bearing discriminators).

## 7. Response contract and stable error codes

Success (`ok: true`) data payload:

```json
{ "op": "push", "auth": "ssh" | "https-gh",
  "remote": "origin", "url": "git@github.com:owner/repo.git",
  "host": "github.com", "fell_back": false,
  "exit_code": 0 }
```

(`url` is the *resolved* remote URL; for a fallback it is the explicit canonical HTTPS
URL we ran; `fell_back` records whether the gh path carried it.) The captured git
stdout/stderr are returned to the face as live output (CLI tees; JSON/MCP put a bounded
tail in the payload — see §9), exactly like the runner's model. **All surfaced URLs and
stderr pass through a secret-redaction filter** (Sol P1): any `userinfo` in a URL
(`https://user:token@host/…`) and any token-shaped material is replaced with `***`
before it reaches a payload, a log, or the live tee — a URL that embeds a credential
must never be echoed.

**On the distinct-code guarantee (narrowed, Sol P2):** the *auth/config/timeout/
argument/resolution* causes each get their own stable code; **`E_GIT_FAILED` is the
honest catch-all** for every non-auth git failure (non-fast-forward, conflict, remote
not-found, DNS, network, unclassifiable) — we do **not** promise a distinct code per
such cause. The raw (redacted) git stderr is always surfaced with `E_GIT_FAILED` so the
real reason is visible; we simply do not fabricate a taxonomy git itself does not give
us cleanly.

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

## 8. Target-URL resolution and the strict URL grammar

- `clone <url> [dir]` — URL is the first positional. Host/scheme parsed from it.
- `push [remote] [refspec…]` — resolve the **push** URL: `git -C <root> remote get-url
  --push <remote>` (Sol P0 — push may use a distinct `pushurl`). `fetch`/`ls-remote
  [remote]` resolve the fetch URL: `git -C <root> remote get-url <remote>`. Remote name
  defaults to `origin`; empty/no-such-remote → `E_GIT_REMOTE_UNRESOLVED`. (v1 does **not**
  consult the branch's configured upstream; `origin` default + explicit `[remote]` arg
  covers the cases. Upstream-derivation is D3.)
- Refspecs pass through **after** a `--` delimiter (mirroring `aira run`), so none can be
  mistaken for a flag or inject an option. No git *option* flags are accepted in v1
  (closed argv: `remote` + refspecs) — small, un-injectable surface; richer flags are D4.

### 8.1 Strict URL grammar (Sol P1 — anti-spoof, no secret leak)

URL parsing is a **strict ASCII grammar**, never substring-contains, over the three
accepted forms: scp-like `git@github.com:owner/repo(.git)`, `ssh://git@github.com/owner/
repo(.git)`, `https://github.com/owner/repo(.git)`. To be treated as **github.com** (the
sole fallback host), ALL must hold:

- host authority, **case-folded** and with a single optional trailing dot stripped,
  equals exactly `github.com` — `GitHub.com` matches; `github.com.evil.example`,
  `github.com-x`, `evilgithub.com`, `github.com@evil` do **not**.
- scheme ∈ {scp-like-ssh, `ssh`, `https`} only; any other (`git://`, `http`, `ftp`,
  file/local path) → `E_GIT_REMOTE_UNSUPPORTED`.
- **userinfo**: for SSH, the user must be `git` (the only user gh's fallback rewrites to);
  a non-`git` user on a github SSH URL is treated as **not a fallback candidate** (run
  as-is; no https-gh reconstruction — we will not silently change the identity). HTTPS
  userinfo (`user:token@`) is **stripped and redacted** before use and never echoed.
- port: empty, or an explicit numeric port equal to the scheme default (22 ssh / 443
  https); any other explicit port on a github URL → not a fallback candidate (run as-is).
- reject control characters, whitespace, and any non-ASCII / IDNA/Unicode host
  (`E_GIT_ARG_INVALID`) — no punycode confusables.

`owner` and `repo` are captured for the canonical-HTTPS reconstruction (§5); `repo` has
at most one trailing `.git` normalised. A URL that parses as github.com but fails a
fallback-candidacy sub-condition above still runs honestly over its native transport;
we simply do not claim the gh path for it.

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
- **URL parse / grammar (§8.1)**: three accepted forms → correct host/scheme/owner/repo;
  `github.com.evil.example`, `git@github.com-x:`, `evilgithub.com`, `GitHub.com.`
  (case+trailing-dot → **matches**), `github.com@evil`, punycode/IDNA host, explicit
  non-default port, non-`git` ssh user (→ not a fallback candidate), control chars,
  `git://`/`http`/local → each the correct classification/error. HTTPS `user:token@`
  userinfo → stripped and **redacted** in every surfaced field.
- **Classifier (§6.1)**: one case per ssh-attributable signature → auth-failure at a
  **git-wrapped exit 128** (not 255) to prove the exit-code fix; and each of
  `Host key verification failed`, `Connection timed out`, `Could not resolve hostname`,
  `Repository not found`, **bare** `Could not read from remote repository`, and the
  three **application-layer** strings (`remote: Invalid username or password`,
  `remote: Support for password authentication`, bare `fatal: Authentication failed`) →
  **not** auth-failure. (Load-bearing discriminators: a "classify everything as auth"
  or an "==255" impl must fail these.)
- **Transport selection** (table-driven over §4 with a fake exec seam `runFn`): each row
  picks the expected transport / code; probe/op/gh calls simulated deterministically.
- **Explicit-URL pinning**: the committed op argv carries an **explicit URL** (never the
  bare remote name); for push the resolved URL comes from `get-url --push`; for a
  fallback the op argv carries the **constructed canonical `https://github.com/owner/
  repo.git`** and the two `credential.helper` `-c` lines, and **no `insteadOf`** and no
  `git config` write ever appears (the seam records every argv).
- **Anti-hang env**: **every** child request (get-url, gh status/credential, probe, op)
  carries `BatchMode=yes` (ssh ones), `GIT_TERMINAL_PROMPT=0`, and shares the one bounded
  deadline; a fake exec blocking past the deadline → `E_GIT_TIMEOUT` with a
  **process-group kill recorded for that child**, and the shutdown returns within the
  bounded grace (does not itself exceed the deadline).
- **Post-probe op auth-failure** → `E_GIT_AUTH_FAILED` (classified real attempt), **not**
  `E_GIT_FAILED`, and the op is **not** retried (op call count == 1).
- **gh scoping**: `gh auth status` succeeding for a **different** host but failing
  `--hostname github.com` → `E_GIT_GH_UNAVAILABLE`; gh credential get returning no
  `password=` → `E_GIT_GH_UNAVAILABLE`.
- **No-fallback matrix**: github SSH-auth-fail + gh-absent → `E_GIT_GH_UNAVAILABLE`;
  + `gh_fallback:false` → `E_GIT_AUTH_FAILED`; non-github SSH-auth-fail →
  `E_GIT_AUTH_FAILED`; probe non-auth-fail → `E_GIT_FAILED` with the mutating op **never
  invoked** (seam asserts op call count == 0).

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
   github.com host (strict grammar) with `gh_fallback` enabled and gh present+authed
   **for github.com**; every other failure surfaces its own code and does **not** fall back.
3. No path can hang: BatchMode + `GIT_TERMINAL_PROMPT=0` + cleared credential helper +
   one bounded op deadline with **best-effort** process-group kill on **every** child
   (including get-url and gh checks) and a bounded shutdown.
4. The repo's stored `.git/config` is never mutated (all `-c`, ephemeral).
5. Success records the transport that **provably** carried the op (explicit URL on the
   command line — never inferred from whether a rewrite fired); failure never reports
   success. Auth/config/timeout/arg/resolution causes each have a code; other git
   failures share `E_GIT_FAILED` with redacted stderr (no fabricated taxonomy).
6. The verb allow-list is closed; an unknown sub-verb is refused, never passed to git.
7. Host detection is structural (strict ASCII grammar, exact `github.com`), never
   substring-contains, IDNA-safe.
8. The endpoint is **pinned as an explicit URL** for both probe and op (push via
   `--push`); a `pushurl`/rewrite cannot make us probe one place and operate on another.
9. No credential or URL-embedded secret ever reaches a payload, log, or the live tee
   (redaction on all surfaced URLs/stderr).
