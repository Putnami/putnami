package toolchain

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// The single go.mod reader this extension owns.
//
// Before this file the repository carried THREE independent hand-rolled go.mod
// parsers: the CLI's project discovery (`workspace/project.go`), the CLI's
// replace-closure codemod (`workspace/gomod_closure.go`), and this package's
// go.work member scan. Two of them lived in a language-neutral orchestrator,
// which is exactly the knowledge the extension model moves out of core. The workspace probe and
// the workspace-sync task both consolidate here.
//
// Three properties are load-bearing:
//
//   - PURE AND OFFLINE. Parsing is a function of the file bytes. No `go`
//     invocation, no module cache, no network. The probe's answer digests into
//     the workspace snapshot, so a byte that varies between two runs over one
//     tree would be a cache-correctness bug (a silent edge loss is the incident this rule
//     exists for: source-blind Go cache keys).
//
//   - THE ONLY GRAMMAR LEFT. This is the grammar the CLI's own parser used to
//     handle, down to the defensive cases: `// indirect` requires are KEPT (they
//     still link code that must key the build), both the single-line and the
//     parenthesized block forms are read, and a replace's target is classified
//     local/module by the same rule. The CLI parsers are gone, so no
//     second implementation exists to disagree with — and no second
//     implementation exists to catch a mistake here either, which is why the
//     corpus in workspace_corpus_test.go is now the sole conformance suite.
//
//   - HAND-ROLLED ON PURPOSE. `golang.org/x/mod/modfile` is stricter than this
//     grammar and REJECTS files this repository must still be able to describe
//     (a half-written go.mod during an edit, a module with no version on a
//     require). A probe that fails on a malformed manifest makes the project
//     vanish from the workspace with no diagnostic — the silent-project-loss failure. Reading
//     what is readable and reporting the rest is the required behavior.

// GoModFile is the parsed subset of a go.mod that workspace discovery needs.
//
// Requires lists every required module path — INCLUDING `// indirect` requires,
// because anything that can change the linked binary must key the build.
// Replaces captures `replace` directives so a local (`./`, `../`) target can be
// resolved back to a workspace directory even when the require line carries a
// placeholder version that exists only in this repository.
type GoModFile struct {
	// Module is the module path the `module` line declares.
	Module string
	// GoVersion is the `go` directive's version, when present.
	GoVersion string
	// Requires are the required module paths, in file order.
	Requires []string
	// Indirect marks the requires the file annotates `// indirect`. An indirect
	// requirement is a fact about the module GRAPH — some dependency of this
	// module needs it — rather than a statement about this module's own code,
	// so a consumer asking "does this project declare an edge nothing of its
	// own imports?" must leave it alone: `go mod tidy` puts it back.
	Indirect map[string]bool
	// Replaces are the replace directives, in file order.
	Replaces []GoModReplace
	// Ignores are the `ignore` directive paths (Go 1.25), in file order,
	// unquoted: a path that starts with `./` names one directory under the
	// module root, any other path a directory at any depth. The go command
	// matches no package pattern in an ignored directory; it still loads a
	// package there that another package imports.
	Ignores []string
}

// GoModReplace is a single `replace` directive. Old is the replaced module
// path. NewPath is the replacement module path (module→module replaces) or the
// local relative directory (`./`/`../` replaces); NewLocal distinguishes the
// two so a consumer resolves each correctly.
type GoModReplace struct {
	Old      string
	NewPath  string
	NewLocal bool
}

// ReadGoMod parses the go.mod at path. It returns (nil, nil) when the file does
// not exist — the ordinary "not a Go module directory" answer — and an error
// only when a file exists but cannot be read or declares no module line.
func ReadGoMod(path string) (*GoModFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseGoMod(string(data))
}

