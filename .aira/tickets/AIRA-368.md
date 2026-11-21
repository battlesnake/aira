---
{"schema":1,"id":"AIRA-368","project":"aira","title":"confine --list / aira top: show the job's real argv for detached jobs, not the __confine-supervise launcher line","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":"v0.46","labels":["backlog"],"hold":false,"relations":[]}
---

In `aira top` / `confine --list` a `--detach` job's row showed the hidden
supervisor's own command line, `/proc/self/exe __confine-supervise --control
<ctrl> --ready-fd 3 --ack-fd 4`, instead of the job. The supervisor does not
exec the job: it stays resident and runs it as a child, and the listing reads
the supervisor's `/proc/<pid>/cmdline` (everything after the first bare `--`,
or the whole argv when there is none). A queued job has no child yet, and this
kernel has no `/proc/<pid>/task/<tid>/children`, so reading the child cannot
cover every row.

Fix: the launcher passes a display copy of the job's argv on the supervisor's
command line after a bare `--` (`__confine-supervise --control C --ready-fd 3
--ack-fd 4 -- <argv...>`). The verb parses its flags only before that `--` and
never reads what follows; the control file stays the one source of what runs.
The copy is bounded to the listing's read bound (ConfineCommandWireLimit+1
bytes; the crossing argument is cut), so the shim's exec cannot grow towards
ARG_MAX and the listing still marks a long command elided. No new IO, no record
lookup; running and queued rows and `ps` all show the job. The job argv was
already visible in the child's own cmdline, so this adds no exposure.
