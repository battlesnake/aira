---
{"schema":1,"id":"AIRA-182","project":"aira","title":"aira confine rejects an unrecognised option with no did-you-mean (--reserve -> --memory-reserve)","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["confine","ux"],"hold":false,"relations":[]}
---

Peer report (split, 2026-09-08), verified from source.

    $ aira confine --reserve 24G -- make merge-gate
    E_CONFINE_ARGUMENT_INVALID: option --reserve is not valid for confine

The correct flag is `--memory-reserve`; `--reserve` is the obvious wrong
guess and the error names nothing close to it.

## Verified from source

`parseConfineArgs` (`cmd/aira/main.go:763-800`) checks the option name
against a fixed, small set (`slice`, `name`, `owner`, `memory-reserve`,
`memory-max`, `memory-high`, `admit-timeout`, `timeout`, `cpu-timeout`,
plus the valueless `delegate-ram`/`detach`/`exclusive`) and on no match
returns a flat `option --%s is not valid for confine` (`:797`) with no
suggestion. Small, fixed vocabulary — a simple closest-match check against
it (or even a literal-substring special case for `reserve`/`max`/`high`
against their `memory-` prefixed counterparts) would resolve this without
new machinery.

## Not designed here

Whether this belongs as a small addition local to `parseConfineArgs` or as
a shared did-you-mean helper other verbs' option-parsers could reuse (this
repo has several similar small fixed-vocabulary option checks, e.g. the
`allowed[verb][name]` table at `cmd/aira/main.go:685-708`) is left for
whoever picks this up.
