package core

import (
	"context"
	"testing"
)

func TestLeaseListDispatchesAsRead(t *testing.T) {
	s, _, _ := coreTestStoreWithClock(t)
	response := New(s).Do(context.Background(), Request{Verb: "lease", Args: map[string]any{"subverb": "ls"}})
	if !response.OK {
		t.Fatalf("lease ls response=%#v", response)
	}
	var data struct {
		Total int   `json:"total"`
		Rows  []any `json:"rows"`
	}
	marshalRoundTrip(t, response.Data, &data)
	if data.Total != 0 || len(data.Rows) != 0 {
		t.Fatalf("lease ls data=%#v", data)
	}

	descriptor, ok := descriptorByName(New(nil).DispatchDescriptors(), "lease")
	if !ok || descriptor.Safety != SafetyRead || descriptor.MCPTool != "aira_lease" || len(descriptor.Operations) != 1 || descriptor.Operations[0].Name != "ls" || descriptor.Operations[0].Safety != SafetyRead {
		t.Fatalf("lease descriptor=%#v, found=%v", descriptor, ok)
	}
	if _, route := Classify("lease", "ls"); route != RouteDaemon {
		t.Fatalf("lease route=%v, want daemon", route)
	}
}
