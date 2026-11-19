// Package notify injects a short advisory into a Responses stream when the
// request is close to a hard limit, so the user learns about it before the next
// turn fails.
//
// The advisory is appended as its own message item rather than editing existing
// events: rewriting a delta in place would corrupt the provider's sequence
// numbers and could leave the client's accumulated text inconsistent with the
// final "done" payload.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
)

var (
	frameSeparator = []byte("\n\n")
	fieldData      = []byte("data:")
	completedMark  = []byte(`"response.completed"`)
	itemAddedMark  = []byte(`"response.output_item.added"`)
)

// Injector appends one advisory message just before the stream completes.
type Injector struct {
	enabled bool
	text    string
	pending []byte
	sent    bool
	// nextIndex is the output_index to use for the injected item.
	nextIndex int
}

// New returns an injector. When enabled is false it forwards bytes unchanged.
func New(enabled bool, text string) *Injector {
	return &Injector{enabled: enabled, text: text, nextIndex: 1}
}

// Write consumes an upstream chunk and returns bytes to forward.
func (in *Injector) Write(chunk []byte) []byte {
	if !in.enabled {
		return chunk
	}

	in.pending = append(in.pending, chunk...)

	var out bytes.Buffer
	for {
		idx := bytes.Index(in.pending, frameSeparator)
		if idx < 0 {
			break
		}
		frame := in.pending[:idx+len(frameSeparator)]
		in.pending = in.pending[idx+len(frameSeparator):]

		// Track the highest output_index seen so the injected item lands after
		// every existing one. The frame is SSE text, so the JSON payload has to
		// be extracted first.
		if bytes.Contains(frame, itemAddedMark) {
			if payload := framePayload(frame); payload != nil {
				if idx := gjson.GetBytes(payload, "output_index"); idx.Exists() {
					if v := int(idx.Int()) + 1; v > in.nextIndex {
						in.nextIndex = v
					}
				}
			}
		}

		// Inject once, immediately before the terminal event.
		if !in.sent && bytes.Contains(frame, completedMark) {
			out.Write(in.buildItem())
			in.sent = true
		}

		out.Write(frame)
	}

	return out.Bytes()
}

// Flush releases any trailing bytes.
func (in *Injector) Flush() []byte {
	if !in.enabled || len(in.pending) == 0 {
		return nil
	}
	out := in.pending
	in.pending = nil
	return out
}

// Sent reports whether the advisory was emitted.
func (in *Injector) Sent() bool { return in.sent }

// buildItem renders the advisory as a complete message item lifecycle.
func (in *Injector) buildItem() []byte {
	const itemID = "msg_gateway_notice"
	idx := in.nextIndex

	type frame struct {
		event string
		data  map[string]any
	}

	frames := []frame{
		{
			event: "response.output_item.added",
			data: map[string]any{
				"type":         "response.output_item.added",
				"output_index": idx,
				"item": map[string]any{
					"id": itemID, "type": "message", "status": "in_progress",
					"role": "assistant", "content": []any{},
				},
			},
		},
		{
			event: "response.content_part.added",
			data: map[string]any{
				"type": "response.content_part.added", "item_id": itemID,
				"output_index": idx, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": ""},
			},
		},
		{
			event: "response.output_text.delta",
			data: map[string]any{
				"type": "response.output_text.delta", "item_id": itemID,
				"output_index": idx, "content_index": 0, "delta": in.text,
			},
		},
		{
			event: "response.output_text.done",
			data: map[string]any{
				"type": "response.output_text.done", "item_id": itemID,
				"output_index": idx, "content_index": 0, "text": in.text,
			},
		},
		{
			event: "response.content_part.done",
			data: map[string]any{
				"type": "response.content_part.done", "item_id": itemID,
				"output_index": idx, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": in.text},
			},
		},
		{
			event: "response.output_item.done",
			data: map[string]any{
				"type": "response.output_item.done", "output_index": idx,
				"item": map[string]any{
					"id": itemID, "type": "message", "status": "completed",
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "output_text", "text": in.text},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	for _, f := range frames {
		payload, err := json.Marshal(f.data)
		if err != nil {
			continue
		}
		buf.WriteString("event: ")
		buf.WriteString(f.event)
		buf.WriteString("\ndata: ")
		buf.Write(payload)
		buf.WriteString("\n\n")
	}
	return buf.Bytes()
}

// framePayload returns the JSON carried by a frame's data line.
func framePayload(frame []byte) []byte {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, fieldData) {
			return bytes.TrimSpace(trimmed[len(fieldData):])
		}
	}
	return nil
}

// BuildMessage renders the advisory text for a given limit.
func BuildMessage(images, limit int) string {
	return fmt.Sprintf(
		"\n\n---\n⚠️ 当前上下文已有 %d 张图片，接近上限 %d 张。建议尽快压缩上下文，否则超过后请求会被上游拒绝。",
		images, limit)
}
