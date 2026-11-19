package reasoning

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const deltaFrame = "event: response.reasoning_text.delta\n" +
	`data: {"type":"response.reasoning_text.delta","delta":"思考","item_id":"rs_1","output_index":0,"content_index":0,"sequence_number":4}` +
	"\n\n"

const partAddedFrame = "event: response.content_part.added\n" +
	`data: {"type":"response.content_part.added","part":{"text":"","type":"reasoning_text"},"item_id":"rs_1","output_index":0,"content_index":0,"sequence_number":3}` +
	"\n\n"

const partDoneFrame = "event: response.content_part.done\n" +
	`data: {"type":"response.content_part.done","part":{"text":"全部","type":"reasoning_text"},"item_id":"rs_1","output_index":0,"content_index":0,"sequence_number":9}` +
	"\n\n"

const textFrame = "event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","delta":"答案","item_id":"msg_1","output_index":1,"content_index":0,"sequence_number":12}` +
	"\n\n"

// TestDeltaRewrite checks the core conversion: event name, type field and the
// summary_index the SDK needs to build stable part ids.
func TestDeltaRewrite(t *testing.T) {
	r := New(true)
	out := string(r.Write([]byte(deltaFrame)))

	if !strings.Contains(out, "event: response.reasoning_summary_text.delta\n") {
		t.Errorf("event line not rewritten:\n%s", out)
	}

	payload := extractData(t, out)
	if got := gjson.GetBytes(payload, "type").String(); got != "response.reasoning_summary_text.delta" {
		t.Errorf("type = %q", got)
	}
	if got := gjson.GetBytes(payload, "summary_index").Int(); got != 0 {
		t.Errorf("summary_index = %d, want 0", got)
	}
	// Content must survive untouched.
	if got := gjson.GetBytes(payload, "delta").String(); got != "思考" {
		t.Errorf("delta = %q, want 思考", got)
	}
	if got := gjson.GetBytes(payload, "item_id").String(); got != "rs_1" {
		t.Errorf("item_id = %q", got)
	}

	if st := r.Stats(); st.Deltas != 1 {
		t.Errorf("deltas = %d, want 1", st.Deltas)
	}
}

// TestDeltaSplitAcrossWrites is the important one: TCP does not respect event
// boundaries, so a delta may arrive in two pieces.
func TestDeltaSplitAcrossWrites(t *testing.T) {
	r := New(true)

	var out strings.Builder
	half := len(deltaFrame) / 2
	out.Write(r.Write([]byte(deltaFrame[:half])))
	if out.Len() != 0 {
		t.Errorf("emitted %d bytes before the frame was complete", out.Len())
	}
	out.Write(r.Write([]byte(deltaFrame[half:])))

	got := out.String()
	if !strings.Contains(got, "response.reasoning_summary_text.delta") {
		t.Errorf("frame not rewritten after split:\n%s", got)
	}
	if n := strings.Count(got, "\n\n"); n != 1 {
		t.Errorf("frame count = %d, want 1:\n%s", n, got)
	}
}

// TestByteByByteWrite pushes the split case to the extreme.
func TestByteByByteWrite(t *testing.T) {
	r := New(true)

	var out strings.Builder
	for i := 0; i < len(deltaFrame); i++ {
		out.Write(r.Write([]byte{deltaFrame[i]}))
	}

	got := out.String()
	if got != strings.Replace(deltaFrame, "reasoning_text.delta\",\"delta", "reasoning_summary_text.delta\",\"delta", 1) &&
		!strings.Contains(got, "response.reasoning_summary_text.delta") {
		t.Errorf("byte-by-byte write produced wrong output:\n%s", got)
	}
	if !json.Valid(extractData(t, got)) {
		t.Error("byte-by-byte write produced invalid JSON")
	}
}

// TestPartEvents verifies the lifecycle events are retyped too.
func TestPartEvents(t *testing.T) {
	r := New(true)

	added := string(r.Write([]byte(partAddedFrame)))
	if !strings.Contains(added, "response.reasoning_summary_part.added") {
		t.Errorf("part.added not rewritten:\n%s", added)
	}
	if got := gjson.GetBytes(extractData(t, added), "summary_index").Int(); got != 0 {
		t.Errorf("part.added summary_index = %d", got)
	}

	done := string(r.Write([]byte(partDoneFrame)))
	if !strings.Contains(done, "response.reasoning_summary_part.done") {
		t.Errorf("part.done not rewritten:\n%s", done)
	}
	// The part body must be preserved so the client sees the final text.
	if got := gjson.GetBytes(extractData(t, done), "part.text").String(); got != "全部" {
		t.Errorf("part.text = %q", got)
	}

	if st := r.Stats(); st.Parts != 2 {
		t.Errorf("parts = %d, want 2", st.Parts)
	}
}

// TestNonReasoningPassThrough makes sure normal text events are untouched.
func TestNonReasoningPassThrough(t *testing.T) {
	r := New(true)
	out := string(r.Write([]byte(textFrame)))
	if out != textFrame {
		t.Errorf("text frame was modified:\ntotal got = %q\nwant = %q", out, textFrame)
	}
	if st := r.Stats(); st.Changed() {
		t.Errorf("unexpected stats: %+v", st)
	}
}

// TestContentPartWithOtherType ignores content parts that are not reasoning.
func TestContentPartWithOtherType(t *testing.T) {
	frame := "event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","part":{"text":"","type":"output_text"},"item_id":"msg_1","output_index":1,"content_index":0,"sequence_number":3}` +
		"\n\n"

	r := New(true)
	if out := string(r.Write([]byte(frame))); out != frame {
		t.Errorf("non-reasoning content part was modified:\n%s", out)
	}
}

