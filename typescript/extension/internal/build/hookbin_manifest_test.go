package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/typescript/extension/internal/project"
)

// TestHookReferencedBinsAreDeclared enforces the invariant that every bin
// script referenced by a package's putnami.extension.json hooks is declared in
// that package's package.json "bin" map.
//
// Why it matters (regression guard for undeclared hook bins): resolveBinEntrypoints
// (transpile.go) transpiles ONLY the bin/*.ts files listed in the "bin" map. A
// hook runs `bun run {extensionRoot}/bin/<name>`, which in a published package
// resolves to the transpiled bin/<name>.js. Meanwhile the declaration emit
// (types.go includes bin/**/*.ts) still ships bin/<name>.d.ts, so a hook bin
// that is NOT declared ships its types but not its runnable .js — every
// consumer's build/deploy then dies at that hook with "Module not found". This
// is exactly what happened to @putnami/application: its configExtract hook
// referenced bin/config-extract but the bin map declared only bin/generate, so
// config-extract.js never shipped and every TS deploy failed.
//
// The check walks the real repo manifests (not a fixture) so the actually
// shipped packages are guarded, and reads only JSON, so it runs in
// milliseconds with no bun/transpile/pack/network.
func TestHookReferencedBinsAreDeclared(t *testing.T) {
	root, ok := repoRoot()
	if !ok {
		t.Skip("repo root (go.work) not found walking up from test dir; out-of-repo test run")
	}

	manifests := findExtensionManifests(t, root)
	if len(manifests) == 0 {
		t.Fatal("no putnami.extension.json manifests found — repo walk is broken")
	}

	checked := 0
	for _, manifestPath := range manifests {
		projectDir := filepath.Dir(manifestPath)

		// Only npm-transpiled packages have a sibling package.json. Go/python/
		// shell extensions invoke compiled launcher scripts and never hit the
		// bun transpile path, so they cannot exhibit this bug.
		pkg := project.ReadPackageJSONSafe(filepath.Join(projectDir, "package.json"))
		if pkg == nil {
			continue
		}

		declared := declaredBinRefs(pkg)

		for _, ref := range hookReferencedBins(t, manifestPath) {
			// Only .ts/.tsx hook bins need transpiling+declaring. A bare
			// launcher script (no .ts source) ships as-is and is irrelevant.
			if !binSourceExists(projectDir, ref) {
				continue
			}
			checked++
			if declared[ref] {
				continue
			}
			rel, _ := filepath.Rel(root, manifestPath)
			name := filepath.Base(ref)
			t.Errorf(
				"%s: a hook references {extensionRoot}/%s but %s.ts is not declared in package.json %q.\n"+
					"resolveBinEntrypoints only transpiles declared bins, so %s.js will not be published and the hook will fail at runtime with \"Module not found\" .\n"+
					"Fix: add an entry like %q: %q to the bin map.",
				rel, ref, ref, "bin",
				ref,
				"<pkg-prefix>-"+name, "./"+ref+".ts",
			)
		}
	}

	// Sentinel: if the matcher silently stops recognizing hook bins (e.g. the
	// {extensionRoot}/bin/ template changes), every package would "pass"
	// vacuously. At least @putnami/application's generate + config-extract must
	// be exercised.
	if checked == 0 {
		t.Fatal("guard checked zero hook bins — the repo walk or the {extensionRoot}/bin/ matcher is broken")
	}
}

// repoRoot walks up from the test working directory to the directory containing
// go.work, which marks the monorepo root. Returns false when no go.work is
// found (the test is being run outside the repo).
func repoRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if project.FileExists(filepath.Join(dir, "go.work")) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// manifestWalkSkipDirs are non-hidden directories that never hold first-party
// source manifests but may contain installed-extension or vendored copies that
// would pollute the walk. Hidden directories (.git, .gen, .claude, .putnami,
// .context, …) are skipped separately by the dot-prefix rule.
var manifestWalkSkipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
	"dist":         true,
	"out":          true,
}

// findExtensionManifests returns every putnami.extension.json under root,
// skipping junk and hidden directories so installed/worktree copies don't
// pollute the result.
func findExtensionManifests(t *testing.T, root string) []string {
	t.Helper()
	var manifests []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || manifestWalkSkipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "putnami.extension.json" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo from %s: %v", root, err)
	}
	return manifests
}

const extensionRootPrefix = "{extensionRoot}/"

// hookReferencedBins parses a putnami.extension.json and returns the
// package-relative, extension-stripped paths of every bin script referenced by
// a hook arg of the form "{extensionRoot}/bin/<name>" (e.g. "bin/config-extract").
func hookReferencedBins(t *testing.T, manifestPath string) []string {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v", manifestPath, err)
	}
	var manifest struct {
		Hooks map[string]struct {
			Args []string `json:"args"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parsing %s: %v", manifestPath, err)
	}
	var refs []string
	seen := map[string]bool{}
	for _, hook := range manifest.Hooks {
		for _, arg := range hook.Args {
			ref, ok := hookBinRef(arg)
			if !ok || seen[ref] {
				continue
			}
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	return refs
}

// hookBinRef extracts the package-relative bin path from a hook arg that
// references "{extensionRoot}/bin/...", stripping the script extension. Returns
// ("", false) for any arg that does not point under the package's bin/ dir.
func hookBinRef(arg string) (string, bool) {
	if !strings.HasPrefix(arg, extensionRootPrefix) {
		return "", false
	}
	rel := strings.TrimPrefix(arg, extensionRootPrefix)
	if !strings.HasPrefix(rel, "bin/") {
		return "", false
	}
	return stripScriptExt(rel), true
}

// declaredBinRefs returns the set of package-relative, extension-stripped bin
// paths declared in package.json's "bin" field (map or single string).
func declaredBinRefs(pkg *project.PackageJSON) map[string]bool {
	refs := map[string]bool{}
	for _, v := range pkg.GetBinMap() {
		if v != "" {
			refs[normalizeBinPath(v)] = true
		}
	}
	if s := pkg.GetBinString(); s != "" {
		refs[normalizeBinPath(s)] = true
	}
	return refs
}

// binSourceExists reports whether a transpilable source file exists for the
// given package-relative bin ref (e.g. "bin/config-extract" → bin/config-extract.ts).
func binSourceExists(projectDir, ref string) bool {
	for _, ext := range []string{".ts", ".tsx"} {
		if project.FileExists(filepath.Join(projectDir, filepath.FromSlash(ref)+ext)) {
			return true
		}
	}
	return false
}

// normalizeBinPath strips a leading "./" and the script extension from a bin
// map value so it compares equal to a hook's extensionless reference.
func normalizeBinPath(p string) string {
	return stripScriptExt(strings.TrimPrefix(p, "./"))
}

// stripScriptExt removes a recognized TypeScript/JavaScript extension.
func stripScriptExt(p string) string {
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
		if base, ok := strings.CutSuffix(p, ext); ok {
			return base
		}
	}
	return p
}
