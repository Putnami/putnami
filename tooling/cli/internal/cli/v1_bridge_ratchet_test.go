package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The no-v1-adapter ratchets.
//
// An earlier change deleted the version-1 machine emitters, another deleted the
// manifest adaptation that let an older-contract extension load anyway, and a
// third made the runtime-event reader accept exactly one version. Each of those is
// currently true because a change made it true — and each is the kind of thing
// that comes back one well-meaning branch at a time, because "just read the old
// shape too" always looks like compatibility rather than like a second contract.
//
// These tests make the three absences STRUCTURAL. They read the AST, like
// engine_boundary_test.go (A6a) and for the same reason: an assertion over
// selector expressions and declarations ignores comments, doc references and
// test fixtures that name a forbidden symbol on purpose — and this package's
// history files are full of those. Line-oriented counting
// (structural_baseline_test.go) cannot tell prose from code, which is why the
// pins there count TOKENS and these count MEANINGS.
//
// Every check below is paired with a non-vacuity assertion. A structural test
// whose scan silently matches nothing passes forever, which is worse than not
// having it: it reports a guarded invariant that nothing guards.

// v1RatchetProtocolCLI and v1RatchetProtocolRuntime are the protocol modules
// whose version-1 surfaces these ratchets keep out of production code.
const (
	v1RatchetProtocolCLI     = "go.putnami.dev/protocol/cli"
	v1RatchetProtocolRuntime = "go.putnami.dev/protocol/runtime"
)

// parseProductionASTs parses dir's production .go files into files, keyed by
// prefix + their module-relative slash path, all sharing fset so a position
// taken from one module is readable beside another's.
func parseProductionASTs(t *testing.T, fset *token.FileSet, dir, prefix string, files map[string]*ast.File) {
	t.Helper()
	production, _ := goSources(t, dir)
	for _, rel := range production {
		file, err := parser.ParseFile(fset, filepath.Join(dir, filepath.FromSlash(rel)), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", prefix+rel, err)
		}
		files[prefix+rel] = file
	}
}

// productionASTs parses every production .go file in ONE module, keyed by its
// module-relative slash path. Use it for a ratchet whose subject is a property
// of tooling/cli itself; moduleProductionASTs is for one that pins a property of
// the CLI as a whole.
func productionASTs(t *testing.T, root string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	parseProductionASTs(t, fset, root, "", files)
	if len(files) == 0 {
		t.Fatal("parsed no production files — the walk root is wrong")
	}
	return fset, files
}

// moduleProductionASTs parses every production .go file across cliModules, keyed
// in the cliModules key space, so a scan sees the CLI's code wherever an
// earlier reorganization has moved it to by now.
func moduleProductionASTs(t *testing.T, root string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, module := range cliModules {
		parseProductionASTs(t, fset, filepath.Join(root, filepath.FromSlash(module.dir)), module.prefix, files)
	}
	if len(files) == 0 {
		t.Fatal("parsed no production files — the walk root is wrong")
	}
	return fset, files
}

// importedAs returns the identifier path is bound to in file (its alias, or the
// package's own name) and whether the file imports it at all. A blank or dot
// import reports false: neither can produce a "pkg.Symbol" selector.
func importedAs(file *ast.File, path, defaultName string) (string, bool) {
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil || imported != path {
			continue
		}
		if spec.Name == nil {
			return defaultName, true
		}
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return "", false
		}
		return spec.Name.Name, true
	}
	return "", false
}

// qualifiedSelectors reports every selector expression in file that names one
// of symbols through an import of path, as "line: symbol" entries.
func qualifiedSelectors(fset *token.FileSet, file *ast.File, path, defaultName string, symbols map[string]string) []string {
	local, imported := importedAs(file, path, defaultName)
	if !imported {
		return nil
	}
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		// pkg.Obj != nil means the identifier resolved to a local declaration
		// shadowing the import name, so it is not the package qualifier.
		if !ok || pkg.Name != local || pkg.Obj != nil {
			return true
		}
		if _, restricted := symbols[selector.Sel.Name]; !restricted {
			return true
		}
		found = append(found, strconv.Itoa(fset.Position(selector.Sel.Pos()).Line)+": "+selector.Sel.Name)
		return true
	})
	return found
}

// --- Ratchet 1: the retired machine-output selection cannot select again ---

// machineOutputEnvLiteral is the retired contract-selection variable, spelled
// here as a literal because the ratchet is about who may spell it.
const machineOutputEnvLiteral = "PUTNAMI_MACHINE_OUTPUT"

// machineEnvDeclFile is the ONE production file allowed to name the variable:
// internal/machine owns the retired lever and the notice printed for it.
const machineEnvDeclFile = "internal/machine/machine.go"

