# Go developer tooling

The repository Makefile contains the common local and CI commands:

`make fmt` formats Go files with `gofmt` and uses `goimports` when installed.
`make fmt-check` checks formatting without changing files. `make vet`, `make lint`,
`make build`, `make test`, `make race`, and `make cover` run the corresponding
verification or build task. `make tidy` updates module metadata, and `make ci`
runs formatting checks, vet, build, and tests.

Install the hooks with:

```sh
make install-hooks
```

Run every repository fuzz target for a short smoke test with `make fuzz`. A
single target can be run directly, for example:

```sh
go test -run '^$' -fuzz=FuzzParseTicket -fuzztime=30s ./internal/domain
```

The parser fuzz targets include `FuzzParseTicket`, `FuzzParseFinding`,
`FuzzParseRequirement`, `FuzzParseSelector`, and
`FuzzParseRequirementsTable`.

## Test deadlines

Tests must not assert the scheduler's mood. Every wall-clock deadline a test waits
on goes through `internal/testdeadline`, which draws the distinction the discipline
rests on:

- `testdeadline.After` / `testdeadline.Wait` — a **liveness backstop**, the "did this
  ever happen?" arm of a select or a polling loop. Its value is not a property under
  test, so it is floored at `testdeadline.MinBackstop` and scaled. On a passing run
  the timer never fires and a generous value costs nothing.
- `testdeadline.Eventually` — the preferred replacement for a fixed sleep followed by
  an assertion. It returns the moment the condition holds.
- `testdeadline.Exceeded` — a **latency assertion**, where the bound really is the
  property ("returned promptly rather than waiting out its timeout"). It scales but
  is not floored, so choose a budget that genuinely separates the fast path from the
  slow one, and widen the slow alternative rather than tightening the budget.
- A **negative wait** ("must NOT arrive within X") stays a plain `time.After`.
  Contention delays the thing under test and the timer alike, so it cannot produce a
  false failure, and scaling one only makes the suite slower.

`AIRA_TEST_DEADLINE_SCALE` multiplies every deadline the package hands out. Raise it
on a slow or heavily loaded runner; a value below 1 is ignored rather than honoured.
Building with `-race` applies a further built-in ×4, because race instrumentation
stretches every interval.

`goimports` and `golangci-lint` are optional. If they are unavailable, the
the Makefile skips the corresponding step with an install hint. The configured
lint checks require a golangci-lint v2 build compatible with Go 1.25; an older
binary, such as v1 built with Go 1.24, is skipped. Install the tools with:

```sh
go install golang.org/x/tools/cmd/goimports@latest
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```
