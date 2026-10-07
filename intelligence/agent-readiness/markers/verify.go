package markers

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/internal/ci"
	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
	"go.putnami.dev/intelligence/agent-readiness/internal/scope"
)

// testRuns are the invocations that show CI runs tests.
var testRuns = []string{
	"go test", "pytest", "cargo test", "vitest", "jest", "playwright test", "rspec", "mvn test", "mvn verify",
	"gradle test", "gradlew test", "putnami test", "npm test", "npm run test", "pnpm test", "pnpm run test",
	"yarn test", "bun test", "make test", "just test", "task test", "turbo run test", "turbo test",
	"nx affected", "nx run-many", "bazel test", "bazelisk test",
}

func (c *computation) testJobs() []ci.Config {
	return ci.Runs(c.configs, append(c.fragments(true), testRuns...)...)
}

func (c *computation) tests(area int) contract.Marker {
	const id = "verify.tests"
	var sample []string
	for _, file := range c.Files {
		if IsTest(file.Path) && !scope.Skipped(file.Path) && (c.inArea(file.Path, area) || c.mirrors(file.Path, area)) {
			sample = append(sample, file.Path)
			if len(sample) == maxSample {
				break
			}
		}
	}
	command := "git log --since=90.days --no-merges --name-only --format=%H"
	if area >= 0 {
		command += " -- " + c.Layout.Areas[area].Path
	}
	activity := c.activity(area)
	value := ratio(activity.CodeWithTest, activity.CodeCommits)
	if len(sample) == 0 {
		return marker(id, contract.MarkerAbsent, value, command, nil)
	}
	if jobs := c.testJobs(); len(jobs) > 0 {
		return marker(id, contract.MarkerEnforced, value, command, append(sample[:min(len(sample), maxSample-1)], jobs[0].Path))
	}
	return marker(id, contract.MarkerExists, value, command, sample)
}

// mirrors reports whether a test lies in a test tree that tests a code area
// from outside it, as spec/ tests app/ in a Rails application: the tree's
// parent directory holds the area.
func (c *computation) mirrors(file string, area int) bool {
	if area < 0 || c.Layout.Areas[area].Role == contract.AreaTests || c.Layout.Areas[area].Role == contract.AreaDocs {
		return false
	}
	tree, ok := areas.TestTree(file)
	if !ok {
		return false
	}
	parent := path.Dir(tree)
	return parent == "." || strings.HasPrefix(c.dir(area), parent+"/")
}

// staticConfigs are the type checker, linter and formatter configurations
// the collector recognizes by name.
var staticConfigs = []string{
	".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json", "biome.json", "biome.jsonc",
	"ruff.toml", ".ruff.toml", "mypy.ini", ".mypy.ini", ".flake8", "pyrightconfig.json", "clippy.toml",
	".rubocop.yml", "rustfmt.toml", ".rustfmt.toml", "deno.json", "putnami.workspace.json",
	".pylintrc", "pylintrc", ".pre-commit-config.yaml",
}

// pyprojectChecks are the pyproject.toml tables that configure a check.
var pyprojectChecks = []string{"[tool.mypy]", "[tool.ruff", "[tool.black]", "[tool.pylint", "[tool.pyright]"}

// mavenChecks are the Maven plugins, named by artifact id, that lint,
// format-check or statically analyze Java: Spotless, Checkstyle, PMD,
// SpotBugs, and Error Prone, which runs as a compiler plugin.
var mavenChecks = []string{
	"spotless-maven-plugin", "maven-checkstyle-plugin", "maven-pmd-plugin", "spotbugs-maven-plugin", "error_prone_core",
}

// mavenConfig reports whether a file is a Maven project file the static
// checks read, at any depth. Vendored code and test data do not count.
func mavenConfig(file string) bool {
	return path.Base(file) == "pom.xml" && !scope.Skipped(file) && !areas.Fixture(path.Dir(file))
}

func datedMavenConfig(file string) bool {
	return mavenConfig(file) && strings.Count(file, "/") <= maxDatedConfigDepth
}