// machineEnvReaderFile is the ONE production file allowed to READ it, and it
// may only hand the value to the warning.
const machineEnvReaderFile = "internal/cli/app.go"

// TestV1Ratchet_MachineOutputSelectionStaysRetired fails if any production code
// but internal/machine spells PUTNAMI_MACHINE_OUTPUT, or if any file but the
// CLI shell reads it.
//
// Why this predicate: the v1 emitters are deleted, so the variable cannot
// select a renderer today — but the FIRST step back to two output contracts is
// a second reader of this value, and it reads as a harmless feature flag at the
// review. machine.RetiredSelectionEnv deliberately survives as the warn (a CI
// runner still exporting the canary's lever is told its request is ignored
// rather than discovering it by parsing an unexpected document), and this test
// is what keeps "the warn" from quietly becoming "the switch" again.
//
// It asserts on string literals and selectors, so the several doc comments that
// name the variable — including the one directly above app.go's warn call — do
// not count. Prose about a retired lever is exactly what should be free.
func TestV1Ratchet_MachineOutputSelectionStaysRetired(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	fset, files := productionASTs(t, root)

	declSites := 0
	readerFiles := map[string]bool{}
	warnCalls := 0
	for rel, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil && value == machineOutputEnvLiteral {
					if rel != machineEnvDeclFile {
						t.Errorf("%s:%d spells %q — only %s may name the retired machine-output "+
							"selector, and only to warn that it selects nothing (an earlier change deleted "+
							"the v1 emitters; a later change pinned that).",
							rel, fset.Position(lit.Pos()).Line, machineOutputEnvLiteral, machineEnvDeclFile)
					}
					declSites++
				}
			}
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "RetiredSelectionEnv":
				readerFiles[rel] = true
			case "WarnRetiredSelection":
				warnCalls++
			}
			return true
		})
	}

	if declSites == 0 {
		t.Fatalf("no production file spells %q — the scan is broken, or the constant moved without "+
			"this pin. Move the pin with it.", machineOutputEnvLiteral)
	}
	for rel := range readerFiles {
		if rel != machineEnvReaderFile && rel != machineEnvDeclFile {
			t.Errorf("%s reads machine.RetiredSelectionEnv — the value has exactly one production "+
				"reader (%s), which passes it straight to WarnRetiredSelection. A second reader is a "+
				"contract SELECTION coming back; rolling back to v1 documents means pinning an older "+
				"published build, not setting a variable.", rel, machineEnvReaderFile)
		}
	}
	if !readerFiles[machineEnvReaderFile] {
		t.Errorf("%s no longer reads machine.RetiredSelectionEnv; if the warn moved, move this pin "+
			"with it — otherwise the check above passes vacuously", machineEnvReaderFile)
	}
	if warnCalls != 1 {
		t.Errorf("production calls WarnRetiredSelection %d times, want exactly 1 (the CLI shell, once "+
			"per process). More than one is a per-surface notice; none means the lever is silently "+
			"ignored again.", warnCalls)
	}
}

// --- Ratchet 2: nothing can emit a versionless machine document ---

// deletedV1ResultSymbols are the protocols/cli exports B1b left unreachable and
// B7a deleted. Naming one from production code is a versionless document being
// built or read where every machine surface must speak protocolVersion 2.
var deletedV1ResultSymbols = map[string]string{
	"NewResult":   "the v1 envelope constructor, deleted by B7a; build NewResultV2",
	"WriteResult": "the v1 envelope writer, deleted by B7a; write WriteResultV2",
	"Result":      "the v1 envelope type, retained in protocols/cli as a READ shape for old documents only — the CLI writes ResultV2 and reads its own recorded sessions through internal/commands' projections",
}

