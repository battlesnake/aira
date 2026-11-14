#!/usr/bin/env python3
"""AIRA-329 diagnosis: how often does the FIRST run of an exact argv, sized by the 1 GiB
cold default, get OOM-killed, and what would a coarser command key have predicted?
Reuses keys()/usable()/push() from aira-history-charge-replay.py. Read-only on the state DB.
Run: aira confine --memory-reserve 1G -- python3 -I docs/dev/aira-329-cold-start-measure.py
Measured 2026-11-14. The OOM row's recorded peak is CENSORED at its cap, so "prediction >
cap" is a plausibility test, not proof the kill would have been avoided."""
import collections, importlib.util, os
spec=importlib.util.spec_from_file_location("r",os.path.join(os.path.dirname(os.path.abspath(__file__)),"aira-history-charge-replay.py"))
R=importlib.util.module_from_spec(spec); spec.loader.exec_module(R)
hist={l:collections.defaultdict(list) for l in R.LV}
seen_exact=set(); rows=[]
for sig,peak,bud,bb,oom,at in R.c.execute(R.SQL):
    ks=R.keys(sig)
    cold = sig not in seen_exact          # first row ever for this exact argv
    if at>='2026-10-20' and bb and 'auto' in bb and cold:
        pred=None; lvl=None
        for l in ("N1_masked_all","N2_first4","N3_cmd+sub"):
            h=R.usable(hist[l].get(ks[l],[]))
            if h: pred=max(h)*1.25; lvl=l; break
        rows.append((oom,peak,bud,pred,lvl,sig))
    seen_exact.add(sig)
    R.push(hist,ks,peak,oom)
n=len(rows); oomn=sum(1 for r in rows if r[0])
print("cold auto-sized first runs since 2026-10-20:",n," of which OOM-killed:",oomn," (%.1f%%)"%(100*oomn/n))
print("budget distribution of cold runs (MiB):",collections.Counter((r[2]>>20) for r in rows).most_common(4))
o=[r for r in rows if r[0]]
cov=[r for r in o if r[3]]
print("OOM cold runs with a coarser-key prediction available:",len(cov),"of",len(o))
# the OOM row's peak is censored at the cap; a prediction above the cap means the kill might have been avoided
print("  prediction > cap (kill plausibly avoided):",sum(1 for r in cov if r[3]>r[2]),
      " median prediction MiB:",sorted(int(r[3])>>20 for r in cov)[len(cov)//2] if cov else None)
ok=[r for r in rows if not r[0] and r[3]]
print("non-OOM cold runs with a prediction:",len(ok)," where the prediction would have been a bigger cap than the 1G default:",sum(1 for r in ok if r[3]>r[2]),
      " median ratio pred/peak: %.1f"%sorted(r[3]/r[1] for r in ok)[len(ok)//2])
by=collections.Counter(r[4] for r in cov); print("levels used for OOM rows:",dict(by))