// staticConfig reports whether a file configures a static check, at any
// depth: a monorepo often configures its linter per package, not at the
// root. Vendored code and test data do not count.
func staticConfig(file string) bool {
	if scope.Skipped(file) || areas.Fixture(path.Dir(file)) {
		return false
	}
	base := path.Base(file)
	for _, name := range staticConfigs {
		if base == name {
			return true
		}
	}
	return isESLintConfig(base) || strings.HasPrefix(base, ".prettierrc") || (strings.HasPrefix(base, "tsconfig") && strings.HasSuffix(base, ".json"))
}

// maxDatedConfigDepth bounds which static configurations the history pass
// dates: the root's and the top two levels', where the oldest one lives.
const maxDatedConfigDepth = 2

func datedStaticConfig(file string) bool {
	return staticConfig(file) && strings.Count(file, "/") <= maxDatedConfigDepth
}

// staticRuns are the invocations that show CI runs a static check.
var staticRuns = []string{
	"lint", "tsc", "typecheck", "type-check", "golangci-lint", "go vet", "mypy", "ruff", "biome",
	"clippy", "pyright", "rubocop", "prettier --check", "flake8",
	"spotless:check", "checkstyle:check", "pmd:check", "spotbugs:check",
}

func (c *computation) staticChecks() contract.Marker {
	const id = "verify.static-checks"
	var configs []string
	for _, file := range c.Files {
		switch {
		case staticConfig(file.Path):
			configs = append(configs, file.Path)
		case path.Base(file.Path) == "pyproject.toml" && !scope.Skipped(file.Path) && mentionsAny(string(c.Contents[file.Path]), pyprojectChecks):
			configs = append(configs, file.Path)
		case mavenConfig(file.Path) && mentionsAny(string(c.Contents[file.Path]), mavenChecks):
			configs = append(configs, file.Path)
		}
	}
	if len(configs) == 0 {
		return marker(id, contract.MarkerAbsent, nil,
			"git ls-files -- '*tsconfig.json' '*.golangci.yml' '*.eslintrc*' '*eslint.config.*' '*biome.json' '*ruff.toml' '*mypy.ini'", nil)
	}
	// The shallowest configurations describe the whole repository; they come
	// first in the sample and the command.
	sort.SliceStable(configs, func(i, j int) bool {
		return strings.Count(configs[i], "/") < strings.Count(configs[j], "/")
	})
	command := fileList("git log --diff-filter=A --format=%cs --", configs)
	jobs := ci.Runs(c.configs, staticRuns...)
	if len(jobs) == 0 {
		return marker(id, contract.MarkerExists, nil, command, configs)
	}
	return marker(id, contract.MarkerEnforced, days(c.Now, c.enforcedSince(configs, jobs)), command, append(configs[:min(len(configs), maxSample-1)], jobs[0].Path))
}

// SignalPattern matches the lines that let a failing test pass: retries,
// skipped tests and focused tests. The collector greps HEAD with it once,
// restricted to SignalPathspecs, keeps the files SignalSource accepts, then
// counts the matching lines the window added to them.
const SignalPattern = `rerun-fails|--retries|retries:|t\.Skip\(|\.skip\(|\.only\(|pytest\.mark\.skip|@Disabled`

// SignalPathspecs leave documentation, vendored and generated directories
// out of the grep: a skip quoted in a guide hides no failure.
var SignalPathspecs = append([]string{".", ":(exclude,glob)**/*.md", ":(exclude,glob)**/*.mdx", ":(exclude,glob)**/*.rst"},
	scope.ExcludePathspecs()...)

// testRunnerConfigs are the base-name prefixes of the files that configure a
// test runner, where a retry count or a skip list applies to every test.
var testRunnerConfigs = []string{
	"jest.config.", "vitest.config.", "vitest.workspace.", "playwright.config.", "cypress.config.", "karma.conf.",
	"wdio.conf.", ".mocharc", "pytest.ini", "tox.ini", "conftest.py", "phpunit.xml", ".rspec",
}

