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
	// shells limits the case to these shells; empty means every shell.
	shells []string
}

// writeValueFlagWorkspace writes a workspace whose @acme/flow extension declares
// the job command lint-docs with the value flags --mode (short -m) and --level
// (choices, short written without its dash), and a command group whose subcommand flag lists choices.
func writeValueFlagWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"putnami.workspace.json": `{"name": "value-flag-ws", "includes": ["flow"]}`,
		"flow/putnami.json":      `{"name": "@acme/flow"}`,
		"flow/putnami.extension.json": `{"name": "@acme/flow", "cliContract": 4, "commands": {"flow-run": {"description": "Run.", "run": [{"id": "r", "task": "t"}]}, "lint-docs": {"description": "Run.", "flags": {"mode": {"type": "string", "short": "-m", "description": "Mode"}, "level": {"type": "string", "short": "l", "choices": ["low", "high"], "description": "Level"}, "strict": {"type": "boolean", "description": "Strict"}}, "run": [{"id": "r", "task": "t"}]}}, ` +
			`"commandGroups": {"flowg": {"description": "Group.", "subcommands": {"run": {"description": "Run.", "command": "flow-run", "flags": {"kind": {"type": "string", "choices": ["a", "b"], "description": "Kind"}}}}}}, ` +
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

	// A job-only flag on a core command is refused, so it offers nothing.
	for _, words := range [][]string{
		{"doctor", "--max-parallel", ""},
		{"doctor", "--exclude", ""},
		{"help", "--max-parallel", ""},
	} {
		jobCases = append(jobCases, flagValueCompletion{words: words, want: nothing, exact: true})
	}

	own := writeValueFlagWorkspace(t)
	formats := []string{"text", "json", "jsonl", "cloud-logging"}
	ownCases := []flagValueCompletion{
		// An extension flag whose value is free offers nothing, and one that
		// lists choices offers them, whether it is a job flag or a group flag,
		// and whether it is typed long or short.
		{words: []string{"lint-docs", "--mode", ""}, want: nothing, exact: true},
		{words: []string{"lint-docs", "-m", ""}, want: nothing, exact: true},
		{words: []string{"lint-docs", "--level", ""}, want: []string{"low", "high"}, exact: true},
		{words: []string{"lint-docs", "-l", ""}, want: []string{"low", "high"}, exact: true},
		{words: []string{"flowg", "run", "--kind", ""}, want: []string{"a", "b"}, exact: true},
		{words: []string{"lint-docs", "--strict", ""}, want: []string{"/flow"}, not: []string{"--mode"}},
		// A core command's flag offers the values the catalog lists for it, or
		// nothing, over a global flag of the same name. An alias, and the init
		// spelling, take the values of their command.
		{words: []string{"workspace", "init", "--extension", ""}, want: []string{"ts", "go", "py"}, exact: true},
		{words: []string{"init", "--extension", ""}, want: []string{"ts", "go", "py"}, exact: true},
		{words: []string{"wi", "--extension", ""}, want: []string{"ts", "go", "py"}, exact: true},
		{words: []string{"workspace", "init", "--workspace", ""}, want: nothing, exact: true},
		{words: []string{"upgrade", "--channel", ""}, want: []string{"stable", "canary"}, exact: true},
		{words: []string{"up", "--channel", ""}, want: []string{"stable", "canary"}, exact: true},
		{words: []string{"cache", "verify", "--exclude", ""}, want: nothing, exact: true},
		{words: []string{"cache", "verify", "--profile", ""}, want: []string{"dev", "test", "production"}, exact: true},
		{words: []string{"cache", "verify", "--output", ""}, want: formats, exact: true},
		// bash splits a word at "=": the value follows the words --flag and "=".
		{words: []string{"lint-docs", "--output", "=", ""}, want: formats, exact: true, shells: []string{"bash"}},
		{words: []string{"lint-docs", "--projects", "=", ""}, want: []string{"/flow", "@acme/flow", "flow"}, exact: true, shells: []string{"bash"}},
		{words: []string{"lint-docs", "--mode", "=", ""}, want: nothing, exact: true, shells: []string{"bash"}},
		// fish completes the value of --flag=value in one word.
		{words: []string{"lint-docs", "--output="}, want: []string{"--output=text", "--output=json", "--output=jsonl", "--output=cloud-logging"}, exact: true, shells: []string{"fish"}},
		// A value slot comes before a flag in bash and zsh: a value may start
		// with a dash. fish lists the options natively after a dash.
		{words: []string{"lint-docs", "--serve-env", "-"}, want: []string{"-"}, exact: true, shells: []string{"bash", "zsh"}},
		{words: []string{"lint-docs", "--mode", "-"}, want: nothing, exact: true, shells: []string{"bash", "zsh"}},
	}

	for _, ws := range []struct {
		root    string
		aliases map[string]string
		cases   []flagValueCompletion
	}{{jobs, nil, jobCases}, {own, map[string]string{"wi": "workspace init", "up": "upgrade"}, ownCases}} {
		cfg := &wsproto.Config{Aliases: ws.aliases}
		var bash, zsh, fish bytes.Buffer
		CompletionBash(&bash, ws.root, cfg, nil)
		CompletionZsh(&zsh, ws.root, cfg, nil)
		CompletionFish(&fish, ws.root, cfg, nil)
		fishScript := filepath.Join(t.TempDir(), "putnami.fish")
		if err := os.WriteFile(fishScript, fish.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		offers := map[string]func(t *testing.T, c flagValueCompletion) []string{
			"bash": func(t *testing.T, c flagValueCompletion) []string {
				replies := bashCompletionReplies(t, t.TempDir(), bash.String(), append([]string{"putnami"}, c.words...))
				return bashReadBack(t, nonEmpty(replies))
			},
			"zsh": func(t *testing.T, c flagValueCompletion) []string {
				return zshCompletionOffers(t, t.TempDir(), zsh.String(), "args", c.words)
			},
			"fish": func(t *testing.T, c flagValueCompletion) []string {
				return fishCompletionOffers(t, fishScript, "putnami "+strings.Join(c.words, " "))
			},
		}
		for _, shell := range []string{"bash", "zsh", "fish"} {
			t.Run(shell, func(t *testing.T) {
				if _, err := exec.LookPath(shell); err != nil {
					t.Skipf("%s unavailable on this platform", shell)
				}
				for _, c := range ws.cases {
					if len(c.shells) > 0 && !slices.Contains(c.shells, shell) {
						continue
					}
					assertFlagValueCompletion(t, shell, "putnami "+strings.Join(c.words, " "), offers[shell](t, c), c)
				}
			})
		}
	}
}

