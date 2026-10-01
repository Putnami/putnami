package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// An earlier change moved two facts out of hand-maintained tables and into
// the command catalog: which subcommand a bare `putnami <root>` runs
// (Command.DefaultSub), and the CLI's global-flag vocabulary (GlobalFlag). Both
// describe code that lives in this package, and neither can be checked by
// running the command — `putnami cache` really cleans, `putnami telemetry`
// really reads machine-global consent.
//
// So these tests read the source instead. That is not a trick: the thing being
// asserted IS a property of the source ("the handler's `case "list", "":` arm
// and the catalog's DefaultSub name the same subcommand"), and reading it
// directly is what makes the assertion total. The hand-keyed switch A1b deleted
// was missing `scopes` precisely because nothing compared it to the handlers.
//
// Both use go/ast rather than string matching so a reformat, a comment, or a
// renamed local cannot quietly change what is counted.

// parseSourceFile parses one .go file from this package's directory.
func parseSourceFile(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return fset, file
}

// stringLiteral returns the value of a quoted string expression, and false for
// anything else (an identifier, a constant, a non-string literal).
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

// sortedKeys returns a set's members in a deterministic order, so a failure
// message reads the same on every run. It lived in command_tables_baseline_test.go
// until a later change deleted that scaffolding.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// handlerRootsByFunc maps each cmd* handler's function name to the command root
// it is registered under, read from registry_commands.go's init().
func handlerRootsByFunc(t *testing.T, file *ast.File) map[string]string {
	t.Helper()
	roots := map[string]string{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "registerCommand" || len(call.Args) != 2 {
			return true
		}
		name, ok := stringLiteral(call.Args[0])
		if !ok {
			return true
		}
		handler, ok := call.Args[1].(*ast.Ident)
		if !ok {
			return true
		}
		roots[handler.Name] = name
		return true
	})
	if len(roots) == 0 {
		t.Fatal("found no registerCommand calls in registry_commands.go — the parse is wrong, not the registry")
	}
	return roots
}

// handlerDefaultSubs returns, per command root, the subcommand its handler
// treats an empty subcommand as: the single non-empty string in a switch case
// that also lists "". A case listing "" alone (cmdMcp) means the bare root has
// behavior of its own and is deliberately absent from the result.
func handlerDefaultSubs(t *testing.T, file *ast.File, roots map[string]string) map[string]string {
	t.Helper()
	defaults := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		root, registered := roots[fn.Name.Name]
		if !registered {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			var named []string
			empty := false
			for _, expr := range clause.List {
				value, ok := stringLiteral(expr)
				if !ok {
					continue
				}
				if value == "" {
					empty = true
					continue
				}
				named = append(named, value)
			}
			if !empty || len(named) == 0 {
				return true
			}
			if len(named) > 1 {
				t.Errorf("%s aliases the bare root to %v — a bare invocation cannot run two subcommands, so the catalog cannot declare a DefaultSub for %q",
					fn.Name.Name, named, root)
				return true
			}
			if previous, seen := defaults[root]; seen && previous != named[0] {
				t.Errorf("%s aliases the bare root to both %q and %q", fn.Name.Name, previous, named[0])
			}
			defaults[root] = named[0]
			return true
		})
	}
	return defaults
}

// TestCatalog_DefaultSubMatchesHandlers is the assertion the deleted
// canonicalStructuredCommand switch never had: every root whose handler treats
// "" as a subcommand declares that subcommand as its catalog DefaultSub, and no
// root declares one its handler does not implement. Both directions matter —
// the switch had twelve of the thirteen arms, and the missing one (scopes) is
// what made `putnami scopes --json` exit 2 while `putnami scopes list --json`
// worked.
func TestCatalog_DefaultSubMatchesHandlers(t *testing.T) {
	t.Parallel()
	_, file := parseSourceFile(t, "registry_commands.go")
	roots := handlerRootsByFunc(t, file)
	fromHandlers := handlerDefaultSubs(t, file, roots)

	if len(fromHandlers) == 0 {
		t.Fatal("no handler was found to alias a bare root — the AST walk broke, not the handlers")
	}

	fromCatalog := map[string]string{}
	for _, command := range commandmeta.StructuredRoots() {
		if command.DefaultSub != "" {
			fromCatalog[command.Path] = command.DefaultSub
		}
	}

	for _, root := range sortedKeys(mapKeysToSetString(fromHandlers)) {
		switch declared := fromCatalog[root]; declared {
		case fromHandlers[root]:
		case "":
			t.Errorf("%s's handler runs %q for a bare `putnami %s`, but the catalog declares no DefaultSub — "+
				"every surface that keys a path will miss the bare spelling", root, fromHandlers[root], root)
		default:
			t.Errorf("catalog says a bare `putnami %s` runs %q, but its handler runs %q", root, declared, fromHandlers[root])
		}
	}
	for _, root := range sortedKeys(mapKeysToSetString(fromCatalog)) {
		if _, ok := fromHandlers[root]; !ok {
			t.Errorf("catalog declares DefaultSub %q for %q, but its handler has no case aliasing the bare root — "+
				"the bare spelling would claim metadata it does not run", fromCatalog[root], root)
		}
	}
}

