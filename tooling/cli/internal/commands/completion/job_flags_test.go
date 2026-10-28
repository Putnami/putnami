package completion

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// writeExtensionWorkspace writes a workspace whose @acme/flow extension
// declares the given commands, each running one cache-free command task.
func writeExtensionWorkspace(t *testing.T, commands ...string) string {
	t.Helper()
	return writeFlowWorkspace(t, commands, "")
}

// writeGroupWorkspace writes a workspace whose @acme/flow extension declares
// only the command group name, which carries the one command name-run: it
// declares no job command a user types.
func writeGroupWorkspace(t *testing.T, name string) string {
	t.Helper()
	groups := `, "commandGroups": {"` + name + `": {"description": "Group.", "subcommands": {"run": {"description": "Run.", "command": "` + name + `-run"}}}}`
	return writeFlowWorkspace(t, []string{name + "-run"}, groups)
}

func writeFlowWorkspace(t *testing.T, commands []string, groups string) string {
	t.Helper()
	dir := t.TempDir()
	declared := make([]string, 0, len(commands))
	for _, name := range commands {
		declared = append(declared, `"`+name+`": {"description": "Run.", "run": [{"id": "r", "task": "t"}]}`)
	}
	files := map[string]string{
		"putnami.workspace.json":      `{"name": "job-flags-ws", "includes": ["flow"]}`,
		"flow/putnami.json":           `{"name": "@acme/flow"}`,
		"flow/putnami.extension.json": `{"name": "@acme/flow", "cliContract": 4, "commands": {` + strings.Join(declared, ", ") + `}, "tasks": {"t": {"kind": "command", "command": "echo", "cache": false}}` + groups + `}`,
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

// jobOnlyFlagNames returns the flags that only extension commands read.
func jobOnlyFlagNames() []string {
	var names []string
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.JobOnly {
			names = append(names, flag.Long)
		}
	}
	return names
}

// hiddenJobFlags returns the global flags that only extension commands read
// and that no core command declares as its own flag.
func hiddenJobFlags() []string {
	own := map[string]bool{}
	for _, command := range commandmeta.Commands() {
		for _, flag := range command.Flags {
			own[flag.Long] = true
		}
	}
	var names []string
	for _, flag := range jobOnlyFlagNames() {
		if !own[flag] {
			names = append(names, flag)
		}
	}
	return names
}

// scriptNamesFlag reports whether a script names flag as a whole word, in its
// "--name" form or in fish's "-l 'name'" form: "--exclude" is not named by
// "--exclude-tag".
func scriptNamesFlag(script, flag string) bool {
	name := regexp.QuoteMeta(strings.TrimPrefix(flag, "--"))
	return regexp.MustCompile(`(^|[^A-Za-z0-9-])(--|-l '?)` + name + `($|[^A-Za-z0-9-])`).MatchString(script)
}

func TestCompletionOffersTheJobFlagsOnlyWithAJobCommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "job-flags-follow-job-commands", "help-and-completion-hide-the-job-flags-where-no-job-command-is-loaded")
	jobFlags := jobOnlyFlagNames()
	if len(jobFlags) == 0 {
		t.Fatal("the catalog marks no flag that only extension commands read")
	}

	withJob := writeExtensionWorkspace(t, "lint-docs")
	cases := []struct {
		name    string
		wsRoot  string
		offered bool
	}{
		{"outside a workspace", "", false},
		{"a workspace with no extension", writeCompletionWorkspace(t), false},
		{"an extension that declares only a provider command", writeExtensionWorkspace(t, cache.ProviderCommandName), false},
		{"an extension that declares a command group that a core root shadows", writeGroupWorkspace(t, "projects"), false},
		{"an extension that declares a job command", withJob, true},
		{"an extension that declares only a command group", writeGroupWorkspace(t, "ship"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := gatherCompletionContext(c.wsRoot, &wsproto.Config{}, nil)
			names := ctx.globalFlagNames()
			for _, flag := range commandmeta.GlobalFlags() {
				if !flag.JobOnly {
					continue
				}
				// A provider-bound flag also needs its provider, which none of
				// these workspaces declares.
				want := c.offered && len(flag.Providers) == 0
				if got := sharedtest.Contains(names, flag.Long); got != want {
					t.Errorf("global flags offer %s = %v, want %v", flag.Long, got, want)
				}
			}
			// What the core reads stays, with the flag hosted launchers read from help.
			for _, flag := range []string{"--credential-fd", "--no-cache", "--projects", "--output", "--help", "--version"} {
				if !sharedtest.Contains(names, flag) {
					t.Errorf("global flags miss %s, which no job command is needed to read", flag)
				}
			}

			for shell, generate := range map[string]func(*bytes.Buffer){
				"bash": func(b *bytes.Buffer) { CompletionBash(b, c.wsRoot, &wsproto.Config{}, nil) },
				"zsh":  func(b *bytes.Buffer) { CompletionZsh(b, c.wsRoot, &wsproto.Config{}, nil) },
				"fish": func(b *bytes.Buffer) { CompletionFish(b, c.wsRoot, &wsproto.Config{}, nil) },
			} {
				var script bytes.Buffer
				generate(&script)
				if got := strings.Contains(script.String(), "max-parallel"); got != c.offered {
					t.Errorf("%s script offers --max-parallel = %v, want %v", shell, got, c.offered)
				}
				if c.offered {
					continue
				}
				// Not even a value branch names a flag the script hides.
				for _, flag := range hiddenJobFlags() {
					if scriptNamesFlag(script.String(), flag) {
						t.Errorf("%s script names %s, which it hides", shell, flag)
					}
				}
			}
		})
	}
}

