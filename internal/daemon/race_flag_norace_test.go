//go:build !race

package daemon

// raceDetectorEnabled reports a binary built with -race; see race_flag_race_test.go.
const raceDetectorEnabled = false
