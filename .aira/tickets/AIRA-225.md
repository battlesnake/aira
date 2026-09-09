---
{"schema":1,"id":"AIRA-225","project":"aira","title":"aira confine does not contain snap-packaged tools (gcloud/terraform re-parent into snap.*.scope); escape reported only in an unread field","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["confine","containment","dogfood","honesty"],"hold":false,"relations":[]}
---
From RANT-37 (dogfood, 2026-09-09), verified by controlled experiment on this box.

`aira confine -- gcloud ...` and `-- terraform ...` each report scope-integrity=descendant-escaped with escaped-cgroup=.../snap.google-cloud-cli.gcloud-<uuid>.scope (resp. snap.terraform.*). snapd re-parents the tool into its own transient scope, so the job is neither RAM-capped by aira.slice nor killable via cgroup.kill. Controls confirm it: `-- aira version`, `-- cat <file>`, and `-- az account show` (az is /usr/bin, not snap) all stay contained/descendant-killed and accounted. So this is snapd, not a general confine defect.

Why it matters: gcloud and terraform are exactly what a cloud/deploy workflow shells out to, and on this machine both are snap builds. Three sibling projects (flavour-kernel, stoner, fastest-ee) drive GCP through snap gcloud today. The core containment guarantee silently does not hold in the workflow where a runaway is most expensive.

The honesty machinery WORKS (it reports descendant-escaped every time) — the defect is that it reads as per-run telemetry, not as "your containment did not happen". Same lesson class as AIRA-220/AIRA-222: "not contained" must not be quiet.

OWNER DECISION (fork): does a snap-escape earn (a) a LOUD signal / non-zero-ish prominence (cf. AIRA-222 --require-containment), or (b) an outright refusal, or (c) documented-gap only? Plus: write down that any future cloud/offload verb must call the GCP REST API from Go, never shell out to a snap binary.

refs: RANT-37. Relates to AIRA-222 (silent-ungoverned launch path).
