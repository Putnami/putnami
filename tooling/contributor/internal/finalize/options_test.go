package finalize

import (
	"testing"
)

// TestParseOptionsReadsEveryFlag pins every flag the core's parser reads, in
// both the separate and the inline value form.
func TestParseOptionsReadsEveryFlag(t *testing.T) {
	t.Parallel()
	got, err := ParseOptions([]string{"--base", "main", "--branch=topic", "--title", "feat: x", "--body-file", "body.md",
		"--task-source", "local:s", "--task-id", "T-1", "--verification-file", "dossier.json", "--workflow-file", "workflow.json",
		"--gate", "ci", "--proof", "ran", "--draft", "--partial-delivery"})
	if err != nil {
		t.Fatal(err)
	}
	want := Options{Base: "main", Branch: "topic", Title: "feat: x", BodyFile: "body.md", TaskSource: "local:s", TaskID: "T-1",
		VerificationFile: "dossier.json", WorkflowFile: "workflow.json", Gate: "ci", Proof: "ran", Draft: true, PartialDelivery: true}
	if got != want {
		t.Errorf("ParseOptions = %+v, want %+v", got, want)
	}
	if got, err := ParseOptions(nil); err != nil || got != (Options{}) {
		t.Errorf("ParseOptions(nil) = %+v, %v, want no option", got, err)
	}
}

// TestParseOptionsRefusesWithTheCoreMessages pins every refusal, message for
// message.
func TestParseOptionsRefusesWithTheCoreMessages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--draft=true"}, "--draft takes no value"},
		{[]string{"--partial-delivery=yes"}, "--partial-delivery takes no value"},
		{[]string{"--draft", "--draft"}, "--draft given twice"},
		{[]string{"--partial-delivery", "--partial-delivery"}, "--partial-delivery given twice"},
		{[]string{"--unknown"}, `unexpected finalize argument "--unknown"`},
		{[]string{"positional"}, `unexpected finalize argument "positional"`},
		{[]string{"--base=a", "--base", "b"}, "--base given twice"},
		{[]string{"--title"}, "--title needs a value"},
		{[]string{"--title", ""}, "--title needs a nonempty value"},
		{[]string{"--title="}, "--title needs a nonempty value"},
		{[]string{"--body-file", "--draft"}, "--body-file needs a nonempty value"},
		{[]string{"--gate", "local"}, "--gate accepts only ci; local proof uses --verification-file"},
		{[]string{"--gate=--ci"}, "--gate needs a nonempty value"},
	} {
		if _, err := ParseOptions(tc.args); err == nil || err.Error() != tc.want {
			t.Errorf("ParseOptions(%q) error %v, want %q", tc.args, err, tc.want)
		}
	}
}

// TestParseOptionsTakesAValueThatStartsWithOneDash pins that only a value
// that starts with -- reads as a missing value: -h is a value.
func TestParseOptionsTakesAValueThatStartsWithOneDash(t *testing.T) {
	t.Parallel()
	got, err := ParseOptions([]string{"--verification-file", "-h", "--proof", "-"})
	if err != nil || got.VerificationFile != "-h" || got.Proof != "-" {
		t.Errorf("ParseOptions = %+v, %v, want -h and - as values", got, err)
	}
}