func TestCompletionOffersAProviderFlagOnlyWithItsProvider(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "job-flags-follow-job-commands", "the-references-and-completion-list-the-flags-help-lists")
	cases := []struct {
		name    string
		wsRoot  string
		offered bool
	}{
		{"outside a workspace", "", false},
		{"a workspace with no extension", writeCompletionWorkspace(t), false},
		{"an extension that declares a job command", writeExtensionWorkspace(t, "lint-docs"), false},
		{"an extension that declares the credential provider", writeExtensionWorkspace(t, "lint-docs", registry.CredentialProviderCommand), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			names := gatherCompletionContext(c.wsRoot, &wsproto.Config{}, nil).globalFlagNames()
			if got := sharedtest.Contains(names, "--providers"); got != c.offered {
				t.Errorf("global flags offer --providers = %v, want %v", got, c.offered)
			}
		})
	}
}

// A core command takes a job-only flag only as one of its own flags, so
// completion offers `--dry-run` after `projects sync`, which declares it, and
// after a job command, but not after `projects list`.
func TestCompletionOffersAJobFlagOnlyAfterACommandThatTakesIt(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "job-flags-follow-job-commands", "a-core-command-refuses-a-job-flag-it-does-not-read")
	wsRoot := writeExtensionWorkspace(t, "lint-docs")

	for _, command := range [][]string{{"projects", "list"}, {"cache", "clean"}} {
		path := strings.Join(command, " ")
		for _, flag := range jobOnlyFlagNames() {
			if !commandmeta.RefusesGlobalFlag(path, flag) {
				t.Fatalf("the catalog lets %s take %s: pick another command for this test", path, flag)
			}
		}
	}

	// An alias that resolves to a core command, directly or through another
	// alias, is a core command; an alias of a job command is not.
	cfg := &wsproto.Config{Aliases: map[string]string{"pl": "projects", "pl2": "pl", "jl": "lint-docs", "p $l;x": "projects"}}
	var bash, zsh, fish bytes.Buffer
	CompletionBash(&bash, wsRoot, cfg, nil)
	CompletionZsh(&zsh, wsRoot, cfg, nil)
	CompletionFish(&fish, wsRoot, cfg, nil)

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable on this platform")
	}
	offers := func(words ...string) string {
		return " " + runBashCompletion(t, bash.String(), append([]string{"putnami"}, append(words, "--")...)) + " "
	}
	for _, flag := range []string{"--dry-run", "--watch", "--serve-env", "--max-parallel"} {
		if got := offers("projects", "list"); strings.Contains(got, " "+flag+" ") {
			t.Errorf("bash offers %s after `projects list`: %s", flag, got)
		}
		if got := offers("lint-docs"); !strings.Contains(got, " "+flag+" ") {
			t.Errorf("bash does not offer %s after a job command: %s", flag, got)
		}
	}
	for _, alias := range []string{"pl", "pl2"} {
		if got := offers(alias); strings.Contains(got, " --dry-run ") || strings.Contains(got, " --watch ") {
			t.Errorf("bash offers a job-only flag after the alias %s of a core command: %s", alias, got)
		}
	}
	if got := offers("jl"); !strings.Contains(got, " --dry-run ") {
		t.Errorf("bash does not offer --dry-run after the alias jl of a job command: %s", got)
	}
	if got := offers("projects", "sync"); !strings.Contains(got, " --dry-run ") {
		t.Errorf("bash does not offer --dry-run after `projects sync`, which declares it: %s", got)
	}
	if got := offers("projects", "list"); !strings.Contains(got, " --output ") {
		t.Errorf("bash does not offer --output after `projects list`: %s", got)
	}

	if !strings.Contains(zsh.String(), "job_global_flags=(\n        '--watch[") ||
		!strings.Contains(zsh.String(), `if ! _putnami_core_root "${path}"; then`) {
		t.Error("zsh does not keep the job-only flags apart from the core commands")
	}
	for name, script := range map[string]string{"bash": bash.String(), "zsh": zsh.String()} {
		for _, alias := range []string{"pl", "pl2"} {
			if !strings.Contains(script, "'"+alias+"') return 0 ;;") {
				t.Errorf("%s does not treat the alias %s as a core command", name, alias)
			}
		}
	}
	if !regexp.MustCompile(`contains -- \$tokens\[2\] .* 'pl' 'pl2' `).MatchString(fish.String()) {
		t.Error("fish does not treat the aliases of a core command as core commands")
	}
	// An alias name is workspace input: fish reads it as one quoted word.
	if !strings.Contains(fish.String(), ` 'p $l;x'`) {
		t.Error("fish does not quote an alias name in its core-command check")
	}
	// Fish reads an -a string a second time, so the name is quoted twice.
	if !strings.Contains(fish.String(), `-a '\'p $l;x\'' -d `) {
		t.Error("fish does not quote an alias name it completes as a command for both reads")
	}
	if !strings.Contains(fish.String(), "complete -c putnami -n '__putnami_job_flag' -l 'dry-run'") {
		t.Error("fish offers --dry-run after a core command")
	}
}

