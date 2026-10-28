package completion

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// hostileNames are workspace names that a shell reads as code, or as several
// words, or as another word, unless the script quotes them for every read. A
// name that runs code creates a file whose name starts with "pwned-" in the
// shell's working directory. "y:z\w" holds the colon and the backslash that
// zsh's _describe reads, and "r[s]t" the brackets that zsh's _values reads.
// "l\nm" holds a newline, which splits a line of compgen replies.
var hostileNames = []string{
	"a$(touch pwned-a)",
	"b(touch pwned-b)",
	"c;touch pwned-c",
	"d'e",
	"p $l",
	"g`touch pwned-g`",
	"x';touch pwned-x;'",
	`h"$(touch pwned-h)"`,
	`y:z\w`,
	"r[s]t",
	"l\nm",
}

// hostileWorkspace is a workspace whose command aliases, project aliases,
// groups, project names, project directories and the flags of "lint-docs" are
// the hostileNames, with a prefix per kind. A project directory gives the
// project its ID and its basename. The command aliases of "lint-docs" are job
// commands; those of "projects" are core commands. A project alias also starts
// with "~", which bash reads as a home directory.
type hostileWorkspace struct {
	root     string
	cfg      *wsproto.Config
	commands []string
	targets  []string
	flags    []string
}

