package main

// AIRA-257 `aira board` plain-English translation. The board can, ON DEMAND ('t'),
// show a ticket's title+description rewritten from dense "claude-ish" into plain
// English via an LLM, cached on disk so repeat views are instant. This lives
// ENTIRELY in the TUI face: aira's core.Do / store / daemon never call an LLM.
// The rewrite is honestly a PARAPHRASE — the render marks it "plain-English (AI)",
// keeps the original one keystroke away, and shows "translation unavailable
// (CODE)" on a failed call, never a fabrication (spec §14). The disk-cache key is
// a content hash of the ticket, so a changed ticket re-translates rather than
// showing a stale rewrite.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// boardHumanizeState is the info pane's translation state for ONE ticket at a
// time (the last one 't' was pressed on). ID is the ticket the rewrite is for; a
// result or a render for a DIFFERENT current ticket is ignored, so a stale rewrite
// never shows under the wrong card (the honesty seam boardHumanizeDisplay
// enforces). State is "" | "loading" | "ready" | "unevaluated:<CODE>". Shown is
// whether the plain version is currently displayed ('t' toggles it, so the
// original is always one keystroke away). Only one ticket's rewrite is held in
// memory; the disk cache makes re-translating any other ticket instant.
type boardHumanizeState struct {
	ID         string
	State      string
	PlainTitle string
	PlainBody  string
	Shown      bool
}

// boardHumanizeResult is one ticket's plain-English rewrite. Code != "" means the
// translation could not be established (the LLM call failed / was unreachable, or
// its output was unparseable); the renderer then keeps the original and says so,
// never a fabricated rewrite. Code is never persisted — a cached entry is always a
// real rewrite.
type boardHumanizeResult struct {
	PlainTitle string `json:"plain_title"`
	PlainBody  string `json:"plain_body"`
	Code       string `json:"-"`
}

// boardTranslator turns a ticket's (id, title, body) into a plain-English rewrite.
// Injected into the executor so tests use a fake and never touch the network.
type boardTranslator func(ctx context.Context, id, title, body string) boardHumanizeResult

// boardHumanizeDisplay is the render honesty seam shared by the info pane and the
// expand overlay. It decides what title/body to show for a ticket given the
// translation toggle, and returns a short header label. The plain rewrite is shown
// ONLY when it is this ticket's (h.ID == id) and toggled on (h.Shown) and ready;
// in every other case the ORIGINAL is shown, annotated ("translating…" /
// "translation unavailable (CODE)") but never replaced by a fabricated value. A
// partial parse (only a title, or only a body) falls back to the original for the
// missing half rather than showing a blank.
func boardHumanizeDisplay(id, origTitle, origBody string, h boardHumanizeState) (title, body, label string) {
	if h.ID != id || !h.Shown {
		return origTitle, origBody, ""
	}
	switch {
	case h.State == "ready":
		title, body = origTitle, origBody
		if strings.TrimSpace(h.PlainTitle) != "" {
			title = h.PlainTitle
		}
		if strings.TrimSpace(h.PlainBody) != "" {
			body = h.PlainBody
		}
		return title, body, "plain-English (AI)"
	case h.State == "loading":
		return origTitle, origBody, "translating…"
	case strings.HasPrefix(h.State, "unevaluated:"):
		return origTitle, origBody, "translation unavailable (" + strings.TrimPrefix(h.State, "unevaluated:") + ")"
	default:
		return origTitle, origBody, ""
	}
}

// boardHumanizeCacheKey is the content hash keying a translation on disk: id + the
// exact title + body. Any change to the ticket's text yields a new key, so the
// cache self-invalidates rather than showing a rewrite of the old text.
func boardHumanizeCacheKey(id, title, body string) string {
	sum := sha256.Sum256([]byte(id + "\x00" + title + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

// boardHumanizePrompt asks for a parseable two-field rewrite aimed at a technically
// literate reader — an undergraduate engineer/scientist or a technical manager — who
// does not know this codebase's internal jargon. It asks for the FULL description
// rewritten into readable prose (NOT condensed to a summary), with jargon/shorthand
// (the "claude-ish" the owner wants gone) unpacked while the technical meaning is
// preserved (AIRA-258).
func boardHumanizePrompt(title, body string) string {
	desc := strings.TrimSpace(body)
	if desc == "" {
		desc = "(none)"
	}
	return "You are rewriting a software-project ticket into clear, human-readable English " +
		"for a technically literate reader — an undergraduate engineer or scientist, or a " +
		"technical manager — who does not know this codebase's internal jargon. Rewrite it in " +
		"plain, direct prose: keep ALL the technical meaning and detail, but unpack internal " +
		"jargon, shorthand, bare ticket-ID references, and dense compound phrasing into " +
		"readable language.\n\n" +
		"Rewrite the FULL description — do NOT condense it into a short summary. Preserve " +
		"every point the original makes; a long description yields a correspondingly full " +
		"rewrite. You may open the body with a one-sentence summary ONLY as a lead-in above " +
		"the full rewritten text, never as a replacement for it.\n\n" +
		"Output EXACTLY this and nothing else:\n" +
		"TITLE: <the ticket title, rewritten as one clear plain-English line>\n" +
		"BODY: <the full description, rewritten in clear readable English for the reader above>\n\n" +
		"Ticket to rewrite:\n" +
		"Title: " + strings.TrimSpace(title) + "\n" +
		"Description: " + desc + "\n"
}

// boardParseHumanizeOutput extracts (plainTitle, plainBody) from the LLM's reply.
// It prefers the requested "TITLE:"/"BODY:" markers — tolerating a Markdown code
// fence, a leading "**"/"#"/">" on the marker line, and a bold-wrapped value — but
// falls back to "first non-blank line is the title, the rest is the body" so an
// off-format reply still yields both.
func boardParseHumanizeOutput(raw string) (string, string) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", ""
	}
	lines := strings.Split(text, "\n")
	title := ""
	var bodyLines []string
	inBody := false
	markered := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			continue // skip Markdown code-fence lines
		}
		marker := strings.TrimLeft(trimmed, "#*> ") // strip a Markdown heading/bold/quote lead
		switch {
		case matchPrefixFold(marker, "TITLE:"):
			title = cleanHumanizeValue(marker[len("TITLE:"):])
			markered, inBody = true, false
		case matchPrefixFold(marker, "BODY:"):
			bodyLines = append(bodyLines, cleanHumanizeValue(marker[len("BODY:"):]))
			markered, inBody = true, true
		case inBody:
			bodyLines = append(bodyLines, line)
		}
	}
	if markered {
		return title, strings.TrimSpace(strings.Join(bodyLines, "\n"))
	}
	// No markers: first non-blank non-fence line is the title, the rest the body.
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		kept = append(kept, line)
	}
	for i, line := range kept {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line), strings.TrimSpace(strings.Join(kept[i+1:], "\n"))
		}
	}
	return "", ""
}

