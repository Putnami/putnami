package cli

import (
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// The anti-reentry ratchets: provider knowledge must not creep back into core
// code.
//
// The core-isolation pins (core_isolation_ratchets_test.go) hold the tree's
// SHAPE — three lifecycle primitives, no provider-named package, no package-count growth.
// They do not stop provider knowledge from coming back INSIDE a core package,
// which is how it got there the first time: nobody ever added a package called
// `internal/typescript`; somebody added `if ext.Name == "@putnami/typescript"`
// to a scheduler, and a `"pyproject.toml"` to a marker table, and a
// `docker`-shaped branch to a runner.
//
// These ratchets read the AST, so they see code and not prose. That distinction
// is the whole design: this tree's comments discuss TypeScript, Docker and
// Postgres constantly — usually to explain that core no longer knows about
// them — and a grep-shaped ratchet over comments would be raised into
// meaninglessness within a slice. What is counted is what executes: string
// literals and identifiers.

// providerVocabulary is the language / package-manager / toolchain / container
// / database vocabulary that must not name core behavior.
//
// A term with a separator ('.', '-', '_', '/') matches as a case-insensitive
// SUBSTRING of the raw text; every other term matches a whole WORD after the
// text is split on non-alphanumerics and camelCase boundaries. So `go.mod`
// matches the literal "go.mod" but not "gomodule"; `bun` matches the identifier
// resolveBunVersion but not the word "bundle".
var providerVocabulary = []string{
	// languages and ecosystems
	"typescript", "javascript", "python", "golang",
	// language manifests and lockfiles
	"package.json", "package-lock.json", "go.mod", "go.sum", "go.work",
	"pyproject.toml", "requirements.txt", "tsconfig", "bun.lock", "bun.lockb",
	"uv.lock", "pnpm-lock", "yarn.lock", "cargo.toml", "gemfile",
	// package managers and toolchains
	"npm", "pnpm", "yarn", "bun", "uv", "pip", "poetry", "gradle", "maven",
	"node_modules", "golangci", "gofmt", "mypy", "ruff", "eslint", "biome",
	// containers and databases
	"docker", "dockerfile", "podman", "postgres", "postgresql", "psql",
	"pg_isready", "mysql", "sqlite", "redis",
}

// providerVocabularyScopes are the core subtrees the epic emptied of provider
// knowledge: the run composer, the plan/execute layer, the workspace model, and
// extension resolution — which is in scope precisely because it holds the one
// adapter the epic deliberately QUARANTINED rather than deleted
// (TestQuarantineAdapterIsPinned).
//
// The model module is scoped WHOLE rather than package by package: it exists to
// hold the pure workspace, job and extension model and nothing else, so all of
// it is core by construction and a package added to it is in scope the day it
// is added — moving another file there re-keys its entry and nothing more.
var providerVocabularyScopes = []string{
	"internal/engine/",
	"internal/extension/",
	"internal/jobs/",
	"internal/workspace/",
	"cli-model/",
}

// providerVocabularyResidue is one allowlisted file: how many provider-naming
// sites it holds today, and why they are not a regression.
type providerVocabularyResidue struct {
	// sites is a CEILING on the naming sites in this file. Fewer passes (the
	// residue shrank); more fails (the file grew a new provider branch).
	sites int
	// why states what the residue is and what would remove it.
	why string
}

// providerVocabularyAllowlist is the provider knowledge that SURVIVES this
// program, inventoried file by file at closeout. It may only shrink: a listed
// file that no longer names a provider must lose its entry, and an unlisted
// file may not start.
//
// This list IS the effort's remaining tail, and it is short on purpose — seven
// files, one genuine behavioral branch among them. Each entry names what would
// remove it, so the next program starts from an inventory rather than from a
// grep. Adding an entry is a program-level decision: the epic's whole claim is
// that core does not need these names.
var providerVocabularyAllowlist = map[string]providerVocabularyResidue{
	"cli-model/workspace/probe_view.go": {
		sites: 1,
		why: "A CROSS-PROJECT ARTIFACT EDGE. The authored dockerBaseProject option " +
			"is a real dependency so graph ordering, dependency closure, cache ancestry and " +
			"impacted propagation all agree before an extension task starts. Core does not inspect " +
			"or build container content; this one option spelling goes when extension manifests can " +
			"declare option-derived project references generically",
	},
	"internal/extension/discovery.go": {
		sites: 4,
		why: "THE EPIC-D QUARANTINE. Discovery's third source is the root package.json's " +
			"devDependencies: an npm-shaped way of declaring an installed extension, kept working " +
			"because deleting it would break every workspace that installs extensions as npm " +
			"packages. It is quarantined, not sanctioned — epic D owns replacing it with a " +
			"provider-neutral installed-extension declaration. " +
			"TestQuarantineAdapterIsPinned pins its exact shape so it can neither grow " +
			"nor be quietly reshaped while it waits",
	},
	"internal/jobs/extension_runtime.go": {
		sites: 1,
		why: "BOOTSTRAP RESIDUE. runtimeReplacementClosure reads the go.mod files of a LOCAL " +
			"extension's module and its workspace siblings so a from-source extension can be built " +
			"at all. It is the chicken-and-egg floor of the runtime primitive: the first extension " +
			"has to be buildable before any extension exists to build it. It goes when local " +
			"extensions are resolved as prepared artifacts rather than compiled in place",
	},
	"internal/jobs/planner_helpers.go": {
		sites: 1,
		why: "A WALK EXCLUSION, not a branch: `node_modules` sits beside `.git`, `dist` and " +
			"`vendor` in the directories a content probe refuses to descend into. Nothing " +
			"downstream behaves differently for a TypeScript project; it is a cost guard on a " +
			"filesystem walk. It goes when scan exclusions become workspace configuration",
	},
	"cli-model/jobs/result_model.go": {
		sites: 1,
		why: "REGISTRY VOCABULARY IN THE CANONICAL RESULT: a published-artifact record is surfaced " +
			"as a Publication only when its registry is `docker`, because the recap renders " +
			"image/digest/channel fields no other registry has. An earlier change already narrowed this to " +
			"one projection (the output complexity ceiling states it); it goes when a published " +
			"record carries its own renderable shape",
	},
	"internal/workspace/gomod_bootstrap.go": {
		sites: 4,
		why: "BOOTSTRAP RESIDUE, and the one the C4b wave-2 gate report already named: core repairs " +
			"the workspace `replace` directives in an EXTENSION's own go.mod, because an extension " +
			"whose module cannot resolve cannot run the workspace-sync task that would have " +
			"repaired it. Non-extension modules are repaired by the Go extension's own task. It " +
			"goes with the same change as extension_runtime.go's entry",
	},
	"internal/workspace/workspace.go": {
		sites: 1,
		why: "A ROOT MARKER: FindRoot accepts a package.json carrying a `workspaces` array as a " +
			"workspace root, beside putnami.workspace.json. It predates the workspace config and " +
			"exists so `putnami` works in an npm monorepo before anything is configured. It goes " +
			"when first-use bootstrap can create the workspace config instead of inferring it",
	},
}

// tokenizeIdentifier splits text on non-alphanumeric runes and camelCase
// boundaries and lowercases the result.
func tokenizeIdentifier(text string) []string {
	var words []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			words = append(words, strings.ToLower(current.String()))
			current.Reset()
		}
	}
	runes := []rune(text)
	for i, r := range runes {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])):
			flush()
			current.WriteRune(r)
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return words
}

