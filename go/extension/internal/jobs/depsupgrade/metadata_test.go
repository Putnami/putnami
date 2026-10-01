package depsupgrade

import "testing"

// A go.mod path keeps the spelling go.work gives its use directory, in the
// separator of the platform: a Windows path never mixes "\" and "/", and a
// Unix path is the plain join.
func TestJoinPathKeepsTheSpellingInOneSeparator(t *testing.T) {
	for _, tc := range []struct {
		separator byte
		elements  []string
		want      string
	}{
		{'/', []string{"/ws", "./tool", "go.mod"}, "/ws/./tool/go.mod"},
		{'/', []string{"/ws", "libs/a", "go.mod"}, "/ws/libs/a/go.mod"},
		{'\\', []string{`C:\ws`, "./tool", "go.mod"}, `C:\ws\.\tool\go.mod`},
		{'\\', []string{`C:\ws`, "libs/a", "go.mod"}, `C:\ws\libs\a\go.mod`},
		{'\\', []string{`C:\ws`, "go.work"}, `C:\ws\go.work`},
	} {
		if got := joinPathWith(tc.separator, tc.elements...); got != tc.want {
			t.Errorf("joinPathWith(%q, %q) = %q, want %q", tc.separator, tc.elements, got, tc.want)
		}
	}
}
