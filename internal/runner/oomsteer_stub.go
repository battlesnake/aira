//go:build !linux

package runner

import "errors"

// The daemon's AIRA-113 steering subsystem is NOT Linux-gated — it compiles
// everywhere and is wired off by default — so this stub is load-bearing rather
// than decorative. It returns an error, never an empty success, because an
// empty success would let a non-Linux daemon record "steered" for a scope whose
// processes it never touched.
func setSubtreeOOMScoreAdj(string, int) (OOMScoreSteerResult, error) {
	return OOMScoreSteerResult{}, errors.New("E_CONFINE_UNAVAILABLE: oom_score_adj steering requires Linux")
}