// TestCatalog_DefaultSubIsAnInvocablePath keeps the expansion resolvable: a
// DefaultSub that names no catalog path would expand a bare root to a key
// nothing answers.
func TestCatalog_DefaultSubIsAnInvocablePath(t *testing.T) {
	t.Parallel()
	for _, command := range commandmeta.Commands() {
		if command.DefaultSub == "" {
			continue
		}
		expanded := commandmeta.CanonicalPath(command.Path, "")
		if expanded != command.Path+" "+command.DefaultSub {
			t.Errorf("CanonicalPath(%q, \"\") = %q, want the DefaultSub expansion", command.Path, expanded)
		}
		if _, ok := commandmeta.Lookup(expanded); !ok {
			t.Errorf("%q declares DefaultSub %q, but %q is not a catalog path", command.Path, command.DefaultSub, expanded)
		}
	}
}

// parserFlagsExemptFromCatalog records global flags the parser consumes that the
// global-flag table deliberately does not carry, each with the reason. Both are
// cataloged as flags of the `help` command instead, which is where they are
// meaningful; listing them as globals would offer them after every command.
var parserFlagsExemptFromCatalog = map[string]string{
	"--man":      "cataloged as a `help` command flag (putnami help --man)",
	"--markdown": "cataloged as a `help` command flag (putnami help --markdown)",
}

// TestGlobalFlags_CoverTheParser reads the flag parser's own switch and asserts
// the catalog's global-flag table covers every flag it consumes. Until slice A1c
// binds the parser to the table, this is what keeps a flag added to the parser
// from being invisible to help AND to all three completion generators at once —
// which is how --cache-trust, --profile, --json, --color, --no-color, and
// --trace-profile came to be absent from every shell's completion.
func TestGlobalFlags_CoverTheParser(t *testing.T) {
	t.Parallel()
	_, file := parseSourceFile(t, "flags.go")

	known := map[string]bool{}
	for _, flag := range commandmeta.GlobalFlags() {
		known[flag.Long] = true
		if flag.Short != "" {
			known[flag.Short] = true
		}
	}

	consumed := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		clause, ok := node.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, expr := range clause.List {
			value, ok := stringLiteral(expr)
			if !ok || !strings.HasPrefix(value, "-") {
				continue
			}
			consumed[value] = true
		}
		return true
	})
	if len(consumed) == 0 {
		t.Fatal("found no flag cases in flags.go — the AST walk broke, not the parser")
	}

	for _, flag := range sortedKeys(consumed) {
		if known[flag] {
			if reason, exempt := parserFlagsExemptFromCatalog[flag]; exempt {
				t.Errorf("parserFlagsExemptFromCatalog still records %q (%s) but the global-flag table now carries it — delete the exception", flag, reason)
			}
			continue
		}
		if _, exempt := parserFlagsExemptFromCatalog[flag]; exempt {
			continue
		}
		t.Errorf("the parser consumes %q, which the catalog's global-flag table does not declare — "+
			"help and every shell completion are blind to it", flag)
	}
	for flag := range parserFlagsExemptFromCatalog {
		if !consumed[flag] {
			t.Errorf("parserFlagsExemptFromCatalog records %q, which the parser no longer consumes — delete the exception", flag)
		}
	}
}

// TestGlobalFlags_AreWellFormed keeps the table renderable by every surface that
// reads it: help prints Long/Short/ValueName, and the zsh generator turns
// ValueName into a completion metavariable.
func TestGlobalFlags_AreWellFormed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, flag := range commandmeta.GlobalFlags() {
		if !strings.HasPrefix(flag.Long, "--") {
			t.Errorf("global flag %q is not a --long form", flag.Long)
		}
		if seen[flag.Long] {
			t.Errorf("global flag %q is declared twice", flag.Long)
		}
		seen[flag.Long] = true
		if flag.Short != "" && (!strings.HasPrefix(flag.Short, "-") || strings.HasPrefix(flag.Short, "--")) {
			t.Errorf("global flag %q has short form %q, which is not a single-dash alias", flag.Long, flag.Short)
		}
		if (flag.Type == commandmeta.FlagValue) != (flag.ValueName != "") {
			t.Errorf("global flag %q: Type and ValueName disagree (%v vs %q)", flag.Long, flag.Type, flag.ValueName)
		}
		if strings.TrimSpace(flag.Description) == "" {
			t.Errorf("global flag %q has no description", flag.Long)
		}
		if len(flag.Values) > 0 && flag.Type != commandmeta.FlagValue {
			t.Errorf("global flag %q offers candidate values but takes no value", flag.Long)
		}
	}
}

// TestGlobalFlagCategories_AreContiguous pins the grouping convention the table
// header states: a category's flags are declared together, so the listing shows
// each category once, in declaration order.
func TestGlobalFlagCategories_AreContiguous(t *testing.T) {
	t.Parallel()
	categories := commandmeta.GlobalFlagCategories()
	if len(categories) == 0 {
		t.Fatal("GlobalFlagCategories() returned nothing")
	}
	seen := map[string]bool{}
	listed := 0
	for _, category := range categories {
		if seen[category.Name] {
			t.Errorf("category %q appears twice, so the listing splits it", category.Name)
		}
		seen[category.Name] = true
		if len(category.Flags) == 0 {
			t.Errorf("category %q has no flags", category.Name)
		}
		listed += len(category.Flags)
	}
	declared := 0
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.Category != "" {
			declared++
		}
	}
	if listed != declared {
		t.Errorf("GlobalFlagCategories() listed %d flags from %d categorized ones", listed, declared)
	}
}

func mapKeysToSetString(m map[string]string) map[string]bool {
	set := make(map[string]bool, len(m))
	for key := range m {
		set[key] = true
	}
	return set
}
