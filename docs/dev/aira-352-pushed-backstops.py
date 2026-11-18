#!/usr/bin/env python3
"""AIRA-352 evidence: backstop passes whose head jumped older waiters, and what the fix would change.

Usage (both bounds required, UTC, so a rerun never inherits an old default):
  python3 -I docs/dev/aira-352-pushed-backstops.py JOURNAL STORE_COPY \
      --since 2026-11-15T00:00 --until 2026-11-18T12:00

JOURNAL is a daemon journal extract holding the fairness-freeze lines, made with
  journalctl --user -u aira-daemon.service -o short-unix --since ... --until ... \
      | grep -E 'fairness-freeze (hold|yield|idle)'
STORE_COPY is a read-only copy of state.db (opened mode=ro). Neither is committed: both name
other sessions' jobs. This script prints no names, signatures or commands.

Rule (journal only). A yield line names the pass's head. If the NEXT hold line of the same daemon
process (seq restarts with the daemon) names an OLDER
seq, an older waiter was still queued behind that head, so the head jumped it: a `--front` push,
a `--priority high` waiter, or an older waiter that could not be a head at that pass. These are
CANDIDATES for "a backstop pass lost to a pushed head", not verified pushes.

For each candidate, the store copy is searched for the head's own row (budget == reserve and
started - wait within 3 s of the head's enqueue instant, from the yield line's queued-for). Rows
are retention-capped (20 per signature), so some heads have no row. When exactly one row matches,
`gap` = next hold - the head's grant. The AIRA-352 fix changes the outcome only when gap <= 1 s:
then today's code armed a fresh hold at the pass that granted the head, which means a fitting
waiter was budget-withheld there, and the fix backfills it instead. With a larger gap nothing
fitted-and-was-withheld at the head's grant pass (an arm would have been logged), so the deferred
backstop pass grants nothing the budget did not, and the outcome is today's.
"""
import argparse
import datetime
import re
import sqlite3
import sys

LINE = re.compile(r'^(\d+\.\d+) .*?aira\[(\d+)\]:.*fairness-freeze (hold|yield|idle) on \S+?(?:: head seq=(\d+) '
                  r'reserve=(\d+) queued-for=(\S+)|\s)')


def seconds(text):
    match = re.fullmatch(r'(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?', text)
    hours, minutes, secs = (int(part) if part else 0 for part in match.groups())
    return hours * 3600 + minutes * 60 + secs


def utc(text):
    return datetime.datetime.strptime(text, '%Y-%m-%dT%H:%M').replace(tzinfo=datetime.timezone.utc).timestamp()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('journal')
    parser.add_argument('store')
    parser.add_argument('--since', required=True)
    parser.add_argument('--until', required=True)
    args = parser.parse_args()
    since, until = utc(args.since), utc(args.until)
    events = []
    with open(args.journal) as handle:
        for line in handle:
            match = LINE.match(line)
            if not match:
                continue
            at, pid = float(match.group(1)), match.group(2)
            seq = int(match.group(4)) if match.group(4) else None
            reserve = int(match.group(5)) if match.group(5) else None
            queued_for = seconds(match.group(6)) if match.group(6) else None
            events.append((at, match.group(3), seq, reserve, queued_for, pid))
    events.sort()
    database = sqlite3.connect('file:' + args.store + '?mode=ro', uri=True)
    yields = [event for event in events if event[1] == 'yield' and since <= event[0] < until]
    with_head = [event for event in yields if event[2] is not None]
    print(f'window {args.since}Z .. {args.until}Z')
    print(f'backstop passes (yield lines): {len(yields)}; naming a head: {len(with_head)}')
    candidates = []
    for event in with_head:
        # Same daemon process only: seq restarts at a daemon restart.
        following = [later for later in events
                     if later[0] > event[0] and later[1] == 'hold' and later[2] is not None and later[5] == event[5]]
        if following and following[0][2] < event[2]:
            candidates.append((event, following[0]))
    print(f'candidates (head jumped an older waiter still queued at the next hold): {len(candidates)}')
    tally = {'changed (gap <= 1 s)': 0, 'unchanged (gap > 1 s)': 0, 'no row': 0, 'ambiguous': 0}
    for event, hold in candidates:
        enqueued_ms = int((event[0] - event[4]) * 1000)
        rows = database.execute(
            'select started_at_ms from confine_peak_history where budget = ? and wait_ms is not null '
            'and started_at_ms is not null and abs((started_at_ms - wait_ms) - ?) < 3000',
            (event[3], enqueued_ms)).fetchall()
        stamp = datetime.datetime.fromtimestamp(event[0], datetime.timezone.utc).strftime('%m-%d %H:%M:%SZ')
        if len(rows) != 1:
            verdict = 'no row' if not rows else 'ambiguous'
            print(f'  yield {stamp} head {event[2]} ({event[3] >> 20} MiB) next hold head {hold[2]}: {verdict}')
            tally[verdict] += 1
            continue
        gap = hold[0] - rows[0][0] / 1000
        verdict = 'changed (gap <= 1 s)' if gap <= 1 else 'unchanged (gap > 1 s)'
        tally[verdict] += 1
        print(f'  yield {stamp} head {event[2]} ({event[3] >> 20} MiB) next hold head {hold[2]}: '
              f'head granted {gap:.1f} s before the next arm: {verdict}')
    print('summary: ' + ', '.join(f'{key} {value}' for key, value in tally.items()))
    return 0


if __name__ == '__main__':
    sys.exit(main())
