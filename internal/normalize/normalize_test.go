package normalize

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func loadBody(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Skipf("testdata %s not available: %v", name, err)
	}
	return raw
}

// TestAddsMissingMessageType is the regression test for Mistral error 3051.
func TestAddsMissingMessageType(t *testing.T) {
	body := loadBody(t, "body_missing_types.json")

	got, st := Rewrite(body)
	if st.Typed == 0 {
		t.Fatal("expected at least one item to receive a type field")
	}

	for i, it := range gjson.GetBytes(got, "input").Array() {
		if !gjson.Get(it.Raw, "type").Exists() {
			t.Errorf("item %d still has no type: %.120s", i, it.Raw)
		}
	}
}

// TestKeepsExistingTypes makes sure items that already declare a type keep it.
func TestKeepsExistingTypes(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"input": [
			{"type":"function_call","call_id":"c1","name":"t","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"ok"},
			{"role":"user","content":"hi"}
		]
	}`)

	got, st := Rewrite(body)
	if st.Typed != 1 {
		t.Fatalf("expected exactly 1 typed item, got %d", st.Typed)
	}
	if ty := gjson.GetBytes(got, "input.0.type").String(); ty != "function_call" {
		t.Errorf("function_call type was overwritten: %q", ty)
	}
	if ty := gjson.GetBytes(got, "input.1.type").String(); ty != "function_call_output" {
		t.Errorf("function_call_output type was overwritten: %q", ty)
	}
}

// TestLeavesUnrelatedFieldsAlone guards against re-serializing the whole body,
// which would change key order and break caching.
func TestLeavesUnrelatedFieldsAlone(t *testing.T) {
	body := []byte(`{"model":"m","z_field":1,"input":[{"role":"user","content":"hi"}],"a_field":{"nested":true}}`)

	got, st := Rewrite(body)
	if st.Typed != 1 {
		t.Fatalf("expected 1 typed item, got %d", st.Typed)
	}

	for _, want := range []string{`"model":"m"`, `"z_field":1`, `"a_field":{"nested":true}`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("unrelated field changed, missing %s in: %s", want, got)
		}
	}
}

// TestNoChangeReturnsOriginal keeps already-valid bodies byte identical.
func TestNoChangeReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`)

	got, st := Rewrite(body)
	if st.Changed() {
		t.Fatalf("expected no change, got %+v", st)
	}
	if string(got) != string(body) {
		t.Errorf("body modified despite no change:\n got=%s\n want=%s", got, body)
	}
}

// TestInputNotArrayIsUntouched covers providers that accept a plain string.
func TestInputNotArrayIsUntouched(t *testing.T) {
	body := []byte(`{"model":"m","input":"hello"}`)

	got, st := Rewrite(body)
	if st.Changed() || string(got) != string(body) {
		t.Errorf("string input should pass through unchanged, got %s", got)
	}
}

// TestRealBodyIsValidJSON ensures the rewrite never produces malformed JSON.
func TestRealBodyIsValidJSON(t *testing.T) {
	body := loadBody(t, "body_missing_types.json")

	got, _ := Rewrite(body)
	if !json.Valid(got) {
		t.Fatal("rewrite produced invalid JSON")
	}
}
