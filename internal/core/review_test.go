package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"aira/internal/domain"
	"aira/internal/store"
)

type reviewTestStore struct {
	Store
	policy store.ReviewPolicy
	hints  []string
}

func (s reviewTestStore) ReviewPolicy() store.ReviewPolicy { return s.policy }
func (s reviewTestStore) TicketAreaGlobs(string) ([]string, error) {
	return append([]string(nil), s.hints...), nil
}

type reviewSectionFailureStore struct{ reviewTestStore }

func (reviewSectionFailureStore) ListFindings(string) ([]store.FindingRecord, error) {
	return nil, errors.New("E_DB_CORRUPT: findings unavailable")
}
func (reviewSectionFailureStore) Relations(string) ([]domain.RelationView, error) {
	return nil, errors.New("E_DB_CORRUPT: relations unavailable")
}

func TestReviewBundleDistinguishesOmittedAndExplicitEmptyPaths(t *testing.T) {
	s, _ := coreTestStoreWithRoot(t)
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "review", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	defaultTier := 2
	policy := store.ReviewPolicy{Configured: true, DefaultTier: &defaultTier, PathTiers: []store.ReviewPathTier{{Glob: "docs/**", Tier: 0}}}
	c := New(reviewTestStore{Store: s, policy: policy, hints: []string{"docs/**"}})
	omitted := c.Do(context.Background(), Request{Verb: "review", Args: map[string]any{"selector": ticket.ID}})
	if !omitted.OK {
		t.Fatalf("omitted review=%#v", omitted)
	}
	omittedData := omitted.Data.(map[string]any)
	if omittedData["paths"].(map[string]any)["source"] != "area-hints" || !reflect.DeepEqual(omittedData["routing"], []string{"self-review"}) {
		t.Fatalf("omitted paths=%#v", omittedData["paths"])
	}
	empty := c.Do(context.Background(), Request{Verb: "review", Args: map[string]any{"selector": ticket.ID, "paths": []string{}}})
	if !empty.OK {
		t.Fatalf("explicit-empty review=%#v", empty)
	}
	emptyData := empty.Data.(map[string]any)
	if emptyData["paths"].(map[string]any)["source"] != "arg" || emptyData["tier"].(map[string]any)["recommended"] != 2 {
		t.Fatalf("explicit-empty paths/tier=%#v", emptyData)
	}
}

func TestReviewBundleKeepsTierWhenSectionsAreUnevaluated(t *testing.T) {
	s, _ := coreTestStoreWithRoot(t)
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "review", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	defaultTier := 2
	c := New(reviewSectionFailureStore{reviewTestStore{Store: s, policy: store.ReviewPolicy{Configured: true, DefaultTier: &defaultTier}}})
	response := c.Do(context.Background(), Request{Verb: "review", Args: map[string]any{"selector": ticket.ID, "paths": []string{"docs/x.md"}}})
	if !response.OK {
		t.Fatalf("review response=%#v", response)
	}
	data := response.Data.(map[string]any)
	if data["tier"].(map[string]any)["recommended"] != 2 {
		t.Fatalf("tier=%#v", data["tier"])
	}
	for _, key := range []string{"findings", "relations"} {
		section := data[key].(map[string]any)
		if section["code"] != "U_REVIEW_SECTION_UNEVALUATED" || section["unevaluated"] != true {
			t.Fatalf("%s=%#v", key, section)
		}
	}
}

func TestReviewSelectorFailureDoesNotRecommend(t *testing.T) {
	s, _ := coreTestStoreWithRoot(t)
	defaultTier := 3
	c := New(reviewTestStore{Store: s, policy: store.ReviewPolicy{Configured: true, DefaultTier: &defaultTier}})
	response := c.Do(context.Background(), Request{Verb: "review", Args: map[string]any{"selector": "NOPE-9"}})
	if response.OK || response.Code != "E_NOT_FOUND" || response.Data != nil {
		t.Fatalf("missing selector response=%#v", response)
	}

	ambiguous := New(ambiguousReviewStore{reviewTestStore: reviewTestStore{Store: s, policy: store.ReviewPolicy{Configured: true, DefaultTier: &defaultTier}}})
	response = ambiguous.Do(context.Background(), Request{Verb: "review", Args: map[string]any{"selector": "AIRA-1"}})
	if response.OK || response.Code != "E_AMBIGUOUS" {
		t.Fatalf("ambiguous selector response=%#v", response)
	}
}

type ambiguousReviewStore struct{ reviewTestStore }

func (ambiguousReviewStore) Get(string) (store.TicketRecord, error) {
	return store.TicketRecord{}, errors.New("E_AMBIGUOUS: selector matched multiple tickets")
}
