package cli

import (
	"strings"
	"testing"
)

func TestFinalizeOptionsRejectsGateExecutionAndAmbiguousArguments(t *testing.T) {
	args := []string{"--base", "main", "--branch", "topic", "--title", "fix: change", "--body-file", "body.md", "--verification-file", "proof.json", "--partial-delivery"}
	got, err := finalizeOptions(args)
	if err != nil || !got.PartialDelivery || got.VerificationFile != "proof.json" {
		t.Fatalf("options=%+v err=%v", got, err)
	}
	for _, extra := range [][]string{{"--gate", "local"}, {"--retry-failed"}, {"--file", "source.go"}, {"--proof-status", "passed"}, {"--base", "other"}} {
		_, err := finalizeOptions(append(append([]string{}, args...), extra...))
		if err == nil {
			t.Fatalf("unsupported argument %v accepted", extra)
		}
	}
	if _, err := finalizeOptions([]string{"--title", ""}); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := finalizeOptions([]string{"--body-file", "--draft"}); err == nil || !strings.Contains(err.Error(), "value") {
		t.Fatalf("missing body value: %v", err)
	}
}
