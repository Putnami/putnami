package completion

import (
	"bytes"
	"io"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// Shell completion is a projection of the command
// catalog. These tests pin the properties that makes true, none of which the
// goldens can state: the projection is TOTAL (no catalog path is silently
// dropped), it is the ONLY wording (descriptions are catalog Summaries, not a
// third phrasing), it does not MUTATE the surface it reads from, and the prose
// it embeds survives shell quoting.

// completionGenerators are the three scripts, rendered with no workspace so only
// the catalog-derived half is exercised.
var completionGenerators = map[string]func(io.Writer, string, *wsproto.Config){
	"bash": CompletionBash,
	"zsh":  CompletionZsh,
	"fish": CompletionFish,
}

func renderCompletion(t *testing.T, shell string) string {
	t.Helper()
	generate, ok := completionGenerators[shell]
	if !ok {
		t.Fatalf("no %s generator", shell)
	}
	var buf bytes.Buffer
	generate(&buf, "", &wsproto.Config{})
	return buf.String()
}

// TestCompletionProjectsEveryCatalogPath is the assertion the hand-maintained
// lists could not make. Before A1b, eight registered commands and eleven
// documented subcommands were missing from every shell because nothing compared
// the two.
func TestCompletionProjectsEveryCatalogPath(t *testing.T) {
	ctx := gatherCompletionContext("", &wsproto.Config{})

	offered := map[string]bool{}
	for _, name := range ctx.structured {
		offered[name] = true
	}
	for root, subs := range ctx.subcommands {
		for _, sub := range subs {
			offered[root+" "+sub] = true
		}
	}

	for _, command := range commandmeta.StructuredCommands() {
		if !offered[command.Path] {
			t.Errorf("completion never offers %q, which the catalog declares", command.Path)
		}
	}
	for _, root := range commandmeta.StructuredRoots() {
		if got := ctx.structuredDesc[root.Path]; got != root.Summary {
			t.Errorf("completion describes %q as %q, but the catalog Summary is %q — "+
				"a second wording is exactly what A1b removed", root.Path, got, root.Summary)
		}
	}
}

// TestCompletionOffersEveryGlobalFlag pins that all three shells offer the whole
// global-flag vocabulary. --cache-trust, --profile, --json, --color, --no-color,
// and --trace-profile were absent from every shell before A1b because each
// generator carried its own list.
func TestCompletionOffersEveryGlobalFlag(t *testing.T) {
	for shell := range completionGenerators {
		script := renderCompletion(t, shell)
		for _, flag := range commandmeta.GlobalFlags() {
			// fish spells the long form without its leading dashes.
			needle := flag.Long
			if shell == "fish" {
				needle = "-l '" + strings.TrimPrefix(flag.Long, "--") + "'"
			}
			if !strings.Contains(script, needle) {
				t.Errorf("%s completion does not offer global flag %s", shell, flag.Long)
			}
		}
	}
}

// TestCompletionDoesNotMutateStructuredHelp pins the aliasing bug A1b removed:
// completion used to seed itself with the very slices structuredCommandHelp
// stores and then sort them in place, so generating a completion script
// reordered the flags `putnami help projects create` prints for the rest of the
// process. The two surfaces are independent projections now, and the help table
// must keep the catalog's declaration order no matter what ran before it.
func TestCompletionDoesNotMutateStructuredHelp(t *testing.T) {
	for shell := range completionGenerators {
		renderCompletion(t, shell)
	}
	gatherCompletionContext("", &wsproto.Config{})

	checked := 0
	for _, command := range commandmeta.StructuredCommands() {
		detail, ok := commandmeta.Detail(command.Path)
		if !ok || len(detail.Flags) < 2 {
			continue
		}
		root, sub, _ := strings.Cut(command.Path, " ")
		info, documented := StructuredCommandHelp(root, sub)
		if !documented {
			continue
		}
		if len(info.Flags) != len(detail.Flags) {
			t.Errorf("structuredCommandHelp[%q] has %d flags, the catalog declares %d",
				command.Path, len(info.Flags), len(detail.Flags))
			continue
		}
		for i := range detail.Flags {
			if info.Flags[i].Long != detail.Flags[i].Long {
				t.Errorf("structuredCommandHelp[%q] flag %d is %q, the catalog declares %q — "+
					"something reordered the shared slice",
					command.Path, i, info.Flags[i].Long, detail.Flags[i].Long)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no multi-flag command was checked — the walk is wrong, not the table")
	}
}

// TestCompletionFishQuotesEveryDescription pins that the generated fish script's
// single quotes balance. They did not before A1b: six job-command descriptions
// carry an apostrophe ("Run the project's tests"), and an unescaped one in fish
// does not end the string quietly — it swallows the rest of the file, silently
// dropping every completion declared after it.
func TestCompletionFishQuotesEveryDescription(t *testing.T) {
	script := renderCompletion(t, "fish")
	for number, line := range strings.Split(script, "\n") {
		if unescapedQuotes(line)%2 != 0 {
			t.Errorf("fish script line %d has unbalanced single quotes, so fish swallows everything after it:\n  %s",
				number+1, line)
		}
	}
	// Guard against the assertion going vacuous: it only proves anything while
	// some catalog description still carries an apostrophe.
	if !strings.Contains(script, `\'`) {
		t.Error("no escaped apostrophe in the fish script — either quoting regressed or the catalog lost the prose this guards")
	}
}

// unescapedQuotes counts the single quotes fish treats as delimiters, skipping
// the ones a preceding backslash escapes.
func unescapedQuotes(line string) int {
	count := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++ // the escaped character, quote or not, is not a delimiter
		case '\'':
			count++
		}
	}
	return count
}

// TestCompletionZshQuotesEveryDescription is the zsh twin of the fish check
// above. zsh's _describe arrays are emitted as 'name:description' entries, so
// an apostrophe in the prose closes the entry early and corrupts the rest of
// the array — the same defect class, in a different shell.
//
// This one was found in review: the fish fix landed while zsh still emitted
// three raw apostrophes ("the project's tests"), because zsh had its escaping
// applied on the project-alias path only.
func TestCompletionZshQuotesEveryDescription(t *testing.T) {
	script := renderCompletion(t, "zsh")
	for number, line := range strings.Split(script, "\n") {
		if unescapedQuotes(line)%2 != 0 {
			t.Errorf("zsh script line %d has unbalanced single quotes, so the array entry is corrupt:\n  %s",
				number+1, line)
		}
	}
	// Same vacuity guard as fish: only meaningful while some description still
	// carries an apostrophe for the escaping to act on.
	if !strings.Contains(script, `'\''`) {
		t.Error("no POSIX-escaped apostrophe in the zsh script — either quoting regressed or the catalog lost the prose this guards")
	}
}
