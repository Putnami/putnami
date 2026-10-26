package completion

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// groupDefaultPositionalsFixture is a workspace with the projects /apps/web and
// /apps/list and an extension at contract 15: `stage` defaults to `up`, whose
// first positional is project and which declares --keep; `notes` defaults to
// `add`, whose first positional is text.
const groupDefaultPositionalsFixture = "testdata/group-default-positionals"

func copyGroupDefaultPositionalsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(groupDefaultPositionalsFixture)); err != nil {
		t.Fatal(err)
	}
	return dir
}

// scriptFunction returns the body of the shell function name in script.
func scriptFunction(t *testing.T, script, name string) string {
	t.Helper()
	_, body, ok := strings.Cut(script, name+"() {\n")
	if !ok {
		t.Fatalf("the script declares no %s function", name)
	}
	body, _, _ = strings.Cut(body, "\n}\n")
	return body
}

// The word after a group whose default takes a project positional completes
// the subcommands and the project IDs, in every shell; the group word offers
// the default's flags. A default whose first positional is not project
// completes no project.
func TestCompletionScripts_GroupWordCompletesProjectIDs(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-group-default-takes-positionals",
		"completion-offers-project-ids-after-the-group-word")
	dir := copyGroupDefaultPositionalsFixture(t)
	cfg := &wsproto.Config{}

	ctx := gatherCompletionContext(dir, cfg, nil)
	if !ctx.projectPositionals["stage"] || ctx.projectPositionals["notes"] {
		t.Fatalf("projectPositionals = %v, want stage alone", ctx.projectPositionals)
	}
	if flags := ctx.structuredFlags["stage"]; len(flags) != 1 || flags[0].Long != "--keep" {
		t.Errorf("stage flags = %+v, want the default's --keep", flags)
	}

	var bash bytes.Buffer
	CompletionBash(&bash, dir, cfg, nil)
	takes := scriptFunction(t, bash.String(), "_putnami_takes_project")
	if !strings.Contains(takes, "'stage') return 0 ;;") || strings.Contains(takes, "'notes')") {
		t.Errorf("bash _putnami_takes_project = %q, want stage alone", takes)
	}

	var zsh bytes.Buffer
	CompletionZsh(&zsh, dir, cfg, nil)
	zshOut := zsh.String()
	takes = scriptFunction(t, zshOut, "_putnami_takes_project")
	if !strings.Contains(takes, "'stage') return 0 ;;") || strings.Contains(takes, "'notes')") {
		t.Errorf("zsh _putnami_takes_project = %q, want stage alone", takes)
	}
	if !strings.Contains(zshOut, "_describe -t subcmds 'subcommand' nested_subcmds\n"+
		"                if _putnami_takes_project \"${path}\"; then\n"+
		"                    _describe -t project-ids 'project ID' project_ids") {
		t.Error("zsh does not offer the project IDs beside the subcommands")
	}

	var fish bytes.Buffer
	CompletionFish(&fish, dir, cfg, nil)
	fishOut := fish.String()
	if !strings.Contains(fishOut, "complete -c putnami -n '__putnami_complete_children stage' -a '/apps/web' -d 'project'") {
		t.Error("fish does not offer /apps/web after the stage group")
	}
	if strings.Contains(fishOut, "__putnami_complete_children notes' -a '/apps/web'") {
		t.Error("fish offers a project after notes, whose default takes text")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable on this platform")
	}
	for _, tc := range []struct {
		words []string
		want  []string
		not   []string
	}{
		{[]string{"putnami", "stage", ""}, []string{"list", "up", "/apps/web", "/apps/list"}, nil},
		{[]string{"putnami", "stage", "/apps/w"}, []string{"/apps/web"}, []string{"/apps/list"}},
		{[]string{"putnami", "notes", ""}, []string{"add"}, []string{"/apps/web"}},
	} {
		got := runBashCompletion(t, bash.String(), tc.words)
		for _, want := range tc.want {
			if !strings.Contains(" "+got+" ", " "+want+" ") {
				t.Errorf("%v completes %q, want %q among them", tc.words, got, want)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(" "+got+" ", " "+not+" ") {
				t.Errorf("%v completes %q, want no %q", tc.words, got, not)
			}
		}
	}
}

// runBashCompletion sources script in bash with the bash-completion helper it
// calls stubbed, completes words, and returns the candidates.
func runBashCompletion(t *testing.T, script string, words []string) string {
	t.Helper()
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = bashQuote(word)
	}
	driver := `complete() { :; }
_init_completion() {
    words=("${COMP_WORDS[@]}"); cword=${COMP_CWORD}
    cur="${COMP_WORDS[COMP_CWORD]}"; prev="${COMP_WORDS[COMP_CWORD-1]}"
}
` + script + `
COMP_WORDS=(` + strings.Join(quoted, " ") + `)
COMP_CWORD=` + strconv.Itoa(len(words)-1) + `
_putnami_completions
printf '%s ' "${COMPREPLY[@]}"
`
	out, err := exec.Command("bash", "-c", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("bash completion: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}
