package cli

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

// The core-isolation ratchets: three hard pins, set before any later change
// lands so every movement is a reviewed edit here rather than a silent
// increment.
//
// This program's whole shape is a subtraction: core keeps command dispatch, typed
// plans, execution, caching, canonical identity, security boundaries, events
// and results; everything provider-shaped (runtimes, metadata probes, native
// caches, preflight policy, infra defaults, test environments) moves behind
// exactly three generic lifecycle primitives. These pins are what keeps that
// subtraction subtracted — the same reasoning as structural_baseline_test.go
// and complexity_ceilings_test.go, extended to this program's own axes.

func TestNoFourthLifecyclePrimitive(t *testing.T) {
	t.Parallel()
	const want = 3
	if len(extensionproto.ValidLifecyclePrimitives) != want {
		t.Fatalf("protocol registry lists %d lifecycle primitives, pinned at %d — the closed "+
			"list moved. Core gains exactly three generic lifecycle primitives; everything "+
			"else is expressed as typed tasks. A fourth primitive is a program-level decision, not "+
			"an incremental one.", len(extensionproto.ValidLifecyclePrimitives), want)
	}
	expected := []extensionproto.LifecyclePrimitiveID{
		extensionproto.LifecyclePrimitiveInvocation,
		extensionproto.LifecyclePrimitiveRuntime,
		extensionproto.LifecyclePrimitiveWorkspace,
	}
	for i, primitive := range extensionproto.ValidLifecyclePrimitives {
		if primitive != expected[i] {
			t.Errorf("protocol primitive %d is %q, want %q — registry order or identity drifted",
				i, primitive, expected[i])
		}
	}
}

// providerTerms is the provider vocabulary that must not name a NEW core
// package. Short language tokens match as prefix or suffix of the package
// directory name; longer terms match anywhere in it.
var providerTerms = []string{
	"typescript", "python", "docker", "postgres", "node", "bun",
	"go", "ts", "py", "pg", "uv", "npm", "oci",
}

// providerPackageAllowlist names the packages the term scan must not fail on.
// Historically it held the provider-specific packages that existed TODAY and
// were scheduled for deletion by this program itself; no package of that kind may
// join it, because this program moves provider knowledge into extensions and a new
// provider-shaped core package is the exact regression the program exists to
// prevent. Entries of that kind LEAVE when a later change deletes them.
//
// It holds no such package now. internal/testinfra — the docker-CLI Postgres
// provisioner behind `putnami test` — was the last one, and an earlier change
// deleted it along with internal/jobs/test_infra.go: database test environments
// are an extension task over the generic invocation primitive
// (go.putnami.dev/sdk/extension/dbtestenv).
//
// The only other admissible entry is a SUBSTRING FALSE POSITIVE — the scan
// matches short language tokens as a prefix or suffix of the directory name, so
// a package whose name merely ends in one of them is caught by spelling, not by
// meaning. Such an entry must say which term it collides with and why the
// package is provider-neutral, so a reviewer can check the claim in one line
// instead of trusting the map.
var providerPackageAllowlist = map[string]string{
	// Substring false positive: "agentartifacts" ends in "ts". The package
	// materializes lock-pinned agent-workflow files (.agents/**, .claude/**)
	// into a workspace; it names no language, runtime, package manager or
	// image format, and its inputs are a lock entry and a content manifest.
	"internal/agentartifacts": "ends in the \"ts\" token by spelling; agent-workflow materialization is provider-neutral",
}

func matchesProviderTerm(pkgName string) string {
	for _, term := range providerTerms {
		if len(term) <= 3 {
			if strings.HasPrefix(pkgName, term) || strings.HasSuffix(pkgName, term) {
				return term
			}
		} else if strings.Contains(pkgName, term) {
			return term
		}
	}
	return ""
}

