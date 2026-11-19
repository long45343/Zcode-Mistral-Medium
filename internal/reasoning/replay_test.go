package reasoning

import (
	"encoding/json"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// loadStream reads a captured real SSE stream.
func loadStream(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/reasoning_stream.sse")
	if err != nil {
		t.Skipf("captured stream not available: %v", err)
	}
	return raw
}

// replay pushes the whole stream through in one write.
func replay(t *testing.T, stream []byte, enabled bool) string {
	t.Helper()
	r := New(enabled)
	var out strings.Builder
	out.Write(r.Write(stream))
	out.Write(r.Flush())
	return out.String()
}

// countEvents tallies "event: <name>" lines.
func countEvents(s string) map[string]int {
	counts := map[string]int{}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "event: ") {
			counts[strings.TrimPrefix(line, "event: ")]++
		}
	}
	return counts
}

// reasoningText concatenates every reasoning delta payload, regardless of which
// channel name it arrived under.
func reasoningText(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := []byte(strings.TrimPrefix(line, "data: "))
		ty := gjson.GetBytes(payload, "type").String()
		if ty == "response.reasoning_text.delta" || ty == "response.reasoning_summary_text.delta" {
			b.WriteString(gjson.GetBytes(payload, "delta").String())
		}
	}
	return b.String()
}

// TestReplayRealStream replays a stream captured from the provider and checks
// the conversion is complete and lossless.
func TestReplayRealStream(t *testing.T) {
	stream := loadStream(t)

	original := replay(t, stream, false)
	converted := replay(t, stream, true)

	origCounts := countEvents(original)
	convCounts := countEvents(converted)

	// Every raw reasoning delta becomes a summary delta.
	if origCounts["response.reasoning_text.delta"] == 0 {
		t.Fatal("captured stream has no raw reasoning deltas")
	}
	if got := convCounts["response.reasoning_text.delta"]; got != 0 {
		t.Errorf("raw reasoning deltas remain: %d", got)
	}
	if got, want := convCounts["response.reasoning_summary_text.delta"], origCounts["response.reasoning_text.delta"]; got != want {
		t.Errorf("summary deltas = %d, want %d", got, want)
	}

	// Content parts are renamed only when their part type is reasoning_text;
	// output_text parts must stay on the original channel name.
	wantParts := countReasoningParts(t, original)
	if wantParts == 0 {
		t.Fatal("captured stream has no reasoning content parts")
	}
	gotParts := convCounts["response.reasoning_summary_part.added"] + convCounts["response.reasoning_summary_part.done"]
	if gotParts != wantParts {
		t.Errorf("converted reasoning parts = %d, want %d", gotParts, wantParts)
	}
	if left := convCounts["response.content_part.added"] + convCounts["response.content_part.done"]; left != origCounts["response.content_part.added"]+origCounts["response.content_part.done"]-wantParts {
		t.Errorf("non-reasoning content parts changed: %d remain", left)
	}

	// Unrelated events must keep their counts exactly.
	for _, name := range []string{
		"response.output_text.delta",
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.output_item.done",
		"response.completed",
	} {
		if convCounts[name] != origCounts[name] {
			t.Errorf("%s count changed: %d -> %d", name, origCounts[name], convCounts[name])
		}
	}

	// The reasoning text itself must be identical.
	if a, b := reasoningText(t, original), reasoningText(t, converted); a != b {
		t.Errorf("reasoning text changed\noriginal len=%d\nconverted len=%d", len(a), len(b))
	}
}

// TestReplayEveryDataLineIsValidJSON is the safety net: no rewrite may produce
// a payload the client cannot parse.
func TestReplayEveryDataLineIsValidJSON(t *testing.T) {
	converted := replay(t, loadStream(t), true)

	checked := 0
	for _, line := range strings.Split(converted, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if !json.Valid([]byte(payload)) {
			t.Fatalf("invalid JSON payload:\n%s", payload)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no data lines found")
	}
}

// TestReplaySummaryIndexPresent checks every converted event carries the index
// the SDK needs to build stable part ids.
func TestReplaySummaryIndexPresent(t *testing.T) {
	converted := replay(t, loadStream(t), true)

	for _, line := range strings.Split(converted, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := []byte(strings.TrimPrefix(line, "data: "))
		ty := gjson.GetBytes(payload, "type").String()
		if ty != "response.reasoning_summary_text.delta" &&
			ty != "response.reasoning_summary_part.added" &&
			ty != "response.reasoning_summary_part.done" {
			continue
		}
		if !gjson.GetBytes(payload, "summary_index").Exists() {
			t.Errorf("missing summary_index on %s:\n%s", ty, payload)
		}
	}
}

// TestReplayArbitraryChunking splits the captured stream at random boundaries
// and verifies the output matches the single-write result. This is the property
// that matters in production, where TCP decides the boundaries.
func TestReplayArbitraryChunking(t *testing.T) {
	stream := loadStream(t)
	want := replay(t, stream, true)

	rng := rand.New(rand.NewSource(20261119))

	for round := 0; round < 30; round++ {
		r := New(true)
		var out strings.Builder

		pos := 0
		for pos < len(stream) {
			// Chunks from 1 byte to 4 KiB, deliberately irregular.
			step := 1 + rng.Intn(4096)
			if pos+step > len(stream) {
				step = len(stream) - pos
			}
			out.Write(r.Write(stream[pos : pos+step]))
			pos += step
		}
		out.Write(r.Flush())

		if got := out.String(); got != want {
			t.Fatalf("round %d: chunked replay differs from single-write replay", round)
		}
	}
}

// countReasoningParts counts content_part events whose part is reasoning_text.
func countReasoningParts(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := []byte(strings.TrimPrefix(line, "data: "))
		ty := gjson.GetBytes(payload, "type").String()
		if ty != "response.content_part.added" && ty != "response.content_part.done" {
			continue
		}
		if gjson.GetBytes(payload, "part.type").String() == "reasoning_text" {
			n++
		}
	}
	return n
}

// TestReplayFrameCountStable ensures no event is lost or duplicated.
func TestReplayFrameCountStable(t *testing.T) {
	stream := loadStream(t)

	origFrames := strings.Count(string(stream), "\n\n")
	conv := replay(t, stream, true)
	convFrames := strings.Count(conv, "\n\n")

	if convFrames != origFrames {
		t.Errorf("frame count changed: %d -> %d", origFrames, convFrames)
	}
}

// TestReplayStatsMatchEvents cross-checks the counters against the actual
// number of converted events.
func TestReplayStatsMatchEvents(t *testing.T) {
	stream := loadStream(t)

	r := New(true)
	r.Write(stream)
	r.Flush()

	st := r.Stats()
	orig := countEvents(string(stream))

	if st.Deltas != orig["response.reasoning_text.delta"] {
		t.Errorf("deltas = %d, want %d", st.Deltas, orig["response.reasoning_text.delta"])
	}
	wantParts := countReasoningParts(t, string(stream))
	if st.Parts != wantParts {
		t.Errorf("parts = %d, want %d", st.Parts, wantParts)
	}
}
