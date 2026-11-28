package gitread

import (
	"errors"
	"slices"
	"testing"
)

type fakeHeld struct {
	answers map[string]string
	kept    []string
}

func (f *fakeHeld) Answer(dir, stdin string, _, args []string) (string, string, bool) {
	out, ok := f.answers[dir+"|"+stdin+"|"+args[0]]
	return out, "", ok
}

func (f *fakeHeld) Asking(dir, stdin string, _, args []string) func(stdout, stderr string) {
	if args[0] == "untracked" {
		return nil
	}
	return func(stdout, _ string) { f.kept = append(f.kept, args[0]+"="+stdout) }
}

// TestRunHeldAnswersWhatTheSourceHolds holds the hook: a held call is
// answered without running git, a run call is offered to the source once it
// succeeded, and with no source every call runs.
func TestRunHeldAnswersWhatTheSourceHolds(t *testing.T) {
	runs := 0
	run := func(out string, err error) func() (string, string, error) {
		return func() (string, string, error) { runs++; return out, "", err }
	}
	if out, _, _ := RunHeld("d", "", nil, []string{"held"}, run("git", nil)); out != "git" || runs != 1 {
		t.Fatalf("with no source: %q after %d run(s), want git's answer after 1", out, runs)
	}
	source := &fakeHeld{answers: map[string]string{"d|in|held": "kept"}}
	restore := UseHeld(source)
	defer restore()
	runs = 0
	if out, _, _ := RunHeld("d", "in", nil, []string{"held"}, run("git", nil)); out != "kept" || runs != 0 {
		t.Errorf("a held call: %q after %d run(s), want the held answer after none", out, runs)
	}
	if out, _, _ := RunHeld("d", "", nil, []string{"asked"}, run("fresh", nil)); out != "fresh" || runs != 1 {
		t.Errorf("a call the source does not hold: %q after %d run(s), want git's answer after 1", out, runs)
	}
	if _, _, err := RunHeld("d", "", nil, []string{"failed"}, run("", errors.New("exit 1"))); err == nil {
		t.Error("a failed call answered no error")
	}
	if _, _, err := RunHeld("d", "", nil, []string{"untracked"}, run("x", nil)); err != nil {
		t.Errorf("a call the source keeps nothing of: %v", err)
	}
	if want := []string{"asked=fresh"}; !slices.Equal(source.kept, want) {
		t.Errorf("kept %q, want %q: only a successful call the source asked to keep", source.kept, want)
	}
	other := UseHeld(&fakeHeld{})
	restore()
	if held.Load() == nil {
		t.Error("restoring a source replaced since uninstalled the newer one")
	}
	other()
	restore()
	runs = 0
	if out, _, _ := RunHeld("d", "in", nil, []string{"held"}, run("git", nil)); out != "git" || runs != 1 {
		t.Errorf("after restore: %q after %d run(s), want git's answer after 1", out, runs)
	}
}