func TestNoNewProviderSpecificCorePackage(t *testing.T) {
	t.Parallel()
	for _, pkg := range internalProductionPackages(t) {
		if _, allowed := providerPackageAllowlist[pkg]; allowed {
			continue
		}
		if term := matchesProviderTerm(filepath.Base(pkg)); term != "" {
			t.Errorf("package %s matches provider term %q and is not in the allowlist — this "+
				"program moves provider knowledge into extensions; a new provider-specific core "+
				"package reverses that. If the name is a false positive, say so in "+
				"providerPackageAllowlist with the reason.", pkg, term)
		}
	}

	// The allowlist itself only shrinks: a listed package that no longer exists
	// was deleted by a later change, and its entry must go with it.
	existing := make(map[string]bool)
	for _, pkg := range internalProductionPackages(t) {
		existing[pkg] = true
	}
	for pkg := range providerPackageAllowlist {
		if !existing[pkg] {
			t.Errorf("allowlisted package %s no longer exists — it was deleted; remove the "+
				"entry so the allowlist stays an inventory, not a graveyard", pkg)
		}
	}
}

// TestTheCoreExpandsNoInputSelector keeps input selectors extension-owned: the
// extension that lists a selector in runtime.inputSelectors answers what it
// reads, through the workspace protocol's input-resolve call, and the core
// asks. No production file of the CLI imports the SDK's goembed reader, and no
// string literal in one names a go-embed selector, so the core cannot expand
// one by a rule of its own.
func TestTheCoreExpandsNoInputSelector(t *testing.T) {
	t.Parallel()
	fset, files := moduleProductionASTs(t, moduleRoot(t))
	for rel, file := range files {
		for _, spec := range file.Imports {
			if path, err := strconv.Unquote(spec.Path.Value); err == nil && path == "go.putnami.dev/sdk/extension/goembed" {
				t.Errorf("%s imports %s: the Go extension expands its selectors; the core asks it", rel, path)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "go-embed:") {
				t.Errorf("%s:%d names %s: the core names no input selector", rel, fset.Position(lit.Pos()).Line, lit.Value)
			}
			return true
		})
	}
}

// internalPackageCountCeiling pins the number of internal packages holding
// production Go files. Ceiling-only: this program DELETES packages, so
// shrinkage is success. Growth is the sprawl the count exists to catch — a new
// package is a new surface, and the first question about any new surface is
// "why is this not extension-owned?".
//
// A package earns its place when it answers that question and one more: why no
// existing package can host it. The admitted answers so far are a leaf that two
// consumers on opposite sides of an import boundary share, a boundary whose
// tests must not rerun when the rest of its parent changes, a vertical of the
// command layout (vertical_isolation_ratchet_test.go keeps verticals apart),
// and a test-only support package, which takes a *testing.T and so cannot sit
// in a production package. Each adds no vertical beyond its own and no engine
// stage.
//
// The ceiling follows the tree down: a deletion lowers it in the same change,
// because a ceiling above reality is standing permission nobody reviewed. A
// change that raises it says in its commit message which package it adds and
// why no existing one can host it. This comment changes only when the rule
// changes; the history of every move lives in Git.
const internalPackageCountCeiling = 74

func TestInternalPackageCountCeiling(t *testing.T) {
	t.Parallel()
	pkgs := internalProductionPackages(t)
	if len(pkgs) > internalPackageCountCeiling {
		t.Errorf("internal/ holds %d production packages, ceiling %d — %d added.\n  Packages:\n    %s",
			len(pkgs), internalPackageCountCeiling, len(pkgs)-internalPackageCountCeiling,
			strings.Join(pkgs, "\n    "))
	}
}

// internalProductionPackages returns the sorted module-relative directories
// under internal/ that contain at least one production .go file.
func internalProductionPackages(t *testing.T) []string {
	t.Helper()
	root := moduleRoot(t)
	production, _ := goSources(t, root)
	seen := make(map[string]bool)
	for _, rel := range production {
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "internal" || strings.HasPrefix(dir, "internal/") {
			seen[dir] = true
		}
	}
	pkgs := make([]string, 0, len(seen))
	for dir := range seen {
		pkgs = append(pkgs, dir)
	}
	sort.Strings(pkgs)
	return pkgs
}