// TestDisabledIsTransparent ensures the switch really is a no-op path.
func TestDisabledIsTransparent(t *testing.T) {
	r := New(false)
	for _, frame := range []string{deltaFrame, textFrame, partAddedFrame} {
		if out := string(r.Write([]byte(frame))); out != frame {
			t.Errorf("disabled rewriter modified a frame:\n%s", out)
		}
	}
	if st := r.Stats(); st.Changed() {
		t.Errorf("disabled rewriter recorded stats: %+v", st)
	}
}

// TestFlushReleasesTrailingBytes covers a stream that ends without the closing
// blank line.
func TestFlushReleasesTrailingBytes(t *testing.T) {
	r := New(true)
	trailer := "event: response.completed\ndata: {\"type\":\"response.completed\"}"

	if out := r.Write([]byte(trailer)); len(out) != 0 {
		t.Errorf("incomplete frame emitted early: %q", out)
	}
	if out := string(r.Flush()); out != trailer {
		t.Errorf("flush = %q, want %q", out, trailer)
	}
	if out := r.Flush(); out != nil {
		t.Errorf("second flush should be empty, got %q", out)
	}
}

// TestJSONStaysValidAcrossManyFrames exercises a realistic mixed stream.
func TestJSONStaysValidAcrossManyFrames(t *testing.T) {
	stream := partAddedFrame + deltaFrame + deltaFrame + deltaFrame + partDoneFrame + textFrame

	r := New(true)
	out := string(r.Write([]byte(stream)))

	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if !json.Valid([]byte(payload)) {
			t.Fatalf("invalid JSON after rewrite: %s", payload)
		}
	}
	if st := r.Stats(); st.Deltas != 3 || st.Parts != 2 {
		t.Errorf("stats = %+v, want 3 deltas and 2 parts", st)
	}
}

// TestTextMentioningEventNameIsSafe guards the reason we parse JSON instead of
// doing a blind string replace: reasoning text can itself contain the event
// name (this very session did).
func TestTextMentioningEventNameIsSafe(t *testing.T) {
	dangerous := "event: response.reasoning_text.delta\n" +
		`data: {"type":"response.reasoning_text.delta","delta":"把 response.reasoning_text.delta 改成另一个名字","item_id":"rs_1","output_index":0,"content_index":0,"sequence_number":4}` +
		"\n\n"

	r := New(true)
	out := string(r.Write([]byte(dangerous)))

	payload := extractData(t, out)
	if !strings.Contains(gjson.GetBytes(payload, "delta").String(), "response.reasoning_text.delta") {
		t.Errorf("text content was corrupted: %q", gjson.GetBytes(payload, "delta").String())
	}
}

// extractData pulls the JSON payload out of a single-frame response.
func extractData(t *testing.T, frame string) []byte {
	t.Helper()
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "data: ") {
			return []byte(strings.TrimPrefix(line, "data: "))
		}
	}
	t.Fatalf("no data line in frame:\n%s", frame)
	return nil
}
