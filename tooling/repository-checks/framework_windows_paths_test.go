package repositorychecks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// A static guard over the product sources of every TypeScript framework
// package, for three Windows rules that tests on Linux and macOS cannot
// observe:
//
//   - A native path is tested with `isAbsolutePath`, `dirname` or the host
//     separator, never against '/'. The guard reads a camelCase name ending in
//     `Path`, `Dir`, `File`, `Root` or `Directory` as a native path. A name
//     that holds a URL path or a slash-separated wire path goes in
//     windowsSlashFormNames. A bare `path` is out of reach: most are URL
//     paths.
//   - A rename goes through `@putnami/runtime/robustio`, which waits out the
//     moment a Windows reader holds the target open. `@putnami/spectest`
//     cannot depend on the runtime; its rename is content-addressed and
//     tolerates a failure.
//   - Only `getProjectRoot` reads `PWD`, which Windows shells do not maintain.
//
// The guard reads the sources as text, so it lives here rather than in a
// framework package, whose tests would then run again for a change to any
// other package's sources.

// frameworkDir is the TypeScript framework root, relative to the repository.
const frameworkDir = "typescript/framework/"

// windowsSlashFormNames are identifiers that hold a URL path or a
// slash-separated wire path, never a native one.
var windowsSlashFormNames = map[string]bool{
	"pageEvidencePath": true, "remainingPath": true, "routePath": true, "urlPath": true,
}

// windowsRenameOwners and windowsPWDOwners are the files that own a rule's
// exception, relative to typescript/framework.
var (
	windowsRenameOwners = map[string]bool{"runtime/src/robustio/index.ts": true, "spectest/src/index.ts": true}
	windowsPWDOwners    = map[string]bool{"utils/src/server/workspace.utils.ts": true}
)

var (
	nativePathAgainstSlash = regexp.MustCompile(
		`\b([A-Za-z_$][\w$]*[a-z0-9](?:Path|Dir|File|Root|Directory))\)?\.(?:startsWith|endsWith|lastIndexOf|indexOf|split|includes)\(\s*['"` + "`" + `]/['"` + "`" + `]`)
	renameImport = regexp.MustCompile(`import\s*\{[^}]*\brename(?:Sync)?\b[^}]*\}\s*from\s*['"](?:node:)?fs(?:/promises)?['"]`)
	renameMember = regexp.MustCompile(`\b(?:fs|fsp|fsPromises|promises)\.rename(?:Sync)?\(`)
	pwdRead      = regexp.MustCompile(`\benv(?:\.PWD\b|\[\s*['"]PWD['"]\s*\])`)
	// slashLiteral is a quoted '/': nativePathAgainstSlash cannot match a
	// text without one.
	slashLiteral = regexp.MustCompile(`['"` + "`" + `]/['"` + "`" + `]`)
	whitespace   = regexp.MustCompile(`\s+`)
)

// windowsViolations returns the rule violations in text, the content of file
// (relative to typescript/framework), one `file:line: rule` entry each.
func windowsViolations(file, text string) []string {
	var found []string
	if !windowsRenameOwners[file] && renameImport.MatchString(whitespace.ReplaceAllString(text, " ")) {
		found = append(found, file+": imports rename from node:fs; use robustRename from @putnami/runtime/robustio")
	}
	for index, line := range strings.Split(text, "\n") {
		at := fmt.Sprintf("%s:%d", file, index+1)
		for _, match := range nativePathAgainstSlash.FindAllStringSubmatch(line, -1) {
			if !windowsSlashFormNames[match[1]] {
				found = append(found, fmt.Sprintf("%s: tests native path %s against '/'; use isAbsolutePath, dirname or sep", at, match[1]))
			}
		}
		if !windowsRenameOwners[file] && renameMember.MatchString(line) {
			found = append(found, at+": calls rename from node:fs; use robustRename from @putnami/runtime/robustio")
		}
		if !windowsPWDOwners[file] && pwdRead.MatchString(line) {
			found = append(found, at+": reads PWD; use getProjectRoot")
		}
	}
	return found
}

