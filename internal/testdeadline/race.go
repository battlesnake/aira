//go:build race

package testdeadline

// raceScale is the built-in multiplier applied when the binary is built with the race
// detector. Race instrumentation adds a synchronisation-tracking cost to every memory
// access, so wall-clock intervals stretch by a large and load-dependent factor. AIRA-20
// recorded the concrete case: a watch event asserted to arrive inside a 30ms poll
// interval took 124ms under -race on a shared CI runner, with zero data races reported.
//
// This is why the race job could be dropped from CI for a reason that was never a race.
const raceScale = 4.0
