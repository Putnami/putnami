package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/sdk/extension/jsonl"
)

func boundsFragment(message string) evidence.Fragment {
	return evidence.Fragment{
		ProtocolVersion: evidence.FragmentProtocolVersion,
		Kind:            evidence.FragmentKindFinding,
		Source:          evidence.FragmentSource{Format: "putnami-sdd"},
		Subject:         evidence.FragmentSubject{Path: "billing/putnami.features.json"},
		Rule:            "invalid_relation",
		Severity:        "error",
		Message:         message,
	}
}

// TestALongFindingMessageIsCutToTheProtocolBound pins the producer's duty: a
// message longer than evidence.MaxStringBytes is cut at a character boundary
// within the bound, and the file still parses.
func TestALongFindingMessageIsCutToTheProtocolBound(t *testing.T) {
	ctx := featuresJobContext(t.TempDir())
	long := strings.Repeat("é", evidence.MaxStringBytes)
	var err error
	captureStdout(t, func() {
		err = emitEvidenceFragments(ctx, jsonl.New(), []evidence.Fragment{boundsFragment(long)})
	})
	if err != nil {
		t.Fatalf("emit a fragment with a long message: %v", err)
	}
	_, fragments := writtenFragments(t, ctx)
	if len(fragments) != 1 {
		t.Fatalf("fragments = %d, want 1", len(fragments))
	}
	message := fragments[0].Message
	if len(message) > evidence.MaxStringBytes || len(message) < evidence.MaxStringBytes-utf8.UTFMax {
		t.Fatalf("message holds %d bytes, want the most that fit in %d", len(message), evidence.MaxStringBytes)
	}
	if !utf8.ValidString(message) || !strings.HasPrefix(long, message) {
		t.Fatal("the cut message is not a prefix of the finding's message on a character boundary")
	}
}

// TestFindingsPastTheFragmentCapFailTheTask pins the cap: a file that would
// hold more than evidence.MaxFragments fragments is not written cut short;
// the task fails and states the cap.
func TestFindingsPastTheFragmentCapFailTheTask(t *testing.T) {
	ctx := featuresJobContext(t.TempDir())
	fragments := make([]evidence.Fragment, evidence.MaxFragments+1)
	for i := range fragments {
		fragments[i] = boundsFragment("finding")
	}
	var err error
	captureStdout(t, func() {
		err = emitEvidenceFragments(ctx, jsonl.New(), fragments)
	})
	if err == nil {
		t.Fatal("a file past the fragment cap was written")
	}
	if !strings.Contains(err.Error(), "more than the 50000") {
		t.Fatalf("error = %v, want it to state the cap", err)
	}
}
