package core

import (
	"reflect"
	"sort"
	"testing"
)

func TestDispatchMetadataCoversEveryHandlerAndConsumedArgument(t *testing.T) {
	c := New(nil)
	descriptors := c.DispatchDescriptors()
	if len(descriptors) != len(c.verbs) {
		t.Fatalf("descriptor count=%d, dispatch count=%d", len(descriptors), len(c.verbs))
	}
	for name, spec := range c.verbs {
		descriptor, ok := descriptorByName(descriptors, name)
		if !ok {
			t.Fatalf("dispatch verb %q has no descriptor", name)
		}
		declared := map[string]bool{}
		for _, arg := range descriptor.Args {
			if arg.Name == "" || (arg.Kind != ArgKindString && arg.Kind != ArgKindBool && arg.Kind != ArgKindStringList) {
				t.Fatalf("verb %q has invalid arg metadata: %#v", name, arg)
			}
			if declared[arg.Name] {
				t.Fatalf("verb %q declares duplicate arg %q", name, arg.Name)
			}
			declared[arg.Name] = true
		}
		for _, consumed := range spec.Consumed {
			if !declared[consumed] {
				t.Fatalf("verb %q consumes undeclared arg %q", name, consumed)
			}
		}
	}
}

func TestDispatchDescriptorsAreStableReadOnlyCopies(t *testing.T) {
	c := New(nil)
	first := c.DispatchDescriptors()
	index, ok := descriptorByName(first, "create")
	if !ok || len(index.Args) == 0 || len(index.Args[1].Enum) == 0 {
		t.Fatal("expected metadata")
	}
	for i := range first {
		if first[i].Name == "create" {
			first[i].Name = "changed"
			first[i].Args[0].Name = "changed"
			first[i].Args[1].Enum[0] = "changed"
		}
	}
	second := c.DispatchDescriptors()
	got, ok := descriptorByName(second, "create")
	if !ok {
		t.Fatal("descriptor disappeared after mutating returned copy")
	}
	if got.Args[0].Name == "changed" || got.Args[1].Enum[0] == "changed" {
		t.Fatalf("descriptor view aliases dispatch metadata: %#v", got.Args[0])
	}
}

func TestCanonicalDispatchNamesAndAliases(t *testing.T) {
	descriptors := New(nil).DispatchDescriptors()
	got := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		got = append(got, descriptor.Name)
	}
	sort.Strings(got)
	want := []string{"check", "claim", "count", "create", "find", "grep", "heartbeat", "help", "id", "import", "init", "link", "list", "mv", "ready", "reconcile", "release", "set", "show", "touch", "unlink"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatch names=%v, want=%v", got, want)
	}
}

func descriptorByName(descriptors []DispatchDescriptor, name string) (DispatchDescriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor, true
		}
	}
	return DispatchDescriptor{}, false
}
