package completion

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// flagValueCompletion is one command line that ends in a flag's value slot or
// right after it: the words before the cursor, then either exactly the words
// completion must offer or, when exact is false, words it must and must not
// offer.
type flagValueCompletion struct {
	words []string
	// want is the set of words the shell offers. With exact, it is the whole set:
	// an empty want means the shell offers nothing.
	want  []string
	exact bool
	// not lists words the shell must not offer; it applies without exact.
	not []string
}

// writeValueFlagWorkspace writes a workspace whose @acme/flow extension declares
// the job command lint-docs with a flag --mode that takes a value.
func writeValueFlagWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"putnami.workspace.json": `{"name": "value-flag-ws", "includes": ["flow"]}`,
		"flow/putnami.json":      `{"name": "@acme/flow"}`,
		"flow/putnami.extension.json": `{"name": "@acme/flow", "cliContract": 4, "commands": {"lint-docs": {"description": "Run.", "flags": {"mode": {"type": "string", "description": "Mode"}, "strict": {"type": "boolean", "description": "Strict"}}, "run": [{"id": "r", "task": "t"}]}}, ` +
			`"tasks": {"t": {"kind": "command", "command": "echo", "cache": false}}}`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// After a flag that takes a value, global or the command's own, completion
// offers that flag's values and nothing else: the project targets after
// --projects, the project names after --exclude, the values the catalog or the
// extension lists for the flag, and no word after a flag whose value is free.
// Help takes no project target. The word after the value completes as the word
// before the flag.
func TestCompletionOffersOnlyTheValuesOfTheFlagBeforeTheCursor(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "completion-offers-a-flag-value-in-its-slot",
		"completion-offers-only-the-values-of-the-flag-before-the-cursor")
	jobs := writeExtensionWorkspace(t, "lint-docs")
	targets := []string{"/flow", "@acme/flow", "flow"}
	projectNames := []string{"@acme/flow"}
	nothing := []string{}

	jobCases := []flagValueCompletion{
		{words: []string{"lint-docs", "--projects", ""}, want: targets, exact: true},
		{words: []string{"lint-docs", "--exclude", ""}, want: projectNames, exact: true},
		// The word after a value completes as the word before the flag.
		{words: []string{"lint-docs", "--output", "json", ""}, want: []string{"/flow"}, not: []string{"json"}},
		{words: []string{"help", "--projects", ""}, want: nothing, exact: true},
		{words: []string{"help", "--output", ""}, want: []string{"json"}, not: targets},
	}
	// Every other global flag that takes a value offers its own values, or
	// nothing, and never a project target or a flag.
	ctx := gatherCompletionContext(jobs, &wsproto.Config{}, nil)
	valueFlags := ctx.valueGlobalFlags()
	if len(valueFlags) < 10 {
		t.Fatalf("the workspace offers %d value flags besides --projects and --exclude, want the whole catalog", len(valueFlags))
	}
	for _, flag := range valueFlags {
		want := flag.Values
		if want == nil {
			want = nothing
		}
		jobCases = append(jobCases, flagValueCompletion{words: []string{"lint-docs", flag.Long, ""}, want: want, exact: true})
	}

	own := writeValueFlagWorkspace(t)
	ownCases := []flagValueCompletion{
		// An extension flag whose value is free offers nothing.
		{words: []string{"lint-docs", "--mode", ""}, want: nothing, exact: true},
		{words: []string{"lint-docs", "--strict", ""}, want: []string{"/flow"}, not: []string{"--mode"}},
		// A core command's flag offers the values the catalog lists for it, or
		// nothing, over a global flag of the same name.
		{words: []string{"workspace", "init", "--extension", ""}, want: []string{"ts", "go", "py"}, exact: true},
		{words: []string{"workspace", "init", "--workspace", ""}, want: nothing, exact: true},
		{words: []string{"upgrade", "--channel", ""}, want: []string{"stable", "canary"}, exact: true},
		{words: []string{"cache", "verify", "--exclude", ""}, want: nothing, exact: true},
		{words: []string{"cache", "verify", "--profile", ""}, want: []string{"dev", "test", "production"}, exact: true},
		{words: []string{"cache", "verify", "--output", ""}, want: []string{"text", "json", "jsonl", "cloud-logging"}, exact: true},
	}

	for _, ws := range []struct {
		root  string
		cases []flagValueCompletion
	}{{jobs, jobCases}, {own, ownCases}} {
		cfg := &wsproto.Config{}
		var bash, zsh, fish bytes.Buffer
		CompletionBash(&bash, ws.root, cfg, nil)
		CompletionZsh(&zsh, ws.root, cfg, nil)
		CompletionFish(&fish, ws.root, cfg, nil)
		fishScript := filepath.Join(t.TempDir(), "putnami.fish")
		if err := os.WriteFile(fishScript, fish.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, c := range ws.cases {
			line := "putnami " + strings.Join(c.words, " ")
			if _, err := exec.LookPath("bash"); err == nil {
				offered := bashCompletionReplies(t, t.TempDir(), bash.String(), append([]string{"putnami"}, c.words...))
				assertFlagValueCompletion(t, "bash", line, bashReadBack(t, nonEmpty(offered)), c)
			}
			if _, err := exec.LookPath("zsh"); err == nil {
				offered := zshCompletionOffers(t, t.TempDir(), zsh.String(), "args", c.words)
				assertFlagValueCompletion(t, "zsh", line, offered, c)
			}
			if _, err := exec.LookPath("fish"); err == nil {
				assertFlagValueCompletion(t, "fish", line, fishCompletionOffers(t, fishScript, line), c)
			}
		}
	}
}

func nonEmpty(words []string) []string {
	return slices.DeleteFunc(slices.Clone(words), func(word string) bool { return word == "" })
}

// assertFlagValueCompletion checks the words a shell offers at line against c.
func assertFlagValueCompletion(t *testing.T, shell, line string, offered []string, c flagValueCompletion) {
	t.Helper()
	offered = nonEmpty(offered)
	if c.exact {
		got := slices.Compact(slices.Sorted(slices.Values(offered)))
		want := slices.Compact(slices.Sorted(slices.Values(c.want)))
		if !slices.Equal(got, want) {
			t.Errorf("%s offers %q at %q, want exactly %q", shell, got, line, want)
		}
		return
	}
	assertOffersEach(t, shell, line, offered, c.want)
	for _, word := range c.not {
		if slices.Contains(offered, word) {
			t.Errorf("%s offers %q at %q; it offers %q", shell, word, line, offered)
		}
	}
}
