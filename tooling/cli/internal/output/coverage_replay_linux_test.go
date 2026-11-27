package output

import (
	"os"
	"testing"
)

// TestTheLiveRendererReadsTheCoverageReplay holds the other half of
// TestOnlyTheLiveRendererReadsTheCoverageReplay: on a terminal, the live
// renderer reads the replay once and keeps what it returned.
func TestTheLiveRendererReadsTheCoverageReplay(t *testing.T) {
	terminal, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = terminal.Close() })
	t.Setenv("K_SERVICE", "")
	t.Setenv("CI", "")
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	reads := 0
	replay := map[string]CoverageReplay{"/p": {SessionID: "s"}}
	r := NewRenderer(Config{Out: terminal, Err: terminal, CoverageReplay: func() map[string]CoverageReplay {
		reads++
		return replay
	}})
	live, ok := r.(*LiveRenderer)
	if !ok {
		t.Fatalf("NewRenderer on a terminal = %T, want *LiveRenderer", r)
	}
	if reads != 1 || live.coverageReplay["/p"].SessionID != "s" {
		t.Errorf("the live renderer read the replay %d time(s) and holds %v, want once and the replayed entry", reads, live.coverageReplay)
	}
}
