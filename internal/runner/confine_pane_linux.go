//go:build linux

package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// AIRA-277. Which tmux window a confined job was launched from, so `aira top`
// can show a name the operator chose instead of a hex session id.
//
// Nothing here knows who named the window. aira reads two variables tmux itself
// exports into every pane (TMUX = "<socket>,<server pid>,<session>" and
// TMUX_PANE = "%<n>") from the supervisor's /proc/<pid>/environ, then asks that
// server for the pane's window name. The rest of the environment block is never
// kept: it holds tokens and keys, and an allow-list of two names is the whole
// of what leaves this function.
const (
	// confineEnvironReadLimit bounds the environ read. A block larger than this is
	// still parsed, but a MISSING variable in it is then "unestablished", not
	// "absent" (see confinePaneLookup).
	confineEnvironReadLimit = 1 << 20
	// confineTmuxTimeout bounds one `tmux list-panes`; a wedged server must cost a
	// listing at most this long, not hang it.
	confineTmuxTimeout = 500 * time.Millisecond
	// confinePaneLabelMax bounds a label in runes: it is untrusted text bound for
	// a table cell.
	confinePaneLabelMax = 64
)

// confineTmuxRef is the allow-listed part of a supervisor's environment.
type confineTmuxRef struct {
	socket string
	pane   string
}

type confinePaneLookup int

const (
	// paneNotInTmux: the environ was read in full and carries neither variable.
	paneNotInTmux confinePaneLookup = iota
	// paneInTmux: both variables are present and the socket is usable.
	paneInTmux
	// paneUnestablished: anything else (exactly one variable, an unusable socket
	// path, or a truncated read that did not show either). Never "not in tmux".
	paneUnestablished
)

func readProcEnviron(pid int) ([]byte, error) {
	if pid <= 0 {
		return nil, os.ErrInvalid
	}
	file, err := os.Open("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, confineEnvironReadLimit+1))
}

// parseConfineTmuxRef extracts the tmux variables from a raw environ block.
func parseConfineTmuxRef(environ []byte) (confineTmuxRef, confinePaneLookup) {
	truncated := len(environ) > confineEnvironReadLimit
	if truncated {
		environ = environ[:confineEnvironReadLimit]
	}
	var tmuxVar, pane string
	var haveTmux, havePane bool
	for _, entry := range bytes.Split(environ, []byte{0}) {
		switch {
		case bytes.HasPrefix(entry, []byte("TMUX=")):
			tmuxVar, haveTmux = string(entry[len("TMUX="):]), true
		case bytes.HasPrefix(entry, []byte("TMUX_PANE=")):
			pane, havePane = string(entry[len("TMUX_PANE="):]), true
		}
	}
	switch {
	case !haveTmux && !havePane:
		if truncated {
			return confineTmuxRef{}, paneUnestablished
		}
		return confineTmuxRef{}, paneNotInTmux
	case haveTmux && havePane:
		socket, _, _ := strings.Cut(tmuxVar, ",")
		if !filepath.IsAbs(socket) || !strings.HasPrefix(pane, "%") || pane == "%" {
			return confineTmuxRef{}, paneUnestablished
		}
		return confineTmuxRef{socket: filepath.Clean(socket), pane: pane}, paneInTmux
	}
	return confineTmuxRef{}, paneUnestablished
}

// tmuxWindowNames returns pane id -> raw window name for every pane on the
// server behind socket.
func tmuxWindowNames(ctx context.Context, socket string) (map[string]string, error) {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, confineTmuxTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-S", socket, "list-panes", "-a", "-F", "#{pane_id} #{window_name}").Output()
	if err != nil {
		return nil, err
	}
	names := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		id, name, ok := strings.Cut(line, " ")
		if ok && strings.HasPrefix(id, "%") {
			names[id] = name
		}
	}
	return names, nil
}

// confinePaneLabel turns a window name into the label shown to the operator.
//
// Window names are often decorated with a trailing status glyph ("deploy 🤔").
// Any trailing whitespace-separated token that contains no letter or digit is
// dropped, so the label is the name and not its mood; this is a rule about the
// text, not about whatever tool wrote it. A name that is nothing but symbols, or
// empty, falls back to the pane id rather than to an empty label (which would
// read as "not in tmux"). The result is capped at confinePaneLabelMax runes.
func confinePaneLabel(name, paneID string) string {
	fields := strings.Fields(name)
	for len(fields) > 0 && !strings.ContainsFunc(fields[len(fields)-1], func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		fields = fields[:len(fields)-1]
	}
	label := strings.Join(fields, " ")
	if label == "" {
		return paneID
	}
	if runes := []rune(label); len(runes) > confinePaneLabelMax {
		label = string(runes[:confinePaneLabelMax-1]) + "…"
	}
	return label
}

// resolveConfinePanes sets Pane (or names "pane" unevaluated) on every record in
// refs, calling tmux at most once per distinct server socket.
func resolveConfinePanes(ctx context.Context, byID map[string]ConfineRecord, refs map[string]confineTmuxRef, notInTmux map[string]bool, unestablished map[string]bool,
	names func(context.Context, string) (map[string]string, error)) {
	if names == nil {
		names = tmuxWindowNames
	}
	bySocket := make(map[string]map[string]string)
	failed := make(map[string]bool)
	for scopeID, ref := range refs {
		if _, done := bySocket[ref.socket]; !done && !failed[ref.socket] {
			if table, err := names(ctx, ref.socket); err == nil {
				bySocket[ref.socket] = table
			} else {
				failed[ref.socket] = true
			}
		}
		record, present := byID[scopeID]
		if !present {
			continue
		}
		if name, ok := bySocket[ref.socket][ref.pane]; ok {
			label := confinePaneLabel(name, ref.pane)
			record.Pane = &label
		} else {
			record.UnevaluatedFields = append(record.UnevaluatedFields, "pane")
		}
		byID[scopeID] = record
	}
	for scopeID := range notInTmux {
		record, present := byID[scopeID]
		if !present {
			continue
		}
		none := ""
		record.Pane = &none
		byID[scopeID] = record
	}
	for scopeID := range unestablished {
		record, present := byID[scopeID]
		if !present {
			continue
		}
		record.UnevaluatedFields = append(record.UnevaluatedFields, "pane")
		byID[scopeID] = record
	}
}
