// Package normalize fixes request bodies so the upstream provider accepts
// them.
//
// The only transformation is adding "type": "message" to role-bearing items
// that omit it. Mistral rejects such requests with code 3051:
//
//	input: Value error, Input items of type None are not supported.
//
// Items that already declare a type (function_call, function_call_output,
// reasoning, ...) are never touched, and everything outside "input" keeps its
// original bytes so prompt caching is unaffected.
package normalize

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Stats reports what a rewrite changed. Zero means the body already conformed
// and was returned untouched.
type Stats struct {
	// Typed is the number of items that received "type": "message".
	Typed int `json:"typed"`
}

// Changed reports whether the body needs to be rewritten.
func (s Stats) Changed() bool { return s.Typed > 0 }

// Rewrite adds the missing message type to every role-bearing input item.
//
// The body is only rebuilt when something actually changed, so a conforming
// request passes through byte for byte.
func Rewrite(body []byte) ([]byte, Stats) {
	var st Stats

	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, st
	}

	items := input.Array()
	kept := make([]string, 0, len(items))

	for _, item := range items {
		raw := item.Raw
		if raw == "" {
			// Preserve anything gjson cannot round-trip rather than dropping it.
			kept = append(kept, item.String())
			continue
		}
		if typed, changed := ensureMessageType(raw); changed {
			raw = typed
			st.Typed++
		}
		kept = append(kept, raw)
	}

	if !st.Changed() {
		return body, st
	}

	out, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(kept, ",")+"]"))
	if err != nil {
		// Never make the request worse than it already was.
		return body, Stats{}
	}

	return out, st
}

// ensureMessageType adds "type": "message" to an item that has a role but no
// type. Items carrying a type of their own are returned unchanged.
func ensureMessageType(raw string) (string, bool) {
	if !gjson.Get(raw, "role").Exists() {
		return raw, false
	}
	if gjson.Get(raw, "type").Exists() {
		return raw, false
	}

	updated, err := sjson.SetBytes([]byte(raw), "type", "message")
	if err != nil {
		return raw, false
	}

	return string(updated), true
}