// SignalSource reports whether a retry, a skip or a focused test in the file
// can hide a failing test: the file is a test, a test runner's configuration,
// or a CI configuration. The same words elsewhere are production code, such
// as "retries:" in an error message or IndexedDB's IDBKeyRange.only(, and
// hide nothing.
func SignalSource(file string) bool {
	if IsTest(file) || ci.Provider(file) != "" || ci.IncludeCandidate(file) || ci.ActionCandidate(file) {
		return true
	}
	base := path.Base(file)
	for _, prefix := range testRunnerConfigs {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

// SignalFiles returns the signal sources the HEAD grep matched, once each,
// sorted.
func SignalFiles(matches []gitrepo.Match) []string {
	seen := map[string]bool{}
	var files []string
	for _, match := range matches {
		if !seen[match.Path] && SignalSource(match.Path) {
			seen[match.Path] = true
			files = append(files, match.Path)
		}
	}
	sort.Strings(files)
	return files
}

// signalCommand prints the matching lines the window added to files, or
// under dir when the files do not fit the contract's bound, each with the
// lines around it: GuardedSkip reads them to leave out a skip that only a
// missing platform or dependency triggers, so the value is the printed "+"
// lines less the guarded skips. The pattern sits in a shell variable so that
// the command names it once; a long area path drops the Markdown exclusion,
// then the context.
func signalCommand(dir string, files []string) string {
	log := `git log --since=90.days -p -U3 --format= -G"$P" --`
	show := ` | grep -E -B2 -A3 "^\+.*($P)"`
	// The contract refuses a semicolon in a command, so && chains the two.
	prefix := "P='" + SignalPattern + "' && "
	if len(files) > 0 {
		listed := prefix + log
		for _, file := range files {
			if !safeLocation(file) {
				listed = ""
				break
			}
			listed += " " + file
		}
		if listed != "" && len(listed)+len(show) <= maxCommandBytes {
			return listed + show
		}
	}
	scopeSpec := "."
	if dir != "" {
		scopeSpec = strings.TrimSuffix(dir, "/")
	}
	for _, command := range []string{prefix + log + " " + scopeSpec + " ':!*.md'" + show, prefix + log + " " + scopeSpec + show} {
		if len(command) <= maxCommandBytes {
			return command
		}
	}
	return "git log --since=90.days -p -U3 --format= -G'" + SignalPattern + "' -- " + scopeSpec
}

// counts reports whether a HEAD match is one the signal rule keeps.
func (c *computation) counts(match gitrepo.Match) bool {
	content, ok := c.SignalContents[match.Path]
	if !ok {
		return true
	}
	return c.signal.Counts(match.Path, fileLines(content), match.Line-1)
}

func (c *computation) reliableSignal(area int) contract.Marker {
	const id = "verify.reliable-signal"
	var sample, counted, matched []string
	count, unguarded := 0, 0
	dir := c.dir(area)
	for file, added := range c.SignalsAdded {
		// The area's directory holds its nested areas, so the count does
		// too.
		if strings.HasPrefix(file, dir) && SignalSource(file) {
			count += added
		}
	}
	seen := map[string]bool{}
	for _, match := range c.Signals {
		if !strings.HasPrefix(match.Path, dir) || !SignalSource(match.Path) {
			continue
		}
		if c.counts(match) {
			unguarded++
			sample = append(sample, fmt.Sprintf("%s:%d", match.Path, match.Line))
		}
		if !seen[match.Path] {
			seen[match.Path] = true
			matched = append(matched, match.Path)
			if c.SignalsAdded[match.Path] > 0 {
				counted = append(counted, match.Path)
			}
		}
	}
	// The command names the files the count read: those with added lines,
	// or, when none has any, those that match at HEAD, so it prints 0.
	if len(counted) == 0 {
		counted = matched
	}
	command := signalCommand(dir, counted)
	value := number(float64(count))
	// The treatment is removing what HEAD still holds, so the state reads
	// HEAD: enforced once no retry, unguarded skip or focused test is left.
	// The value counts what the window added: L4 needs 90 days without one.
	switch {
	case len(c.testJobs()) == 0:
		return marker(id, contract.MarkerAbsent, value, command, sample)
	case unguarded > 0:
		return marker(id, contract.MarkerExists, value, command, sample)
	}
	return marker(id, contract.MarkerEnforced, value, command, nil)
}

// toolchainPins pin the version of a language toolchain.
var toolchainPins = map[string]bool{
	".nvmrc": true, ".node-version": true, ".tool-versions": true, ".python-version": true, "rust-toolchain": true,
	"rust-toolchain.toml": true, ".mise.toml": true, "mise.toml": true, ".terraform-version": true, "putnamiw": true,
}

// putnamiLock is the Putnami workspace lockfile. It pins the CLI version
// with the extensions and dependencies.
const putnamiLock = "putnami.lock.json"

// frozenInstalls are the install flags that refuse to change a lockfile.
var frozenInstalls = []string{
	"--frozen-lockfile", "npm ci", "--immutable", "--locked", "--frozen", "putnami install", "putnami ci",
}

var goToolchain = regexp.MustCompile(`(?m)^(toolchain\s+go[0-9]|go\s+[0-9]+\.[0-9]+\.[0-9]+)`)

// maxPinDepth is how deep a lockfile or a toolchain pin may sit: at the root
// or in a top-level directory that holds its own workspace.
const maxPinDepth = 1

func (c *computation) pinnedToolchain() contract.Marker {
	const id = "verify.pinned-toolchain"
	var lockfiles, pins []string
	goSum := false
	for _, file := range c.Files {
		if strings.Count(file.Path, "/") > maxPinDepth {
			if path.Base(file.Path) == "go.mod" && goToolchain.Match(c.Contents[file.Path]) {
				pins = append(pins, file.Path)
			}
			continue
		}
		switch {
		case lockfile(file.Path):
			lockfiles = append(lockfiles, file.Path)
			goSum = goSum || path.Base(file.Path) == "go.sum"
		case toolchainPins[path.Base(file.Path)]:
			pins = append(pins, file.Path)
		case path.Base(file.Path) == "go.mod" && goToolchain.Match(c.Contents[file.Path]):
			pins = append(pins, file.Path)
		}
	}
	// putnami.lock.json also pins the version of the Putnami CLI, the
	// workspace's toolchain.
	if slices.Contains(lockfiles, putnamiLock) {
		pins = append(pins, putnamiLock)
	}
	javaLocks, wrappers := c.javaLocks(), c.javaWrappersPresent()
	if len(javaLocks) > 0 {
		if jobs := c.javaBuildJobs(); len(jobs) > 0 {
			sample := append(append(append([]string{}, javaLocks...), wrappers...), jobs[0].Path)
			return marker(id, contract.MarkerEnforced, days(c.Now, c.enforcedSince(javaLocks, jobs)),
				fileList("git ls-files --", append(append([]string{}, javaLocks...), wrappers...)), sample)
		}
	}
	if len(lockfiles) == 0 {
		if java := append(append([]string{}, javaLocks...), wrappers...); len(java) > 0 {
			return marker(id, contract.MarkerExists, nil, fileList("git ls-files --", java), java)
		}
		return marker(id, contract.MarkerAbsent, nil,
			"git ls-files -- pnpm-lock.yaml package-lock.json yarn.lock go.sum Cargo.lock poetry.lock uv.lock '*/*.lock' .mvn/wrapper gradle/wrapper", nil)
	}
	// Root files first: they describe the whole repository.
	sort.SliceStable(lockfiles, func(i, j int) bool {
		return strings.Count(lockfiles[i], "/") < strings.Count(lockfiles[j], "/")
	})
	command := fileList("git ls-files --", append(append([]string{}, lockfiles...), pins...))
	sample := append(append([]string{}, lockfiles...), pins...)
	frozen := ci.Runs(c.configs, frozenInstalls...)
	if len(frozen) == 0 {
		// pnpm refuses to change its lockfile when the CI variable is set,
		// and CI providers set it: a plain pnpm install in CI is frozen
		// unless it opts out.
		frozen = ci.Unless(ci.Runs(c.configs, "pnpm install", "pnpm i "), "--no-frozen-lockfile")
	}
	if len(frozen) == 0 && goSum {
		// The go command verifies go.sum on every build: a Go CI job is a
		// frozen install.
		frozen = ci.Runs(c.configs, "go build", "go test", "go vet", "go mod download")
	}
	pinnedInCI := len(ci.Runs(c.configs, "node-version-file", "go-version-file", "python-version-file")) > 0
	if len(frozen) == 0 || (len(pins) == 0 && !pinnedInCI) {
		return marker(id, contract.MarkerExists, nil, command, sample)
	}
	return marker(id, contract.MarkerEnforced, days(c.Now, c.enforcedSince(lockfiles, frozen)), command, sample)
}
