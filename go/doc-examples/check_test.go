package docexamples

import (
	"os"
	"strings"
	"testing"
)

func TestCheckReportsDriftAndToleratesFragments(t *testing.T) {
	data, err := os.ReadFile("testdata/drift.md")
	if err != nil {
		t.Fatal(err)
	}
	var examples []Example
	for _, block := range Blocks(string(data)) {
		if block.Lang == "go" && block.Checked() {
			examples = append(examples, Example{Doc: "drift.md", Block: block})
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	checker, err := NewChecker(wd)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := checker.Check(examples)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		line     int
		contains string
	}{
		{29, "user.Email undefined"},
		{35, "undefined: strings.NoSuchFunction"},
		{41, "cannot use 42"},
		{51, "does not parse"},
		{58, "undefined: app.NoSuchPlugin"},
		{68, "too many return values"},
		{75, "undefined: inject.NoSuchConstructor"},
		{91, "config.Load"},
		{92, "undefined: grpc.NewPlugins"},
	}
	if len(findings) != len(want) {
		t.Fatalf("got %d findings, want %d: %v", len(findings), len(want), findings)
	}
	for i, w := range want {
		if findings[i].Line != w.line || !strings.Contains(findings[i].Message, w.contains) {
			t.Errorf("finding %d = %v, want line %d containing %q", i, findings[i], w.line, w.contains)
		}
	}
}
