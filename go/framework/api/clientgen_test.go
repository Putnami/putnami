package api

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// collapseHSpace collapses runs of spaces/tabs to a single space so substring
// assertions on generated source stay independent of gofmt's struct-field column
// alignment (which varies with sibling field widths). Newlines are preserved.
func collapseHSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == ' ' || c == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		b.WriteByte(s[i])
	}
	return b.String()
}

// containsNormalized reports whether want appears in src once both are
// horizontal-whitespace-normalized — for field/line assertions that must not
// depend on gofmt's column alignment.
func containsNormalized(src, want string) bool {
	return strings.Contains(collapseHSpace(src), collapseHSpace(want))
}

func TestCanonicalOperationID(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "operation-identity-is-derived-from-method-and-path")
	cases := []struct{ method, path, want string }{
		{"GET", "/users", "getUsers"},
		{"GET", "/users/{id}", "getUsers_Id"},
		{"GET", "/users/[id]", "getUsers_Id"},
		{"POST", "/v1/operator/cli-usage", "postV1_Operator_Cli-usage"},
		{"GET", "/files/{path...}", "getFiles_Path"},
		{"GET", "/", "get"},
	}
	for _, tc := range cases {
		if got := CanonicalOperationID(tc.method, tc.path); got != tc.want {
			t.Errorf("CanonicalOperationID(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
