---
{"schema":1,"id":"AIRA-122","project":"aira","title":"Release matrix: publish aira-linux-arm64 alongside amd64","status":"done","kind":"chore","severity":"P1","assignee":null,"milestone":null,"labels":["ci","release"],"hold":false,"relations":[]}
---
Requested by peer session 'deploy' (building the GCP Batch CI runner), 2026-09-06.

Production already ships linux/arm64 (deploy/fastest-deploy.sh pins PLATFORM;
the Azure app box is an Ampere ps_v6) and the new CI runner is Google Axion
(N4A, Neoverse-N3) to match production and build natively rather than under
qemu emulation. v0.1/v0.2 only publish aira-linux-amd64 + its sha256, so
there is no arm64 release asset to install from -- consumers are forced to
build from source.

## Fix

Extend .github/workflows/release.yml's existing build+publish job to a matrix
over {amd64, arm64} (GOARCH set per matrix leg, CGO_ENABLED=0 already set and
applies unchanged -- AIRA is cgo-free by hard rule, so both cross-compile
cleanly from the same linux/amd64 GitHub runner). Publish aira-linux-arm64 +
aira-linux-arm64.sha256 alongside the existing amd64 pair in the same GitHub
Release. Keep asset naming/shape consistent with the existing amd64 pair (same
sha256sum/file-type verification step, matrixed).

## Test

A local cross-compile sanity check (GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go
build ./cmd/aira) before merging is sufficient verification for this
mechanical CI-config change -- no new runtime behaviour to unit-test.

## Resolution

Built directly (mechanical CI-config change, lighter path per CLAUDE.md — not
routed through the two-loop). release.yml's single job split into a
`release` build job matrixed over {amd64, arm64} (each leg builds+shas its own
binary, uploads as a build artifact) and a `publish` job that downloads both
artifacts and creates the one GitHub Release with all four files
(aira-linux-amd64[.sha256], aira-linux-arm64[.sha256], README.md) — a fan-out/
fan-in shape rather than a flat matrix directly calling `gh release create`,
since two matrix legs racing `gh release create` on the same tag would
conflict. Verified locally: `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build
-trimpath -ldflags "-s -w" -o aira-linux-arm64-test ./cmd/aira` exits 0 and
produces a statically-linked ELF 64-bit ARM aarch64 binary. YAML validated
with `python3 -c "import yaml; yaml.safe_load(...)"`. Not yet exercised by an
actual tag push — the first real `v0.3` release will be the live proof.
