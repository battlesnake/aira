package core

import (
	"bytes"
	"encoding/json"
	"io"
)

// AIRA-237 Task 1 — display-side id_prefix strip.
//
// A namespaced project stores/keys/filenames the compound id (FEE-BL-123), and
// FEE- is removed only at the human boundary. This projection walks a verb's
// already-marshalled response and rewrites the enumerated id-bearing fields
// through the store's displayID, so `list`/`show`/`aira id BL` print BL-123
// while the wire/DB stay compound. It runs in the shared Core.Do, so the daemon
// and in-process faces emit identical bytes.

// displayIDFields are the response fields carrying a ticket id. `path` is
// deliberately ABSENT: a file path (.aira/tickets/FEE-BL-123.md) embeds the
// compound id and must stay compound. `ticket` covers both the finding
// `ticket:` string form and a nested ticket object (recursion strips its id).
var displayIDFields = map[string]bool{
	"id":         true,
	"from":       true,
	"to":         true,
	"blocked_by": true,
	"ticket_id":  true,
	"ticket":     true,
}

// displayIDVerbs are the (canonical) verbs whose success output is projected.
var displayIDVerbs = map[string]bool{
	"id":       true,
	"list":     true,
	"show":     true,
	"create":   true,
	"claim":    true,
	"link":     true,
	"ready":    true,
	"find":     true,
	"worktree": true,
}

// projectDisplayIDs returns the strip-projected bytes for a whitelisted verb's
// data under a namespaced project, or nil when the projection does not apply
// (non-namespaced project, non-whitelisted verb, or a marshal/rewrite failure —
// in which case responseFrame falls back to the un-stripped typed Data, which
// is honest, if compound).
func (c *Core) projectDisplayIDs(verb string, data any) json.RawMessage {
	if data == nil || !displayIDVerbs[verb] {
		return nil
	}
	idPrefix, ns := c.namespacing()
	if idPrefix == "" || ns == nil {
		return nil
	}
	raw, err := marshalNoEscape(data)
	if err != nil {
		return nil
	}
	stripped, err := stripDisplayIDs(raw, ns.DisplayID)
	if err != nil {
		return nil
	}
	return json.RawMessage(stripped)
}

// stripDisplayIDs rewrites raw JSON, replacing every string value that sits
// under a displayIDFields key — at any depth, including a string element of an
// array whose key is in the set (e.g. blocked_by:["FEE-BL-1"]) — with
// strip(value). It preserves object KEY ORDER and numeric SPELLING (json.Number
// literals), so a non-namespaced identity strip reproduces the input byte for
// byte and the cross-face byte invariant holds. A nested object under a strip
// key (e.g. "ticket":{…}) is recursed into rather than string-stripped.
func stripDisplayIDs(raw []byte, strip func(string) string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	type frame struct {
		isObject   bool
		expectKey  bool   // in an object, the next token is a key
		wrote      bool   // an element has been written (for comma placement)
		valueKey   string // in an object, the key whose value comes next
		stripElems bool   // in an array, string elements should be stripped
	}
	var stack []*frame
	var out bytes.Buffer

	top := func() *frame {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}
	// sep writes the punctuation preceding the token about to be emitted.
	sep := func() {
		f := top()
		if f == nil {
			return
		}
		if f.isObject {
			if f.expectKey {
				if f.wrote {
					out.WriteByte(',')
				}
			} else {
				out.WriteByte(':')
			}
		} else if f.wrote {
			out.WriteByte(',')
		}
	}
	// advance updates the parent after a key or value token is emitted.
	advance := func(wasKey bool) {
		f := top()
		if f == nil {
			return
		}
		if f.isObject {
			if wasKey {
				f.expectKey = false
			} else {
				f.expectKey = true
				f.wrote = true
			}
		} else {
			f.wrote = true
		}
	}
	writeString := func(s string) error {
		encoded, err := marshalNoEscape(s)
		if err != nil {
			return err
		}
		out.Write(encoded)
		return nil
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				// A container is always a value, never a key. Decide array
				// element-stripping from the enclosing key before advancing.
				stripElems := false
				if f := top(); f != nil && f.isObject && !f.expectKey && t == '[' {
					stripElems = displayIDFields[f.valueKey]
				}
				sep()
				advance(false)
				out.WriteByte(byte(t))
				stack = append(stack, &frame{isObject: t == '{', expectKey: t == '{', stripElems: stripElems})
			case '}', ']':
				out.WriteByte(byte(t))
				stack = stack[:len(stack)-1]
			}
		case string:
			f := top()
			isKey := f != nil && f.isObject && f.expectKey
			sep()
			if isKey {
				if err := writeString(t); err != nil {
					return nil, err
				}
				f.valueKey = t
				advance(true)
				continue
			}
			value := t
			if f != nil {
				if f.isObject && displayIDFields[f.valueKey] {
					value = strip(t)
				} else if !f.isObject && f.stripElems {
					value = strip(t)
				}
			}
			if err := writeString(value); err != nil {
				return nil, err
			}
			advance(false)
		case json.Number:
			sep()
			out.WriteString(t.String())
			advance(false)
		case bool:
			sep()
			if t {
				out.WriteString("true")
			} else {
				out.WriteString("false")
			}
			advance(false)
		case nil:
			sep()
			out.WriteString("null")
			advance(false)
		}
	}
	return out.Bytes(), nil
}