// The CLI refuses a command line whose first word is a flag, so completion
// offers nothing after it: `putnami -v projects list --` used to read -v as
// the command and offer the job-only flags `projects list` refuses.
func TestCompletionOffersNothingAfterALeadingFlag(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "flags-follow-the-command", "completion-offers-nothing-after-a-leading-flag")
	wsRoot := writeExtensionWorkspace(t, "lint-docs")
	var bash, zsh, fish bytes.Buffer
	CompletionBash(&bash, wsRoot, &wsproto.Config{}, nil)
	CompletionZsh(&zsh, wsRoot, &wsproto.Config{}, nil)
	CompletionFish(&fish, wsRoot, &wsproto.Config{}, nil)

	if !strings.Contains(zsh.String(), "if [[ \"${path}\" == -* ]]; then\n                return 1") {
		t.Error("zsh completes after a leading flag")
	}
	if !strings.Contains(fish.String(), "function __putnami_leading_flag") {
		t.Error("fish declares no leading-flag guard")
	}
	// Each condition is a function that fails after a leading flag: a guard,
	// or a path check, since no path starts with a flag.
	guarded := regexp.MustCompile(`^complete -c putnami -n '(__putnami_use_command|__putnami_no_leading_flag|__putnami_job_flag|__putnami_seen_from|__putnami_job_target|__putnami_help_word|__putnami_target_flag|__putnami_complete_children|__putnami_seen_path)[ ']`)
	for _, line := range strings.Split(fish.String(), "\n") {
		if strings.HasPrefix(line, "complete -c putnami ") && line != "complete -c putnami -f" && !guarded.MatchString(line) {
			t.Errorf("fish completes after a leading flag: %s", line)
		}
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable on this platform")
	}
	for _, words := range [][]string{
		{"putnami", "-v", "projects", "list", "--"},
		{"putnami", "--projects", "web", "projects", "list", "--"},
		{"putnami", "-v", ""},
		{"putnami", "-v", "lint-docs", ""},
	} {
		if got := runBashCompletion(t, bash.String(), words); got != "" {
			t.Errorf("bash offers %q after %v", got, words)
		}
	}
	if got := runBashCompletion(t, bash.String(), []string{"putnami", "projects", "list", "--"}); !strings.Contains(" "+got+" ", " --output ") {
		t.Errorf("bash does not offer --output after `projects list`: %s", got)
	}
}
