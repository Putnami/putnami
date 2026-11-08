package php

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "php/stack-extension"

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture writes the layout of the observe Composer detector's Symfony demo:
// an application at the root that requires demo/pricing, a package its path
// repository provides, and a tool, tools/gen, with a Composer build of its
// own.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "composer.json", `{
	  "type": "project",
	  "require": {"php": ">=8.4", "demo/pricing": "^1.0", "symfony/framework-bundle": "8.1.*"},
	  "require-dev": {"phpstan/phpstan": "^2.1", "phpunit/phpunit": "^12"},
	  "config": {"platform": {"php": "8.4.10"}},
	  "autoload": {"psr-4": {"App\\": "src/"}},
	  "autoload-dev": {"psr-4": {"App\\Tests\\": "tests/"}},
	  "repositories": [{"type": "path", "url": "packages/*", "options": {"symlink": true}}]
	}`)
	writeFile(t, root, "composer.lock", `{"packages": []}`)
	writeFile(t, root, "phpunit.dist.xml", `<phpunit bootstrap="tests/bootstrap.php"><testsuites><testsuite name="app"><directory>tests</directory></testsuite></testsuites></phpunit>`)
	writeFile(t, root, "phpstan.dist.neon", "parameters:\n  level: 6\n")
	writeFile(t, root, ".env", "APP_ENV=dev\n")
	writeFile(t, root, "config/bundles.php", "<?php\n")
	writeFile(t, root, "src/Controller/QuoteController.php", "<?php\n")
	writeFile(t, root, "src/Kernel.php", "<?php\n")
	writeFile(t, root, "tests/Controller/QuoteControllerTest.php", "<?php\n")
	writeFile(t, root, "tests/bootstrap.php", "<?php\n")
	writeFile(t, root, "packages/pricing/composer.json", `{
	  "name": "demo/pricing", "type": "library",
	  "require": {"php": ">=8.4"},
	  "autoload": {"psr-4": {"Demo\\Pricing\\": "src/"}}
	}`)
	writeFile(t, root, "packages/pricing/src/Calculator.php", "<?php\n")
	writeFile(t, root, "packages/pricing/tests/CalculatorTest.php", "<?php\n")
	writeFile(t, root, "tools/gen/composer.json", `{"name": "demo/gen", "require": {"php": "^8.3"}}`)
	writeFile(t, root, "tools/gen/.php-version", "8.3\n")
	writeFile(t, root, "tools/gen/src/Gen.php", "<?php\n")
	return root
}

func stack(t *testing.T) stacks.Stack {
	t.Helper()
	s, err := Stack()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func probe(t *testing.T, root string) wsproto.ProbeResult {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{".", "config", "packages/pricing", "tools/gen"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func metadata(t *testing.T, p wsproto.ProbeProject) stacks.Metadata {
	t.Helper()
	var meta stacks.Metadata
	if err := json.Unmarshal(p.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestTheComponentsOfAComposerRepositoryBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "components-become-projects", "the-components-of-a-composer-repository-become-projects-with-their-edges")
	result := probe(t, fixture(t))
	if len(result.Projects) != 3 {
		t.Fatalf("projects = %+v, diagnostics = %v", result.Projects, result.Diagnostics)
	}
	want := map[string]struct {
		source, component, root string
		deps                    []string
		toolchain               stacks.Expected
	}{
		".":                {"", ".", ".", []string{"packages/pricing"}, stacks.Expected{Version: ">=8.4", Source: "composer.json"}},
		"packages/pricing": {"demo/pricing", "demo/pricing", ".", nil, stacks.Expected{Version: ">=8.4", Source: "packages/pricing/composer.json"}},
		"tools/gen":        {"demo/gen", "demo/gen", "tools/gen", nil, stacks.Expected{Version: "8.3", Source: "tools/gen/.php-version"}},
	}
	for _, p := range result.Projects {
		w, ok := want[p.Path]
		if !ok {
			t.Errorf("unexpected project %s", p.Path)
			continue
		}
		meta := metadata(t, p)
		if p.SourceName != w.source || !reflect.DeepEqual(p.Dependencies, w.deps) || meta.Component != w.component || meta.Root != w.root || meta.Toolchain != w.toolchain {
			t.Errorf("%s = %+v, metadata %+v", p.Path, p, meta)
		}
		if !reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"php"}) {
			t.Errorf("%s extensions %v tags %v", p.Path, p.Extensions, p.Tags)
		}
	}
}

func TestARootPHPVersionWinsOverAPackageRange(t *testing.T) {
	root := fixture(t)
	writeFile(t, root, ".php-version", "8.4.10\n")
	for _, p := range probe(t, root).Projects {
		if p.Path != "packages/pricing" {
			continue
		}
		if meta := metadata(t, p); meta.Toolchain != (stacks.Expected{Version: "8.4.10", Source: ".php-version"}) {
			t.Errorf("pricing expects %+v", meta.Toolchain)
		}
	}
	stubTools(t, root, "8.4.9", 0)
	if status, _, _, err := runJob(t, root, "packages/pricing", stacks.TaskLint); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("8.4.9 is not the root's 8.4.10, though it is in the package's range: %s %v", status, err)
	}
}

func TestAMalformedRootManifestLeavesEachPackageItsOwnRoot(t *testing.T) {
	root := fixture(t)
	writeFile(t, root, "composer.json", `{`)
	result := probe(t, root)
	var diagnosed bool
	for _, d := range result.Diagnostics {
		diagnosed = diagnosed || strings.Contains(d.Message, "path repositories are not read")
	}
	if !diagnosed {
		t.Errorf("diagnostics = %v", result.Diagnostics)
	}
	for _, p := range result.Projects {
		if meta := metadata(t, p); meta.Root != p.Path {
			t.Errorf("%s has stack root %s", p.Path, meta.Root)
		}
	}
}

// stubTools puts first on PATH a php that prints version and runs the stub
// PHPUnit, and writes the stack root's vendor/bin/phpstan and
// vendor/bin/phpunit. Both log their arguments; phpunit writes the JUnit and
// Clover reports PHPUnit writes, each case naming its test file and each
// covered file an absolute path, and exits 1 when a case fails, as PHPUnit
// does. PHP and its tools are the stack's own build:
// the stub stands at that boundary.
func stubTools(t *testing.T, root, version string, failures int) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "tools.log")
	writeFile(t, bin, "php", `#!/bin/sh
if [ "$1" = "--version" ]; then echo "PHP `+version+` (cli) (built: Oct  1 2026 10:00:00) (NTS)"; exit 0; fi
while [ "$#" -gt 0 ] && [ "$1" = "-d" ]; do shift 2; done
exec "$@"
`)
	writeFile(t, root, "vendor/bin/phpstan", `#!/bin/sh
echo "$PWD: phpstan $*" >> "`+log+`"
`)
	failure := ""
	if failures > 0 {
		failure = `<failure type="AssertionFailedError">expected 700</failure>`
	}
	writeFile(t, root, "vendor/bin/phpunit", `#!/bin/sh
echo "$PWD: phpunit $*" >> "`+log+`"
prev=""
for a in "$@"; do
  case "$prev" in
    --log-junit) cat > "$a" <<XML
