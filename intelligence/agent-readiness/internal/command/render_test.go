package command

import (
	"testing"
	"time"
)

func TestReadLine(t *testing.T) {
	got := readLine(Built{Commits: 2431, Areas: 11, Elapsed: 48 * time.Second})
	if want := "Reading 2,431 commits, 11 areas… done (48 s)"; got != want {
		t.Fatalf("readLine = %q, want %q", got, want)
	}
	if got := readLine(Built{Commits: 3, Areas: 1, Elapsed: 20 * time.Millisecond}); got != "Reading 3 commits, 1 areas… done (1 s)" {
		t.Fatalf("readLine = %q", got)
	}
}

func TestVerdictLine(t *testing.T) {
	cases := map[string]string{
		"L1": "This codebase supports agents that assist (L1). 100% of recent changes land where an agent cannot yet work alone.",
		"L2": "This codebase supports supervised agents (L2). 100% of recent changes land where an agent cannot yet work alone.",
		"L3": "This codebase supports delegated agents (L3). 100% of recent changes land where an agent cannot yet work alone.",
		"L4": "This codebase supports agents reviewed by exception (L4). 100% of recent changes land where an agent cannot yet work alone.",
		"L9": "This codebase is at level L9. 100% of recent changes land where an agent cannot yet work alone.",
	}
	for level, want := range cases {
		if got := verdictLine(level, 1); got != want {
			t.Fatalf("verdictLine(%s) = %q, want %q", level, got, want)
		}
	}
	if got := verdictLine("L3", 0.004); got != "This codebase supports delegated agents (L3). 0% of recent changes land where an agent cannot yet work alone." {
		t.Fatalf("verdictLine rounds %q", got)
	}
}

func TestSentLine(t *testing.T) {
	if got, want := sentLine(14*1024+100), "Sent: 14 KB — counts, area names and paths, no file contents. Inspect: putnami agent-readiness --print-payload"; got != want {
		t.Fatalf("sentLine = %q, want %q", got, want)
	}
	if kilobytes(10) != 1 {
		t.Fatal("a payload under 1 KB must read 1 KB")
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4200: "-4,200"} {
		if got := thousands(n); got != want {
			t.Fatalf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}
