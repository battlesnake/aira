# AIRA #30 — `aira git`: non-interactive git network ops with SSH→gh-credential auth fallback

Status: PLAN APPROVED (v8 — Sol APPROVE-PLAN after 6 adversarial rounds; build-review checklist in §14)
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

> **Decide the transport with a read-only probe of the remote's effective endpoint,
> then run the real op exactly once over the chosen transport — keeping the remote name
> so all `remote.<name>.*` semantics hold, overriding only its URL for a verified
> fallback, and reporting git's own effective URL as the transport (never an assumption).**

### 3.0 Keep the remote NAME; override only its URL; report git's EFFECTIVE URL

v2 tried pinning an explicit URL on the command line. Sol round-2 showed that is
**doubly wrong**: (a) inherited `url.*.insteadOf` rewrites an explicit URL too, so a
user's "always-ssh" rewrite would turn our canonical HTTPS back into SSH while we
falsely report `https-gh`; and (b) `git fetch <URL>` ignores `remote.<name>.fetch` (so
`origin/*` tracking refs never update) and `git push <URL>` ignores
`pushurl`/`pushRemote` — an explicit URL silently breaks normal semantics. So v3:

- **Ops keep the remote name.** `git fetch origin [refspecs]`, `git push origin
  [refspecs]`, `git ls-remote origin`; `clone` takes the URL argument. All
  `remote.<name>.*` semantics (fetch refspecs, pushurl, pushRemote) are preserved.
- **The transport we REPORT is git's own effective URL, never our assumption.** We
  obtain it from git: `git ls-remote --get-url <remote>` yields the post-`insteadOf`
  effective fetch URL; the push endpoint is resolved analogously (§8). Host/scheme for
  classification and the reported `url`/`auth` come from **that** effective URL. If git
  would rewrite it, we see the rewritten form and report the truth.
- **The fallback overrides only the remote's URL, invocation-locally**, via `-c
  remote.<name>.url=<canonical-https>` (+ `pushurl` for push) — keeping the remote name
  so refspec/tracking semantics are intact (§5). Before committing a fallback we run the
  **rewrite-safety check** (§5.1): if any effective `url.*.insteadOf` /
  `url.*.pushInsteadOf` value is a prefix of our canonical `https://github.com/owner/
  repo.git` (i.e. git would rewrite our fallback URL), the fallback is **refused
  honestly** with `E_GIT_FALLBACK_BLOCKED` — we never run over an unknown transport and
  claim `https-gh`.

The probe and op therefore both address the same remote under the same config; the sole
residual is a config change landing *between* them (§3.2).

### 3.1 The probe and the classification

The probe is a read-only `git ls-remote --heads <effective-URL>` under BatchMode SSH,
where the effective URL is resolved **per verb** (§8.2) — push probes the effective
*push* URL, fetch/ls-remote the effective *fetch* URL, clone the URL argument — so the
probe endpoint is exactly the op endpoint. It authenticates with no side-effect. **Both the probe AND (for reporting) the committed op are classified** by
the conservative SSH-auth classifier (§6.1). Outcomes:

- Probe exits 0 → SSH auth works → run the real op over SSH (remote name, unchanged).
- Probe fails and is **positively classified as an SSH-auth failure** (§6.1) AND the
  effective host is exactly github.com AND `gh_fallback` is enabled AND the remote is a
  fallback candidate (§8.1) AND `gh` is available+authed for github.com AND the
  rewrite-safety check passes (§5.1) → run the real op over HTTPS via the URL override
  (§5), with the gh credential helper.
- Probe classified auth-fail but no fallback available/safe (non-github, non-candidate,
  gh missing/not authed, fallback disabled, or rewrite-blocked) → `E_GIT_AUTH_FAILED` /
  `E_GIT_GH_UNAVAILABLE` / `E_GIT_FALLBACK_BLOCKED`; op never runs.
- Probe fails for any **non-auth** reason (host-key, DNS, network, repo-not-found, or
  *unclassifiable*) → `E_GIT_FAILED` with redacted stderr; **no fallback, op never runs.**

If the committed op itself then fails with an SSH-auth signature (auth changed after a
passing probe — key expiry, agent swap, endpoint race), it is reported
**`E_GIT_AUTH_FAILED`** (the single real attempt is classified for reporting), **never
retried**, never mislabeled `E_GIT_FAILED`.

`clone` probes the clone URL argument. `ls-remote` **is** the probe (runs once as the
real op); with no mutation risk, on a classified SSH-auth failure it may fall back and
re-run over HTTPS directly.

### 3.2 TOCTOU note (accepted)

The only residual window is a config/auth change landing *between* the probe and the
single committed op. Accepted: auth does not flip mid-second in practice, both address
the same remote under the same config, and a post-probe op auth-failure is reported
honestly as `E_GIT_AUTH_FAILED` (never masked, never retried).

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

## 5. The HTTPS/gh fallback — URL override on the same remote, verified, ephemeral

A `-c remote.<name>.url=…` override is **rejected** (Sol round-3): `remote.<name>.url`
is *multi-valued* and `-c` **appends**, so `git fetch` keeps using the original (first)
URL while the rewrite-check passes — a silently-dishonest transport; and an empty `-c
pushurl=` does not *clear* like `credential.helper=` does. Instead the fallback runs
against a **synthetic, invocation-local remote** in a fresh config key namespace, so its
URL is unambiguously single, while the copied fetch refspecs preserve tracking semantics:

```
git \
  -c remote.aira-fallback.url=https://github.com/<owner>/<repo>.git \
  -c remote.aira-fallback.fetch=<each remote.<name>.fetch value, copied verbatim> \
  -c credential.helper= \
  -c credential.helper="!gh auth git-credential" \
  <op> aira-fallback [refspecs…]
