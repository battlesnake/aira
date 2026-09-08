---
{"schema":1,"id":"AIRA-167","project":"aira","title":"Refusing a too-small slice at install time was considered and NOT taken","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","install"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-167","to":"AIRA-153"}]}
---

AIRA-153 deferral **G8**: one of the three candidate directions the AIRA-153
ticket named, refused with a derivation rather than left as an open option a
later reader might re-propose.

The candidate: make `aira install` refuse to bake a slice whose cap is below
`DefaultConfineMemoryReserve` plus headroom, so the defect cannot exist.

Refused on three grounds, the third decisive:

1. **It does not cover the population.** A fixture slice, a shim-configured
   container budget, a systemd drop-in that lowers `MemoryMax` after install, and
   a CI runner that never runs `aira install` at all are all outside its reach.
2. **It converts a sizing defect into a product prohibition** — "AIRA does not
   support slices below ~6 GiB" — removing a legitimate use case (a small CI box)
   to avoid sizing a number correctly. `internal/install/install.go`'s OWN
   default would then have to refuse to install on an 8 GiB box, since
   `MemTotal - min(MemTotal/4, 16 GiB)` gives a 6 GiB slice there.
3. **It cannot establish the property it claims**, because the admission ceiling
   is a function of concurrent occupancy, not of the install. The entry ceiling
   is `maximum - (base + (jobs+1)*perJob)`; on an 8 GiB slice with production
   defaults, `jobs = 31` puts it exactly at 4294967296 and `jobs = 32` puts it
   below, so the identical unpinned request that was admissible at low occupancy
   is refused at high occupancy on a machine that passed every install-time
   check.

AIRA-153 took candidate C instead (bound the prior by the slice, daemon-side).
This ticket exists so the rejected direction and its derivation survive.
