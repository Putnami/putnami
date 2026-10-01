package compose

import (
	"strings"
	"testing"
)

func TestError_MessageLeavesTheMemberOutputOut(t *testing.T) {
	err := &Error{
		Code:    CodeMemberExited,
		Member:  "/app",
		Phase:   PhaseReadiness,
		Message: "the serve step exited before its ready event (failed, exit code 1)",
		Detail:  []string{"connect postgres://app:s3cret@127.0.0.1:5432/app: refused"},
	}
	want := "compose.member_exited: /app: the serve step exited before its ready event (failed, exit code 1) (phase readiness)"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the message carries the member output: %q", err.Error())
	}
}