// TestV1Ratchet_NoVersionlessMachineDocument fails if production code reaches
// for a deleted v1 result symbol, or stamps a protocolVersion member with an
// integer literal instead of the contract's constant.
//
// Two predicates, because the bridge can come back from either end:
//
//   - the SYMBOL check catches a caller reaching back into protocols/cli's v1
//     surface. Those functions are gone, so it cannot compile today — the value
//     is that re-adding them upstream does not silently re-arm a consumer here,
//     which is the shape of every "temporary compatibility helper" that stays;
//   - the LITERAL check catches the version being written by hand. Every
//     production stamp today goes through a named constant
//     (protocolcli.ResultProtocolVersion, protocoljob.ProtocolVersion2, …), so
//     a literal is either a v1 document being forged or a version the contract
//     package does not know it emits. `protocolVersion: 1` is the specific
//     spelling B7a exists to keep out; the check refuses every integer literal
//     because "1" is not more special than any other number a human types.
func TestV1Ratchet_NoVersionlessMachineDocument(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	fset, files := productionASTs(t, root)

	protocolCLIImporters := 0
	stampSites := 0
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		file := files[rel]
		if _, imported := importedAs(file, v1RatchetProtocolCLI, "cli"); imported {
			protocolCLIImporters++
		}
		for _, site := range qualifiedSelectors(fset, file, v1RatchetProtocolCLI, "cli", deletedV1ResultSymbols) {
			line, symbol, _ := strings.Cut(site, ": ")
			t.Errorf("%s:%s names protocols/cli's %s — %s", rel, line, symbol, deletedV1ResultSymbols[symbol])
		}

		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.KeyValueExpr:
				key, ok := n.Key.(*ast.Ident)
				if !ok || key.Name != "ProtocolVersion" {
					return true
				}
				stampSites++
				if lit, ok := n.Value.(*ast.BasicLit); ok && lit.Kind == token.INT {
					t.Errorf("%s:%d stamps ProtocolVersion with the literal %s — every machine "+
						"document's version comes from its contract package's constant, so a document "+
						"cannot claim a version the package does not emit. A literal 1 here is a "+
						"versionless-era document being forged (B1b, B7a).",
						rel, fset.Position(lit.Pos()).Line, lit.Value)
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					selector, ok := lhs.(*ast.SelectorExpr)
					if !ok || selector.Sel.Name != "ProtocolVersion" || i >= len(n.Rhs) {
						continue
					}
					stampSites++
					if lit, ok := n.Rhs[i].(*ast.BasicLit); ok && lit.Kind == token.INT {
						t.Errorf("%s:%d assigns ProtocolVersion = %s — same rule as the composite "+
							"literal above: the version is the contract package's constant, never a "+
							"typed-in number.", rel, fset.Position(lit.Pos()).Line, lit.Value)
					}
				}
			}
			return true
		})
	}

	if protocolCLIImporters == 0 {
		t.Fatal("no production file imports " + v1RatchetProtocolCLI + " — the symbol scan matched " +
			"nothing and would pass however the v1 surface came back")
	}
	if stampSites == 0 {
		t.Fatal("no production file stamps a ProtocolVersion member — the literal scan is looking at " +
			"the wrong shape, so it would never fire")
	}
}

// --- Ratchet 3: manifest adaptation does not come back ---

// extensionPackageDir is where the CLI loads and validates extension manifests.
// Its twin ratchet lives in protocols/extension (adaptation_ratchet_test.go),
// which is a separate Go module and cannot be walked from here.
const extensionPackageDir = "internal/extension/"