<?xml version="1.0"?>
<testsuites><testsuite name="tests" tests="2" failures="`+map[bool]string{true: "1", false: "0"}[failures > 0]+`">
  <testcase name="testTotal" file="$PWD/tests/CalculatorTest.php" class="Demo\Pricing\Tests\CalculatorTest" classname="Demo.Pricing.Tests.CalculatorTest">`+failure+`</testcase>
  <testcase name="testEmpty" file="$PWD/tests/CalculatorTest.php" class="Demo\Pricing\Tests\CalculatorTest" classname="Demo.Pricing.Tests.CalculatorTest"/>
</testsuite></testsuites>
XML
    ;;
    --coverage-clover) cat > "$a" <<XML
<?xml version="1.0"?>
<coverage><project><file name="$PWD/src/Calculator.php">
  <line num="5" type="stmt" count="3"/><line num="6" type="stmt" count="0"/><line num="7" type="stmt" count="1"/>
  <metrics loc="9" ncloc="9" statements="3" coveredstatements="2" elements="3" coveredelements="2"/>
</file></project></coverage>
XML
    ;;
  esac
  prev="$a"
done
exit `+map[bool]string{true: "1", false: "0"}[failures > 0]+`
`)
	for _, f := range []string{filepath.Join(bin, "php"), filepath.Join(root, "vendor/bin/phpstan"), filepath.Join(root, "vendor/bin/phpunit")} {
		if err := os.Chmod(f, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// runJob runs a task of the component at rel through the extension's job,
// the way the CLI calls it.
func runJob(t *testing.T, root, rel, task string) (string, map[string]any, string, error) {
	t.Helper()
	s := stack(t)
	var meta json.RawMessage
	for _, p := range probe(t, root).Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	out := filepath.Join(root, ".putnami/out", strings.ReplaceAll(rel, "/", "_"), task)
	job := s.Jobs()[task]
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	status, data, runErr := job(ctx, jsonl.New(), []string{"--report-dir", out})
	os.Stdout = stdout
	return status, data, out, runErr
}

func TestATaskRunsTheProfileStepsAfterTheToolchainCheck(t *testing.T) {
	spectest.Proves(t, feature, "tasks-run-the-profile", "a-task-runs-the-profile-steps-after-the-toolchain-check")
	root := fixture(t)
	log := stubTools(t, root, "8.4.10", 0)
	for _, rel := range []string{".", "packages/pricing"} {
		if status, _, _, err := runJob(t, root, rel, stacks.TaskLint); status != "OK" || err != nil {
			t.Fatalf("lint %s: %s %v", rel, status, err)
		}
	}
	ran, _ := os.ReadFile(log)
	for _, want := range []string{
		root + ": phpstan analyse --no-progress --no-interaction src",                                    //nolint:misspell // PHPStan names its command analyse
		filepath.Join(root, "packages/pricing") + ": phpstan analyse --no-progress --no-interaction src", //nolint:misspell // PHPStan names its command analyse
	} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("lint ran\n%s\nwant %q", ran, want)
		}
	}
	if _, ok := stack(t).Jobs()[stacks.TaskBuild]; ok {
		t.Error("the PHP stack declares no build task")
	}

	// The application states >=8.4: 8.3.6 is outside it.
	stubTools(t, root, "8.3.6", 0)
	if status, _, _, err := runJob(t, root, ".", stacks.TaskLint); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("a PHP outside the stated range fails the task: %s %v", status, err)
	}
	// tools/gen pins 8.3 in .php-version, which outranks its composer.json.
	writeFile(t, root, "tools/gen/vendor/bin/phpstan", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(root, "tools/gen/vendor/bin/phpstan"), 0o755); err != nil {
		t.Fatal(err)
	}
	if status, _, _, err := runJob(t, root, "tools/gen", stacks.TaskLint); status != "OK" || err != nil {
		t.Errorf("a PHP .php-version names runs the task: %s %v", status, err)
	}
	writeFile(t, root, "packages/pricing/putnami.json", `{"options": {"@putnami/php": {"toolchain": {"check": "warn"}}}}`)
	if status, _, _, err := runJob(t, root, "packages/pricing", stacks.TaskLint); status != "OK" || err != nil {
		t.Errorf("warn runs the task on a mismatch: %s %v", status, err)
	}
}

func TestJUnitAndCloverBecomeTheTestAndCoverageSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-become-the-summary", "junit-and-clover-become-the-test-and-coverage-summary")
	root := fixture(t)
	log := stubTools(t, root, "8.4.10", 0)
	status, data, out, err := runJob(t, root, "packages/pricing", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v", status, err)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 2 || got["passed"] != 2 || got["failed"] != 0 {
		t.Errorf("testSummary = %v", got)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 2 || got["total"] != 3 {
		t.Errorf("coverageSummary = %v", got)
	}
	ran, _ := os.ReadFile(log)
	want := filepath.Join(root, "packages/pricing") + ": phpunit --log-junit " + filepath.Join(out, "test", "junit.xml") +
		" --coverage-clover " + filepath.Join(out, "test", "clover.xml") + " --coverage-filter src tests"
	if !strings.Contains(string(ran), want) {
		t.Errorf("test ran\n%s\nwant %q", ran, want)
	}

	stubTools(t, root, "8.4.10", 1)
	if status, data, _, err := runJob(t, root, "packages/pricing", stacks.TaskTest); status != "FAILED" || err == nil || data["testSummary"].(map[string]any)["failed"] != 1 {
		t.Errorf("a failed case fails the task: %s %v %v", status, err, data)
	}
}

func resolve(t *testing.T, root, project, selector string) []string {
	t.Helper()
	s := stack(t)
	result := s.ResolveInputs(root, wsproto.InputResolveRequest{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
		Inputs: []wsproto.InputResolveQuery{{Project: project, Selector: selector}}})
	if diags := wsproto.ValidateInputResolveResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	if result.Inputs[0].Error != "" {
		t.Fatalf("%s %s: %s", project, selector, result.Inputs[0].Error)
	}
	return result.Inputs[0].Files
}

func TestATestOnlyChangeLeavesTheLintInputsUnchanged(t *testing.T) {
	spectest.Proves(t, feature, "inputs-per-task", "a-test-only-change-leaves-the-lint-inputs-unchanged")
	root := fixture(t)
	lint := resolve(t, root, ".", "php:lint")
	if want := []string{"composer.json", "composer.lock", "phpstan.dist.neon", "src/Controller/QuoteController.php", "src/Kernel.php"}; !reflect.DeepEqual(lint, want) {
		t.Errorf("lint inputs = %v, want %v", lint, want)
	}
	test := resolve(t, root, ".", "php:test")
	for _, f := range []string{".env", "composer.lock", "config/bundles.php", "phpunit.dist.xml", "src/Kernel.php", "tests/Controller/QuoteControllerTest.php"} {
		if !slices.Contains(test, f) {
			t.Errorf("test inputs %v lack %s", test, f)
		}
	}
	// A package under the root's build reads its own files; the root's
	// composer.json and composer.lock are the manifest's workspace input.
	if got, want := resolve(t, root, "packages/pricing", "php:test"), []string{"composer.json", "src/Calculator.php", "tests/CalculatorTest.php"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pricing test inputs = %v, want %v", got, want)
	}
	// A component that is its own stack root reads its own .php-version.
	if got := resolve(t, root, "tools/gen", "php:lint"); !slices.Contains(got, ".php-version") {
		t.Errorf("gen lint inputs = %v", got)
	}

	writeFile(t, root, "tests/Controller/ProfileTest.php", "<?php\n")
	if got := resolve(t, root, ".", "php:lint"); !reflect.DeepEqual(got, lint) {
		t.Errorf("a test change moved the lint inputs: %v", got)
	}
	if got := resolve(t, root, ".", "php:test"); !slices.Contains(got, "tests/Controller/ProfileTest.php") {
		t.Errorf("the new test is no test input: %v", got)
	}
}

func TestAPutnamiJSONOverrideChangesTheStepsAndTheInputs(t *testing.T) {
	spectest.Proves(t, feature, "overrides-are-honored", "a-putnami-json-override-changes-the-steps-and-the-inputs")
	root := fixture(t)
	writeFile(t, root, "packages/pricing/lib/Legacy.php", "<?php\n")
	writeFile(t, root, "packages/pricing/putnami.json", `{"options": {"@putnami/php": {
	  "inputs": {"sources": ["src/**", "lib/**"]},
	  "tasks": {"lint": {"steps": [["{root}/vendor/bin/php-cs-fixer", "fix", "--dry-run", "--diff"]]}}
	}}}`)
	if got, want := resolve(t, root, "packages/pricing", "php:lint"), []string{"composer.json", "lib/Legacy.php", "putnami.json", "src/Calculator.php"}; !reflect.DeepEqual(got, want) {
		t.Errorf("an input override is honored: %v", got)
	}
	log := stubTools(t, root, "8.4.10", 0)
	writeFile(t, root, "vendor/bin/php-cs-fixer", "#!/bin/sh\necho \"$PWD: php-cs-fixer $*\" >> \""+log+"\"\n")
	if err := os.Chmod(filepath.Join(root, "vendor/bin/php-cs-fixer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if status, _, _, err := runJob(t, root, "packages/pricing", stacks.TaskLint); status != "OK" || err != nil {
		t.Fatalf("lint: %s %v", status, err)
	}
	ran, _ := os.ReadFile(log)
	if lines := strings.Split(strings.TrimSpace(string(ran)), "\n"); len(lines) != 1 || !strings.HasSuffix(lines[0], "php-cs-fixer fix --dry-run --diff") {
		t.Errorf("a step override is honored: %q", ran)
	}

	// A machine with no coverage driver keeps the JUnit report only.
	writeFile(t, root, "packages/pricing/putnami.json", `{"options": {"@putnami/php": {"tasks": {"test": {
	  "steps": [["{root}/vendor/bin/phpunit", "--log-junit", "{output}/junit.xml", "tests"]],
	  "reports": {"coverage": []}
	}}}}}`)
	if status, data, _, err := runJob(t, root, "packages/pricing", stacks.TaskTest); status != "OK" || err != nil || data["coverageSummary"] != nil || data["testSummary"].(map[string]any)["total"] != 2 {
		t.Errorf("a test without coverage: %s %v %v", status, err, data)
	}

	writeFile(t, root, "packages/pricing/putnami.json", `{"options": {"@putnami/php": {"inputs": {"manifests": ["composer.json", "{root}/symfony.lock"]}}}}`)
	if status, _, _, err := runJob(t, root, "packages/pricing", stacks.TaskLint); status != "FAILED" || err == nil || err.Error() != stacks.CodeProjectConfigInvalid {
		t.Errorf("a stack-root input the manifest does not declare fails the task: %s %v", status, err)
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	stackstest.CheckManifest(t, "putnami.extension.json", stack(t))
	if got := stack(t).Selectors(); !slices.Equal(got, []string{"php:lint", "php:test"}) {
		t.Errorf("selectors = %v", got)
	}
}