```

- The synthetic name (`aira-fallback`, validated **absent** from `git remote`; else a
  unique suffix, or refuse) has **no pre-existing config**, so `remote.aira-fallback.url`
  has exactly one value — the fallback provably uses our canonical HTTPS URL, never the
  original (fixes the multi-value append bug).
- **Known `remote.<name>.*` settings are copied** to `remote.aira-fallback.*` from a
  **closed allow-list** — `fetch` (tracking `refs/remotes/<name>/*`), `tagOpt`, `prune`,
  `pruneTags`, `partialclonefilter`, `promisor` — preserving multi-values, so the fallback
  fetch changes the *same* local refs/tags as native `git fetch <name>` (Sol round-5 P1).
  We use an allow-list rather than copy-everything (Sol round-6) so a future/unknown
  `remote.<name>.*` key cannot silently change behaviour after the rename: any
  `remote.<name>.*` key **outside** the allow-list (other than `url`/`pushurl`, which we
  set) → refuse `E_GIT_FALLBACK_BLOCKED` reason=`unsupported-remote-config`. A
  native-vs-fallback ref/tag equivalence test guards the copied semantics.
- **push refspec (fallback only).** We do **not** reimplement `push.default` for the
  synthetic remote (Sol round-4: `simple`/`current`/`matching`/`nothing`/triangular all
  differ — synthesising "current→upstream" would push the wrong branch or wrongly reject).
  Instead: an **explicit** refspec from the caller is used verbatim; else a configured
  `remote.<name>.push` refspec is copied to the synthetic remote; else — a no-refspec
  fallback push that would rely on `push.default` — we **refuse** `E_GIT_FALLBACK_BLOCKED`
  reason=`explicit-refspec-required` (Sol round-5: it is not an *invalid arg* — the push
  is perfectly valid natively; it is only the *fallback* that cannot replicate
  `push.default`; the message suggests `aira git push origin HEAD:<branch>`). The
  exceptional (ssh-down) path asks the caller to be explicit rather than let us guess git's
  push semantics. (The **native** push keeps full `push.default` — §5.2.)
- `credential.helper=` (empty) **first clears** any inherited/system helper so a stray
  interactive helper cannot hang or leak; the second sets **only** gh.
- Nothing is written to `.git/config` (all `-c`, ephemeral). `clone` has no remote — it
  takes the explicit canonical URL directly.

### 5.0 Single-destination requirement for **push** (native AND fallback)

A `git push <remote>` with multiple `pushurl`/`url` values pushes to **all** of them —
several remote side-effects over possibly different transports, only one of which was
probed. That breaks "one transport, at most once" and cannot be honestly reported as a
single transport (Sol round-4: labelling it "best-effort" weakens the invariant rather
than satisfying it). So **every push** — native or fallback — first checks `git remote
get-url --push --all <remote>`: **more than one push destination → refuse**
`E_GIT_REMOTE_UNSUPPORTED` (reason=`multiple-destinations`). For a fallback specifically
we additionally require the (single) URL set to agree on one `owner/repo`; disagreement →
`E_GIT_FALLBACK_BLOCKED`. `fetch` is unaffected: git fetches from a single (first) URL,
so it has one transport, reported honestly. Multi-destination push is **D7 (deferred,
refused honestly in v1)**.

### 5.2 Native path takes no overrides

When SSH auth works (or the remote is native HTTPS with gh serving credentials), the op
runs with the **real remote name and zero URL/refspec overrides** — full native
`push.default`/tracking semantics (subject to the §5.0 single-push-destination guard).
Only the two `credential.helper` `-c` lines are added on the native-HTTPS-github case
(to supply the gh credential); nothing else changes.

### 5.3 Clone is special (no remote to synthesise)

`clone` has no in-repo remote config, so the synthetic-remote mechanism does not apply
(`git clone aira-fallback` is meaningless — Sol round-4). Clone works directly off the
URL argument: probe `git ls-remote <url>`; native `git clone <url> [dir]`; **fallback
`git -c credential.helper= -c credential.helper="!gh auth git-credential" clone
https://github.com/<owner>/<repo>.git [dir]`** using the canonical URL we construct,
gated by the same rewrite-safety check (§5.1) — which for a fresh clone enumerates only
global/system `insteadOf` (there is no repo-local config yet). A test proves the
post-rewrite effective clone URL is our canonical HTTPS.

### 5.1 Rewrite-safety check — never claim `https-gh` over an unknown transport

Because `url.*.insteadOf` / `pushInsteadOf` rewrite *any* URL including our override, we
verify BEFORE committing a fallback that no such rule would redirect our canonical HTTPS
URL. Enumerate the effective rules (`git config --get-regexp
'^url\..*\.(insteadof|pushinsteadof)$'`); if any **value** is a non-empty prefix of
`https://github.com/<owner>/<repo>.git`, git would rewrite our fallback to some other
transport/host → we **refuse honestly** with `E_GIT_FALLBACK_BLOCKED` (message names the
offending rule and the fix: remove it or configure an HTTPS remote). Only when no rule
matches do we run the fallback and report `https-gh`, which is then *provably* the
transport git used. The reported `url`/`host` always come from git's effective
resolution (§3.0), never our assumption.

`gh` availability is verified **before** the fallback, **scoped to github.com**: `gh
auth status --hostname github.com` (non-interactive, under the op deadline) — missing
binary or non-zero → `E_GIT_GH_UNAVAILABLE`. As a strengthening we confirm the helper
actually serves a github.com credential by feeding `protocol=https\nhost=github.com\n\n`
to `gh auth git-credential get` and checking a `password=` line comes back (token value
never logged); no credential → `E_GIT_GH_UNAVAILABLE` (honest: the precondition the
fallback depends on is unmet). Only when all hold do we run the op over the fallback.

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
2. stderr matches one of the closed, **publickey-anchored** signatures (Sol round-2:
   the bare `<user>@<host>: Permission denied` was dropped — it is not publickey-
   specific and remote-program stderr could spoof it):
   - `Permission denied (publickey` — the anchored token; optionally prefixed by
     `<git-user>@<effective-host>:` for the exact normalised host.
   - `Could not read from remote repository` **only when** preceded by a
     `Permission denied (publickey` line in the same stderr (git's generic tail after a
     publickey rejection; alone it is ambiguous → not classified).

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
| `E_GIT_FALLBACK_BLOCKED`  | 1 | a fallback was needed but cannot be synthesised honestly — `reason` ∈ {`insteadof-rewrite` (a rule would rewrite our canonical HTTPS), `multiple-destinations`, `explicit-refspec-required` (no-refspec fallback push that would rely on `push.default`), `unsupported-remote-config` (a `remote.<name>.*` setting we cannot faithfully replicate)} |
| `E_GIT_REMOTE_UNSUPPORTED`| 1 | remote scheme/host not a supported network target (local/file/git:// ) |
| `E_GIT_REMOTE_UNRESOLVED` | 1 | could not resolve the target URL (no such remote / no upstream) |
| `E_GIT_TIMEOUT`           | 3 | op exceeded `git.op_timeout_seconds`; child process-group killed |
| `E_GIT_FAILED`            | 1 | git itself failed for a non-auth reason (non-ff, conflict, not-found, unclassifiable) — raw stderr surfaced |
| `E_GIT_ARG_INVALID`       | 2 | unknown/disallowed sub-verb, or malformed args |
| `E_CONFIG_INVALID`        | 2 | (reused) malformed `git` config block |

Exit-code choice mirrors the runner family (auth/failed = 1, timeout = 3, arg/config = 2).

## 8. Target-URL resolution and the strict URL grammar

- `clone <url> [dir]` — URL is the first positional. Host/scheme parsed from it.
- `push [remote] [refspec…]` — the probed, classified, and reported endpoint is the
  **effective PUSH URL** (Sol round-5: `git ls-remote <remote>` uses the *fetch* URL, so
  a `pushurl` that differs would make us probe the wrong endpoint). fetch/ls-remote use
  the **effective FETCH URL**. Remote name defaults to `origin`; empty/no-such-remote →
  `E_GIT_REMOTE_UNRESOLVED`. (v1 does **not** consult the branch's upstream; `origin`
  default + explicit `[remote]` arg covers the cases — D3.)

### 8.2 Effective-URL resolution (per verb, `insteadOf` applied EXACTLY ONCE)

Sol round-6 P0: `git remote get-url --push` and `git ls-remote --get-url` **already
expand** `insteadOf`/`pushInsteadOf`. Treating their output as raw and applying the rules
again double-applies (wrong host under chained rules), and handing that to `git ls-remote`
would apply them a *third* time — so probe, report, and op could all disagree. The
resolution is therefore:

1. **Raw URL from config, never from a `--get-url` expansion**: push → `git config
   --get-all remote.<name>.pushurl`, and only if **absent** fall back to
   `remote.<name>.url` (single per the §5.0 guard); fetch/ls-remote → `git config
   --get-all remote.<name>.url` (git uses the first value); clone → the URL argument.
   `git config` returns the **raw, unrewritten** value.
2. **Apply `insteadOf`/`pushInsteadOf` EXACTLY ONCE ourselves** — git's own single pass,
   **longest-matching-prefix** `<base>`, no match → passthrough, **not iterated** (a
   chained A→B where B also matches is *not* re-applied, matching git). Precedence: for a
   push, `pushInsteadOf` is tried first, **but only when the raw URL came from
   `remote.<name>.url`** — git does **not** apply `pushInsteadOf`/`insteadOf` to an
   **explicit `pushurl`** (it is used verbatim); for fetch, only `insteadOf`. This pure,
   side-effect-free string rewrite is the **only** git behaviour we reimplement (fully
   specified — unlike `push.default`). The result E is git's committed-op endpoint and the
   reported `url`/`host`/`auth`.
3. **Probe E with rewriting suppressed** so git cannot reapply: the throwaway `git
   ls-remote <E>` runs in a **genuinely rewrite-free config environment** — not just
   `GIT_CONFIG_GLOBAL=/dev/null` + `GIT_CONFIG_SYSTEM=/dev/null` + a non-repo cwd, but
   also with every **command-scope injection cleared**: `GIT_CONFIG_COUNT`, all
   `GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`, and `GIT_CONFIG_PARAMETERS` unset (Sol round-7)
   — so no `insteadOf` exists there and E is used verbatim → probe endpoint == E == op
   endpoint, per verb, even under chained rules.

(Build-review must verify our single-pass matches git — longest-prefix, tie-break, case
sensitivity, `pushInsteadOf` precedence — against fixture repos; see §11.)
- **Remote-name grammar + arg option-injection is closed** (Sol rounds 2–3): because the
  remote name is interpolated into config-query keys (`remote.<name>.fetch`, `--get-all
  remote.<name>.url`), it is validated against a **deliberately narrow grammar** —
  `[A-Za-z0-9][A-Za-z0-9._/-]*`, no leading `-`, no `=`, no control/whitespace, no `..`
  — else `E_GIT_ARG_INVALID`; this prevents a name from mis-targeting a different config
  subsection or being read as an option. Every refspec likewise must not begin with `-`,
  and all git invocations place a `--` terminator before positional remote/refspec/URL
  arguments. Refspecs pass through **after** a `--` delimiter in the AIRA argv (mirroring
  `aira run`). No git *option* flags are accepted in v1 (closed argv: `remote` +
  refspecs) — small, un-injectable surface; richer flags are D4. The synthetic fallback
  remote uses a **fixed constant** name (`aira-fallback`), never a user-derived string,
  so no user input reaches a constructed config key.

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
- **userinfo**: for SSH, the user must be `git` (the only user gh's fallback maps to); a
  non-`git` user on a github SSH URL is **not a fallback candidate** (run as-is; no
  reconstruction — we will not silently change the identity). HTTPS userinfo
  (`user:token@…`) means the **caller supplied their own credential**: we do **not**
  silently strip it and swap in gh (Sol round-2 — that would change requested auth
  without notice). Such a remote is **not a fallback candidate**: it runs as-is,
  honouring the caller's credential, with the userinfo **redacted** in every surfaced
  field (payload/log/tee). (We never *invent* a credential the caller didn't ask for,
  and never *echo* one they did.)
- port: empty, or an explicit numeric port equal to the scheme default (22 ssh / 443
  https); any other explicit port on a github URL → not a fallback candidate (run as-is).
- reject control characters, whitespace, and any non-ASCII / IDNA/Unicode host
  (`E_GIT_ARG_INVALID`) — no punycode confusables.

**Path grammar for fallback reconstruction (Sol round-2)**: to be a fallback candidate
the path must be **exactly two non-empty segments** `owner/repo` after the host, where
`owner` and `repo` each match `[A-Za-z0-9._-]+`, `repo` carries at most one trailing
`.git` (normalised off), and there is **no** query, fragment, percent-escape, `.`/`..`
segment, or duplicate/trailing slash. Anything else (extra path segments, gists, etc.)
parses as github.com but is **not a fallback candidate** — it runs honestly over its
native transport; we simply do not claim the gh path for it.

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
  non-default port, non-`git` ssh user (→ not a candidate), extra path segment / gist /
  `..` / percent-escape / query / fragment (→ not a candidate), control chars,
  `git://`/`http`/local → each the correct classification/error. HTTPS `user:token@`
  userinfo → **not a candidate (run as-is, credential honoured), userinfo redacted** in
  every surfaced field (never stripped-and-swapped). Remote name / refspec beginning
  with `-` → `E_GIT_ARG_INVALID`.
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
- **Synthetic-remote fallback**: the fallback argv targets the **synthetic `aira-fallback`
  remote** (never the user's remote name in a config key, never a raw URL), carries
  exactly one `-c remote.aira-fallback.url=https://github.com/owner/repo.git`, copies each
  `remote.<name>.fetch` value verbatim as `-c remote.aira-fallback.fetch=…`, and the two
  `credential.helper` `-c` lines; **no `git config` write** ever appears (the seam records
  every argv). A stub with the original remote holding a *second* url value proves the
  synthetic remote is used (not the appended-override that the multi-value bug would hit).
- **Single-destination push guard (§5.0)**: a `--push --all` returning two destinations
  → `E_GIT_REMOTE_UNSUPPORTED` reason=`multiple-destinations` **on the native path too**
  (not just fallback), op never runs; a fallback whose URLs disagree on owner/repo →
  `E_GIT_FALLBACK_BLOCKED`.
- **Fallback push refspec**: explicit caller refspec → used verbatim; configured
  `remote.<name>.push` → copied to the synthetic remote; **neither** (would rely on
  `push.default`) → `E_GIT_ARG_INVALID` — asserts we do **not** synthesise a
  push.default-derived refspec.
- **Clone fallback (§5.3)**: clone uses the **explicit canonical HTTPS URL** (not a
  synthetic remote); the argv is `... clone https://github.com/owner/repo.git [dir]` with
  the two credential `-c` lines; rewrite-safety enumerates only global/system insteadOf.
- **Push probes the PUSH URL (§8.2)**: a remote whose `pushurl` differs (different host)
  from its fetch URL → the probe/classification/report use the **push** URL; raw values
  come from `git config --get-all` (never a `--get-url` expansion), so `insteadOf` is
  applied exactly once.
- **Rewrite applied exactly once, no reapplication**: table-driven longest-prefix
  `insteadOf` / `pushInsteadOf`-precedence / no-match passthrough / **chained A→B where B
  also matches is NOT re-applied** (matches git's single pass); a trace-instrumented test
  asserts probe endpoint == committed-op endpoint for fetch, push-with-`pushurl`, clone,
  and a chained-rule config (the probe's clean-config-env means git never reapplies).
- **Allow-list settings copy (§5)**: a remote with `tagOpt`/`prune`/`fetch` → the synthetic
  fallback argv copies each from the allow-list; a remote with an **unknown**
  `remote.<name>.*` key → `E_GIT_FALLBACK_BLOCKED` reason=`unsupported-remote-config`; a
  native-vs-fallback stub asserts identical resulting ref/tag update set.
- **Build-review fixture check (Sol round-6)**: the single-pass `insteadOf`/`pushInsteadOf`
  resolver is validated against **real `git`** fixture repos (longest-prefix, tie-break,
  case sensitivity, push precedence) — an executable reproduction, not only unit stubs, so
  our reimplementation cannot silently drift from git.
- **Refspec-less fallback push** → `E_GIT_FALLBACK_BLOCKED` reason=`explicit-refspec-
  required` (**not** `E_GIT_ARG_INVALID`), op never runs.
- **Remote-name grammar**: names with `-`-lead, `=`, `..`, control chars → `E_GIT_ARG_INVALID`.
- **Rewrite-safety (§5.1)**: a simulated `url."git@github.com:".insteadOf=
  "https://github.com/"` (an "always-ssh" rule) whose value prefixes our canonical HTTPS
  → `E_GIT_FALLBACK_BLOCKED`, op **never runs**; a non-matching rule → fallback proceeds
  and reports `https-gh`.
- **Effective-URL reporting**: the reported `url`/`host`/`auth` come from git's
  effective resolution (a stub `ls-remote --get-url` that rewrites) — a rule that
  rewrites the remote to a non-github host makes the op NOT a fallback candidate and the
  reported host is the rewritten one (never our raw assumption).
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
- **D7 multi-destination push** — a remote with several push destinations is refused for
  *every* push (`E_GIT_REMOTE_UNSUPPORTED`), native and fallback alike; coherent
  multi-destination push is out of v1. (Multi-URL *fetch* runs — git uses the first URL.)

## 13. Invariants (for the build review to attack in both directions)

1. A **mutating** op (`push`) runs **at most once**, over exactly one transport, to
   **exactly one destination** (probe-then-commit; multi-destination push refused §5.0).
   No stderr-scrape-and-retry on the mutation path; no `push.default` reimplementation
   (fallback push requires an explicit/configured refspec).
2. The gh fallback fires **only** on a positively-classified SSH-auth failure to a
   github.com host (strict grammar) with `gh_fallback` enabled and gh present+authed
   **for github.com**; every other failure surfaces its own code and does **not** fall back.
3. No path can hang: BatchMode + `GIT_TERMINAL_PROMPT=0` + cleared credential helper +
   one bounded op deadline with **best-effort** process-group kill on **every** child
   (including get-url and gh checks) and a bounded shutdown.
4. The repo's stored `.git/config` is never mutated (all `-c`, ephemeral).
5. The reported transport is git's **effective** resolved URL (post-`insteadOf`), and a
   fallback is committed only after the rewrite-safety check (§5.1) proves git will use
   our HTTPS URL — so `https-gh`/`ssh` is never a claim we can't back. Failure never
   reports success. Auth/config/timeout/arg/resolution/fallback-blocked causes each have
   a code; other git failures share `E_GIT_FAILED` with redacted stderr (no fabricated
   taxonomy).
6. The verb allow-list is closed; an unknown sub-verb is refused, never passed to git.
   Remote/refspec args beginning with `-` are refused; git invocations use a `--`
   terminator — no option injection.
7. Host detection is structural (strict ASCII grammar, exact `github.com`), never
   substring-contains, IDNA-safe; fallback reconstruction requires a strict two-segment
   `owner/repo` path.
8. The native path keeps the **real remote name** with zero overrides (full semantics);
   the fallback runs a **synthetic single-URL remote** with copied fetch refspecs, and
   only after the single-destination guard (§5.0) and rewrite-safety check (§5.1) — so
   the fallback provably uses our canonical HTTPS URL exactly once, never the original
   (multi-value) URL and never several destinations.
9. No credential is ever invented for the caller or swapped in silently; no
   credential or URL-embedded secret ever reaches a payload, log, or the live tee
   (redaction on all surfaced URLs/stderr).

## 14. Build-review checklist (Sol APPROVE-PLAN, round 7)

Implementation-verified items (not plan blockers) the build review must confirm:

1. **Genuinely rewrite-free probe env** — unset `GIT_CONFIG_COUNT`, all
   `GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`, `GIT_CONFIG_PARAMETERS` (plus
   `GIT_CONFIG_GLOBAL`/`SYSTEM=/dev/null` and a non-repo cwd), so no command-scope config
   injection can reapply `insteadOf` to the probe.
2. **Raw-URL selection** — absent vs multiple `pushurl`/`url`; encode git's rule that an
   explicit `pushurl` is used verbatim (no `pushInsteadOf`/`insteadOf`).
3. **Fixture-test the single-pass resolver against the installed git** — longest-prefix
   tie-breaking, case sensitivity, includes/conditional-includes, chained non-iteration.
4. **Trace-assert endpoint agreement** — resolved endpoint == probe endpoint == committed
   endpoint == reported transport, for every verb and every failure branch.
5. **`unsupported-remote-config` detection** reads every effective config scope and
   **fails closed on an enumeration error** (never silently proceeds).
