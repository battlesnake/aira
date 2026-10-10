package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// AIRA-280. Opt-in paging for the two verbs whose reply grows without bound with
// the machine-wide confine_peak_history: confine-dump and confine-budget.
//
// A request opts in with `paged: true` plus an `after_kind`/`after_signature`
// cursor (both empty on the first page). The daemon streams the history ONE ROW
// AT A TIME from just after the cursor, takes whole subjects while the page's
// marshalled size stays within confineHistoryPageBytes, and answers with a `next`
// cursor exactly when another subject existed. Daemon memory per request is
// therefore one page plus the one subject in flight, whatever the history holds.
//
// Opt-in, not default, so version skew fails loudly in both directions with no
// protocol bump: an old daemon ignores `paged` and answers the whole history with
// no `next` (one exchange); an old client never sends `paged` and gets the single
// reply, which now names itself if it is too large (CodeResponseTooLarge).

// confineHistoryPageBytes bounds one page's variable content: the rows taken, the
// returned cursor (which repeats the last subject's signature) and, on a dump's
// first page, the live Waiters/Queues state. The envelope's fixed
// fields are a few hundred bytes, far inside the 12 MiB of headroom to
// MaxFrameBytes. A page always holds at least one subject, so a single subject
// larger than a frame (a signature is bounded only by the 16 MiB request frame)
// still fails loudly through CodeResponseTooLarge: a documented limit.
const confineHistoryPageBytes = 4 << 20

// historyPage is a parsed paging request.
type historyPage struct {
	paged                     bool
	afterKind, afterSignature string
}

// first reports whether this is the first page (empty cursor).
func (p historyPage) first() bool { return p.afterKind == "" && p.afterSignature == "" }

// parseHistoryPage reads the paging arguments. A cursor without `paged`, exactly
// one half of a cursor, or a kind outside the closed vocabulary is an argument
// error: a cursor that was accepted and ignored would silently re-read from the
// beginning.
func parseHistoryPage(args map[string]any) (historyPage, error) {
	paged, _ := args["paged"].(bool)
	page := historyPage{paged: paged, afterKind: stringArg(args, "after_kind"), afterSignature: stringArg(args, "after_signature")}
	if !paged {
		if page.afterKind != "" || page.afterSignature != "" {
			return page, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: after_kind/after_signature require paged")
		}
		return page, nil
	}
	if (page.afterKind == "") != (page.afterSignature == "") {
		return page, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: after_kind and after_signature must be given together")
	}
	if page.afterKind != "" {
		known := false
		for _, kind := range store.ResourcePeakKinds() {
			if string(kind) == page.afterKind {
				known = true
			}
		}
		if !known {
			return page, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: after_kind %q is not a known subject kind", page.afterKind)
		}
	}
	return page, nil
}

// jsonLen is the marshalled size of v, the measure the page budget is kept in.
func jsonLen(v any) int {
	encoded, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(encoded)
}

// readHistoryPage streams one page. measure turns an offered subject into its
// marshalled wire size and a commit that keeps its rows; the subject is accepted
// only if the running total (starting at charged) stays within
// confineHistoryPageBytes or nothing has been taken yet. It returns the cursor of
// the last subject taken, set exactly when the store reports another subject.
func (s *Server) readHistoryPage(page historyPage, charged int, measure func(store.ResourceBudgetSubjectRows) (int, func())) (*runner.ConfineHistoryCursor, error) {
	// A diagnostic batch read, never the admit hot path: see dumpHistoryTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), dumpHistoryTimeout)
	defer cancel()
	used, taken := charged, 0
	var last runner.ConfineHistoryCursor
	more, err := s.db.StreamResourceBudgetSubjects(ctx, page.afterKind, page.afterSignature, func(subject store.ResourceBudgetSubjectRows) bool {
		size, commit := measure(subject)
		candidate := runner.ConfineHistoryCursor{Kind: string(subject.Kind), Signature: subject.Signature}
		// The cursor of the last subject taken rides on the page too, and it carries
		// that subject's whole signature, so it is charged for the candidate: used
		// holds the rows, and the cursor of whichever subject ends up last is
		// always inside the budget.
		if taken > 0 && used+size+jsonLen(candidate) > confineHistoryPageBytes {
			return false
		}
		commit()
		used += size
		taken++
		last = candidate
		return true
	})
	if err != nil {
		return nil, err
	}
	if more && taken > 0 {
		return &last, nil
	}
	return nil, nil
}

// confineDumpPage answers one page of a paged confine-dump. Waiters and Queues
// (live state) ride on the first page only and are charged against its budget.
func (s *Server) confineDumpPage(page historyPage) core.Response {
	var queues []runner.ConfineDumpQueueRow
	var waiters []runner.ConfineDumpWaiterRow
	charged := 0
	if page.first() {
		queues, waiters = s.buildConfineDumpQueuesAndWaiters()
		charged = jsonLen(waiters) + jsonLen(queues)
	}
	var admissions []runner.ConfineDumpAdmissionRow
	next, err := s.readHistoryPage(page, charged, func(subject store.ResourceBudgetSubjectRows) (int, func()) {
		rows := buildConfineDumpAdmissions([]store.ResourceBudgetSubjectRows{subject})
		return jsonLen(rows), func() { admissions = append(admissions, rows...) }
	})
	if err != nil {
		return core.Response{Code: CodeInternal, Error: CodeInternal + ": read usage history: " + err.Error()}
	}
	return core.Response{OK: true, Code: "OK", Data: runner.ConfineDumpResult{
		Verdict:    "ok",
		Scope:      store.ResourceBudgetUniverseScope(),
		Admissions: admissions,
		Waiters:    waiters,
		Queues:     queues,
		Next:       next,
		Paged:      true,
	}}
}

// confineBudgetPage answers one page of a paged confine-budget. Each page is
// sorted as the unpaged reply is; the client re-sorts the joined rows.
func (s *Server) confineBudgetPage(page historyPage) core.Response {
	var verdicts []store.ResourceBudgetVerdict
	next, err := s.readHistoryPage(page, 0, func(subject store.ResourceBudgetSubjectRows) (int, func()) {
		verdict := store.ClassifyResourceBudget(subject, nil)
		return jsonLen(confineBudgetRow(verdict)), func() { verdicts = append(verdicts, verdict) }
	})
	if err != nil {
		return core.Response{Code: CodeInternal, Error: CodeInternal + ": read usage history: " + err.Error()}
	}
	return core.Response{OK: true, Code: "OK", Data: pagedBudgetResult(verdicts, next)}
}

// pagedBudgetResult is confineBudgetResult plus the `paged` echo the client
// requires on every page after the first.
func pagedBudgetResult(verdicts []store.ResourceBudgetVerdict, next *runner.ConfineHistoryCursor) runner.ConfineBudgetResult {
	result := confineBudgetResult(verdicts, next)
	result.Paged = true
	return result
}