// matchProviderVocabulary returns the terms text names, if any.
func matchProviderVocabulary(text string) []string {
	lower := strings.ToLower(text)
	words := make(map[string]bool)
	for _, w := range tokenizeIdentifier(text) {
		words[w] = true
	}
	var hits []string
	for _, term := range providerVocabulary {
		if strings.ContainsAny(term, "._-/") {
			if strings.Contains(lower, term) {
				hits = append(hits, term)
			}
			continue
		}
		if words[term] {
			hits = append(hits, term)
		}
	}
	return hits
}

// providerVocabularySite is one place a core file names a provider.
type providerVocabularySite struct {
	file  string
	text  string
	terms []string
}

// scanProviderVocabulary walks the scoped production ASTs of every module in
// cliModules and reports every string literal and declared identifier naming a
// provider term. Comments are not visited at all.
//
// Provider knowledge is a property of the CODE, not of the module that happens
// to store it, so when the pure data model was lifted out of tooling/cli into
// go.putnami.dev/cli/model the scan had to follow it: a `docker` spelling that
// walked across a module boundary has not stopped naming a provider in core,
// and a ratchet that lost sight of it would report an invariant nothing guards.
// cliModules is where the module list lives, shared with the structural pins,
// so the next move is one entry rather than a second list to keep in step.
//
// Each module is parsed by the same walk the single-module ratchets use, so a
// module whose directory moved or emptied fails loudly there rather than
// silently contributing nothing here.
func scanProviderVocabulary(t *testing.T) []providerVocabularySite {
	t.Helper()

	_, files := moduleProductionASTs(t, moduleRoot(t))

	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	var sites []providerVocabularySite
	for _, rel := range rels {
		inScope := false
		for _, scope := range providerVocabularyScopes {
			if strings.HasPrefix(rel, scope) {
				inScope = true
				break
			}
		}
		if !inScope {
			continue
		}
		ast.Inspect(files[rel], func(n ast.Node) bool {
			var text string
			switch node := n.(type) {
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				unquoted, err := strconv.Unquote(node.Value)
				if err != nil {
					return true
				}
				text = unquoted
			case *ast.Ident:
				// Only DECLARED names: a selector like filepath.Walk would
				// otherwise report the package it reaches for rather than
				// anything this file owns.
				if node.Obj == nil {
					return true
				}
				text = node.Name
			default:
				return true
			}
			if terms := matchProviderVocabulary(text); len(terms) > 0 {
				sites = append(sites, providerVocabularySite{file: rel, text: text, terms: terms})
			}
			return true
		})
	}
	return sites
}

