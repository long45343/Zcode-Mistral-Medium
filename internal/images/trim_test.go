package images

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const pixel = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func img() map[string]any {
	return map[string]any{"type": "input_image", "image_url": pixel}
}

// buildBody puts n images into one user message.
func buildBody(n int) []byte {
	parts := []any{map[string]any{"type": "input_text", "text": "look"}}
	for i := 0; i < n; i++ {
		parts = append(parts, img())
	}
	body := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": parts},
		},
	}
	raw, _ := json.Marshal(body)
	return raw
}

// countImages walks the same locations the provider counts.
func countImages(t *testing.T, body []byte) int {
	t.Helper()
	n := 0
	for _, item := range gjson.GetBytes(body, "input").Array() {
		for _, part := range item.Get("content").Array() {
			if part.Get("type").String() == "input_image" {
				n++
			}
		}
		for _, part := range item.Get("output").Array() {
			if part.Get("type").String() == "input_image" {
				n++
			}
		}
	}
	return n
}

// TestUnderLimitIsUntouched means a valid request keeps its exact bytes.
func TestUnderLimitIsUntouched(t *testing.T) {
	body := buildBody(20)

	got, st := Trim(body, DefaultMax)
	if st.Changed() {
		t.Fatalf("expected no change, got %+v", st)
	}
	if string(got) != string(body) {
		t.Error("body was modified despite being within the limit")
	}
	if st.Found != 20 {
		t.Errorf("Found = %d, want 20", st.Found)
	}
}

// TestOverLimitDropsOldest keeps the newest images and replaces the rest.
func TestOverLimitDropsOldest(t *testing.T) {
	body := buildBody(25)

	got, st := Trim(body, DefaultMax)
	if st.Found != 25 {
		t.Errorf("Found = %d, want 25", st.Found)
	}
	if st.Dropped != 5 {
		t.Errorf("Dropped = %d, want 5", st.Dropped)
	}
	if n := countImages(t, got); n != 20 {
		t.Errorf("remaining images = %d, want 20", n)
	}
	if !json.Valid(got) {
		t.Fatal("trim produced invalid JSON")
	}
}

// TestPlaceholderExplains keeps the history readable instead of leaving a gap.
func TestPlaceholderExplains(t *testing.T) {
	got, _ := Trim(buildBody(21), DefaultMax)

	text := string(got)
	if !strings.Contains(text, "image omitted") {
		t.Errorf("expected a placeholder note, got %s", text[:200])
	}
}

// TestToolOutputImagesCounted covers images carried by function_call_output,
// which the provider counts the same way.
func TestToolOutputImagesCounted(t *testing.T) {
	outputs := []any{map[string]any{"type": "input_text", "text": "result"}}
	for i := 0; i < 22; i++ {
		outputs = append(outputs, img())
	}
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "go"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shot", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": outputs},
		},
	})

	got, st := Trim(body, DefaultMax)
	if st.Found != 22 || st.Dropped != 2 {
		t.Fatalf("stats = %+v, want found=22 dropped=2", st)
	}
	if n := countImages(t, got); n != 20 {
		t.Errorf("remaining images = %d, want 20", n)
	}
}

// TestMixedSourcesShareBudget is the case that bit in practice: images spread
// across messages and tool outputs share one budget.
func TestMixedSourcesShareBudget(t *testing.T) {
	msgParts := []any{map[string]any{"type": "input_text", "text": "x"}}
	for i := 0; i < 10; i++ {
		msgParts = append(msgParts, img())
	}
	outParts := []any{map[string]any{"type": "input_text", "text": "r"}}
	for i := 0; i < 11; i++ {
		outParts = append(outParts, img())
	}

	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": msgParts},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shot", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": outParts},
		},
	})

	got, st := Trim(body, DefaultMax)
	if st.Found != 21 || st.Dropped != 1 {
		t.Fatalf("stats = %+v, want found=21 dropped=1", st)
	}
	if n := countImages(t, got); n != 20 {
		t.Errorf("remaining images = %d, want 20", n)
	}
}

// TestCustomLimit allows a stricter ceiling.
func TestCustomLimit(t *testing.T) {
	got, st := Trim(buildBody(5), 2)
	if st.Dropped != 3 {
		t.Errorf("Dropped = %d, want 3", st.Dropped)
	}
	if n := countImages(t, got); n != 2 {
		t.Errorf("remaining images = %d, want 2", n)
	}
}

// TestNoImagesIsUntouched covers text-only traffic.
func TestNoImagesIsUntouched(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`)

	got, st := Trim(body, DefaultMax)
	if st.Found != 0 || st.Changed() {
		t.Errorf("unexpected stats: %+v", st)
	}
	if string(got) != string(body) {
		t.Error("text-only body was modified")
	}
}

// TestInputNotArrayIsUntouched covers the string form of input.
func TestInputNotArrayIsUntouched(t *testing.T) {
	body := []byte(`{"model":"m","input":"hello"}`)

	got, st := Trim(body, DefaultMax)
	if st.Changed() || string(got) != string(body) {
		t.Errorf("string input should pass through, got %s", got)
	}
}
