package ruby

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

const feature = "ruby/stack-extension"

// fixture writes a Rails modular monolith: the application at the root, the
// RSpec pack pricing, the Minitest pack checkout that lists pricing, and the
// gem audit_log with a Gemfile of its own.
func fixture(t *testing.T) string {
	t.Helper()
	// The steps' $PWD is the real path: on macOS the temporary directory is
	// a symbolic link.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "shop")
	stackstest.WriteFile(t, root, "Gemfile", "source \"https://rubygems.org\"\n\ngem \"rails\", \"~> 7.2.0\"\n")
	stackstest.WriteFile(t, root, "Gemfile.lock", "GEM\n  remote: https://rubygems.org/\n  specs:\n    rails (7.2.4)\n\nBUNDLED WITH\n   2.5.22\n")
	stackstest.WriteFile(t, root, ".ruby-version", "ruby-3.3.6\n")
	stackstest.WriteFile(t, root, ".rspec", "--require spec_helper\n")
	stackstest.WriteFile(t, root, ".rubocop.yml", "inherit_gem: { rubocop-rails-omakase: rubocop.yml }\n")
	stackstest.WriteFile(t, root, "package.yml", "enforce_dependencies: true\n")
	stackstest.WriteFile(t, root, "app/services/order_summary.rb", "class OrderSummary; end\n")
	stackstest.WriteFile(t, root, "config/application.rb", "require \"rails\"\n")
	stackstest.WriteFile(t, root, "spec/spec_helper.rb", "require \"simplecov\"\n")
	stackstest.WriteFile(t, root, "spec/services/order_summary_spec.rb", "RSpec.describe OrderSummary\n")
	stackstest.WriteFile(t, root, "packs/pricing/package.yml", "enforce_dependencies: true\n")
	stackstest.WriteFile(t, root, "packs/pricing/app/models/pricing/calculator.rb", "module Pricing; end\n")
	stackstest.WriteFile(t, root, "packs/pricing/app/views/pricing/show.html.erb", "<p></p>\n")
	stackstest.WriteFile(t, root, "packs/pricing/spec/models/pricing/calculator_spec.rb", "RSpec.describe Pricing\n")
	stackstest.WriteFile(t, root, "packs/pricing/spec/fixtures/prices.yml", "unit: 100\n")
	stackstest.WriteFile(t, root, "packs/checkout/package.yml", "dependencies:\n  - packs/pricing\n")
	stackstest.WriteFile(t, root, "packs/checkout/app/models/checkout/cart.rb", "module Checkout; end\n")
	stackstest.WriteFile(t, root, "packs/checkout/test/models/checkout/cart_test.rb", "class CartTest; end\n")
	stackstest.WriteFile(t, root, "gems/audit_log/audit_log.gemspec", "Gem::Specification.new do |s|\n  s.name = \"audit_log\"\nend\n")
	stackstest.WriteFile(t, root, "gems/audit_log/Gemfile", "source \"https://rubygems.org\"\ngemspec\n")
	stackstest.WriteFile(t, root, "gems/audit_log/lib/audit_log.rb", "module AuditLog; end\n")
	stackstest.WriteFile(t, root, "gems/audit_log/spec/audit_log_spec.rb", "RSpec.describe AuditLog\n")
	stackstest.WriteFile(t, root, "packs/pricing/.simplecov", "SimpleCov.start\n")
	stackstest.WriteFile(t, root, "packs/pricing/app/views/pricing/show.json.jbuilder", "json.unit 100\n")
	stackstest.WriteFile(t, root, "packs/pricing/vendor/bundle/ruby/3.3.0/gems/rake/lib/rake.rb", "module Rake; end\n")
	stackstest.WriteFile(t, root, "packs/pricing/tmp/cache/bootsnap.rb", "\n")
	stackstest.WriteFile(t, root, "packs/pricing/.gen/version.json", "{}\n")
	stackstest.WriteFile(t, root, "packs/docs/package.yml", "enforce_dependencies: true\n")
	stackstest.WriteFile(t, root, "packs/docs/app/models/docs/page.rb", "module Docs; end\n")
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
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension,
		Paths: []string{".", "docs", "gems/audit_log", "packs/checkout", "packs/docs", "packs/pricing"}})
	if err != nil {
		t.Fatal(err)
	}
	if diags := wsproto.ValidateProbeResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	return result
}

func TestTheApplicationPacksAndGemsBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "components-become-projects", "the-application-packs-and-gems-become-projects-with-their-edges")
	result := probe(t, fixture(t))
	want := map[string]struct {
		name, file, root string
		deps             []string
	}{
		".":              {"shop", "package.yml", ".", nil},
		"gems/audit_log": {"audit_log", "gems/audit_log/audit_log.gemspec", "gems/audit_log", nil},
		"packs/checkout": {"packs/checkout", "packs/checkout/package.yml", ".", []string{"packs/pricing"}},
		"packs/docs":     {"packs/docs", "packs/docs/package.yml", ".", nil},
		"packs/pricing":  {"packs/pricing", "packs/pricing/package.yml", ".", nil},
	}
	if len(result.Projects) != len(want) {
		t.Fatalf("projects = %+v, diagnostics = %v", result.Projects, result.Diagnostics)
	}
	for _, p := range result.Projects {
		w := want[p.Path]
		var meta stacks.Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if p.SourceName != w.name || p.SourceFile != w.file || !reflect.DeepEqual(p.Dependencies, w.deps) || meta.Component != w.name || meta.Root != w.root {
			t.Errorf("%s = %+v, metadata %+v", p.Path, p, meta)
		}
		if !reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"ruby"}) {
			t.Errorf("%s extensions %v tags %v", p.Path, p.Extensions, p.Tags)
		}
		if w.root == "." {
			if meta.Toolchain != (stacks.Expected{Version: "3.3.6", Source: ".ruby-version"}) || len(meta.Toolchains) != 1 || meta.Toolchains[0] != (stacks.Expected{Version: "2.5.22", Source: "Gemfile.lock"}) {
				t.Errorf("%s expects %+v and %+v", p.Path, meta.Toolchain, meta.Toolchains)
			}
		}
	}
}

