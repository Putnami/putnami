package contributor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestLanguageFileExclusions(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"src/a_test.go", "src/a.test.ts", "fixtures/sample.md", "testdata/file.md"} {
		if !ExcludedLanguagePath(path) {
			t.Errorf("fixture %s was not excluded", path)
		}
	}
	if !ExcludedLanguagePath("tooling/cli/internal/commands/shared/language.go") ||
		!ExcludedLanguagePath(`tooling\cli\internal\commands\shared\language.go`) {
		t.Fatal("the native detector's signal vocabulary was scanned as public prose")
	}
	for _, path := range []string{"tooling/cli/internal/commands/contributor/language.go", "somewhere/else/language.go"} {
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

func TestLanguageRangesRejectGitOptionsBeforeSpawn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := exec.Command("git", "init", "-q", root).Run(); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "injected-output")
	for _, mode := range []string{"files", "commits"} {
		var errors bytes.Buffer
		runner := LanguageRunner{Root: root, Out: &bytes.Buffer{}, Err: &errors}
		args := []string{"--output=" + output}
		if mode == "files" {
			args = append([]string{"--diff"}, args...)
		}
		if status := runner.RunLanguage(context.Background(), mode, args); status != 2 || !strings.Contains(errors.String(), "option-like") {
			t.Fatalf("%s accepted Git option-like range: status=%d diagnostic=%q", mode, status, errors.String())
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("%s wrote through a revision range: stat=%v", mode, err)
		}
	}
	for _, mode := range []string{"files", "commits"} {
		args := []string{" "}
		if mode == "files" {
			args = append([]string{"--diff"}, args...)
		}
		var errors bytes.Buffer
		if status := (LanguageRunner{Root: root, Out: &bytes.Buffer{}, Err: &errors}).RunLanguage(context.Background(), mode, args); status != 2 || !strings.Contains(errors.String(), "option-like") {
			t.Fatalf("%s accepted empty range: status=%d diagnostic=%q", mode, status, errors.String())
		}
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
	read = LoadReader{OS: "darwin", NumCPU: func() int { return 4 }, Command: func(name string, args ...string) ([]byte, error) {
		if name != "/usr/sbin/sysctl" || len(args) != 2 || args[0] != "-n" || args[1] != "vm.loadavg" {
			t.Fatalf("load reader used PATH-selected or unexpected command: %q %v", name, args)
		}
		return []byte("{ 2.00 1.00 0.50 }"), nil
	}}
	if sample := MachineLoad(read); !sample.Known || sample.Ratio != 0.5 {
		t.Fatalf("absolute Darwin load probe failed: %+v", sample)
	}
	for _, platform := range []string{"freebsd", "openbsd", "netbsd"} {
		read.OS = platform
		if sample := MachineLoad(read); sample.Known {
			t.Fatalf("unverified %s path was treated as trustworthy: %+v", platform, sample)
		}
	}
}
