package main

import (
	"context"
	"encoding/json"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
)

type tuiFakeDispatcher struct {
	responses []core.Response
	requests  []core.Request
}

func (d *tuiFakeDispatcher) Dispatch(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	copyRequest := request
	copyRequest.Args = make(map[string]any, len(request.Args))
	for key, value := range request.Args {
		copyRequest.Args[key] = value
	}
	d.requests = append(d.requests, copyRequest)
	if len(d.responses) == 0 {
		return core.Response{Code: "E_FAKE_EMPTY"}
	}
	response := d.responses[0]
	d.responses = d.responses[1:]
	return response
}

func TestDecodeTUIResponseOKCodeAndMalformed(t *testing.T) {
	var target listEnvelope
	if code := decodeTUIResponse(core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"total":1,"rows":[{"id":"AIRA-1"}]}`)}, &target); code != "" || target.Total != 1 {
		t.Fatalf("decoded target=%#v code=%q", target, code)
	}
	if code := decodeTUIResponse(core.Response{OK: false, Code: "E_NOT_FOUND", RawData: json.RawMessage(`{"total":1}`)}, &target); code != "E_NOT_FOUND" {
		t.Fatalf("non-OK code=%q", code)
	}
	if code := decodeTUIResponse(core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"rows":`)}, &target); code != tuiDecodeError {
		t.Fatalf("malformed code=%q", code)
	}
	if code := decodeTUIResponse(core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`null`)}, &target); code != tuiDecodeError {
		t.Fatalf("null data code=%q", code)
	}
}

func TestFetchReadyPanelAcceptsMissingOptionalEnvelopeKeys(t *testing.T) {
	dispatcher := &tuiFakeDispatcher{responses: []core.Response{{OK: true, Code: "OK", RawData: json.RawMessage(`{"total":1,"rows":[{"id":"AIRA-1","ready":true,"verdict":"pass"}]}`)}}}
	result := fetchTUIView(context.Background(), dispatcher, daemon.WorktreeScope{}, viewReady, 7)
	if result.Code != "" || result.Generation != 7 || len(result.Model.Rows) != 1 || result.Model.Footer != "" {
		t.Fatalf("fetch result=%#v", result)
	}
	if len(dispatcher.requests) != 1 || dispatcher.requests[0].Verb != "ready" {
		t.Fatalf("requests=%#v", dispatcher.requests)
	}
}

func TestFetchInsightsKeepsPerTileErrorsInOneGeneration(t *testing.T) {
	dispatcher := &tuiFakeDispatcher{responses: []core.Response{
		{OK: true, Code: "OK", RawData: json.RawMessage(`[{"name":"wip"},{"name":"quota"}]`)},
		{OK: true, Code: "OK", RawData: json.RawMessage(`{"name":"wip","kind":"count","value":2,"unevaluated":false}`)},
		{OK: false, Code: "E_CONFIG_INVALID"},
	}}
	result := fetchTUIView(context.Background(), dispatcher, daemon.WorktreeScope{}, viewInsights, 11)
	if result.Code != "" || result.Generation != 11 || len(result.Model.Tiles) != 2 || result.Model.Tiles[1].ErrorCode != "E_CONFIG_INVALID" {
		t.Fatalf("fetch result=%#v", result)
	}
	for _, request := range dispatcher.requests[1:] {
		if request.Verb != "insights" || request.Args["subverb"] != "show" {
			t.Fatalf("request=%#v", request)
		}
	}
}
