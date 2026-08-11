package store

import "testing"

func FuzzParseSelector(f *testing.F) {
	for _, seed := range []string{
		"AIRA-1",
		".aira/tickets/AIRA-2.md",
		"kind:bug severity:P1",
		`text:"queue body"`,
		"garbage",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(_ *testing.T, raw string) {
		_, _ = ParseSelector(raw)
	})
}