// stubTools puts first on PATH a ruby that prints rubyVersion and a bundle
// that prints bundlerVersion and logs every other call. Its rspec and rails
// test write a JUnit report where they are told to, with failures failed
// cases, and SimpleCov's Cobertura report in the directory they run in,
// naming a file of each pack, one of the application and one outside the
// workspace.
// The real tools are the repository's own: the stub stands at that boundary.
func stubTools(t *testing.T, rubyVersion, bundlerVersion string, failures int) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "bundle.log")
	failure := ""
	if failures > 0 {
		failure = `<failure message="expected 400, got 0"/>`
	}
	stackstest.WriteFile(t, bin, "ruby", "#!/bin/sh\necho 'ruby "+rubyVersion+" (2024-11-05 revision 75015d4c1f) [x86_64-linux]'\n")
	stackstest.WriteFile(t, bin, "bundle", `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'Bundler version `+bundlerVersion+`'; exit 0; fi
if [ "$1" = "check" ]; then exit "${STUB_BUNDLE_CHECK:-0}"; fi
echo "$PWD: bundle $*" >> "`+log+`"
out=""
prev=""
for a in "$@"; do
  case "$a" in --junit-filename=*) out="${a#--junit-filename=}";; esac
  if [ "$prev" = "--out" ]; then out="$a"; fi
  prev="$a"
done
[ -n "$out" ] || exit 0
cat > "$out" <<XML
<?xml version="1.0"?>
<testsuite name="rspec" tests="2"><testcase classname="pricing" name="sums the lines" file="./packs/pricing/spec/models/pricing/calculator_spec.rb">`+failure+`</testcase><testcase classname="pricing" name="totals no line as zero"/></testsuite>
XML
if [ -z "$STUB_NO_COVERAGE" ]; then
mkdir -p coverage
cat > coverage/coverage.xml <<XML
<?xml version="1.0"?>
<coverage line-rate="0.5" lines-covered="4" lines-valid="8" version="0" timestamp="1">
  <sources><source>$PWD</source></sources>
  <packages><package name="shop"><classes>
    <class name="a" filename="packs/pricing/app/models/pricing/calculator.rb"><lines><line number="1" hits="3"/><line number="2" hits="0"/></lines></class>
    <class name="b" filename="packs/checkout/app/models/checkout/cart.rb"><lines><line number="1" hits="1"/><line number="2" hits="1"/></lines></class>
    <class name="c" filename="app/services/order_summary.rb"><lines><line number="1" hits="1"/></lines></class>
    <class name="d" filename="/nowhere/gone.rb"><lines><line number="1" hits="1"/></lines></class>
  </classes></package></packages>
</coverage>
XML
fi
`)
	for _, f := range []string{"ruby", "bundle"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

type recorded struct {
	status string
	data   map[string]any
	out    string
	err    error
	// events is what the job emitted.
	events string
}

// runJob runs a task of the component at rel through the extension's job,
// the way the CLI calls it.
func runJob(t *testing.T, root, rel, task string) recorded {
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
	events, err := os.Create(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	stdout := os.Stdout
	os.Stdout = events
	status, data, runErr := s.Jobs()[task](ctx, jsonl.New(), []string{"--report-dir", out})
	os.Stdout = stdout
	emitted, _ := os.ReadFile(events.Name())
	return recorded{status, data, out, runErr, string(emitted)}
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	data, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestATaskRunsTheProfileStepsAfterTheRubyAndBundlerChecks(t *testing.T) {
	spectest.Proves(t, feature, "tasks-run-the-profile", "a-task-runs-the-profile-steps-after-the-ruby-and-bundler-checks")
	root := fixture(t)
	log := stubTools(t, "3.3.6", "2.5.22", 0)
	pricing := filepath.Join(root, "packs/pricing")
	if r := runJob(t, root, "packs/pricing", stacks.TaskLint); r.status != "OK" || r.err != nil {
		t.Fatalf("lint: %+v", r)
	}
	if r := runJob(t, root, "packs/pricing", stacks.TaskTest); r.status != "OK" || r.err != nil {
		t.Fatalf("test: %+v", r)
	}
	want := []string{
		pricing + ": bundle exec rubocop",
		root + ": bundle exec rspec " + pricing + "/spec --format progress --format RspecJunitFormatter --out " + filepath.Join(root, ".putnami/out/packs_pricing/test/test/junit.xml"),
	}
	if got := calls(t, log); !reflect.DeepEqual(got, want) {
		t.Errorf("ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// bundle install switches to the locked Bundler, so a mismatch warns.
	stubTools(t, "3.3.6", "2.4.1", 0)
	if r := runJob(t, root, "packs/pricing", stacks.TaskLint); r.status != "OK" || r.err != nil || !strings.Contains(r.events, stacks.CodeToolchainMismatch) {
		t.Errorf("a Bundler mismatch warns and runs the task: %+v", r)
	}
	stubTools(t, "3.2.4", "2.5.22", 0)
	if r := runJob(t, root, "packs/pricing", stacks.TaskLint); r.status != "FAILED" || r.err == nil || r.err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("a Ruby mismatch fails the task: %+v", r)
	}
}

func TestAComponentRunsRSpecOrRailsTestByItsTestFiles(t *testing.T) {
	spectest.Proves(t, feature, "test-runner-per-component", "a-component-runs-rspec-or-rails-test-by-its-test-files")
	root := fixture(t)
	log := stubTools(t, "3.3.6", "2.5.22", 0)
	checkout := filepath.Join(root, "packs/checkout")
	if r := runJob(t, root, "packs/checkout", stacks.TaskTest); r.status != "OK" || r.err != nil {
		t.Fatalf("test: %+v", r)
	}
	junit := filepath.Join(root, ".putnami/out/packs_checkout/test/test/junit.xml")
	if got := calls(t, log); !reflect.DeepEqual(got, []string{root + ": bundle exec rails test " + checkout + "/test --junit --junit-filename=" + junit}) {
		t.Errorf("a Minitest pack runs rails test: %q", got)
	}

	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if r := runJob(t, root, "packs/docs", stacks.TaskTest); r.status != "OK" || r.err != nil {
		t.Fatalf("a pack with no test file passes: %+v", r)
	}
	if got, _ := os.ReadFile(log); len(got) != 0 {
		t.Errorf("a pack with no test file runs nothing: %s", got)
	}

	_ = os.Remove(log)
	stackstest.WriteFile(t, root, "gems/audit_log/test/test_audit_log.rb", "class TestAuditLog; end\n")
	if err := os.RemoveAll(filepath.Join(root, "gems/audit_log/spec")); err != nil {
		t.Fatal(err)
	}
	if r := runJob(t, root, "gems/audit_log", stacks.TaskTest); r.status != "OK" || r.err != nil {
		t.Fatalf("test with the stub: %+v", r)
	}
	if got := calls(t, log); len(got) != 1 || !strings.Contains(got[0], "bundle exec rspec "+filepath.Join(root, "gems/audit_log")+"/spec ") {
		t.Errorf("a gem whose test files match no runner keeps RSpec, which fails without a spec directory: %q", got)
	}

	s := stack(t)
	answer := func() []string {
		result := s.ResolveInputs(root, wsproto.InputResolveRequest{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
			Inputs: []wsproto.InputResolveQuery{{Project: "packs/checkout", Selector: "ruby:test"}}})
		return result.Inputs[0].Files
	}
	before := answer()
	stackstest.WriteFile(t, root, "packs/checkout/spec/cart_spec.rb", "RSpec.describe Checkout\n")
	if after := answer(); slices.Equal(before, after) || !slices.Contains(after, "spec/cart_spec.rb") {
		t.Errorf("a spec file that changes the runner is an input: %v", after)
	}
	_ = os.Remove(log)
	if r := runJob(t, root, "packs/checkout", stacks.TaskTest); r.status != "OK" {
		t.Fatalf("test: %+v", r)
	}
	if got := calls(t, log); len(got) != 1 || !strings.HasPrefix(got[0], root+": bundle exec rspec "+checkout+"/spec ") {
		t.Errorf("a pack with specs runs rspec: %q", got)
	}
}

func TestJUnitAndCoberturaBecomeTheComponentSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-become-the-summary", "junit-and-cobertura-become-the-component-summary")
	root := fixture(t)
	stubTools(t, "3.3.6", "2.5.22", 0)
	r := runJob(t, root, "packs/pricing", stacks.TaskTest)
	if r.status != "OK" || r.err != nil {
		t.Fatalf("test: %+v", r)
	}
	if got := r.data["testSummary"].(map[string]any); got["total"] != 2 || got["passed"] != 2 || got["failed"] != 0 {
		t.Errorf("testSummary = %v", got)
	}
	if got := r.data["coverageSummary"].(map[string]any); got["covered"] != 1 || got["total"] != 2 {
		t.Errorf("only the pack's own file counts: %v", got)
	}
	if _, err := os.Stat(filepath.Join(r.out, "test", "reports", "01-coverage.xml")); err != nil {
		t.Errorf("the Cobertura report is copied into the task output: %v", err)
	}
	app := runJob(t, root, ".", stacks.TaskTest)
	if got := app.data["coverageSummary"].(map[string]any); app.status != "OK" || got["covered"] != 1 || got["total"] != 1 {
		t.Errorf("the application leaves its packs' files out: %+v", app)
	}

	// A bundle that does not set up SimpleCov writes no coverage report: the
	// task passes, with no coverage.
	t.Setenv("STUB_NO_COVERAGE", "1")
	if err := os.Remove(filepath.Join(root, "coverage", "coverage.xml")); err != nil {
		t.Fatal(err)
	}
	if r := runJob(t, root, "packs/pricing", stacks.TaskTest); r.status != "OK" || r.err != nil || r.data["coverageSummary"] != nil || r.data["testSummary"].(map[string]any)["passed"] != 2 {
		t.Errorf("no SimpleCov: %+v", r)
	}
	t.Setenv("STUB_NO_COVERAGE", "")

	stubTools(t, "3.3.6", "2.5.22", 1)
	if r := runJob(t, root, "packs/pricing", stacks.TaskTest); r.status != "FAILED" || r.err == nil || r.data["testSummary"].(map[string]any)["failed"] != 1 {
		t.Errorf("a failed case fails the task: %+v", r)
	}
}

func lintFiles(t *testing.T, root, project, selector string) []string {
	t.Helper()
	s := stack(t)
	result := s.ResolveInputs(root, wsproto.InputResolveRequest{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
		Inputs: []wsproto.InputResolveQuery{{Project: project, Selector: selector}}})
	if diags := wsproto.ValidateInputResolveResult(&result); len(diags) != 0 || result.Inputs[0].Error != "" {
		t.Fatalf("the answer breaks the protocol: %v %s", diags, result.Inputs[0].Error)
	}
	return result.Inputs[0].Files
}

func TestATestOnlyChangeKeepsTheLintInputs(t *testing.T) {
	spectest.Proves(t, feature, "lint-reads-the-ruby-files", "a-test-only-change-keeps-the-lint-inputs")
	root := fixture(t)
	want := []string{".simplecov", "app/models/pricing/calculator.rb", "app/views/pricing/show.json.jbuilder", "spec/models/pricing/calculator_spec.rb"}
	if got := lintFiles(t, root, "packs/pricing", "ruby:lint"); !reflect.DeepEqual(got, want) {
		t.Errorf("lint reads the files RuboCop inspects, outside vendor and tmp: %v", got)
	}
	test := lintFiles(t, root, "packs/pricing", "ruby:test")
	for _, want := range []string{"app/views/pricing/show.html.erb", "spec/fixtures/prices.yml", "package.yml"} {
		if !slices.Contains(test, want) {
			t.Errorf("test reads %s: %v", want, test)
		}
	}
	if slices.ContainsFunc(test, func(f string) bool {
		return strings.HasPrefix(f, "vendor/") || strings.HasPrefix(f, "tmp/") || strings.HasPrefix(f, ".gen/")
	}) {
		t.Errorf("test leaves vendor, tmp and Putnami's .gen out: %v", test)
	}
	app := lintFiles(t, root, ".", "ruby:lint")
	if !slices.Contains(app, ".rubocop.yml") || slices.Contains(app, ".rspec") || !slices.Contains(app, "packs/pricing/app/models/pricing/calculator.rb") {
		t.Errorf("the application's lint reads its RuboCop configuration and every Ruby file RuboCop inspects from the root, not .rspec: %v", app)
	}
}

func TestAPutnamiJSONOverrideChangesTheStepsAndTheInputs(t *testing.T) {
	spectest.Proves(t, feature, "overrides-are-honored", "a-putnami-json-override-changes-the-steps-and-the-inputs")
	root := fixture(t)
	stackstest.WriteFile(t, root, "packs/pricing/putnami.json", `{"options": {"@putnami/ruby": {
	  "inputs": {"ruby": ["app/**/*.rb"]},
	  "tasks": {"lint": {"steps": [["bundle", "exec", "rubocop", "--parallel"]]}}
	}}}`)
	if got := lintFiles(t, root, "packs/pricing", "ruby:lint"); !reflect.DeepEqual(got, []string{"app/models/pricing/calculator.rb", "putnami.json"}) {
		t.Errorf("an input override is honored: %v", got)
	}
	log := stubTools(t, "3.3.6", "2.5.22", 0)
	if r := runJob(t, root, "packs/pricing", stacks.TaskLint); r.status != "OK" || r.err != nil {
		t.Fatalf("lint: %+v", r)
	}
	if got := calls(t, log); len(got) != 1 || !strings.HasSuffix(got[0], "bundle exec rubocop --parallel") {
		t.Errorf("a step override is honored: %q", got)
	}
	stackstest.WriteFile(t, root, "packs/pricing/putnami.json", `{"options": {"@putnami/ruby": {"tasks": {"lint": {"stepz": []}}}}}`)
	if r := runJob(t, root, "packs/pricing", stacks.TaskLint); r.status != "FAILED" || r.err == nil || r.err.Error() != stacks.CodeProjectConfigInvalid {
		t.Errorf("an invalid override fails the task: %+v", r)
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	stackstest.CheckManifest(t, "putnami.extension.json", stack(t))
	var manifest struct {
		Tasks map[string]struct {
			Inputs map[string]struct {
				Files []string `json:"files"`
			} `json:"inputs"`
		} `json:"tasks"`
	}
	data, err := os.ReadFile("putnami.extension.json")
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	stackRoot := manifest.Tasks["test-run"].Inputs["stackRoot"].Files
	for _, written := range []string{"spec/examples.txt", ".rspec_status", "coverage/coverage.xml", "tmp/cache/bootsnap.rb", "log/test.log"} {
		if wsproto.SelectsPath(written, stackRoot) {
			t.Errorf("the stack-root test input selects %s, which a run writes: %v", written, stackRoot)
		}
	}
	for _, read := range []string{"app/services/order_summary.rb", "config/application.rb", "spec/spec_helper.rb", "spec/fixtures/prices.yml", "spec/cassettes/checkout.yml", "spec/support/schemas/cart.json", "test/test_helper.rb"} {
		if !wsproto.SelectsPath(read, stackRoot) {
			t.Errorf("the stack-root test input misses %s, which the booted application reads: %v", read, stackRoot)
		}
	}
	if got := stack(t).Selectors(); !slices.Equal(got, []string{"ruby:lint", "ruby:test"}) {
		t.Errorf("selectors = %v", got)
	}
}
