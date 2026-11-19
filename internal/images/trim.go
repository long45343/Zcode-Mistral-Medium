// Package images enforces the upstream limit on how many images a single
// request may carry.
//
// The provider rejects a request outright once the total number of images
// exceeds 20, regardless of how they are distributed across messages or tool
// outputs:
//
//	400 {"error":{"code":"3051",
//	     "message":"Total number of images exceeds the maximum allowed of 20."}}
//
// A long agent session accumulates screenshots, so the whole conversation
// eventually becomes unusable. Trimming keeps the most recent images (the ones
// the model is actually working from) and replaces older ones with a short
// text note, so the request stays valid and the text history is untouched.
package images

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DefaultMax is the provider's documented ceiling.
const DefaultMax = 20

// Stats reports what a trim changed.
type Stats struct {
	// Found is how many images the request carried.
	Found int `json:"found"`
	// Dropped is how many were replaced with a placeholder.
	Dropped int `json:"dropped"`
}

// Changed reports whether anything was replaced.
func (s Stats) Changed() bool { return s.Dropped > 0 }

// Trim keeps at most max images, dropping the oldest ones first.
//
// Order follows the input array, which is chronological: later items are more
// recent. Both message content parts and function_call_output parts are
// counted, because the provider counts both.
func Trim(body []byte, max int) ([]byte, Stats) {
	var st Stats

	if max <= 0 {
		max = DefaultMax
	}

	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, st
	}

	type imageRef struct {
		path string
	}

	var refs []imageRef

	for i, item := range input.Array() {
		// Message content parts.
		if content := item.Get("content"); content.IsArray() {
			for j, part := range content.Array() {
				if part.Get("type").String() == "input_image" {
					refs = append(refs, imageRef{path: fmt.Sprintf("input.%d.content.%d", i, j)})
				}
			}
		}
		// Tool output parts.
		if output := item.Get("output"); output.IsArray() {
			for j, part := range output.Array() {
				if part.Get("type").String() == "input_image" {
					refs = append(refs, imageRef{path: fmt.Sprintf("input.%d.output.%d", i, j)})
				}
			}
		}
	}

	st.Found = len(refs)
	if st.Found <= max {
		return body, st
	}

	drop := st.Found - max
	placeholder := fmt.Sprintf(`{"type":"input_text","text":%q}`,
		fmt.Sprintf("[image omitted: request exceeded the %d-image limit]", max))

	out := body
	for _, ref := range refs[:drop] {
		updated, err := sjson.SetRawBytes(out, ref.path, []byte(placeholder))
		if err != nil {
			// Keep going: a single failure should not discard the others, and
			// the request is rejected otherwise anyway.
			continue
		}
		out = updated
		st.Dropped++
	}

	if !st.Changed() {
		return body, Stats{Found: st.Found}
	}

	if !gjson.ValidBytes(out) {
		return body, Stats{Found: st.Found}
	}

	return out, st
}

// Describe renders a short human summary for logs.
func (s Stats) Describe() string {
	if !s.Changed() {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("images=%d dropped=%d", s.Found, s.Dropped))
}