// cleanHumanizeValue trims surrounding whitespace and Markdown bold markers from a
// parsed marker value (e.g. "**Fix the thing**" → "Fix the thing").
func cleanHumanizeValue(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "*"))
}

func matchPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// boardHumanizeCacheDir is where translations are cached (per-user, off-store). A
// failure to resolve it is not fatal: the translator falls back to no caching.
func boardHumanizeCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, "aira", "humanized")
}

// boardHumanizeLoadCache reads a cached translation. It reports a miss for an
// absent, unreadable, unparseable (e.g. a crash-truncated write), or empty file,
// so a corrupt cache entry re-translates rather than rendering blank.
func boardHumanizeLoadCache(dir, key string) (boardHumanizeResult, bool) {
	if dir == "" {
		return boardHumanizeResult{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		return boardHumanizeResult{}, false
	}
	var result boardHumanizeResult
	if err := json.Unmarshal(data, &result); err != nil {
		return boardHumanizeResult{}, false
	}
	if strings.TrimSpace(result.PlainTitle) == "" && strings.TrimSpace(result.PlainBody) == "" {
		return boardHumanizeResult{}, false
	}
	return result, true
}

// boardHumanizeStoreCache writes a translation to disk (best-effort; a write error
// only forfeits caching, never the shown result).
func boardHumanizeStoreCache(dir, key string, result boardHumanizeResult) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, key+".json"), data, 0o644)
}

// newAgentmuxTranslator builds a translator over an explicit cache dir and command
// argv (the prompt is appended as the final argument). It checks the on-disk cache
// first, else runs the command, parses its reply, and caches a real rewrite. Any
// failure returns a Code, so the pane shows "unavailable", never a fabrication.
// dir/argv are parameters so a test drives it over a temp dir with a deterministic
// command instead of the owner's real cache and the network.
func newAgentmuxTranslator(dir string, argv []string) boardTranslator {
	return func(ctx context.Context, id, title, body string) boardHumanizeResult {
		key := boardHumanizeCacheKey(id, title, body)
		if cached, ok := boardHumanizeLoadCache(dir, key); ok {
			return cached
		}
		out, err := boardRunTranslator(ctx, argv, boardHumanizePrompt(title, body))
		if err != nil {
			return boardHumanizeResult{Code: "E_TUI_TRANSLATE_UNAVAILABLE"}
		}
		plainTitle, plainBody := boardParseHumanizeOutput(out)
		if strings.TrimSpace(plainTitle) == "" && strings.TrimSpace(plainBody) == "" {
			return boardHumanizeResult{Code: "E_TUI_DECODE"}
		}
		result := boardHumanizeResult{PlainTitle: plainTitle, PlainBody: plainBody}
		boardHumanizeStoreCache(dir, key, result)
		return result
	}
}

// deepseekTranslatorArgv is the production translator command: the agentmux LLM
// gateway with Deepseek (cheap, reliable, no rate limit — Gemini is free-tier and
// flaky per aira's own SKILL). --concise=false sends the prompt verbatim — agentmux
// defaults concise to true, which prepends a verdict-first "answer concise for a
// machine reader" preamble that would compress the rewrite; a full readable
// translation is the opposite of that, so it is explicitly disabled. (--concise=false
// is the same as the --raw alias, spelled out so the intent is self-evident.)
func deepseekTranslatorArgv() []string {
	return []string{"agentmux", "ask", "--provider", "deepseek", "--model", "deepseek-v4-flash", "--concise=false", "--timeout", "90s"}
}

// boardTranslatorFactory builds the translator the executor uses to satisfy a
// cmdBoardTranslate. Production returns the agentmux/Deepseek translator over the
// per-user cache; tests swap in a fake so a board test never shells out or hits the
// network. A package var so it can be overridden, mirroring boardScreenFactory /
// boardHopObserver.
var boardTranslatorFactory = func() boardTranslator {
	return newAgentmuxTranslator(boardHumanizeCacheDir(), deepseekTranslatorArgv())
}

// boardRunTranslator runs the translator argv with the prompt as its final arg,
// bounded so a hung sidecar cannot wedge the translate goroutine. It captures
// stdout only, so a wrapping confine trailer (stderr) never pollutes the parse.
func boardRunTranslator(ctx context.Context, argv []string, prompt string) (string, error) {
	if len(argv) == 0 {
		return "", exec.ErrNotFound
	}
	runCtx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	args := append(append([]string(nil), argv[1:]...), prompt)
	cmd := exec.CommandContext(runCtx, argv[0], args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
