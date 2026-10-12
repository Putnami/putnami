package contributor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestLanguageSignalsAndExclusions(t *testing.T) {
	cases := []struct {
		text     string
		offender bool
	}{
		{"Nous corrigons le code", true},
		{"corrige le parseur des tableaux", true},
		{"école café", true},
		{"An English naïve example", false},
		{"Des Moines", false},
		{"The RGPD and CNIL review `nous avec`\n```\nnous avec\n```", false},
		{"Use --avec on the command line", false},
	}
	for _, tc := range cases {
		if got := EvaluateLanguage("unit", tc.text) != nil; got != tc.offender {
			t.Errorf("%q offender=%t, want %t", tc.text, got, tc.offender)
		}
	}
	if hit := EvaluateLanguage("commit", "Explain the reason\nNous corrigeons le code"); hit == nil || len(hit.Lines) == 0 || !strings.HasPrefix(hit.Lines[0], "2:") {
		t.Fatalf("diagnostic lost actionable line: %+v", hit)
	}
	for _, path := range []string{"src/a_test.go", "src/a.test.ts", "fixtures/sample.md", "testdata/file.md"} {
		if !ExcludedLanguagePath(path) {
			t.Errorf("fixture %s was not excluded", path)
		}
	}
	if !ExcludedLanguagePath("tooling/cli/internal/commands/contributor/language.go") ||
		!ExcludedLanguagePath(`tooling\cli\internal\commands\contributor\language.go`) {
		t.Fatal("the native detector's signal vocabulary was scanned as public prose")
	}
	for _, path := range []string{"tooling/cli/internal/commands/contributor/language_modes.go", "somewhere/else/language.go"} {
		if ExcludedLanguagePath(path) {
			t.Errorf("unrelated source %s was excluded", path)
		}
	}
}

func TestTaskLanguageFollowsPagesThroughProvider(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-publication", "the-language-rule-reads-tasks-through-the-contract")
	var out, errors bytes.Buffer
	calls := 0
	runner := LanguageRunner{Root: t.TempDir(), Out: &out, Err: &errors, Call: func(_ context.Context, contract, operation string, request json.RawMessage) collab.Envelope {
		if contract != collab.ContractTasks || operation != collab.OperationFind {
			t.Fatalf("wrong route: %s.%s", contract, operation)
		}
		var input collab.TaskFindInput
		if err := json.Unmarshal(request, &input); err != nil {
			t.Fatal(err)
		}
		calls++
		var page collab.TaskListResult
		if calls == 1 {
			if input.Page.Cursor != "" {
				t.Fatalf("first cursor %q", input.Page.Cursor)
			}
			page = collab.TaskListResult{Items: []collab.Task{{Ref: collab.Ref{Source: "local:x", ID: "one"}, Title: "English title", Body: "audit-fp: workspace/language/english-only"}}, Page: collab.Page{Next: "next"}}
		} else {
			if input.Page.Cursor != "next" {
				t.Fatalf("next cursor %q", input.Page.Cursor)
			}
			page = collab.TaskListResult{Items: []collab.Task{{Ref: collab.Ref{Source: "local:x", ID: "two"}, Title: "Nous corrigons le code"}}}
		}
		data, _ := json.Marshal(page)
		return collab.Envelope{Outcome: collab.OutcomeOK, Result: data}
	}}
	if status := runner.RunLanguage(context.Background(), "tasks", nil); status != 1 || calls != 2 || !strings.Contains(out.String(), "task two") {
		t.Fatalf("status=%d calls=%d output=%q error=%q", status, calls, out.String(), errors.String())
	}
}

func TestLanguageFilesDistinguishesToolFailure(t *testing.T) {
	root := t.TempDir()
	var out, errors bytes.Buffer
	runner := LanguageRunner{Root: root, Out: &out, Err: &errors}
	if status := runner.RunLanguage(context.Background(), "files", []string{"--diff", "missing..HEAD"}); status != 2 {
		t.Fatalf("invalid Git range answered %d", status)
	}
	file := filepath.Join(root, "text.txt")
	if err := os.WriteFile(file, []byte("Nous avec le code"), 0600); err != nil {
		t.Fatal(err)
	}
	runner.Stdin = strings.NewReader("Nous avec le code")
	if status := runner.RunLanguage(context.Background(), "text", []string{"--label", "proposal"}); status != 1 || !strings.Contains(out.String(), "proposal:1:") {
		t.Fatalf("status=%d output=%q", status, out.String())
	}
}

func TestMachineLoadKnownAndUnknown(t *testing.T) {
	read := LoadReader{OS: "linux", NumCPU: func() int { return 4 }, ReadFile: func(string) ([]byte, error) { return []byte("2.00 1.00 0.50 1/4 100"), nil }}
	sample := MachineLoad(read)
	if !sample.Known || sample.CPUs != 4 || sample.Ratio != 0.5 || sample.String() != "0.50" {
		t.Fatalf("sample %+v", sample)
	}
	read.OS = "windows"
	if sample := MachineLoad(read); sample.Known || sample.String() != "unknown" {
		t.Fatalf("unsupported host %+v", sample)
	}
	read.OS = "linux"
	read.ReadFile = func(string) ([]byte, error) { return []byte("invalid"), nil }
	if MachineLoad(read).Known {
		t.Fatal("invalid load was reported as known")
	}
}
