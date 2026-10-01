package output

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

func TestPaintRewritesOnlyChangedLines(t *testing.T) {
	var buf bytes.Buffer
	r := &LiveRenderer{errOut: &buf}

	r.paint([]string{"alpha", "bravo", "charlie"})
	buf.Reset()

	// Only the middle line changes.
	r.paint([]string{"alpha", "BRAVO", "charlie"})
	out := buf.String()

	if !strings.Contains(out, "BRAVO") {
		t.Errorf("changed line must be repainted: %q", out)
	}
	if strings.Contains(out, "alpha") || strings.Contains(out, "charlie") {
		t.Errorf("unchanged lines must not be repainted: %q", out)
	}
	if !strings.Contains(out, "\033[3A") {
		t.Errorf("expected a move to the top of the 3-line zone: %q", out)
	}
}

func TestPaintSkipsIdenticalFrame(t *testing.T) {
	var buf bytes.Buffer
	r := &LiveRenderer{errOut: &buf}

	r.paint([]string{"x", "y"})
	buf.Reset()
	r.paint([]string{"x", "y"})

	if buf.Len() != 0 {
		t.Errorf("an identical frame should produce no output, got %q", buf.String())
	}
}

func TestPaintClearsOrphanedLinesWhenFrameShrinks(t *testing.T) {
	var buf bytes.Buffer
	r := &LiveRenderer{errOut: &buf}

	r.paint([]string{"a", "b", "c", "d"})
	buf.Reset()
	r.paint([]string{"a", "b"})

	if len(r.prevFrame) != 2 {
		t.Fatalf("prevFrame after shrink = %d lines, want 2", len(r.prevFrame))
	}
	// Orphaned lines are blanked with a clear-to-EOL.
	if !strings.Contains(buf.String(), ClearToEOL) {
		t.Errorf("shrinking frame should clear orphaned lines: %q", buf.String())
	}
}

func TestSpinnerFrameAt(t *testing.T) {
	if got := spinnerFrameAt(0); got != 0 {
		t.Errorf("frame at 0 = %d, want 0", got)
	}
	if got := spinnerFrameAt(spinnerInterval); got != 1 {
		t.Errorf("frame after one interval = %d, want 1", got)
	}
	if got := spinnerFrameAt(time.Duration(len(liveSpinnerFrames)) * spinnerInterval); got != 0 {
		t.Errorf("frame should wrap after a full cycle, got %d", got)
	}
}

func TestSpinnerGlyphAdvancesWithFrame(t *testing.T) {
	r := &LiveRenderer{}
	r.spinnerFrame = 0
	first := r.spinnerGlyph()
	r.spinnerFrame = 1
	second := r.spinnerGlyph()
	if first == second {
		t.Fatalf("spinner glyph should advance between frames (%q == %q)", first, second)
	}
	// statusIcon uses the spinner for running rows.
	if icon := r.statusIcon(statusRunning, Cyan); icon.text != r.spinnerGlyph() {
		t.Errorf("running icon = %q, want spinner glyph %q", icon.text, r.spinnerGlyph())
	}
}

func TestRenderFrameIncludesBarAndFailurePane(t *testing.T) {
	test := makeLiveTestJob("app", "test")
	r, _ := newTestRenderer(t, 120, test)
	r.JobStart(test)
	r.JobComplete(test, &jobs.JobResult{Status: "failed"})

	joined := strings.Join(r.renderFrame(time.Now()), "\n")
	if !strings.Contains(joined, "█") && !strings.Contains(joined, "░") {
		t.Errorf("frame header should include the mini progress bar: %q", joined)
	}
	if !strings.Contains(joined, "failures") {
		t.Errorf("frame should include the failure pane after a failure: %q", joined)
	}
}
