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

// helpCompletion is one command line after `putnami help`: the words before
// the cursor, the word under it, the words completion must offer there and
// the words it must not offer.
type helpCompletion struct {
	words []string
	want  []string
	not   []string
}

// help, or an alias of it, reads the command it describes from its next two
// words, and the CLI refuses a flag before either of them. Completion follows:
// after help it offers the command names, after `help <command>` the
// subcommands of that command, after a flag before those words nothing, and
// never a project target, which help does not take.
func TestCompletionAfterHelpOffersTheCommandItDescribes(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "flags-follow-the-command",
		"completion-after-help-offers-the-command-it-describes")
	groups := copyGroupDefaultPositionalsFixture(t)
	jobs := writeExtensionWorkspace(t, "lint-docs")
	targets := []string{"/apps/web", "/apps/list", "/flow", "flow"}
	nothing := []string{}
	for _, ws := range []struct {
		root    string
		aliases map[string]string
		cases   []helpCompletion
	}{
		{groups, nil, []helpCompletion{
			{[]string{"help", ""}, []string{"stage", "notes", "projects", "help"}, targets},
			{[]string{"help", "stage", ""}, []string{"list", "up"}, targets},
			{[]string{"help", "projects", ""}, []string{"list", "describe"}, targets},
			{[]string{"help", "-v", ""}, nothing, append([]string{"stage", "projects", "--output"}, targets...)},
			{[]string{"help", "stage", "-v", ""}, nothing, append([]string{"list", "up", "--output"}, targets...)},
			{[]string{"help", "projects", "-v", ""}, nothing, append([]string{"list", "describe", "--output"}, targets...)},
			{[]string{"help", "stage", "up", ""}, nothing, append([]string{"--output"}, targets...)},
			{[]string{"help", "--output", ""}, []string{"json"}, targets},
			{[]string{"help", "--output", "json", ""}, nothing, append([]string{"json", "stage"}, targets...)},
			{[]string{"help", "--projects", ""}, nothing, targets},
			{[]string{"help", "projects", "--"}, []string{"--man", "--markdown", "--output"}, nil},
			{[]string{"help", "pro"}, []string{"projects"}, targets},
			{[]string{"help", "stage", "u"}, []string{"up"}, []string{"upgrade"}},
		}},
		{jobs, map[string]string{"h": "help"}, []helpCompletion{
			{[]string{"help", ""}, []string{"lint-docs", "projects"}, targets},
			{[]string{"help", "lint-docs", ""}, nothing, targets},
			{[]string{"h", ""}, []string{"lint-docs", "projects"}, targets},
			{[]string{"h", "projects", ""}, []string{"list", "describe"}, targets},
			{[]string{"h", "-v", ""}, nothing, append([]string{"projects", "--output"}, targets...)},
			{[]string{"h", "projects", "-v", ""}, nothing, append([]string{"list", "--output"}, targets...)},
			// A job command still takes a project target after a flag value.
			{[]string{"lint-docs", "--output", "json", ""}, []string{"/flow"}, []string{"json"}},
		}},
	} {
		cfg := &wsproto.Config{Aliases: ws.aliases}
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
				assertHelpCompletion(t, "bash", line, offered, c)
			}
			if _, err := exec.LookPath("zsh"); err == nil {
				offered := zshCompletionOffers(t, t.TempDir(), zsh.String(), "args", c.words)
				assertHelpCompletion(t, "zsh", line, offered, c)
			}
			if _, err := exec.LookPath("fish"); err == nil {
				assertHelpCompletion(t, "fish", line, fishCompletionOffers(t, fishScript, line), c)
			}
		}
	}
}

// assertHelpCompletion checks the words a shell offers at line against c. An
// empty want means the shell offers no word at all.
func assertHelpCompletion(t *testing.T, shell, line string, offered []string, c helpCompletion) {
	t.Helper()
	offered = slices.DeleteFunc(offered, func(word string) bool { return word == "" })
	if c.want != nil && len(c.want) == 0 && len(offered) > 0 {
		t.Errorf("%s offers %q at %q, want nothing", shell, offered, line)
	}
	assertOffersEach(t, shell, line, offered, c.want)
	for _, word := range c.not {
		if slices.Contains(offered, word) {
			t.Errorf("%s offers %q at %q; it offers %q", shell, word, line, offered)
		}
	}
}

// fishCompletionOffers runs fish on the script at scriptPath to complete line
// and returns each candidate.
func fishCompletionOffers(t *testing.T, scriptPath, line string) []string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("fish", "--no-config", "-c", `source $argv[1]
for line in (complete --escape -C $argv[2])
    set -l candidate (string split -m1 \t -- $line)[1]
    printf '%s\0' (string unescape -- $candidate | string collect)
end`, scriptPath, line)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fish completion of %q: %v\n%s", line, err, out)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}
