---
{"schema":1,"id":"AIRA-242","project":"aira","title":"aira confine --dump times out on a long-lived daemon: reuses the 250ms admit hot-path deadline (admitHistoryTimeout) for a FULL confine_peak_history read, which has no total cap (only newest-per-signature) so it grows unbounded in distinct signatures; --dump/--budget should use a generous batch deadline (or cap/paginate the history read). Repro: aira confine --dump \u003cf\u003e -\u003e E_DAEMON_INTERNAL: read usage history: context deadline exceeded (client sees E_DAEMON_UNAVAILABLE: EOF). version+list fine; admission unaffected (per-signature read is small).","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

