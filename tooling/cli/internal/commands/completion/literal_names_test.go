package completion

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// hostileNames are workspace names that a shell reads as code, or as several
// words, or as another word, unless the script quotes them for every read. A
// name that runs code creates a file whose name starts with "pwned-" in the
// shell's working directory. "y:z\w" holds the colon and the backslash that
// zsh's _describe and _values read, and "r[s]t" the brackets that _values
// reads. "l\nm" holds a newline.
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
// the hostileNames, with a prefix per kind. A flag's description is its name
// followed by a backslash. A project directory gives the
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
		flags["f"+name] = extension.FlagDefinition{Type: "boolean", Description: name + `\`}
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
	spectest.Proves(t, "cli/job-planning-execution", "completion-reads-names-literally", "bash-offers-each-name-as-one-literal-word")
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
		// The current word as the command line holds it: quoted, escaped.
		{[]string{"putnami", `jp\ \$`}, []string{"jp $l"}},
		{[]string{"putnami", "'jp"}, []string{"jp $l"}},
		{[]string{"putnami", "lint-docs", `nd\'`}, []string{"nd'e"}},
	} {
		dir := t.TempDir()
		replies := bashCompletionReplies(t, dir, script.String(), c.words)
		assertNoCodeRan(t, "bash", dir)
		assertOffersEach(t, "bash", strings.Join(c.words, " "), bashReadBack(t, replies), c.want)
	}
}

