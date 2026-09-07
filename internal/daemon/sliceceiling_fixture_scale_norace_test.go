//go:build linux && !race

package daemon

// ceilingFixtureRaceScale is how many bytes of REAL cgroup memory one byte of the
// ceiling fixture helper's anonymous allocation actually charges in this build
// mode. An ordinary build charges one for one, so the scale is 1 and every
// constant derived from it is byte-identical to what it was before AIRA-117.
//
// AIRA-117, in full, because the failure it fixes was invisible from the parent:
//
// newCeilingCgroupFixture starts the helper by re-exec'ing THE TEST BINARY
// (os.Args[0] with -test.run=^TestSliceCeilingAllocHelper$), so a `go test -race`
// run gets a race-INSTRUMENTED helper. ThreadSanitizer keeps shadow memory for
// the allocation, and Go's racemalloc imitates a write across the whole block at
// allocation time, so the shadow is not merely reserved, it is resident.
//
// MEASURED, not assumed (a 6 GiB fixture cap, this kernel, this toolchain):
//
//	touch     app bytes   cgroup anon after   delta
//	initial   600 MiB     1220 MiB            1220 MiB
//	anon      600 MiB     2421 MiB            1201 MiB
//	anon      600 MiB     3625 MiB            1204 MiB
//
// i.e. 2.03x, near enough exactly 2. The worst-case test drives three anon
// touches through one fixture, which is 3.54 GiB of NON-reclaimable memory
// against what used to be a flat 2 GiB cap -- so the kernel OOM-killed the
// helper (CONSTRAINT_MEMCG, anon-rss 2090240 kB, oom_kill 1 in the fixture's
// memory.events, memory.peak pinned exactly at the 2 GiB cap), and all the
// parent ever saw was bufio.Scanner returning false with a nil error: the
// "helper did not acknowledge anon growth: <nil>" that failed all three growing
// real-cgroup ceiling tests under -race.
//
// The same measurement showed the ORDINARY build was not comfortable either:
// three 600 MiB touches is ~1.81 GiB against that same 2 GiB cap, 12% of margin.
// So the fix is not "make -race fit"; it is to size the cap from the fixture's
// worst-case NON-reclaimable footprint with a stated margin, and to make that
// footprint independent of the build mode. See ceilingFixtureCap.
const ceilingFixtureRaceScale = int64(1)