func writeHostileWorkspace(t *testing.T) hostileWorkspace {
	t.Helper()
	root := writeExtensionWorkspace(t, "lint-docs")
	ws := hostileWorkspace{root: root, cfg: &wsproto.Config{Aliases: map[string]string{}}}
	projectAliases := map[string]string{}
	groups := map[string]string{}
	includes := []string{"flow"}
	flags := map[string]extension.FlagDefinition{}
	for i, name := range hostileNames {
		flags["f"+name] = extension.FlagDefinition{Type: "boolean", Description: "Flag."}
		ws.flags = append(ws.flags, "--f"+name)
		ws.cfg.Aliases["j"+name] = "lint-docs"
		ws.cfg.Aliases["k"+name] = "projects"
		ws.commands = append(ws.commands, "j"+name, "k"+name)
		projectAliases["~"+name] = "/flow"
		groups["gr"+name] = "/flow"
		// An include is a glob, which reads a bracket.
		dir := "q" + strings.NewReplacer(`\`, "", "[", "", "]", "").Replace(name)
		if runtime.GOOS == "windows" {
			// Windows refuses a quote or a colon in a file name.
			dir = "q" + strconv.Itoa(i)
		}
		includes = append(includes, dir)
		project, err := json.Marshal(map[string]string{"name": "n" + name})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "putnami.json"), project, 0o644); err != nil {
			t.Fatal(err)
		}
		ws.targets = append(ws.targets, "~"+name, "gr"+name, "n"+name, "/"+dir, dir)
	}
	config, err := json.Marshal(struct {
		Name           string            `json:"name"`
		Includes       []string          `json:"includes"`
		ProjectAliases map[string]string `json:"projectAliases"`
		Groups         map[string]string `json:"groups"`
	}{"hostile-ws", includes, projectAliases, groups})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	flagsJSON, err := json.Marshal(flags)
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"name": "@acme/flow", "cliContract": 4, "commands": {"lint-docs": {"description": "Run.", "flags": ` +
		string(flagsJSON) + `, "run": [{"id": "r", "task": "t"}]}}, "tasks": {"t": {"kind": "command", "command": "echo", "cache": false}}}`
	if err := os.WriteFile(filepath.Join(root, "flow", "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := gatherCompletionContext(root, ws.cfg, nil)
	for _, target := range ws.targets {
		if !slices.Contains(ctx.projects, target) && !slices.Contains(ctx.projectIDs, target) &&
			!slices.Contains(ctx.projectBasenames, target) &&
			!slices.ContainsFunc(ctx.aliases, func(a completionValue) bool { return a.Value == target }) {
			t.Fatalf("the workspace does not load the target %q", target)
		}
	}
	for _, flag := range ws.flags {
		if !containsStructuredFlag(ctx.structuredFlags["lint-docs"], flag) {
			t.Fatalf("the workspace does not load the flag %q", flag)
		}
	}
	return ws
}

// assertNoCodeRan fails when a shell ran a command that a hostile name holds.
func assertNoCodeRan(t *testing.T, shell, dir string) {
	t.Helper()
	ran, err := filepath.Glob(filepath.Join(dir, "pwned-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range ran {
		t.Errorf("%s ran the code of a name: it created %s", shell, filepath.Base(file))
	}
}

// assertOffersEach fails unless every name is one of the offered words.
func assertOffersEach(t *testing.T, shell, position string, offered, names []string) {
	t.Helper()
	for _, name := range names {
		if !slices.Contains(offered, name) {
			t.Errorf("%s does not offer %q as one word at %s; it offers %q", shell, name, position, offered)
		}
	}
}

// The bash script holds each name as one word of an array, and offers it
// quoted, so the command line reads it back as the same word.
func TestBashCompletionReadsEveryNameAsOneLiteralWord(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable on this platform")
	}
	ws := writeHostileWorkspace(t)
	var script bytes.Buffer
	CompletionBash(&script, ws.root, ws.cfg, nil)

	for _, c := range []struct {
		words []string
		want  []string
	}{
		{[]string{"putnami", ""}, ws.commands},
		{[]string{"putnami", "lint-docs", ""}, ws.targets},
		{[]string{"putnami", "lint-docs", "--projects", ""}, ws.targets},
		{[]string{"putnami", "lint-docs", "--"}, ws.flags},
		{[]string{"putnami", ws.commands[0], "--"}, []string{"--projects"}},
		{[]string{"putnami", ws.commands[1], "--"}, []string{"--projects"}},
	} {
		dir := t.TempDir()
		replies := bashCompletionReplies(t, dir, script.String(), c.words)
		assertNoCodeRan(t, "bash", dir)
		assertOffersEach(t, "bash", strings.Join(c.words, " "), bashReadBack(t, replies), c.want)
	}
}

// bashCompletionReplies runs the bash script in dir to complete words and
// returns COMPREPLY.
func bashCompletionReplies(t *testing.T, dir, script string, words []string) []string {
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
printf '%s\0' "${COMPREPLY[@]}"
`
	cmd := exec.Command("bash", "-c", driver)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash completion of %q: %v\n%s", words, err, stderr.String())
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// bashReadBack returns the words bash reads from each reply once the reply is
// on the command line. A reply that bash reads as several words reads back as
// none of them.
func bashReadBack(t *testing.T, replies []string) []string {
	t.Helper()
	dir := t.TempDir()
	driver := `for reply in "$@"; do
    if eval "set -- ${reply}" 2>/dev/null && [ $# -eq 1 ]; then printf '%s\0' "$1"; else printf '\0'; fi
done
`
	cmd := exec.Command("bash", append([]string{"-c", driver, "bash"}, replies...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash reads back %q: %v\n%s", replies, err, out)
	}
	assertNoCodeRan(t, "a bash command line that holds a reply", dir)
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// The zsh script keeps each name one word in its arrays, and escapes the colon
// that _describe reads as the start of a description.
func TestZshCompletionReadsEveryNameAsOneLiteralWord(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh unavailable on this platform")
	}
	ws := writeHostileWorkspace(t)
	var script bytes.Buffer
	CompletionZsh(&script, ws.root, ws.cfg, nil)

	for _, c := range []struct {
		state string
		words []string
		want  []string
	}{
		{"command", []string{""}, ws.commands},
		{"args", []string{"lint-docs", ""}, ws.targets},
		{"args", []string{"lint-docs", "--projects", ""}, ws.targets},
		{"args", []string{"lint-docs", "--"}, ws.flags},
		{"args", []string{ws.commands[0], "--"}, []string{"--projects"}},
		{"args", []string{ws.commands[1], "--"}, []string{"--projects"}},
	} {
		dir := t.TempDir()
		offered := zshCompletionOffers(t, dir, script.String(), c.state, c.words)
		assertNoCodeRan(t, "zsh", dir)
		assertOffersEach(t, "zsh", strings.Join(c.words, " "), offered, c.want)
	}
}

// zshCompletionOffers runs the zsh script in dir with the completion system's
// functions replaced by recorders, and returns the words zsh would offer:
// the name of each _describe entry, each compadd word and the name of each
// _values spec.
func zshCompletionOffers(t *testing.T, dir, script, state string, words []string) []string {
	t.Helper()
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = bashQuote(word)
	}
	driver := `_arguments() { state=` + state + `; return 0; }
_describe() { local entry; for entry in "${(@P)4}"; do print -rn -- "describe:${entry}"; print -n '\0'; done; }
compadd() { local word; for word in "$@"; do print -rn -- "word:${word}"; print -n '\0'; done; }
_values() { shift; local spec; for spec in "$@"; do print -rn -- "value:${spec}"; print -n '\0'; done; }
words=(` + strings.Join(quoted, " ") + `)
CURRENT=` + strconv.Itoa(len(words)) + `
` + script
	cmd := exec.Command("zsh", "-f", "-c", driver)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("zsh completion of %q: %v\n%s", words, err, stderr.String())
	}
	var offered []string
	for _, record := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		kind, value, _ := strings.Cut(record, ":")
		switch kind {
		case "describe":
			value = zshReadName(value, ":")
		case "value":
			value = zshReadName(value, "[]:")
		}
		offered = append(offered, value)
	}
	return offered
}

// zshReadName returns the name zsh reads from one _describe entry
// ("name:description", stops ":") or _values spec ("name[description]:...",
// stops "[]:"): up to the first stop that no backslash escapes, with the
// escapes removed.
func zshReadName(entry, stops string) string {
	var name strings.Builder
	for i := 0; i < len(entry); i++ {
		switch {
		case entry[i] == '\\' && i+1 < len(entry):
			i++
			name.WriteByte(entry[i])
		case strings.IndexByte(stops, entry[i]) >= 0:
			return name.String()
		default:
			name.WriteByte(entry[i])
		}
	}
	return name.String()
}

// fishRead returns the words fish reads from s, and false when fish would
// expand any part of s: a variable, a command substitution, a glob, a brace or
// a separator. It accepts the two forms the generator writes: plain words and
// single-quoted strings, where \' and \\ are the only escapes.
func fishRead(s string) ([]string, bool) {
	var words []string
	var word strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			closed := false
			for i++; i < len(s); i++ {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '\'' || s[i+1] == '\\') {
					i++
					word.WriteByte(s[i])
					continue
				}
				if s[i] == '\'' {
					closed = true
					break
				}
				word.WriteByte(s[i])
			}
			if !closed {
				return nil, false
			}
		case strings.IndexByte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./@:,=+-", c) >= 0:
			inWord = true
			word.WriteByte(c)
		default:
			return nil, false
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, true
}

// fishLines splits a fish script into the lines fish reads: at each newline
// outside a single-quoted string, where \' and \\ are the only escapes.
func fishLines(script string) []string {
	var lines []string
	start, quoted := 0, false
	for i := 0; i < len(script); i++ {
		switch {
		case quoted && script[i] == '\\' && i+1 < len(script) && (script[i+1] == '\'' || script[i+1] == '\\'):
			i++
		case script[i] == '\'':
			quoted = !quoted
		case !quoted && script[i] == '\n':
			lines = append(lines, script[start:i])
			start = i + 1
		}
	}
	return append(lines, script[start:])
}

// fishCompletions returns, for each `complete` line of a fish script, the
// words fish reads from its -n condition and from its -a arguments when it
// completes: fish reads both strings a second time.
func fishCompletions(t *testing.T, script string) (conditions, arguments [][]string) {
	t.Helper()
	for _, line := range fishLines(script) {
		if !strings.HasPrefix(line, "complete ") {
			continue
		}
		words, ok := fishRead(line)
		if !ok {
			t.Errorf("fish reads part of this line as code: %s", line)
			continue
		}
		for i := 0; i+1 < len(words); i++ {
			if words[i] != "-n" && words[i] != "-a" {
				continue
			}
			read, ok := fishRead(words[i+1])
			if !ok {
				t.Errorf("fish reads part of %s %q as code when it completes: %s", words[i], words[i+1], line)
				continue
			}
			if words[i] == "-n" {
				conditions = append(conditions, read)
			} else {
				arguments = append(arguments, read)
			}
		}
	}
	return conditions, arguments
}

// The fish script quotes each name twice where fish reads it twice: in the
// candidates of -a and in the condition of -n. This reads the generated text
// as fish does, so it runs where fish is not installed.
func TestFishCompletionTextQuotesEveryNameForBothReads(t *testing.T) {
	ws := writeHostileWorkspace(t)
	var script bytes.Buffer
	CompletionFish(&script, ws.root, ws.cfg, nil)
	conditions, arguments := fishCompletions(t, script.String())

	var candidates []string
	for _, words := range arguments {
		candidates = append(candidates, words...)
	}
	assertOffersEach(t, "fish", "an -a candidate", candidates, append(slices.Clone(ws.commands), ws.targets...))

	// The positional targets follow a job command, an alias of a job command
	// included: the condition names each one as a literal word.
	jobCommandCondition := false
	for _, words := range conditions {
		if len(words) > 0 && words[0] == "__putnami_seen_from" && slices.Contains(words, "lint-docs") {
			jobCommandCondition = true
			assertOffersEach(t, "fish", "a job-command condition", words[1:], []string{"j" + hostileNames[0], "j" + hostileNames[3]})
		}
	}
	if !jobCommandCondition {
		t.Error("fish names no job command in a condition")
	}
}

// Fish itself completes each name as one word and runs none of them.
func TestFishCompletionReadsEveryNameAsOneLiteralWord(t *testing.T) {
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish unavailable on this platform")
	}
	ws := writeHostileWorkspace(t)
	var script bytes.Buffer
	CompletionFish(&script, ws.root, ws.cfg, nil)
	scriptPath := filepath.Join(t.TempDir(), "putnami.fish")
	if err := os.WriteFile(scriptPath, script.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		line string
		want []string
	}{
		{"putnami ", ws.commands},
		{"putnami lint-docs ", ws.targets},
		{"putnami lint-docs --projects ", ws.targets},
		{"putnami lint-docs --", ws.flags},
		{"putnami " + fishQuote(ws.commands[0]) + " --", []string{"--projects"}},
		{"putnami " + fishQuote(ws.commands[1]) + " --", []string{"--projects"}},
	} {
		dir := t.TempDir()
		// --escape prints each candidate on one line, a newline included;
		// string unescape reads it back.
		cmd := exec.Command("fish", "--no-config", "-c", `source $argv[1]
for line in (complete --escape -C $argv[2])
    set -l candidate (string split -m1 \t -- $line)[1]
    printf '%s\0' (string unescape -- $candidate | string collect)
end`, scriptPath, c.line)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("fish completion of %q: %v\n%s", c.line, err, out)
		}
		assertNoCodeRan(t, "fish", dir)
		offered := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		assertOffersEach(t, "fish", c.line, offered, c.want)
	}
}
