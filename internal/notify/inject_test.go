package notify

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const completedFrame = "event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}` +
	"\n\n"

const itemAddedFrame = "event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message"}}` +
	"\n\n"

const textFrame = "event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"hi"}` +
	"\n\n"

// events splits a stream into (event name, data json) pairs.
func events(t *testing.T, s string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, frame := range strings.Split(s, "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		out = append(out, [2]string{name, data})
	}
	return out
}

// TestDisabledIsTransparent keeps the off path byte-identical.
func TestDisabledIsTransparent(t *testing.T) {
	in := New(false, "notice")
	stream := itemAddedFrame + textFrame + completedFrame

	got := string(in.Write([]byte(stream))) + string(in.Flush())
	if got != stream {
		t.Error("disabled injector modified the stream")
	}
}

// TestInjectsBeforeCompleted places the advisory ahead of the terminal event.
func TestInjectsBeforeCompleted(t *testing.T) {
	in := New(true, "WARNING")
	got := string(in.Write([]byte(itemAddedFrame + textFrame + completedFrame))) + string(in.Flush())

	evs := events(t, got)
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	if evs[len(evs)-1][0] != "response.completed" {
		t.Errorf("last event = %q, want response.completed", evs[len(evs)-1][0])
	}

	var names []string
	for _, e := range evs {
		names = append(names, e[0])
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, names)
		}
	}
}

// TestInjectedIndexFollowsExisting makes sure the advisory item lands after
// every real output item.
func TestInjectedIndexFollowsExisting(t *testing.T) {
	got := string(New(true, "WARNING").Write([]byte(itemAddedFrame + completedFrame)))

	var injected int64 = -1
	for _, e := range events(t, got) {
		if e[0] == "response.output_item.added" && strings.Contains(e[1], "msg_gateway_notice") {
			injected = gjson.Get(e[1], "output_index").Int()
		}
	}
	if injected != 2 {
		t.Errorf("injected output_index = %d, want 2 (existing max was 1)", injected)
	}
}

// TestInjectedJSONIsValid guards against emitting something the client cannot parse.
func TestInjectedJSONIsValid(t *testing.T) {
	got := string(New(true, "WARNING").Write([]byte(textFrame + completedFrame)))

	for _, e := range events(t, got) {
		if !json.Valid([]byte(e[1])) {
			t.Errorf("invalid JSON for %s: %s", e[0], e[1])
		}
	}
}

// TestInjectsOnlyOnce keeps a single advisory per stream.
func TestInjectsOnlyOnce(t *testing.T) {
	stream := itemAddedFrame + completedFrame + completedFrame
	got := string(New(true, "WARNING").Write([]byte(stream)))

	if n := strings.Count(got, "msg_gateway_notice"); n == 0 {
		t.Fatal("advisory not injected")
	}
	if n := strings.Count(got, `"type":"response.output_text.delta"`); n != 1 {
		t.Errorf("advisory injected %d times, want 1", n)
	}
}

// TestSplitAcrossWrites covers a completed frame arriving in pieces.
func TestSplitAcrossWrites(t *testing.T) {
	in := New(true, "WARNING")
	stream := itemAddedFrame + completedFrame
	half := len(stream) / 2

	var out strings.Builder
	out.Write(in.Write([]byte(stream[:half])))
	out.Write(in.Write([]byte(stream[half:])))
	out.Write(in.Flush())

	if !strings.Contains(out.String(), "msg_gateway_notice") {
		t.Error("advisory missing after split writes")
	}
	if !strings.HasSuffix(strings.TrimSpace(out.String()), `"status":"completed"}}`) {
		t.Errorf("stream does not end with the completed frame: %s", out.String()[len(out.String())-80:])
	}
}

// TestBuildMessageMentionsNumbers keeps the text actionable.
func TestBuildMessageMentionsNumbers(t *testing.T) {
	msg := BuildMessage(18, 20)
	if !strings.Contains(msg, "18") || !strings.Contains(msg, "20") {
		t.Errorf("message should mention both numbers: %q", msg)
	}
}
