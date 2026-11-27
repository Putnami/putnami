package build

import (
	"strings"
	"testing"
)

// A declared executable's tags join the build's own tags, whichever spelling
// the -tags flag uses, and the shared flags are never modified.
func TestWithTargetTags(t *testing.T) {
	cases := []struct {
		flags, tags []string
		want        string
	}{
		{nil, nil, ""},
		{[]string{"-trimpath"}, nil, "-trimpath"},
		{[]string{"-trimpath"}, []string{"a", "b"}, "-trimpath -tags a,b"},
		{[]string{"-tags", "x"}, []string{"a"}, "-tags x,a"},
		{[]string{"-tags", ""}, []string{"a"}, "-tags a"},
		{[]string{"-tags=x"}, []string{"a"}, "-tags=x,a"},
		{[]string{"--tags=x"}, []string{"a"}, "--tags=x,a"},
	}
	for _, tc := range cases {
		shared := append([]string(nil), tc.flags...)
		if got := strings.Join(withTargetTags(tc.flags, tc.tags), " "); got != tc.want {
			t.Errorf("withTargetTags(%q, %q) = %q, want %q", tc.flags, tc.tags, got, tc.want)
		}
		if strings.Join(shared, " ") != strings.Join(tc.flags, " ") {
			t.Errorf("withTargetTags modified its flags %q", tc.flags)
		}
	}
}
