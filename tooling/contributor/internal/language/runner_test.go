package language

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if !ExcludedLanguagePath("tooling/contributor/internal/language/evaluate.go") ||
		!ExcludedLanguagePath(`tooling\contributor\internal\language\evaluate.go`) {
		t.Fatal("the native detector's signal vocabulary was scanned as public prose")
	}
	for _, path := range []string{"tooling/contributor/internal/language/runner.go", "somewhere/else/language.go"} {
		if ExcludedLanguagePath(path) {
			t.Errorf("unrelated source %s was excluded", path)
		}
	}
}

func TestTaskLanguageFollowsPagesThroughProvider(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "change-language-parity", "the-language-check-pages-tasks-through-the-call-seam")
	var out, errors bytes.Buffer
	calls := 0
	runner := LanguageRunner{Root: t.TempDir(), Out: &out, Err: &errors, Call: func(_ context.Context, contract, operation string, request json.RawMessage) (collab.Envelope, error) {
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
		return collab.Envelope{Outcome: collab.OutcomeOK, Result: data}, nil
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

func TestLanguageCommitsNeverRunsConfiguredVerifier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the verifier sentinel is a POSIX executable")
	}
	t.Parallel()
	root := t.TempDir()
	command := func(input []byte, args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Stdin = bytes.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return output
	}
	command(nil, "init", "-q")
	command(nil, "config", "user.name", "Test")
	command(nil, "config", "user.email", "test@example.invalid")
	command(nil, "config", "commit.gpgsign", "false")
	command(nil, "commit", "-q", "--allow-empty", "-m", "base")
	base := strings.TrimSpace(string(command(nil, "rev-parse", "HEAD")))
	command(nil, "commit", "-q", "--allow-empty", "-m", "feat: English title", "-m", "Body kept exactly")
	raw := command(nil, "cat-file", "commit", "HEAD")
	parts := bytes.SplitN(raw, []byte("\n\n"), 2)
	if len(parts) != 2 {
		t.Fatal("commit object has no body boundary")
	}
	signed := append(bytes.Clone(parts[0]), []byte("\ngpgsig -----BEGIN PGP SIGNATURE-----\n fake-signature\n -----END PGP SIGNATURE-----\n\n")...)
	signed = append(signed, parts[1]...)
	sha := strings.TrimSpace(string(command(signed, "hash-object", "-t", "commit", "-w", "--stdin")))
	command(nil, "update-ref", "HEAD", sha)
	verifier := filepath.Join(root, "signature-verifier")
	marker := verifier + ".invoked"
	if err := os.WriteFile(verifier, []byte("#!/bin/sh\nprintf invoked > \"$0.invoked\"\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	command(nil, "config", "log.showSignature", "true")
	command(nil, "config", "gpg.format", "openpgp")
	command(nil, "config", "gpg.program", verifier)
	probe := exec.Command("git", "-C", root, "log", "-1", "--format=%B", sha)
	_, _ = probe.CombinedOutput()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fixture did not invoke configured verifier: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	runner := LanguageRunner{Root: root, Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	message, err := runner.git("log", "-1", "--format=%B", sha)
	if err != nil || string(message) != "feat: English title\n\nBody kept exactly\n\n" {
		t.Fatalf("commit body changed: %q, err=%v", message, err)
	}
	if status := runner.RunLanguage(context.Background(), "commits", []string{base + "..HEAD"}); status != 0 {
		t.Fatalf("language scan status=%d", status)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("read invoked the configured verifier: %v", err)
	}
}

// TestLanguageTextReadsTheFileTheResolverNames pins that the text mode opens
// the resolved path, and that without a resolver it opens the argument.
func TestLanguageTextReadsTheFileTheResolverNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("Plain English text.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolve := func(p string) string { return filepath.Join(dir, p) }

	var out, errOut bytes.Buffer
	runner := LanguageRunner{Out: &out, Err: &errOut, ResolvePath: resolve}
	if code := runner.RunLanguage(context.Background(), "text", []string{"notes.md"}); code != 0 {
		t.Fatalf("exit %d, stderr %q; want the resolved file read", code, errOut.String())
	}

	errOut.Reset()
	runner = LanguageRunner{Out: &out, Err: &errOut}
	if code := runner.RunLanguage(context.Background(), "text", []string{filepath.Join(dir, "notes.md")}); code != 0 {
		t.Fatalf("exit %d, stderr %q; want the absolute argument read without a resolver", code, errOut.String())
	}
}
