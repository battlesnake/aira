package daemon

import (
	"context"
	"fmt"
	"time"

	"aira/internal/core"
	"aira/internal/store"
)

// confineReportFields is the closed wire vocabulary. AIRA-180 widened the frame
// from {signature, oom, peak_rss} to carry the subject kind and the budget pair,
// because the reserve/cap a job was actually granted was persisted NOWHERE — so
// the over-provisioned direction, this feature's own headline evidence, was
// unevaluable. Anything outside this set is still refused rather than ignored.
var confineReportFields = map[string]bool{
	"signature": true, "peak_rss": true, "oom": true,
	"kind": true, "budget": true, "budget_basis": true,
}

func (s *Server) confineReport(args map[string]any) core.Response {
	if len(args) < 2 || len(args) > len(confineReportFields) {
		return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report requires signature and oom, plus optional peak_rss, kind, budget and budget_basis"}
	}
	for name := range args {
		if !confineReportFields[name] {
			return core.Response{Code: CodeProtocol, Error: fmt.Sprintf("%s: unexpected confine-report field %q", CodeProtocol, name)}
		}
	}
	signature, ok := args["signature"].(string)
	if !ok || signature == "" {
		return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report signature must be non-empty"}
	}
	oom, ok := args["oom"].(bool)
	if !ok {
		return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report oom must be boolean"}
	}
	// An ABSENT kind means confine, matching the column default and the shape
	// every pre-AIRA-180 reporter sends. A PRESENT one must name a known subject
	// kind: silently accepting an unknown string would create rows no reader
	// names and would defeat the discriminator's whole purpose.
	kind := store.ResourcePeakKindConfine
	if raw, exists := args["kind"]; exists {
		named, valid := raw.(string)
		if !valid {
			return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report kind must be a string"}
		}
		known := false
		for _, candidate := range store.ResourcePeakKinds() {
			if string(candidate) == named {
				kind, known = candidate, true
				break
			}
		}
		if !known {
			return core.Response{Code: CodeProtocol, Error: fmt.Sprintf("%s: confine-report kind %q is not a known subject kind", CodeProtocol, named)}
		}
	}
	var peak *int64
	if raw, exists := args["peak_rss"]; exists {
		value, valid := exactAdmitInt64(raw)
		if !valid || value <= 0 {
			return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report peak_rss must be positive"}
		}
		peak = &value
	}
	var budget *int64
	if raw, exists := args["budget"]; exists {
		value, valid := exactAdmitInt64(raw)
		if !valid || value <= 0 {
			return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report budget must be positive"}
		}
		budget = &value
	}
	basis := ""
	if raw, exists := args["budget_basis"]; exists {
		named, valid := raw.(string)
		if !valid {
			return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report budget_basis must be a string"}
		}
		basis = named
	}
	// Refused at the boundary rather than normalised away. Half a budget pair is
	// a reporter bug, and a budget whose basis names no cap:/reserve: family is a
	// quantity the classifier cannot compare with anything — see
	// store.ResourceBudgetFamily.
	if (budget == nil) != (basis == "") {
		return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report budget and budget_basis must be reported together"}
	}
	if basis != "" && store.ResourceBudgetFamily(basis) == "" {
		return core.Response{Code: CodeProtocol, Error: fmt.Sprintf("%s: confine-report budget_basis %q names no cap:/reserve: family", CodeProtocol, basis)}
	}
	if s.db == nil {
		return core.Response{Code: CodeUnavailable, Error: CodeUnavailable + ": state database is unavailable"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)
	defer cancel()
	if err := s.db.RecordConfinePeak(ctx, store.ResourcePeakObservation{
		Kind: kind, Signature: signature, Peak: peak, OOM: oom,
		Budget: budget, BudgetBasis: basis, At: time.Now(),
	}); err != nil {
		return core.Response{Code: CodeInternal, Error: CodeInternal + ": record confine peak: " + err.Error()}
	}
	return core.Response{OK: true, Code: "OK", Data: map[string]any{"recorded": true}}
}