func describeSites(sites []providerVocabularySite) string {
	var lines []string
	for _, site := range sites {
		lines = append(lines, "      "+strconv.Quote(site.text)+"  → "+strings.Join(site.terms, ", "))
	}
	return strings.Join(lines, "\n")
}

// TestNoProviderVocabularyInCore is the anti-reentry ratchet: an
// unlisted core file may not name a provider, a package manager, a toolchain,
// Docker or Postgres in code.
func TestNoProviderVocabularyInCore(t *testing.T) {
	t.Parallel()
	byFile := map[string][]providerVocabularySite{}
	for _, site := range scanProviderVocabulary(t) {
		byFile[site.file] = append(byFile[site.file], site)
	}

	names := make([]string, 0, len(byFile))
	for file := range byFile {
		names = append(names, file)
	}
	sort.Strings(names)

	for _, file := range names {
		if _, allowed := providerVocabularyAllowlist[file]; allowed {
			continue
		}
		t.Errorf("%s names provider vocabulary in CODE (not in a comment):\n%s\n"+
			"  This program moved runtimes, language manifests, toolchains, Docker and Postgres out of "+
			"the engine, the job layer and the workspace model. A provider name back inside one of "+
			"them is the regression the epic exists to prevent — express it as a typed task the "+
			"owning extension answers, or, if it genuinely cannot be, add a providerVocabularyAllowlist "+
			"entry stating why.",
			file, describeSites(byFile[file]))
	}
}

// TestProviderVocabularyAllowlistOnlyShrinks keeps the allowlist an
// INVENTORY rather than a graveyard or a budget: a listed file that stopped
// naming providers loses its entry, and a listed file may not grow new naming
// sites under cover of an entry written for the ones it already had.
func TestProviderVocabularyAllowlistOnlyShrinks(t *testing.T) {
	t.Parallel()
	byFile := map[string][]providerVocabularySite{}
	for _, site := range scanProviderVocabulary(t) {
		byFile[site.file] = append(byFile[site.file], site)
	}

	listed := make([]string, 0, len(providerVocabularyAllowlist))
	for file := range providerVocabularyAllowlist {
		listed = append(listed, file)
	}
	sort.Strings(listed)

	for _, file := range listed {
		residue := providerVocabularyAllowlist[file]
		sites := byFile[file]
		if len(sites) == 0 {
			t.Errorf("allowlisted file %s no longer names any provider — a slice removed the "+
				"residue; remove the entry with it so the list stays an inventory.\n  It said: %s",
				file, residue.why)
			continue
		}
		if len(sites) > residue.sites {
			t.Errorf("%s holds %d provider-naming sites, pinned at %d — %d added:\n%s\n"+
				"  The entry covers the residue that was there, not new branches beside it: %s",
				file, len(sites), residue.sites, len(sites)-residue.sites, describeSites(sites), residue.why)
		}
	}
}

// epicDQuarantineFile is the one adapter this program deliberately left in place.
const epicDQuarantineFile = "internal/extension/discovery.go"

// epicDQuarantineTokens are the literal tokens the root-package.json adapter is
// built from. All of them must still appear in the quarantined file: together
// they are its shape — read the root package.json, take devDependencies, look
// each one up under node_modules.
// The function token is spelled with its parenthesis so a RENAME is caught: a
// bare "extractDevDeps" would still match "extractDevDepsRenamed", and a pin
// that survives the thing it pins being renamed is not a pin.
var epicDQuarantineTokens = []string{
	`"package.json"`,
	`"node_modules"`,
	`"devDependencies"`,
	"extractDevDeps(",
}