// ParseGoMod parses go.mod content. Split from ReadGoMod so the grammar can be
// exercised without touching the filesystem.
func ParseGoMod(content string) (*GoModFile, error) {
	mod := &GoModFile{}
	// block tracks which directive block we are inside: "", "require",
	// "replace" or "ignore". Directives appear both as single lines and as
	// parenthesized blocks; go.mod blocks do not nest, so no stack is needed.
	block := ""
	haveModule := false

	for _, raw := range strings.Split(content, "\n") {
		// Strip the trailing `// ...` comment before parsing. `// indirect`
		// requires are intentionally NOT filtered: an indirect dependency still
		// links code that must key the build, so the marker is
		// irrelevant to what is collected here.
		line := strings.TrimSpace(stripGoModComment(raw))
		if line == "" {
			continue
		}

		if block != "" {
			if line == ")" {
				block = ""
				continue
			}
			// A top-level directive inside a block ends a block
			// that never closed, and the line is re-read as the directive it is.
			if !startsGoModDirective(line) {
				switch block {
				case "require":
					if m := goModRequirePath(line); m != "" {
						mod.recordRequire(m, goModIndirect(raw))
					}
				case "replace":
					if r, ok := goModReplace(line); ok {
						mod.Replaces = append(mod.Replaces, r)
					}
				case "ignore":
					mod.recordIgnore(line)
				}
				continue
			}
			block = ""
		}

		switch {
		case strings.HasPrefix(line, "module "):
			mod.Module = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			haveModule = true
		case strings.HasPrefix(line, "go "):
			mod.GoVersion = strings.TrimSpace(strings.TrimPrefix(line, "go "))
		case line == "require (":
			block = "require"
		case line == "replace (":
			block = "replace"
		case strings.HasPrefix(line, "require ("):
			// Defensive: `require (` with trailing content on the same line is
			// not valid go.mod, but treat the paren as opening a block rather
			// than losing every entry that follows it. The block this opens can
			// be one that never closes — `require ( foo v1 )` already consumed
			// its `)` — which is precisely what the recovery above bounds: the
			// phantom block ends at the next top-level directive instead of
			// running to end of file and eating it.
			block = "require"
		case strings.HasPrefix(line, "replace ("):
			block = "replace"
		case strings.HasPrefix(line, "require "):
			if m := goModRequirePath(strings.TrimPrefix(line, "require ")); m != "" {
				mod.recordRequire(m, goModIndirect(raw))
			}
		case strings.HasPrefix(line, "replace "):
			if r, ok := goModReplace(strings.TrimPrefix(line, "replace ")); ok {
				mod.Replaces = append(mod.Replaces, r)
			}
		case goModVerb(line, "ignore"):
			// The go lexer splits on any whitespace and reads `(` as a token of
			// its own, so `ignore(` opens a block and `ignore<TAB>x` is a line.
			// A paren with trailing content opens a block too, like `require (`,
			// except the empty block `()`.
			rest := strings.TrimSpace(strings.TrimPrefix(line, "ignore"))
			switch {
			case strings.Join(strings.Fields(rest), "") == "()":
			case strings.HasPrefix(rest, "("):
				block = "ignore"
			default:
				mod.recordIgnore(rest)
			}
		}
	}

	if !haveModule {
		return nil, fmt.Errorf("missing module line")
	}
	return mod, nil
}

// ReplacedModules returns the set of module paths that already carry a replace
// directive, which is what the replace-closure computation checks against.
func (m *GoModFile) ReplacedModules() map[string]bool {
	replaced := make(map[string]bool, len(m.Replaces))
	for _, r := range m.Replaces {
		replaced[r.Old] = true
	}
	return replaced
}

// goModDirectiveKeywords are the words that open a TOP-LEVEL stanza in the
// go.mod family of files. `use` belongs to go.work rather than go.mod, and is
// listed anyway: it is no more valid as a require or replace entry than the
// others, so treating it as a stanza opener can only end a block that was
// already broken.
var goModDirectiveKeywords = []string{
	"module", "go", "toolchain", "godebug", "require", "exclude", "replace", "retract", "tool", "ignore", "use",
}

// startsGoModDirective reports whether a line opens a top-level directive.
//
// Inside a well-formed block it is always false: a require entry is
// `path version`, a replace entry is `old => new` and an ignore entry is one
// path, and none begins with one of these keywords followed by whitespace or
// an opening paren.
func startsGoModDirective(line string) bool {
	for _, keyword := range goModDirectiveKeywords {
		if goModVerb(line, keyword) {
			return true
		}
	}
	return false
}

