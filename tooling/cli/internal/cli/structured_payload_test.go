package cli

import (
	"encoding/json"
	"testing"
)

// TestStructuredPayloadIsAnArrayWhenEmpty pins the empty stream to data: [].
// A nil slice marshaled to null, which a list consumer cannot read as a list.
func TestStructuredPayloadIsAnArrayWhenEmpty(t *testing.T) {
	t.Parallel()
	for captured, want := range map[string]string{
		"":                                 "[]",
		"[]\n":                             "[]",
		`{"id":"a"}` + "\n":                `{"id":"a"}`,
		"{\"id\":\"a\"}\n{\"id\":\"b\"}\n": `[{"id":"a"},{"id":"b"}]`,
	} {
		data, err := structuredPayload(captured)
		if err != nil {
			t.Fatalf("structuredPayload(%q): %v", captured, err)
		}
		got, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("structuredPayload(%q) = %s, want %s", captured, got, want)
		}
	}
}

// TestContextCommandsAcceptStructuredOutput holds the catalog entries that let
// `context pack --output=json` and `context map --output=json` reach their
// structured renderers.
func TestContextCommandsAcceptStructuredOutput(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"pack", "map"} {
		for _, output := range []string{"json", "jsonl"} {
			if _, rejected := rejectStructuredIfUnsupported(output, "context", sub); rejected {
				t.Errorf("context %s --output=%s is rejected", sub, output)
			}
		}
	}
}
