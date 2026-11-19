// Package reasoning converts raw-reasoning stream events into summary events.
//
// Why this exists: OpenAI's Responses API streams two reasoning channels.
//
//	response.reasoning_text.delta          raw chain of thought
//	response.reasoning_summary_text.delta  model-written summary
//
// OpenAI's own models expose only the summary, so the Vercel AI SDK (used by
// ZCode) implements only the summary branch. Providers of open-weight models
// such as Mistral emit the raw channel, so a client built on that SDK shows
// nothing even though the tokens were produced and billed.
//
// Rewriting the event names makes the raw stream look like a summary stream.
// Text, item ids and event ordering are preserved.
package reasoning

import (
	"bytes"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Event names involved in the conversion.
const (
	evtContentPartAdded = "response.content_part.added"
	evtContentPartDone  = "response.content_part.done"
	evtReasoningDelta   = "response.reasoning_text.delta"

	evtSummaryPartAdded = "response.reasoning_summary_part.added"
	evtSummaryPartDone  = "response.reasoning_summary_part.done"
	evtSummaryDelta     = "response.reasoning_summary_text.delta"
)

// maxPending bounds how much of an unterminated frame is held in memory. A
// provider that never sends the blank-line delimiter would otherwise grow this
// buffer without limit. On overflow the buffered bytes are flushed verbatim so
// nothing is lost.
const maxPending = 4 << 20

var (
	frameSeparator = []byte("\n\n")
	fieldData      = []byte("data:")
	fieldEvent     = []byte("event:")
	reasoningMark  = []byte("reasoning_text")
)

// Stats counts what a stream rewrite did.
type Stats struct {
	// Deltas is the number of reasoning text deltas retyped.
	Deltas int
	// Parts is the number of part lifecycle events retyped.
	Parts int
}

// Changed reports whether anything was rewritten.
func (s Stats) Changed() bool { return s.Deltas > 0 || s.Parts > 0 }

// Rewriter converts a raw SSE byte stream frame by frame.
//
// Frames are buffered only until their terminating blank line arrives, so a
// TCP read that splits an event in the middle is handled correctly. Frames
// carrying no reasoning content are forwarded immediately without parsing.
type Rewriter struct {
	enabled bool
	pending []byte
	stats   Stats
}

// New returns a rewriter. When enabled is false every byte passes through
// untouched and nothing is buffered.
func New(enabled bool) *Rewriter {
	return &Rewriter{enabled: enabled}
}

// Write consumes an upstream chunk and returns the bytes that may be forwarded
// immediately. The result is empty while an event is still incomplete.
func (r *Rewriter) Write(chunk []byte) []byte {
	if !r.enabled {
		return chunk
	}

	r.pending = append(r.pending, chunk...)

	// Never hold an unbounded amount of unterminated data.
	if len(r.pending) > maxPending {
		out := r.pending
		r.pending = nil
		return out
	}

	var out bytes.Buffer
	for {
		idx := bytes.Index(r.pending, frameSeparator)
		if idx < 0 {
			break
		}
		frame := r.pending[:idx+len(frameSeparator)]
		r.pending = r.pending[idx+len(frameSeparator):]
		out.Write(r.rewriteFrame(frame))
	}

	return out.Bytes()
}

// Flush returns whatever is still buffered. Call it once the upstream stream
// ends so a trailing incomplete frame is not swallowed.
func (r *Rewriter) Flush() []byte {
	if !r.enabled || len(r.pending) == 0 {
		return nil
	}
	out := r.pending
	r.pending = nil
	return out
}

// Stats returns the counters accumulated so far.
func (r *Rewriter) Stats() Stats { return r.stats }

// rewriteFrame retypes a single SSE frame when it carries raw reasoning.
func (r *Rewriter) rewriteFrame(frame []byte) []byte {
	// Fast path: the vast majority of frames mention neither the reasoning
	// text channel nor its content part, so they skip JSON parsing entirely.
	if !bytes.Contains(frame, reasoningMark) {
		return frame
	}

	_, dataStart, dataEnd := findField(frame, fieldData)
	if dataStart == 0 && dataEnd == 0 {
		return frame
	}

	payload := bytes.TrimSpace(frame[dataStart:dataEnd])
	if len(payload) == 0 || payload[0] != '{' {
		return frame
	}

	eventType := gjson.GetBytes(payload, "type").String()
	newType, isPart := classify(eventType, payload)
	if newType == "" {
		return frame
	}

	updated := payload
	if isPart || eventType == evtReasoningDelta {
		// The summary channel keys parts by summary_index; the raw channel uses
		// content_index. Without this the SDK builds ids ending in ":undefined"
		// and separate reasoning blocks collide.
		updated = copyIndex(updated, payload)
	}

	updated, err := sjson.SetBytes(updated, "type", newType)
	if err != nil {
		return frame
	}

	rebuilt := make([]byte, 0, len(frame)+len(newType)-len(eventType)+8)
	rebuilt = append(rebuilt, frame[:dataStart]...)
	rebuilt = append(rebuilt, updated...)
	rebuilt = append(rebuilt, frame[dataEnd:]...)

	rebuilt = setEventName(rebuilt, newType)

	if isPart {
		r.stats.Parts++
	} else {
		r.stats.Deltas++
	}

	return rebuilt
}

// classify maps an upstream event type to its summary-channel equivalent.
// It returns an empty name when the event needs no change, and reports whether
// the event is a part lifecycle event rather than a text delta.
func classify(eventType string, payload []byte) (newType string, isPart bool) {
	switch eventType {
	case evtReasoningDelta:
		return evtSummaryDelta, false

	case evtContentPartAdded:
		if gjson.GetBytes(payload, "part.type").String() != "reasoning_text" {
			return "", false
		}
		return evtSummaryPartAdded, true

	case evtContentPartDone:
		if gjson.GetBytes(payload, "part.type").String() != "reasoning_text" {
			return "", false
		}
		return evtSummaryPartDone, true
	}

	return "", false
}

// copyIndex duplicates content_index onto summary_index so the SDK's part id
// stays stable across the whole stream.
func copyIndex(updated, payload []byte) []byte {
	value := int64(0)
	if idx := gjson.GetBytes(payload, "content_index"); idx.Exists() {
		value = idx.Int()
	}
	out, err := sjson.SetBytes(updated, "summary_index", value)
	if err != nil {
		return updated
	}
	return out
}

// findField locates a "field:" line inside a frame and returns the value span.
func findField(frame, field []byte) (line []byte, valueStart, valueEnd int) {
	offset := 0
	for offset < len(frame) {
		next := bytes.IndexByte(frame[offset:], '\n')
		var lineEnd int
		if next < 0 {
			lineEnd = len(frame)
		} else {
			lineEnd = offset + next
		}

		current := frame[offset:lineEnd]
		trimmed := bytes.TrimLeft(current, " ")
		if bytes.HasPrefix(trimmed, field) {
			start := offset + (len(current) - len(trimmed)) + len(field)
			if start < lineEnd && frame[start] == ' ' {
				start++
			}
			return frame[offset:lineEnd], start, lineEnd
		}

		if next < 0 {
			break
		}
		offset = lineEnd + 1
	}
	return nil, 0, 0
}

// setEventName rewrites the "event:" line to match the new type, adding one when
// the frame does not carry it.
func setEventName(frame []byte, name string) []byte {
	offset := 0
	for offset < len(frame) {
		next := bytes.IndexByte(frame[offset:], '\n')
		var lineEnd int
		if next < 0 {
			lineEnd = len(frame)
		} else {
			lineEnd = offset + next
		}

		current := frame[offset:lineEnd]
		trimmed := bytes.TrimLeft(current, " ")
		if bytes.HasPrefix(trimmed, fieldEvent) {
			out := make([]byte, 0, len(frame)+len(name))
			out = append(out, frame[:offset]...)
			out = append(out, "event: "...)
			out = append(out, name...)
			out = append(out, frame[lineEnd:]...)
			return out
		}

		if next < 0 {
			break
		}
		offset = lineEnd + 1
	}

	// No event line present: prepend one so the client sees a consistent pair.
	out := make([]byte, 0, len(frame)+len(name)+8)
	out = append(out, "event: "...)
	out = append(out, name...)
	out = append(out, '\n')
	out = append(out, frame...)
	return out
}
