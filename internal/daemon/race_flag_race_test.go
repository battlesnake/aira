//go:build race

package daemon

// raceDetectorEnabled reports a binary built with -race. Race instrumentation
// adds shadow memory to every touched page, so a real-cgroup fixture sized
// against an absolute memory cap measures a different peak under -race.
const raceDetectorEnabled = true