// stripGoModComment removes a trailing `//` comment from a go.mod line. Module
// paths and versions never contain `//`, so cutting at the first occurrence is
// safe (Cut returns the whole line when `//` is absent).
func stripGoModComment(line string) string {
	before, _, _ := strings.Cut(line, "//")
	return before
}

// goModRequirePath returns the module path from a require entry
// (`path version`), or "" when the entry is malformed or empty.
func goModRequirePath(entry string) string {
	fields := strings.Fields(strings.TrimSpace(entry))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// goModReplace parses a replace entry (`old [oldver] => new [newver]`). The
// `new` operand is either a module path or a local (`./`, `../`, or absolute)
// directory; NewLocal records which, so a consumer resolves a local target back
// to a workspace directory instead of looking it up as a module.
func goModReplace(entry string) (GoModReplace, bool) {
	before, after, found := strings.Cut(entry, "=>")
	if !found {
		return GoModReplace{}, false
	}
	left := strings.Fields(strings.TrimSpace(before))
	right := strings.Fields(strings.TrimSpace(after))
	if len(left) == 0 || len(right) == 0 {
		return GoModReplace{}, false
	}
	return GoModReplace{
		Old:      left[0],
		NewPath:  right[0],
		NewLocal: isLocalReplaceTarget(right[0]),
	}, true
}

// isLocalReplaceTarget reports whether a replace target is a local filesystem
// path rather than a module path. go.mod treats targets beginning with `./`,
// `../` or an absolute path as directories.
func isLocalReplaceTarget(p string) bool {
	return strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") ||
		p == "." || p == ".." || filepath.IsAbs(p)
}

// ParseGoWorkUses returns the `use` directives of the go.work file at
// goWorkPath, handling both the `use (...)` block form and the single-line
// `use ./x` form, with `//` comments stripped. Paths come back slash-separated
// and cleaned, in file order. A missing go.work returns no members and no
// error: a workspace without one is an ordinary state, not a failure.
func ParseGoWorkUses(goWorkPath string) ([]string, error) {
	data, err := os.ReadFile(goWorkPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rels []string
	inUseBlock := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripGoModComment(raw))
		var rel string
		switch {
		case line == "use (":
			inUseBlock = true
			continue
		case inUseBlock && line == ")":
			inUseBlock = false
			continue
		case inUseBlock && line != "":
			rel = line
		case strings.HasPrefix(line, "use "):
			rel = strings.TrimSpace(strings.TrimPrefix(line, "use "))
		default:
			continue
		}
		rels = append(rels, path.Clean(rel))
	}
	return rels, nil
}

// recordRequire collects one require line, keeping the `// indirect` marker
// the comment stripper drops.
func (m *GoModFile) recordRequire(module string, indirect bool) {
	m.Requires = append(m.Requires, module)
	if !indirect {
		return
	}
	if m.Indirect == nil {
		m.Indirect = make(map[string]bool)
	}
	m.Indirect[module] = true
}

// goModVerb reports whether line is the directive verb followed by
// whitespace or `(`.
func goModVerb(line, verb string) bool {
	rest, found := strings.CutPrefix(line, verb)
	return found && rest != "" && strings.ContainsRune(" \t(", rune(rest[0]))
}

// recordIgnore collects one `ignore` path. The go command reads the path as a
// single argument that only a double quote may quote, and rejects an unquoted
// argument holding a quote; an entry it would reject is dropped.
func (m *GoModFile) recordIgnore(entry string) {
	ignored := strings.TrimSpace(entry)
	if strings.HasPrefix(ignored, `"`) {
		unquoted, err := strconv.Unquote(ignored)
		if err != nil {
			return
		}
		ignored = unquoted
	} else if len(strings.Fields(ignored)) != 1 || strings.ContainsAny(ignored, "\"'`") {
		return
	}
	if ignored == "" {
		return
	}
	m.Ignores = append(m.Ignores, ignored)
}

// goModIndirect reads the marker off the RAW line, before the comment is
// stripped.
func goModIndirect(raw string) bool {
	return strings.Contains(raw, "// indirect")
}
