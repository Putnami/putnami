package cli

import (
	"testing"
)

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
