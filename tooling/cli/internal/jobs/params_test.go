package jobs

import (
	"testing"
)

func TestParamStrings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		params map[string]any
		key    string
		want   int // expected length, -1 for nil
	}{
		{"nil params", nil, "key", -1},
		{"missing key", map[string]any{"other": "val"}, "key", -1},
		{"wrong type", map[string]any{"key": "not-a-slice"}, "key", -1},
		{"empty slice", map[string]any{"key": []any{}}, "key", 0},
		{"string slice", map[string]any{"key": []any{"a", "b", "c"}}, "key", 3},
		{"mixed slice", map[string]any{"key": []any{"a", 42, "b"}}, "key", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := paramStrings(tt.params, tt.key)
			if tt.want == -1 {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != tt.want {
				t.Errorf("len = %d, want %d (got %v)", len(got), tt.want, got)
			}
		})
	}

	// Verify actual values
	result := paramStrings(map[string]any{"fp": []any{"*.go", "*.ts"}}, "fp")
	if result[0] != "*.go" || result[1] != "*.ts" {
		t.Errorf("values = %v, want [*.go *.ts]", result)
	}
}
