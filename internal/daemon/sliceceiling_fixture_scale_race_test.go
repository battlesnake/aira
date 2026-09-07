//go:build linux && race

package daemon

// ceilingFixtureRaceScale is the RACE build's value. See the !race file beside
// this one for the full derivation; in one line: the ceiling fixture's helper is
// the test binary re-exec'd, so under -race it is race-instrumented too and every
// byte it touches carries ThreadSanitizer shadow. MEASURED on this project's
// kernel (AIRA-117): 1.20 GiB of cgroup anon charged per 600 MiB touched, a
// factor of 2.03. The app-level touch is divided by this scale so the REAL
// footprint the tests measure stays the same in both build modes.
const ceilingFixtureRaceScale = int64(2)
