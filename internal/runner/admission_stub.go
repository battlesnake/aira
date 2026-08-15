//go:build !linux

package runner

// nonLinuxAdmission is kept separate from the Linux gate so non-Linux faces
// can report the platform's deliberately disabled admission state.
func nonLinuxAdmission(Request) (state, reason string, waitedMS int64) {
	return "disabled", "", 0
}
