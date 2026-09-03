---
{"schema":1,"id":"AIRA-57","project":"aira","title":"aira has no human-readable CLI help — bare aira/--help/help dumps JSON with escaped angle brackets","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["cli","dogfood","ux"],"hold":false,"relations":[]}
---
## Symptom

`aira`, `aira help`, and `aira --help` all print the raw dispatch-table introspection data instead of a human-formatted usage listing — a flat JSON array of `{"usage": "...", "verb": "..."}` objects, pretty-printed via Go's default `json.MarshalIndent`, which HTML-escapes angle brackets (`<id>` renders as `<id>`). Reported live by the project owner while trying to use the CLI.

## Root cause (verified by direct source read)

`cmd/aira/main.go:79-82`:
```go
if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
	response := core.New(nil).Do(context.Background(), core.Request{Verb: "help"})
	return render(response, jsonOutput, stdout, stderr)
}
```
`render` (~line 2057) only has three shapes: `jsonOutput=true` → raw `json.Marshal` of the whole `core.Response`; a few verbs get bespoke human renderers (`renderConfineListResponse`, `renderRunLog`, `renderTime` — these ARE genuinely human-friendly, e.g. `confine --list`'s table); everything else, including "help", falls into `renderHuman`'s generic tail (~line 2208-2220), which is just `json.MarshalIndent(response.Data, "", "  ")` — indented JSON is the "human" output. There is no dedicated formatter for the verb/usage listing itself, and no code anywhere calls `json.Encoder.SetEscapeHTML(false)`, so even this "human" JSON carries the ugly `<`/`>` escaping a terminal user never wants.

## Impact

First-run discoverability is bad for both humans and agents relying on `--help`/`help` output to learn the tool. This is the literal front door of the CLI.

## Suggested direction

Give the "help" verb (and arguably the generic `renderHuman` fallback for any verb without a bespoke renderer) an actual plain-text formatter: verb name, usage string, and — if the dispatch metadata carries a summary/description (check `OperationSpec`/dispatch-table structures in `internal/core/core.go`, which already carry `Summary` fields used elsewhere) — a one-line description, aligned/grouped, similar in spirit to what `git help`, `docker --help`, or this project's own `confine --list` table already do well. Reserve raw/indented JSON strictly for `--json`. Independently of the formatter, stop HTML-escaping JSON emitted for terminal consumption anywhere in `render`/`renderHuman` (there's no HTML context on a terminal) — construct the encoder with `SetEscapeHTML(false)` rather than using the package-level `json.Marshal`/`MarshalIndent` convenience functions.

## Owner direction: broaden the default, don't just patch "help"

Rather than a bespoke formatter verb-by-verb, make the DEFAULT rendering policy TTY-aware, the way essentially every well-behaved CLI (git, docker, kubectl, gh itself) does it: when stdout is a real terminal (`isatty`/`term.IsTerminal` equivalent — Go's `golang.org/x/term` has `IsTerminal(int(fd))`) and `--json` was NOT explicitly passed, default to human output; otherwise (piped, redirected, or not a TTY at all — the common case for an agent invoking this CLI via a subprocess) default to JSON. `--json` remains an explicit override in either direction if useful (e.g. `--json=false` to force human even when piped, if anyone needs that). This fixes "help" AND every other verb currently falling into the generic JSON-dump tail (e.g. `aira list`) in one architectural change, rather than requiring a bespoke human formatter per verb. Still write a real plain-text formatter for the generic fallback case per the direction above — the TTY default only decides WHEN to use it, not what it looks like.
