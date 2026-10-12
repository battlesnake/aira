---
{"schema":1,"id":"AIRA-299","project":"aira","title":"Gate files escape &, < and > in JSON (internal/gate/gate.go MarshalIndent)","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":"v0.33","labels":[],"hold":false,"relations":[]}
---
Split out of AIRA-216 (v0.32). Ticket, finding and requirement front matter now write without HTML escaping; internal/gate/gate.go uses json.MarshalIndent, which still writes \u0026, \u003c, \u003e. Gate files are bound by definition_digest in attestations and proofs (internal/store/gate_eval.go), so a byte change there has consequences that need their own review. Held: needs a design and review.