// Fish completes the files of the current directory after a flag whose value
// is a path, and no project target; after any other flag it offers no file.
func TestFishCompletionOffersFilesAfterAPathFlag(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "completion-offers-a-flag-value-in-its-slot",
		"completion-offers-only-the-values-of-the-flag-before-the-cursor")
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish unavailable on this platform")
	}
	jobs := writeExtensionWorkspace(t, "lint-docs")
	var fish bytes.Buffer
	CompletionFish(&fish, jobs, &wsproto.Config{}, nil)
	fishScript := filepath.Join(t.TempDir(), "putnami.fish")
	if err := os.WriteFile(fishScript, fish.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "trace.json"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []flagValueCompletion{
		{words: []string{"lint-docs", "--trace-profile", ""}, want: []string{"trace.json"}, exact: true},
		{words: []string{"cache", "verify", "--trace-profile", ""}, want: []string{"trace.json"}, exact: true},
		{words: []string{"projects", "create", "--path", ""}, want: []string{"trace.json"}, exact: true},
		{words: []string{"extensions", "install", "--dest", ""}, want: []string{"trace.json"}, exact: true},
		{words: []string{"lint-docs", "--baseline", ""}, want: nothing(), exact: true},
		{words: []string{"cache", "verify", "--retry", ""}, want: nothing(), exact: true},
	} {
		line := "putnami " + strings.Join(c.words, " ")
		assertFlagValueCompletion(t, "fish", line, fishCompletionOffersIn(t, fishScript, line, cwd), c)
	}
}

func nothing() []string { return []string{} }

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
