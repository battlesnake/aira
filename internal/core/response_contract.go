package core

import (
	"sort"

	"aira/internal/store"
)

// ResponseContractSpec is the transport-neutral response contract projected
// by generated faces. ExitCodes includes both store errors and core verdicts.
type ResponseContractSpec struct {
	StableCodes       []string       `json:"stable_codes"`
	Verdicts          []string       `json:"verdicts"`
	UnevaluatedIsPass bool           `json:"unevaluated_is_pass"`
	ExitCodes         map[string]int `json:"exit_codes"`
}

// ResponseContract is the single source for stable response and exit facts
// rendered by the Skill and guide artifacts.
func ResponseContract() ResponseContractSpec {
	exitCodes := make(map[string]int, len(store.ExitCodes)+4)
	for code, exit := range store.ExitCodes {
		exitCodes[code] = exit
	}
	exitCodes["OK"] = 0
	exitCodes["PASS"] = 0
	exitCodes["FAIL"] = 1
	exitCodes["UNEVALUATED"] = 3
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
	}
}
