---
{"schema":1,"id":"AIRA-66","project":"aira","title":"go:embed all: bakes untracked local scratch (__pycache__, .pytest_cache) into the shipped binary","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["build","pylib","reproducibility"],"hold":false,"relations":[]}
---
## Symptom

The release target is one static Go binary, but its contents currently vary with whoever last ran Python in the tree.

`internal/pylib/extract.go:29,46` uses:

```go
//go:embed all:aira_xdist_governor
//go:embed all:aitest
```

`go list` confirms these untracked, developer-local artefacts are embedded:
- `aira_xdist_governor/__pycache__/__init__.cpython-312.pyc`
- `aitest/.pytest_cache/v/cache/nodeids`
- `aitest/testdata/.pytest_cache/v/cache/lastfailed`

## Why it happens

The directory has a `.gitignore` for `__pycache__/`, but **`go:embed` does not honour `.gitignore`**, and the `all:` prefix specifically defeats the default exclusion of paths beginning `_` or `.`. So `all:` is doing exactly what it says — including the caches.

## Why it matters

Build non-hermeticity: two developers on identical commits produce different binaries, and CI output differs from local output. A stale `.pyc` is *probably* inert (CPython invalidates on mtime), but embedding a `lastfailed` pytest cache means a shipped binary carries one machine's test-failure history.

## Suggested direction

Replace `all:` with explicit patterns (or embed only the files actually needed), so the embedded set is declared rather than inherited from whatever is lying around. A test asserting the embedded file list matches an expected manifest would keep it honest — the current failure mode is silent.

Found during AIRA-58/AIRA-59 merge verification; pre-existing and unrelated to that change.

## Resolution (2026-09-04, backlog-remediation Phase 0, plan §2)

`all:` replaced with a **glob**, not a hand-listed manifest:

```go
//go:embed aira_xdist_governor/*.py
//go:embed aitest/*.py
```

`go:embed`'s `.`/`_` exclusion applies only when a pattern names a DIRECTORY and
go walks its subtree, so a glob matching files directly still picks up
`__init__.py` — verified, not assumed, with
`go list -f '{{.EmbedFiles}}' ./internal/pylib/`: the embedded set is now exactly
the ten tracked `*.py` sources, and `__pycache__/`, `.pytest_cache/`, `testdata/`,
`.gitignore` and `README.md` are all gone. The two artefacts this ticket named as
proof of the defect (`aitest/.pytest_cache/v/cache/nodeids`,
`aitest/testdata/.pytest_cache/v/cache/lastfailed`) were still present in the
working tree when this landed, so the binary really was shipping one machine's
test-failure history until now.

### The equality test, with one oracle

`TestEmbeddedTreesMatchTrackedSources` (`internal/pylib/extract_test.go`) asserts
the embedded set equals the tracked-file listing of each root minus a declared
exclusion list. That listing is the single source of truth — there is no second
hand-maintained manifest to drift from the first, which is why the ticket's
"expected manifest" suggestion was not taken literally. It fails in **both**
directions: a surplus (anything embedded that is untracked) and a shortfall
(a tracked file the pattern misses — a future runtime `.json`, or a runtime
subpackage a `*.py` glob cannot reach, both of which force an explicit decision
rather than silently shipping wrong). Mutation-verified: restoring `all:aitest`
fails it with 15 surplus entries, including the two cache paths above.

It reports `t.Skip` — honestly `unevaluated`, never a fake pass — only when the
VCS binary is absent or the package is outside a work tree. Once both exist,
every subsequent error is a hard failure, and an empty oracle is a failure too
(a vacuous oracle would pass anything).

### Two decisions recorded, not left implicit

- **`.gitignore` and `README.md` are intentionally no longer shipped.** The old
  tests `TestEmbeddedPyLibIncludesImportPackageAndDocumentation` /
  `TestEmbeddedAitestIncludesImportPackageAndDocumentation` asserted their
  presence and `extract.go` called `.gitignore` "the extraction hygiene file",
  but **no Go consumer reads either** (verified by grep: the only references were
  those two tests). They are source-hygiene and documentation for the checkout,
  not runtime dependencies of the extracted package. Both tests are replaced by
  the equality test — they were subset assertions, so `all:`'s untracked scratch
  passed them happily; they could not have caught this ticket's own defect.
- **aitest's co-located `conftest.py` and `test_*.py` stay embedded**, rather
  than moving to a `tests/` subdirectory. A glob cannot exclude them from
  `aitest/*.py`, and moving them would reshape the source-level Python test tier
  for a few KB. They are tracked, so they cost hermeticity nothing, and pytest
  never collects a `conftest.py` reached only through `PYTHONPATH`.
  `aitest/testdata/` is not embedded, because a glob does not recurse — those are
  fixtures for aitest's own source-tree tier, never runtime inputs.

Consequence for AIRA-88 site 3: the extracted tree is now smaller and, more
importantly, its content hash no longer changes with whoever last ran pytest —
so per-machine extraction directories stop multiplying for non-reasons.

`make ci`: exit 0.

### Build-review (Sol, 2026-09-04) — two findings folded in, one recorded as residual

- **FIXED — the equality test only walked the expected root.** `embeddedFiles`
  walked `fs.WalkDir(tree, root)`, so a second `//go:embed` directive on the same
  variable, or a pattern reaching outside the root, embedded bytes the test could
  never see: it would pass while the binary grew. It now walks `"."` — the whole
  `embed.FS` — and anything outside the root is a surplus. Mutation-verified:
  adding `//go:embed aira_xdist_governor/shim.py` to `embeddedAitest` now fails
  the test; before the fix it passed.
- **RECORDED, not fixed — the glob narrows build non-hermeticity, it does not
  eliminate it.** `aitest/*.py` still matches an *untracked* scratch `.py` left in
  the package directory, so `go build` alone can still bake one in; it is the
  equality test (run by `go test ./...`, i.e. `make ci`) that catches it, after
  the fact. Closing that at build time would require a generated tracked-file
  manifest — a second source of truth that drifts from the first, which is
  precisely what the glob was chosen over. The residual is strictly narrower than
  the defect it replaces: `__pycache__`/`.pytest_cache` appear from merely running
  pytest and were being shipped continuously; an untracked scratch `.py` takes a
  deliberate act and fails CI. Mutation-verified in that direction too: an
  untracked `aitest/debug.py` fails the test.
- **NOTED — `t.Skip` is `unevaluated`, but `go test` still exits 0 overall.** So
  this test protects a git checkout (every CI and developer run on this project)
  and not a source-archive build with no VCS. Said plainly in the test's own
  comment rather than left to be discovered.

Verified non-blocking by the same review: no Go or Python consumer reads the
dropped `.gitignore`/`README.md` from the extracted tree; the pytest e2e tests
take their fixtures from the source tree, not the extraction
(`pytest_aitest_e2e_test.go:121`), so dropping `testdata/` breaks nothing; the
changed content hash publishes a new extraction directory safely (older digest
directories stay inert — that is AIRA-88 site 3, unchanged).
