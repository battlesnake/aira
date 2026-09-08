package daemon

import (
	"context"
	"fmt"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// confineBudget answers AIRA-180's project-less Face 2: "for the commands this
// machine has actually run, what was granted, what did they actually use, and
// what would be recommended instead".
//
// READ-ONLY, structurally. It calls exactly one store reader
// (ResourceBudgetSubjects) and one pure classifier, and there is no write path
// in this file to get wrong. The recommendation it returns is a counterfactual:
// nothing here, and nothing downstream of here, applies it.
//
// It takes the owner/slice arguments the other confine management verbs take
// and ignores the slice deliberately — the history is machine-wide and has no
// per-slice term, so filtering by one would be a fabricated restriction. Owner
// is still validated, because an invalid identity is an argument error whether
// or not this particular verb branches on it.
func (s *Server) confineBudget(args map[string]any) core.Response {
	callerOwner := stringArg(args, "owner")
	if err := runner.ValidateConfineOwner(callerOwner); err != nil {
		return confineManagementError(fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: owner: %w", err))
	}
	if s.db == nil {
		return core.Response{Code: CodeUnavailable, Error: CodeUnavailable + ": state database is unavailable"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)
	defer cancel()
	subjects, err := s.db.ResourceBudgetSubjects(ctx)
	if err != nil {
		return core.Response{Code: CodeInternal, Error: CodeInternal + ": read usage history: " + err.Error()}
	}
	verdicts := make([]store.ResourceBudgetVerdict, 0, len(subjects))
	for _, subject := range subjects {
		verdicts = append(verdicts, store.ClassifyResourceBudget(subject, nil))
	}
	store.SortResourceBudgetVerdicts(verdicts)
	rows := make([]runner.ConfineBudgetRow, 0, len(verdicts))
	for _, verdict := range verdicts {
		rows = append(rows, runner.ConfineBudgetRow{
			Kind: string(verdict.Kind), Subject: store.RenderResourceBudgetSubject(verdict.Kind, verdict.Signature),
			Direction: verdict.Direction, Unevaluated: verdict.Unevaluated,
			UnevaluatedReason: verdict.UnevaluatedReason,
			Budget:            verdict.CurrentBudget, BudgetBasis: verdict.CurrentBudgetBasis,
			ObservedMax: verdict.ObservedMax, UsableSamples: verdict.UsableSamples,
			TotalSamples: verdict.TotalSamples, Buckets: verdict.Buckets,
			Recommendation: verdict.Recommendation, RecommendedBudget: verdict.RecommendedBudget,
		})
	}
	return core.Response{OK: true, Code: "OK", Data: runner.ConfineBudgetResult{
		Verdict: "ok", Scope: store.ResourceBudgetUniverseScope(), Subjects: rows,
	}}
}
