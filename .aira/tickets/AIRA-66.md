---
{"schema":1,"id":"AIRA-66","project":"aira","title":"go:embed all: bakes untracked local scratch (__pycache__, .pytest_cache) into the shipped binary","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["build","pylib","reproducibility"],"hold":false,"relations":[]}
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
