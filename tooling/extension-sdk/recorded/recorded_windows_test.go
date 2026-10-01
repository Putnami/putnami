//go:build windows

package recorded

import "testing"

// A value cmd.exe would read differently than it is written fails the test
// instead of replaying another exchange than the recorded one.
func TestExecutableRefusesAValueABatchFileCannotCarry(t *testing.T) {
	refusal := Command(t, "testdata/refusal")
	for _, value := range []string{`say "hi"`, "100%", "wow!", "a^b"} {
		refuses(t, "cmd.exe reads differently", func(tb testing.TB) {
			Executable(tb, refusal, Branch{Env: "RECORDED_BRANCH", Value: value, Exchange: refusal})
		})
	}
	refuses(t, "Ctrl+Z", func(tb testing.TB) {
		Executable(tb, Exchange{stdout: []byte("text\x1aafter")})
	})
}
