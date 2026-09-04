---
{"schema":1,"id":"AIRA-90","project":"aira","title":"Two byte-size grammars disagree: run.memory_headroom refuses 1.5G while --memory-max accepts it","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["config","runner","simplification"],"hold":false,"relations":[]}
---
PR #12 finding **B15** / plan candidate **79**, filed by the simplification programme's
Phase 0 (plan §4.2, §4.3). Source-verified against master `22cedd6`. Suggested severity **P3**;
the schema has no P3, so filed P2 at the bottom of the band.

## The defect

Two independent byte-size parsers with different grammars:

- `internal/runner/memory_size.go:20 parseMemorySize` (exported as `ParseMemorySize`, `:82`) —
  accepts a decimal mantissa and every spelling of each scale: `K/KB/KiB`, `M/MB/MiB`,
  `G/GB/GiB`, `T/TB/TiB`, bare `B`, case-insensitive. `1.5G` is valid, floored exactly via
  `math/big`.
- `internal/app/project.go:749 parseByteCount` — a single-character suffix only
  (`k/K/m/M/g/G/t/T`), integer mantissa, no `GB`/`GiB`, no decimals.

`parseByteCount` is what validates `run.memory_headroom` (`project.go:673-688`), so
`memory_headroom: "1.5G"` in `.aira/config` is refused with `E_CONFIG_INVALID` while
`--memory-max 1.5G` on the very same concept is accepted. **AIRA-13 (done)** fixed the flag
side and left the config side behind; this is the residue.

## Direction

Delete `parseByteCount` and have the config path use `runner.ParseMemorySize`, keeping the
config-specific range checks (positive, within the reserve cap) where they are. Then one
grammar governs both surfaces and a user who learns `1.5G` from the flag can use it in the
config.

Being fixed by the programme's Phase 0 mechanical sweep.

## Fixed — simplification programme Phase 0 (branch `aira-phase0-mechanical`)

`internal/app/parseByteCount` deleted. `run.memory_headroom` now parses with
`runner.ParseMemorySize`, plus an explicit positivity check because
`ParseMemorySize` accepts `0` while the old parser did not.

One test had to change, and it is worth recording why:
`TestRunAdmissionConfigRejectsMalformedAndHalfConfig` listed
`MemoryHeadroom: "4GB"` under `"malformed"` — a test that pinned the defect in
place. It now uses `"4Gigs"` for the malformed case, and a new test
(`TestRunMemoryHeadroomAcceptsTheSameGrammarAsTheFlag`) asserts that
`4G`, `4GB`, `4GiB`, `1.5G`, `512MiB` and `1024B` all parse and that the config
surface and `runner.ParseMemorySize` agree on every one.