// epicDQuarantineExclusiveTokens are the tokens that identify the adapter
// UNIQUELY: reading an npm manifest's devDependencies as a source of extension
// identity. They may appear nowhere else in production, which is what
// "quarantined" means here — one reader, in one file, where epic D already
// knows to look.
//
// The other two tokens above are deliberately NOT exclusive: `"package.json"`
// and `"node_modules"` appear in scan exclusions, template scaffolding, cache
// hashing and the workspace-root marker, none of which read npm membership.
// Claiming exclusivity for them would make this pin fire on unrelated work and
// be deleted within a slice.
var epicDQuarantineExclusiveTokens = []string{
	`"devDependencies"`,
	"extractDevDeps(",
}

// TestQuarantineAdapterIsPinned pins the quarantine two ways.
//
// The epic's contract is that core stops knowing about providers. This adapter
// is the one exception it did not take: reading the root package.json's
// devDependencies to find installed extensions is npm membership, and deleting
// it would break every workspace that installs extensions that way. Epic D owns
// the replacement.
//
// A quarantine that is not pinned is just a TODO. This test fails if the block
// is removed or renamed here (an untracked deletion, which epic D must do
// deliberately and with a migration) and if any of its tokens shows up in
// another production file (the adapter spreading while it waits).
func TestQuarantineAdapterIsPinned(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	production, _ := goSources(t, root)

	for _, tok := range epicDQuarantineTokens {
		if count, _ := countTokenLines(t, root, []string{epicDQuarantineFile}, tok); count == 0 {
			t.Errorf("%s no longer contains %s — the Epic-D quarantine (the root package.json "+
				"devDependencies adapter) moved or was deleted. This test does not own its removal: deleting "+
				"it breaks every workspace that installs extensions as npm packages. If epic D did "+
				"remove it, delete this pin and its providerVocabularyAllowlist entry in the same "+
				"change.", epicDQuarantineFile, tok)
		}
	}

	var others []string
	for _, rel := range production {
		if rel != epicDQuarantineFile {
			others = append(others, rel)
		}
	}
	for _, tok := range epicDQuarantineExclusiveTokens {
		if count, sites := countTokenLines(t, root, others, tok); count > 0 {
			t.Errorf("%s appears outside the quarantine (%s):\n%s\n"+
				"  The root package.json adapter is contained on purpose. A second reader of npm "+
				"membership is a second thing epic D has to find and delete.",
				tok, epicDQuarantineFile, indentSites(sites))
		}
	}
}

// TestEngineHoldsNoCommandEdge pins the anti-reentry edge inversion.
//
// internal/engine is the package every adapter routes through. Its single
// import of internal/commands — the production doctor gate — was enough to
// force `putnami mcp`'s spawn point out of internal/commands (see
// internal/cli/mcp_serve.go) and to make commands.WorkspaceJobRunner an
// injected callback rather than a call. This program inverted it: the gate is
// engine.Request.Preflight, supplied by each adapter.
//
// Restoring the import would restore all of that, silently. Test files are not
// counted — engine's preflight test deliberately drives the REAL
// commands.DoctorPreflight through the seam, which is how the injection is
// proven compatible with its one production implementation.
func TestEngineHoldsNoCommandEdge(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	production, _ := goSources(t, root)

	var engineFiles []string
	for _, rel := range production {
		if strings.HasPrefix(rel, "internal/engine/") {
			engineFiles = append(engineFiles, rel)
		}
	}
	if len(engineFiles) == 0 {
		t.Fatal("found no production files under internal/engine — the walk root is wrong")
	}

	count, sites := countTokenLines(t, root, engineFiles, `"go.putnami.dev/tooling/cli/internal/commands"`)
	if count != 0 {
		t.Errorf("internal/engine imports internal/commands again (%d site(s)):\n%s\n"+
			"  This program inverted that edge — the production doctor gate is injected as "+
			"engine.Request.Preflight, so the engine depends on the workspace model, the job layer "+
			"and the protocols and nothing else. Reinstating the import re-closes "+
			"mcp → engine → commands → mcp and turns commands' injected seams back into "+
			"consequences of a cycle. Inject what you need instead.",
			count, indentSites(sites))
	}
}
