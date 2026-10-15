package cachekey

import (
	"testing"
)

func TestKebabToCamel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"config-path", "configPath"},
		{"put-registry-url", "putRegistryUrl"},
		{"also-branch-tag", "alsoBranchTag"},
		{"dry-run", "dryRun"},
		{"simple", "simple"},
		{"a-b-c-d", "aBCD"},
		{"", ""},
	}
	for _, tt := range tests {
		got := KebabToCamel(tt.input)
		if got != tt.want {
			t.Errorf("KebabToCamel(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
