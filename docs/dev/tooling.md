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

`goimports` and `golangci-lint` are optional. If they are unavailable, the
the Makefile skips the corresponding step with an install hint. The configured
lint checks require a golangci-lint v2 build compatible with Go 1.25; an older
binary, such as v1 built with Go 1.24, is skipped. Install the tools with:

```sh
go install golang.org/x/tools/cmd/goimports@latest
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```
