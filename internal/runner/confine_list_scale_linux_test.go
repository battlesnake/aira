//go:build linux

package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// AIRA-24 evidence. The queue-position probe pays for a whole ListConfines
// scan on every progress tick, and that scan is O(live scopes) in syscalls —
// so a per-call cost measured against a nearly-idle slice does not, on its
// own, bound the cost on a genuinely contended one. This benchmark supplies
// the missing slope. Build review raised exactly this gap in the first
// version of the cost note.
//
// Run it with:
//
//	go test ./internal/runner/ -run '^$' -bench BenchmarkListConfinesByScopeCount -benchtime 20x
//
// It is a benchmark, so `make test` never pays for it.
func BenchmarkListConfinesByScopeCount(b *testing.B) {
	for _, scopes := range []int{1, 8, 32, 128} {
		b.Run(fmt.Sprintf("scopes=%d", scopes), func(b *testing.B) {
			slice := b.TempDir()
			for i := 0; i < scopes; i++ {
				dir := filepath.Join(slice, fmt.Sprintf(".aira-CONFINE-job-%d-abc%d", 5000+i, i))
				if err := os.Mkdir(dir, 0o755); err != nil {
					b.Fatal(err)
				}
				for name, data := range map[string]string{
					"cgroup.events":  "populated 1\nfrozen 0\n",
					"cgroup.procs":   fmt.Sprintf("%d\n", 5000+i),
					"memory.current": "4096\n",
					"memory.max":     "8192\n",
				} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
						b.Fatal(err)
					}
				}
			}
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ListConfines(ctx, slice, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