// windowsMayViolate reports whether text holds a literal one of the rules
// needs: a quoted '/', `rename` or `PWD`. Most files hold none, and they skip
// the line scan.
func windowsMayViolate(text string) bool {
	return slashLiteral.MatchString(text) || strings.Contains(text, "rename") || strings.Contains(text, "PWD")
}

// frameworkProductSources lists the product sources of the framework
// packages, relative to typescript/framework: the `.ts`, `.tsx`, `.mts` and
// `.mjs` files under a package's `src/` or `bin/`, without generated files or
// dependencies, from the candidate cut this project's inputs key.
func frameworkProductSources(t *testing.T, root string) []string {
	t.Helper()
	paths, err := candidatePathsOnly(root)
	if err != nil {
		t.Fatalf("list the candidate cut: %v", err)
	}
	var sources []string
	for _, path := range paths {
		rel, ok := strings.CutPrefix(path, frameworkDir)
		if !ok {
			continue
		}
		segments := strings.Split(rel, "/")
		if len(segments) < 3 || (segments[1] != "src" && segments[1] != "bin") {
			continue
		}
		if slices.ContainsFunc(segments, func(segment string) bool {
			return strings.HasPrefix(segment, ".") || segment == "node_modules"
		}) || strings.Contains(rel, ".gen.") {
			continue
		}
		switch filepath.Ext(rel) {
		case ".ts", ".tsx", ".mts", ".mjs":
			sources = append(sources, rel)
		}
	}
	return sources
}

func TestFrameworkSourcesFollowTheWindowsPathRules(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	sources := frameworkProductSources(t, root)
	if len(sources) <= 100 {
		t.Fatalf("found %d framework product sources; a walk that finds so few is broken", len(sources))
	}
	for _, file := range sources {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(frameworkDir+file)))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if text := string(data); windowsMayViolate(text) {
			for _, violation := range windowsViolations(file, text) {
				t.Error(violation)
			}
		}
	}
}

func TestWindowsPathRulesReportEachRule(t *testing.T) {
	t.Parallel()
	source := strings.Join([]string{
		"import { mkdir, rename } from 'node:fs/promises';",
		"const importPath = this.reactApplicationPath.startsWith('/') ? a : b;",
		"const dir = fullPath.slice(0, fullPath.lastIndexOf('/'));",
		"if (routePath.startsWith('/')) return routePath;",
		"fs.renameSync(from, to);",
		"const cwd = process.env['PWD'];",
	}, "\n")
	want := []string{
		"web/src/x.ts: imports rename from node:fs; use robustRename from @putnami/runtime/robustio",
		"web/src/x.ts:2: tests native path reactApplicationPath against '/'; use isAbsolutePath, dirname or sep",
		"web/src/x.ts:3: tests native path fullPath against '/'; use isAbsolutePath, dirname or sep",
		"web/src/x.ts:5: calls rename from node:fs; use robustRename from @putnami/runtime/robustio",
		"web/src/x.ts:6: reads PWD; use getProjectRoot",
	}
	if got := windowsViolations("web/src/x.ts", source); !slices.Equal(got, want) {
		t.Errorf("violations = %q\nwant %q", got, want)
	}
	// Each line breaks a rule, so none may be skipped before the line scan.
	for _, line := range strings.Split(source, "\n") {
		if !windowsMayViolate(line) {
			t.Errorf("%q is skipped before the line scan", line)
		}
	}
}

func TestWindowsPathRulesKeepEachExceptionToItsOwner(t *testing.T) {
	t.Parallel()
	if got := windowsViolations("runtime/src/robustio/index.ts", "import { rename, rm } from 'node:fs/promises';"); len(got) != 0 {
		t.Errorf("the robustio owner reports %q", got)
	}
	if got := windowsViolations("utils/src/server/workspace.utils.ts", "const pwd = env['PWD'];"); len(got) != 0 {
		t.Errorf("the PWD owner reports %q", got)
	}
	if got := windowsViolations("storage/src/x.ts", "import {\n  rename,\n} from 'node:fs/promises';"); len(got) != 1 {
		t.Errorf("a multi-line rename import reports %q, want one violation", got)
	}
}
