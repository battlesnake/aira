package core

import (
	"sort"

	"aira/internal/codes"
)

// ResponseContractSpec is the transport-neutral response contract projected
// by generated faces. ExitCodes lists the documented codes and their exits;
// DefaultExit is applied to any error code not listed, so the vocabulary is
// never presented as exhaustive.
type ResponseContractSpec struct {
	StableCodes       []string       `json:"stable_codes"`
	Verdicts          []string       `json:"verdicts"`
	UnevaluatedIsPass bool           `json:"unevaluated_is_pass"`
	ExitCodes         map[string]int `json:"exit_codes"`
	DefaultExit       int            `json:"default_exit"`
}

// ResponseContract is the single source for stable response and exit facts
// rendered by the Skill and guide artifacts. The verdict exits are derived
// from verdictExit — the same function Do uses — so the contract cannot drift
// from real dispatch behaviour.
func ResponseContract() ResponseContractSpec {
	exitCodes := make(map[string]int, len(codes.ExitCodes)+4)
	for code, exit := range codes.ExitCodes {
		exitCodes[code] = exit
	}
	// Verdict codes are not error codes and are not in codes.ExitCodes; derive
	// their exits from the authoritative verdictExit so a change there cannot
	// leave this contract lying.
	exitCodes["OK"] = verdictExit("")
	exitCodes["PASS"] = verdictExit("pass")
	exitCodes["FAIL"] = verdictExit("fail")
	exitCodes["UNEVALUATED"] = verdictExit("unevaluated")
	stableCodes := make([]string, 0, len(exitCodes))
	for code := range exitCodes {
		stableCodes = append(stableCodes, code)
	}
	sort.Strings(stableCodes)
	return ResponseContractSpec{
		StableCodes:       stableCodes,
		Verdicts:          []string{"pass", "fail", "unevaluated"},
		UnevaluatedIsPass: false,
		ExitCodes:         exitCodes,
		DefaultExit:       codes.ExitForCode("E_CODE_NOT_IN_MAP_SENTINEL"),
	}
}
