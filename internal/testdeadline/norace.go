//go:build !race

package testdeadline

// raceScale is 1 without the race detector: AIRA_TEST_DEADLINE_SCALE is then the only
// multiplier, and the MinBackstop floor carries the contention margin on its own.
const raceScale = 1.0