// TestV1Ratchet_NoManifestAdaptation fails if the extension loader grows a
// function that ADAPTS a manifest instead of accepting or rejecting it.
//
// B6r deleted protocols/extension's AdaptReservedFlagShadows and its
// two helpers: an older-contract manifest used to LOAD after the loader
// silently deleted the flag definitions that shadowed a reserved global. That
// is the bridge in its purest form — the CLI running a manifest that is not
// what the extension author wrote, with a warning nobody reads. Contract 3 is
// required now, so the answer is a hard error and a `putnami upgrade`.
//
// The predicate is a declaration-name prefix, which is a convention made
// enforceable rather than a semantic proof: a rewriting function could be
// called anything. It holds because adaptation code in this tree has always
// been named for what it does, and because the review question it forces —
// "why is this function named Adapt?" — is exactly the right one to ask before
// a manifest is rewritten behind its author's back.
func TestV1Ratchet_NoManifestAdaptation(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	_, files := productionASTs(t, root)

	scanned := 0
	for rel, file := range files {
		if !strings.HasPrefix(rel, extensionPackageDir) {
			continue
		}
		scanned++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			lower := strings.ToLower(fn.Name.Name)
			if strings.HasPrefix(lower, "adapt") {
				t.Errorf("%s declares %s — %s must accept a manifest or reject it, never rewrite it. "+
					"an earlier change deleted the last adaptation (reserved-flag shadows silently dropped from an "+
					"older-contract manifest); an extension that does not meet contract 3 gets a hard "+
					"error and an upgrade, not a quietly different manifest.",
					rel, fn.Name.Name, extensionPackageDir)
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("no production files under %s — the scan is looking in the wrong place", extensionPackageDir)
	}
}

// --- Ratchet 4: the runtime-event reader accepts exactly one version ---

// jobsPackageName is the Go package the runtime-event reader lives in. The
// forbidden-symbol scan below is scoped by PACKAGE rather than by directory
// because the job layer is split across two modules — the pure model,
// ParseRawEvent included, into go.putnami.dev/cli/model/jobs; the scheduler,
// executor and cache into tooling/cli's internal/jobs — and both halves are the
// same package to a reader of the acceptance rule. A package name follows the
// code through every remaining move; a directory prefix would have to be
// re-listed after each one, and would quietly scan nothing in between.
const jobsPackageName = "jobs"

// eventParserFile is the file whose acceptance rule is pinned. It is named
// rather than searched for, so a move stays a REVIEWED event: the pin fails,
// and re-keying it is the review. An earlier change moved it from internal/jobs to
// go.putnami.dev/cli/model/jobs (where the parser is exported as ParseRawEvent
// and internal/jobs keeps the parseRawEvent spelling as an alias).
const eventParserFile = "cli-model/jobs/events.go"

// loosenedRuntimeVersionSymbols are protocols/runtime helpers whose whole
// purpose is to admit MORE than one version. They are legitimate in the
// protocol package (it defines every version it knows) and wrong in the CLI's
// reader, which requires the exact version it advertised.
var loosenedRuntimeVersionSymbols = map[string]string{
	"IsKnownProtocolVersion": "accepts v1 as well as v2; the CLI advertises PUTNAMI_RUNTIME_EVENTS and accepts back EXACTLY what it advertised",
	"ProtocolVersion":        "the v1 constant; a contract-3 extension must answer at the advertised version, so a v1 line means the stream disagrees with the manifest",
	"NegotiatedVersion":      "the EMITTER's resolution (clamping down to what an invoker accepts); a reader that clamps accepts a stream older than it asked for",
}

// TestV1Ratchet_RuntimeEventAcceptanceIsExact pins both halves of the rule B6c
// settled: the reader compares against MaxKnownProtocolVersion for EQUALITY,
// and nothing in the package reaches for a looser predicate.
//
// The equality is the interesting half. A range check ("v1 or newer") is the
// natural thing to write and is wrong here: the CLI requires extension contract
// 3, a contract-3 extension is required to read the advertisement and answer at
// it, so a lower-versioned line means the manifest and the stream disagree —
// not that the emitter is old. Accepting it would resurrect exactly the dual
// vocabulary B6a/B6c removed, and would do it silently, because a v1 stream
// parses fine and merely never emits the `ready` event a serve watcher waits
// for.
func TestV1Ratchet_RuntimeEventAcceptanceIsExact(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	fset, files := moduleProductionASTs(t, root)

	parserFile, ok := files[eventParserFile]
	if !ok {
		t.Fatalf("%s not found — the runtime-event parser moved; move this pin with it. It is a "+
			"re-key, never a deletion: the rule is a property of whatever file reads the stream, "+
			"and the scan spans every module in cliModules, so the file is findable wherever "+
			"the reorganization has put it.", eventParserFile)
	}

	exactChecks := 0
	ast.Inspect(parserFile, func(node ast.Node) bool {
		binary, ok := node.(*ast.BinaryExpr)
		if !ok || (binary.Op != token.NEQ && binary.Op != token.EQL) {
			return true
		}
		if !namesSelector(binary.X, "Version") && !namesSelector(binary.Y, "Version") {
			return true
		}
		if namesSelector(binary.X, "MaxKnownProtocolVersion") || namesSelector(binary.Y, "MaxKnownProtocolVersion") {
			exactChecks++
		}
		return true
	})
	if exactChecks == 0 {
		t.Errorf("%s no longer compares an event's Version against runtimeproto.MaxKnownProtocolVersion "+
			"for equality. The CLI accepts back EXACTLY the version it advertises in "+
			"PUTNAMI_RUNTIME_EVENTS: a >= or a range check accepts a v1 stream from a contract-3 "+
			"extension, which is the manifest and the wire disagreeing (B6c, B7a).", eventParserFile)
	}

	scanned := 0
	for rel, file := range files {
		if file.Name.Name != jobsPackageName {
			continue
		}
		scanned++
		for _, site := range qualifiedSelectors(fset, file, v1RatchetProtocolRuntime, "runtime", loosenedRuntimeVersionSymbols) {
			line, symbol, _ := strings.Cut(site, ": ")
			t.Errorf("%s:%s names protocols/runtime's %s — %s", rel, line, symbol, loosenedRuntimeVersionSymbols[symbol])
		}
	}
	if scanned == 0 {
		t.Fatalf("no production file declares package %s — the forbidden-symbol scan matched nothing "+
			"and would pass however a looser predicate came back", jobsPackageName)
	}

	// Non-vacuity: the scan must be able to SEE the runtime protocol's symbols
	// in this package, or the loop above proves nothing.
	if _, imported := importedAs(parserFile, v1RatchetProtocolRuntime, "runtime"); !imported {
		t.Fatalf("%s no longer imports %s — the forbidden-symbol scan matched nothing",
			eventParserFile, v1RatchetProtocolRuntime)
	}
}

// namesSelector reports whether expr is a selector or identifier whose final
// name is name (x.Version, runtimeproto.MaxKnownProtocolVersion, Version).
func namesSelector(expr ast.Expr, name string) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name == name
	case *ast.Ident:
		return e.Name == name
	}
	return false
}