// bashCompletionReplies runs the bash script in dir, with set -u, to complete
// words and returns COMPREPLY. It fills COMP_WORDS as bash 4 and later do,
// which split a typed word at each colon that no quote or backslash protects,
// and joins the pieces back the way bash-completion's _init_completion -n :
// does.
func bashCompletionReplies(t *testing.T, dir, script string, words []string) []string {
	t.Helper()
	var compWords, glue []string
	cword := 0
	for i, word := range words {
		pieces := bashColonPieces(word)
		if i == len(words)-1 {
			cword = len(compWords) + len(pieces) - 1
		}
		for j, piece := range pieces {
			compWords = append(compWords, bashQuote(piece))
			glue = append(glue, strconv.Itoa(min(j, 1)))
		}
	}
	driver := `set -u
complete() { :; }
_init_completion() {
    local i n=-1 exclude=""
    while [[ $# -gt 0 ]]; do
        if [[ "$1" == -n ]]; then exclude+="${2-}"; shift; fi
        shift
    done
    words=()
    for ((i = 0; i < ${#COMP_WORDS[@]}; i++)); do
        if [[ "${exclude}" == *:* && ${GLUE[i]} -eq 1 ]]; then
            words[n]+="${COMP_WORDS[i]}"
        else
            n=$((n + 1))
            words[n]="${COMP_WORDS[i]}"
        fi
        if [[ ${i} -eq ${COMP_CWORD} ]]; then cword=${n}; fi
    done
    cur="${words[cword]}"; prev="${words[cword-1]}"
}
` + script + `
COMP_WORDS=(` + strings.Join(compWords, " ") + `)
GLUE=(` + strings.Join(glue, " ") + `)
COMP_CWORD=` + strconv.Itoa(cword) + `
_putnami_completions
printf '%s\0' ${COMPREPLY[@]+"${COMPREPLY[@]}"}
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

// bashColonPieces splits a typed word as bash 4 and later split a completion
// word: before and after each run of colons that no quote or backslash
// protects. The run is a piece of its own.
func bashColonPieces(word string) []string {
	var pieces []string
	start := 0
	quote := "" // the opening quote: ', ", $' or none
	for i := 0; i < len(word); i++ {
		c := word[i]
		switch {
		case quote == "'":
			if c == '\'' {
				quote = ""
			}
		case c == '\\':
			i++
		case quote != "":
			if c == quote[len(quote)-1] {
				quote = ""
			}
		case c == '\'' || c == '"':
			quote = string(c)
		case strings.HasPrefix(word[i:], "$'"):
			quote = "$'"
			i++
		case c == ':':
			end := i
			for end < len(word) && word[end] == ':' {
				end++
			}
			if i > start {
				pieces = append(pieces, word[start:i])
			}
			pieces = append(pieces, word[i:end])
			start = end
			i = end - 1
		}
	}
	if start < len(word) || len(pieces) == 0 {
		pieces = append(pieces, word[start:])
	}
	return pieces
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

// The zsh script keeps each name one word in its arrays, escapes the colon
// that _describe reads as the start of a description, and escapes what _values
// reads in a flag spec. The test records the specs _values receives and reads
// them by the rules of zsh's parser; it does not run _values.
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
	// A completion function returns 1 when it offers nothing.
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1 && stderr.Len() == 0) {
		t.Fatalf("zsh completion of %q: %v\n%s", words, err, stderr.String())
	}
	var offered []string
	for _, record := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		kind, value, _ := strings.Cut(record, ":")
		switch kind {
		case "describe":
			value = zshReadName(value, ":")
		case "value":
			value = zshValuesName(value)
		}
		offered = append(offered, value)
	}
	return offered
}

// Bash and zsh hand completion each word as the user typed it, quotes and
// backslashes included. Each script reads an earlier word as the shell reads
// it, so a command alias typed with quotes takes the flags of its command: the
// extension flags for a job command, and no job-only flag for a core command.
// In bash, a word is also typed as printf %q writes it in the C locale, which
// is how a completed reply reaches the command line: $'...' with a letter,
// octal or hex escape for a byte that is not printable ASCII.
func TestCompletionReadsAQuotedCommandAliasAsItsName(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "completion-reads-names-literally", "a-quoted-command-alias-takes-its-command-flags")
	ws := writeHostileWorkspace(t)
	jobAliases := make([]string, 0, len(hostileNames)+3)
	coreAliases := make([]string, 0, len(hostileNames))
	for _, name := range hostileNames {
		jobAliases = append(jobAliases, "j"+name)
		coreAliases = append(coreAliases, "k"+name)
	}
	// "jd\u00e9ploy" reads back from octal escapes, "jc\rr" and "je\x1be" from
	// letter escapes.
	for _, name := range []string{"jd\u00e9ploy", "jc\rr", "je\x1be"} {
		ws.cfg.Aliases[name] = "lint-docs"
		jobAliases = append(jobAliases, name)
	}
	jobOnly := hiddenJobFlags()
	shells := []struct {
		name   string
		forms  func(t *testing.T, name string) []string
		offers func(t *testing.T, dir string, words []string) []string
	}{
		{"bash", func(t *testing.T, name string) []string {
			return append(typedForms(name), bashPrintfQ(t, name))
		}, func(t *testing.T, dir string, words []string) []string {
			var script bytes.Buffer
			CompletionBash(&script, ws.root, ws.cfg, nil)
			return bashReadBack(t, bashCompletionReplies(t, dir, script.String(), append([]string{"putnami"}, words...)))
		}},
		{"zsh", func(t *testing.T, name string) []string {
			return typedForms(name)
		}, func(t *testing.T, dir string, words []string) []string {
			var script bytes.Buffer
			CompletionZsh(&script, ws.root, ws.cfg, nil)
			return zshCompletionOffers(t, dir, script.String(), "args", words)
		}},
	}
	for _, shell := range shells {
		t.Run(shell.name, func(t *testing.T) {
			if _, err := exec.LookPath(shell.name); err != nil {
				t.Skip(shell.name + " unavailable on this platform")
			}
			for _, alias := range jobAliases {
				for _, typed := range shell.forms(t, alias) {
					dir := t.TempDir()
					offered := shell.offers(t, dir, []string{typed, "--"})
					assertNoCodeRan(t, shell.name, dir)
					assertOffersEach(t, shell.name, typed+" --", offered, append([]string{"--projects"}, ws.flags...))
				}
			}
			for _, alias := range coreAliases {
				for _, typed := range shell.forms(t, alias) {
					dir := t.TempDir()
					offered := shell.offers(t, dir, []string{typed, "--"})
					assertNoCodeRan(t, shell.name, dir)
					assertOffersEach(t, shell.name, typed+" --", offered, []string{"--projects"})
					for _, flag := range jobOnly {
						if slices.Contains(offered, flag) {
							t.Errorf("%s offers the job-only flag %s after the core alias %s", shell.name, flag, typed)
						}
					}
				}
			}
			dir := t.TempDir()
			offered := shell.offers(t, dir, []string{"lint-docs", "'--projects'", ""})
			assertOffersEach(t, shell.name, "lint-docs '--projects'", offered, ws.targets)
		})
	}
}

// bashPrintfQ returns name as bash's printf %q writes it in the C locale.
func bashPrintfQ(t *testing.T, name string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", `LC_ALL=C; printf '%q' "$1"`, "bash", name)
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash printf %%q of %q: %v", name, err)
	}
	return string(out)
}

// Bash 4 and later split a completion word at each colon that no quote or
// backslash protects. The script joins the pieces back, so a command alias
// that holds a colon takes the flags of its command, and a current word that
// holds one completes in place: the command line replaces only the text after
// that colon, and the reply holds only that text.
func TestBashCompletionKeepsAColonInsideItsWord(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "completion-keeps-a-colon-in-its-word", "bash-reads-a-colon-word-as-one-word")
	// The pieces GNU bash 5.3 puts in COMP_WORDS for each typed word.
	for typed, want := range map[string][]string{
		"build:prod": {"build", ":", "prod"},
		"bu:":        {"bu", ":"},
		"'a:b'":      {"'a:b'"},
		`"a:b"`:      {`"a:b"`},
		`a\:b`:       {`a\:b`},
		"":           {""},
	} {
		if got := bashColonPieces(typed); !slices.Equal(got, want) {
			t.Errorf("bash splits %q into %q, not %q", typed, want, got)
		}
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable on this platform")
	}
	ws := writeHostileWorkspace(t)
	ws.cfg.Aliases["build:prod"] = "lint-docs"
	ws.cfg.Aliases["core:list"] = "projects"
	var script bytes.Buffer
	CompletionBash(&script, ws.root, ws.cfg, nil)
	jobOnly := hiddenJobFlags()

	for _, c := range []struct {
		alias string
		want  []string
		job   bool
	}{
		{"build:prod", append([]string{"--projects"}, ws.flags...), true},
		{"jy:z\\w", append([]string{"--projects"}, ws.flags...), true},
		{"core:list", []string{"--projects"}, false},
		{"ky:z\\w", []string{"--projects"}, false},
	} {
		for _, typed := range append(typedForms(c.alias), bashPrintfQ(t, c.alias)) {
			dir := t.TempDir()
			offered := bashReadBack(t, bashCompletionReplies(t, dir, script.String(), []string{"putnami", typed, "--"}))
			assertOffersEach(t, "bash", typed+" --", offered, c.want)
			if c.job {
				continue
			}
			for _, flag := range jobOnly {
				if slices.Contains(offered, flag) {
					t.Errorf("bash offers the job-only flag %s after the core alias %s", flag, typed)
				}
			}
		}
	}

	// The current word ends after its colon, or one character later.
	for _, c := range []struct {
		before []string
		name   string
	}{
		{nil, "build:prod"},
		{nil, "jy:z\\w"},
		{[]string{"lint-docs"}, "~y:z\\w"},
		{[]string{"lint-docs"}, "--fy:z\\w"},
	} {
		escaped := typedForms(c.name)[2]
		colon := strings.LastIndex(escaped, ":") + 1
		for _, typed := range []string{escaped[:colon], escaped[:colon+1]} {
			words := append(append([]string{"putnami"}, c.before...), typed)
			var lines []string
			for _, reply := range bashCompletionReplies(t, t.TempDir(), script.String(), words) {
				lines = append(lines, typed[:colon]+reply)
			}
			assertOffersEach(t, "bash", strings.Join(words, " "), bashReadBack(t, lines), []string{c.name})
		}
		// A colon that a quote or a backslash protects splits nothing, so the
		// reply replaces the whole word. The script reads a current word that
		// starts with a quote as no flag, so a flag is typed escaped only.
		prefix := typedForms(c.name[:strings.LastIndex(c.name, ":")+2])
		protected := []string{strings.ReplaceAll(prefix[2], ":", `\:`)}
		if !strings.HasPrefix(c.name, "-") {
			protected = append(protected, prefix[0], prefix[1])
		}
		for _, typed := range protected {
			words := append(append([]string{"putnami"}, c.before...), typed)
			replies := bashCompletionReplies(t, t.TempDir(), script.String(), words)
			assertOffersEach(t, "bash", strings.Join(words, " "), bashReadBack(t, replies), []string{c.name})
		}
	}
}

// typedForms returns the ways a user types name as one word: single-quoted,
// double-quoted, and with a backslash before each character the shell reads,
// a newline written $'\n'.
func typedForms(name string) []string {
	var escaped strings.Builder
	for _, c := range name {
		switch {
		case c == '\n':
			escaped.WriteString(`$'\n'`)
		case !plainWord(string(c)):
			escaped.WriteRune('\\')
			escaped.WriteRune(c)
		default:
			escaped.WriteRune(c)
		}
	}
	doubled := `"` + strings.NewReplacer(`\`, `\\`, `$`, `\$`, "`", "\\`", `"`, `\"`).Replace(name) + `"`
	return []string{bashQuote(name), doubled, escaped.String()}
}

// Each _describe entry and array word reads back, by the rules of zsh's
// _describe, as the name. This reads the generated text, so it runs where zsh
// is not installed.
func TestZshDescribeEntriesReadBackEveryNameLiterally(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "completion-reads-names-literally", "zsh-describe-entries-read-each-name-literally")
	for _, name := range hostileNames {
		for _, word := range []string{zshDescribeEntry(name, "Description."), zshDescribeNames([]string{name})} {
			if got := zshReadName(shellUnquote(word), ":"); got != name {
				t.Errorf("_describe reads the word %s as %q, not %q", word, got, name)
			}
		}
	}
}

// shellUnquote returns the value of one word that bashWord or bashQuote
// writes: a bare word, or a single-quoted word whose apostrophes bashQuote
// escapes.
func shellUnquote(word string) string {
	if !strings.HasPrefix(word, "'") {
		return word
	}
	return strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(word, "'"), "'"), `'\''`, "'")
}

// Each extension flag spec reads back, by the rules of zsh's _values parser, as
// the flag's name, and its description ends where the spec says. This reads
// the generated text, so it runs where zsh is not installed.
func TestZshFlagSpecReadsBackEveryNameLiterally(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "completion-reads-names-literally", "zsh-flag-specs-read-each-name-literally")
	for _, name := range hostileNames {
		flag := "--f" + name
		for _, valueName := range []string{"", "<value>"} {
			spec := zshFlagSpec(flag, name+`\`, valueName, nil)
			if got := zshValuesName(spec); got != flag {
				t.Errorf("_values reads the spec %q as %q, not %q", spec, got, flag)
			}
		}
	}
}

// zshValuesName returns the name _values reads from one spec
// ("name[description]:..."): up to the first "[" or ":" that no backslash
// escapes, with the escapes removed. A description runs to the first "]" that
// no backslash escapes; _values refuses a spec whose description has no end,
// and so does this.
func zshValuesName(spec string) string {
	name := zshReadName(spec, "[:")
	rest := spec[len(zshEscapedPrefix(spec, "[:")):]
	if !strings.HasPrefix(rest, "[") {
		return name
	}
	description := rest[1:]
	if len(zshEscapedPrefix(description, "]")) == len(description) {
		return "unterminated description: " + spec
	}
	return name
}

// zshEscapedPrefix returns s up to its first stop that no backslash escapes.
func zshEscapedPrefix(s, stops string) string {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i++
		case strings.IndexByte(stops, s[i]) >= 0:
			return s[:i]
		}
	}
	return s
}

// zshReadName returns the name zsh reads from one _describe entry
// ("name:description", stops ":") or _values spec (stops "[:"): up to the
// first stop that no backslash escapes, with the escapes removed.
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
// completes, since fish reads both strings a second time, and the flag name
// of each -l, which fish reads once.
func fishCompletions(t *testing.T, script string) (conditions, arguments [][]string, longs []string) {
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
			if words[i] == "-l" {
				longs = append(longs, words[i+1])
				continue
			}
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
	return conditions, arguments, longs
}

// The fish script quotes each name twice where fish reads it twice: in the
// candidates of -a and in the condition of -n. It quotes each -l flag name,
// which fish reads once, and names an alias as one word in the condition of
// its flags. This reads the generated text as fish does, so it runs where fish
// is not installed.
func TestFishCompletionTextQuotesEveryNameForBothReads(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "completion-reads-names-literally", "fish-quotes-each-name-for-both-reads")
	ws := writeHostileWorkspace(t)
	var script bytes.Buffer
	CompletionFish(&script, ws.root, ws.cfg, nil)
	conditions, arguments, longs := fishCompletions(t, script.String())

	var candidates []string
	for _, words := range arguments {
		candidates = append(candidates, words...)
	}
	assertOffersEach(t, "fish", "an -a candidate", candidates, append(slices.Clone(ws.commands), ws.targets...))
	var flagNames []string
	for _, flag := range ws.flags {
		flagNames = append(flagNames, strings.TrimPrefix(flag, "--"))
	}
	assertOffersEach(t, "fish", "an -l flag name", longs, flagNames)

	// The flags of lint-docs follow each of its aliases, named as one word.
	var flagConditions, lintDocsAliases []string
	for _, words := range conditions {
		if len(words) == 2 && words[0] == "__putnami_seen_path" {
			flagConditions = append(flagConditions, words[1])
		}
	}
	for _, name := range hostileNames {
		lintDocsAliases = append(lintDocsAliases, "j"+name)
	}
	assertOffersEach(t, "fish", "a flag condition", flagConditions, lintDocsAliases)

	// The positional targets follow a job command, an alias of a job command
	// included: the condition names each one as a literal word.
	jobCommandCondition := false
	for _, words := range conditions {
		if len(words) > 0 && words[0] == "__putnami_job_target" && slices.Contains(words, "lint-docs") {
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
		{"putnami " + fishQuote("j"+hostileNames[4]) + " --", ws.flags},
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
