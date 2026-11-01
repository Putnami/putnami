package composer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

const feature = "tooling/observe-composer"

// demo is a copy of gitlab.com/putnami/demo-observe-symfony: a Symfony
// application at the root and its pricing package in packages/pricing, which
// the application requires through a path repository.
const demo = "testdata/symfony-demo"

// writeTree writes files, keyed by slash path, under a new directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func byPath(t *testing.T, result wsproto.ProbeResult) map[string]wsproto.ProbeProject {
	t.Helper()
	projects := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		projects[p.Path] = p
	}
	return projects
}

func metadata(t *testing.T, p wsproto.ProbeProject) Metadata {
	t.Helper()
	m, err := ReadMetadata(p)
	if err != nil {
		t.Fatalf("metadata of %s: %v", p.Path, err)
	}
	return m
}

func noDiagnostics(t *testing.T, result wsproto.ProbeResult) {
	t.Helper()
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v", result.Diagnostics)
	}
}

func TestOneComponentPerComposerJSON(t *testing.T) {
	spectest.Proves(t, feature, "components-from-manifests", "one-component-per-composer-json")
	root := writeTree(t, map[string]string{
		"composer.json":                         `{"type": "project"}`,
		"packages/billing/composer.json":        `{"name": "acme/billing", "version": "2.1.0"}`,
		"packages/billing/README.md":            "billing",
		"bundles/AuditBundle/composer.json":     `{"name": "acme/audit-bundle", "type": "symfony-bundle"}`,
		"vendor/brick/math/composer.json":       `{"name": "brick/math"}`,
		"node_modules/tool/composer.json":       `{"name": "tool/tool"}`,
		".cache/copy/composer.json":             `{"name": "acme/copy"}`,
		"docs/composer.json.example":            `{}`,
		"packages/billing/vendor/composer.json": `{"name": "nested/vendor"}`,
	})
	result := Detect(root)
	noDiagnostics(t, result)
	got := make([]string, 0, len(result.Projects))
	for _, p := range result.Projects {
		got = append(got, p.Path+" "+p.SourceName+" "+p.Type+" "+p.Version+" "+p.SourceFile)
	}
	want := []string{
		". " + " application " + " composer.json",
		"bundles/AuditBundle acme/audit-bundle library  bundles/AuditBundle/composer.json",
		"packages/billing acme/billing library 2.1.0 packages/billing/composer.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projects\n got %q\nwant %q", got, want)
	}
	if result.Extension != ExtensionName || result.Version != wsproto.ProbeProtocolVersion {
		t.Fatalf("result names %q v%d", result.Extension, result.Version)
	}
}

func TestTheAnswerIsAValidProbeResult(t *testing.T) {
	spectest.Proves(t, feature, "components-from-manifests", "the-answer-is-a-valid-probe-result")
	root, err := filepath.Abs(demo)
	if err != nil {
		t.Fatal(err)
	}
	paths, diags := Candidates(root)
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	var request bytes.Buffer
	if err := wsproto.EncodeProbeRequest(&request, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: ExtensionName, Reason: wsproto.ProbeReasonLoad, Paths: paths,
	}); err != nil {
		t.Fatal(err)
	}
	var answer bytes.Buffer
	handled, err := wsproto.ServeProbe(wsproto.ProbeControlArgs(), &request, &answer, Answer(root))
	if !handled || err != nil {
		t.Fatalf("served %v: %v", handled, err)
	}
	result, problems := wsproto.ParseAndValidateProbeResult(answer.Bytes())
	if diag.HasErrors(problems) {
		t.Fatalf("the answer does not validate: %v\n%s", problems, answer.Bytes())
	}
	if !reflect.DeepEqual(byPath(t, *result)["."].Dependencies, []string{"packages/pricing"}) {
		t.Fatalf("the answer lost the edge: %s", answer.Bytes())
	}
	if strings.Contains(answer.String(), root) {
		t.Fatalf("the answer carries the absolute root: %s", answer.Bytes())
	}
}

func TestARequiredPathRepositoryPackageIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "path-repository-edges", "a-required-path-repository-package-is-an-edge")
	result := Detect(demo)
	noDiagnostics(t, result)
	projects := byPath(t, result)
	if got := projects["."].Dependencies; !reflect.DeepEqual(got, []string{"packages/pricing"}) {
		t.Fatalf("the application depends on %v", got)
	}
	if got := projects["packages/pricing"].Dependencies; len(got) != 0 {
		t.Fatalf("pricing depends on %v", got)
	}

	// A repository list keyed by name, a literal url, require-dev, a name in
	// another case, and a package's own path repository.
	root := writeTree(t, map[string]string{
		"composer.json": `{
			"type": "project",
			"require": {"Acme/Billing": "*"},
			"require-dev": {"acme/fixtures": "*"},
			"repositories": {"billing": {"type": "path", "url": "./libs/billing/"}, "packagist.org": false}
		}`,
		"libs/billing/composer.json": `{
			"name": "acme/billing",
			"require": {"acme/money": "^1"},
			"repositories": [{"type": "path", "url": "../money"}]
		}`,
		"libs/money/composer.json":     `{"name": "acme/money"}`,
		"tools/fixtures/composer.json": `{"name": "acme/fixtures"}`,
		"tools/composer.json":          `{"name": "acme/tools", "repositories": [{"type": "path", "url": "*"}]}`,
	})
	result = Detect(root)
	noDiagnostics(t, result)
	projects = byPath(t, result)
	if got := projects["."].Dependencies; !reflect.DeepEqual(got, []string{"libs/billing", "tools/fixtures"}) {
		t.Fatalf("the application depends on %v", got)
	}
	if got := projects["libs/billing"].Dependencies; !reflect.DeepEqual(got, []string{"libs/money"}) {
		t.Fatalf("billing depends on %v", got)
	}

	// A checkout directory with glob characters in its name keeps the edge.
	checkout := filepath.Join(t.TempDir(), "repo[1]")
	for name, content := range map[string]string{
		"composer.json":            `{"type": "project", "require": {"acme/a": "*"}, "repositories": [{"type": "path", "url": "packages/*"}]}`,
		"packages/a/composer.json": `{"name": "acme/a"}`,
	} {
		file := filepath.Join(checkout, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := byPath(t, Detect(checkout))["."].Dependencies; !reflect.DeepEqual(got, []string{"packages/a"}) {
		t.Fatalf("in %s the application depends on %v", checkout, got)
	}
}

func TestARegistryPackageIsNotAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "path-repository-edges", "a-registry-package-is-not-an-edge")
	outside := writeTree(t, map[string]string{"shared/composer.json": `{"name": "acme/shared"}`})
	root := writeTree(t, map[string]string{
		"composer.json": `{
			"name": "acme/app",
			"type": "project",
			"require": {"acme/app": "*", "acme/registry": "^1", "acme/shared": "*", "acme/unlisted": "*", "php": ">=8.4"},
			"repositories": [
				{"type": "path", "url": "` + filepath.ToSlash(filepath.Join(outside, "shared")) + `"},
				{"type": "path", "url": "../*"},
				{"type": "vcs", "url": "https://example.com/acme/registry.git"},
				{"type": "path", "url": "."}
			]
		}`,
		"libs/unlisted/composer.json": `{"name": "acme/unlisted"}`,
	})
	result := Detect(root)
	noDiagnostics(t, result)
	if got := byPath(t, result)["."].Dependencies; len(got) != 0 {
		t.Fatalf("the application depends on %v", got)
	}
}

func TestAutoloadDirectoriesAreAreas(t *testing.T) {
	spectest.Proves(t, feature, "areas", "autoload-directories-are-areas")
	projects := byPath(t, Detect(demo))
	app := metadata(t, projects["."])
	if want := []Area{{Name: "Controller", Paths: []string{"src/Controller", "tests/Controller"}}}; !reflect.DeepEqual(app.Areas, want) {
		t.Fatalf("application areas %+v", app.Areas)
	}
	if !reflect.DeepEqual(app.Sources, []string{"src"}) || !reflect.DeepEqual(app.Tests, []string{"tests"}) {
		t.Fatalf("application roots %v %v", app.Sources, app.Tests)
	}
	// The application's autoload-dev and PHPUnit test suites name the
	// package's directories; they belong to the package.
	pricing := metadata(t, projects["packages/pricing"])
	if !reflect.DeepEqual(pricing.Sources, []string{"packages/pricing/src"}) || !reflect.DeepEqual(pricing.Tests, []string{"packages/pricing/tests"}) {
		t.Fatalf("pricing roots %v %v", pricing.Sources, pricing.Tests)
	}
	if len(pricing.Areas) != 0 {
		t.Fatalf("pricing areas %+v", pricing.Areas)
	}

	// A list of roots, a root at the component directory, src/ without a
	// PSR-4 root, and a nested component that keeps its directory.
	root := writeTree(t, map[string]string{
		"composer.json":                 `{"type": "project", "autoload": {"psr-4": {"App\\": ["src/", "lib/"], "Root\\": ""}}}`,
		"src/Billing/Invoice.php":       "<?php\n",
		"src/Billing/.hidden/x.php":     "<?php\n",
		"src/Kernel.php":                "<?php\n",
		"lib/Shipping/Rate.php":         "<?php\n",
		"src/Inner/composer.json":       `{"name": "acme/inner"}`,
		"src/Inner/src/Model/Item.php":  "<?php\n",
		"legacy/composer.json":          `{"name": "acme/legacy"}`,
		"legacy/src/Report/Monthly.php": "<?php\n",
	})
	projects = byPath(t, Detect(root))
	app = metadata(t, projects["."])
	want := []Area{{Name: "Billing", Paths: []string{"src/Billing"}}, {Name: "Shipping", Paths: []string{"lib/Shipping"}}}
	if !reflect.DeepEqual(app.Areas, want) {
		t.Fatalf("application areas %+v", app.Areas)
	}
	legacy := metadata(t, projects["legacy"])
	if want := []Area{{Name: "Report", Paths: []string{"legacy/src/Report"}}}; !reflect.DeepEqual(legacy.Areas, want) || !reflect.DeepEqual(legacy.Sources, []string{"legacy/src"}) {
		t.Fatalf("legacy %+v", legacy)
	}
	inner := metadata(t, projects["src/Inner"])
	if want := []Area{{Name: "Model", Paths: []string{"src/Inner/src/Model"}}}; !reflect.DeepEqual(inner.Areas, want) {
		t.Fatalf("inner areas %+v", inner.Areas)
	}
}

func TestTheSymfonyDemoMapsEachChangedFile(t *testing.T) {
	spectest.Proves(t, feature, "areas", "the-symfony-demo-maps-each-changed-file")
	result := Detect(demo)
	noDiagnostics(t, result)
	// The files merge request !1 changes, then files of the application.
	for file, want := range map[string]Location{
		"composer.lock":                             {Component: "."},
		"packages/pricing/composer.json":            {Component: "packages/pricing"},
		"packages/pricing/src/Calculator.php":       {Component: "packages/pricing"},
		"packages/pricing/tests/CalculatorTest.php": {Component: "packages/pricing"},
		"src/Controller/QuoteController.php":        {Component: ".", Area: "Controller"},
		"tests/Controller/QuoteControllerTest.php":  {Component: ".", Area: "Controller"},
		"src/Kernel.php":                            {Component: "."},
		"config/packages/framework.yaml":            {Component: "."},
		"packages/pricingextra/x.php":               {Component: "."},
	} {
		got, ok := Locate(result, file)
		if !ok || got != want {
			t.Errorf("%s maps to %+v (%v), want %+v", file, got, ok, want)
		}
	}
	if _, ok := Locate(result, "../outside.php"); ok {
		t.Error("a path outside the repository maps to a component")
	}
	if _, ok := Locate(wsproto.ProbeResult{Projects: []wsproto.ProbeProject{{Path: "packages/pricing"}}}, "src/Kernel.php"); ok {
		t.Error("a path no component holds maps to a component")
	}
	if got, ok := Locate(wsproto.ProbeResult{Projects: []wsproto.ProbeProject{{Path: ".", Metadata: json.RawMessage(`{"areas": 1}`)}}}, "src/A.php"); !ok || got != (Location{Component: "."}) {
		t.Errorf("unreadable metadata maps to %+v (%v)", got, ok)
	}
}

func TestTheLockfileAndReportOutputsAreExposed(t *testing.T) {
	spectest.Proves(t, feature, "analysis-inputs", "the-lockfile-and-report-outputs-are-exposed")
	projects := byPath(t, Detect(demo))
	app := metadata(t, projects["."])
	if app.Lockfile != "composer.lock" || !reflect.DeepEqual(app.PHPUnit, &PHPUnit{Config: "phpunit.dist.xml"}) {
		t.Fatalf("application %+v %+v", app, app.PHPUnit)
	}
	if got := projects["."].WatchedFiles; !reflect.DeepEqual(got, []string{"composer.lock", "phpunit.dist.xml"}) {
		t.Fatalf("watched files %v", got)
	}
	if pricing := metadata(t, projects["packages/pricing"]); pricing.Lockfile != "" || pricing.PHPUnit != nil {
		t.Fatalf("pricing %+v", pricing)
	}

	root := writeTree(t, map[string]string{
		"composer.json": `{"type": "project"}`,
		"phpunit.xml": `<phpunit>
			<testsuites><testsuite name="unit"><directory> tests/Unit </directory><directory>/abs</directory></testsuite></testsuites>
			<logging><junit outputFile="build/junit.xml"/></logging>
			<coverage><report><clover outputFile="build/clover.xml"/><cobertura outputFile="../escape.xml"/></report></coverage>
		</phpunit>`,
		"phpunit.dist.xml":        `not read: phpunit.xml comes first`,
		"legacy/composer.json":    `{"name": "acme/legacy"}`,
		"legacy/phpunit.xml.dist": `not read: PHPUnit looks up phpunit.dist.xml first`,
		"legacy/phpunit.dist.xml": `<phpunit><logging>
			<log type="junit" target="var/junit.xml"/>
			<log type="coverage-clover" target="var/clover.xml"/>
			<log type="coverage-cobertura" target="var/cobertura.xml"/>
			<log type="coverage-html" target="var/html"/>
		</logging></phpunit>`,
	})
	projects = byPath(t, Detect(root))
	app = metadata(t, projects["."])
	if want := (&PHPUnit{Config: "phpunit.xml", JUnit: "build/junit.xml", Clover: "build/clover.xml"}); !reflect.DeepEqual(app.PHPUnit, want) {
		t.Fatalf("application PHPUnit %+v", app.PHPUnit)
	}
	if !reflect.DeepEqual(app.Tests, []string{"tests/Unit"}) {
		t.Fatalf("application tests %v", app.Tests)
	}
	legacy := metadata(t, projects["legacy"])
	if want := (&PHPUnit{Config: "legacy/phpunit.dist.xml", JUnit: "legacy/var/junit.xml", Clover: "legacy/var/clover.xml", Cobertura: "legacy/var/cobertura.xml"}); !reflect.DeepEqual(legacy.PHPUnit, want) {
		t.Fatalf("legacy PHPUnit %+v", legacy.PHPUnit)
	}
}

func TestAMalformedManifestIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "analysis-inputs", "a-malformed-manifest-is-a-diagnostic")
	root := writeTree(t, map[string]string{
		"composer.json":             `{"type": "project", "require": {"acme/ok": "*"}, "repositories": [{"type": "path", "url": "libs/*"}]}`,
		"phpunit.xml":               `<phpunit><testsuites>`,
		"libs/broken/composer.json": `{"name": `,
		"libs/ok/composer.json":     `{"name": "acme/ok", "autoload": {"psr-4": {"Ok\\": 3}}}`,
		"libs/odd/composer.json":    `{"name": "acme/odd", "repositories": "none"}`,
	})
	result := Detect(root)
	codes := make([]string, 0, len(result.Diagnostics))
	for _, d := range result.Diagnostics {
		codes = append(codes, d.Code+" "+d.Field)
	}
	want := []string{CodePHPUnitInvalid + " phpunit.xml", CodeManifestInvalid + " libs/broken/composer.json"}
	slicesEqualUnordered(t, codes, want)
	projects := byPath(t, result)
	if len(projects) != 3 || !reflect.DeepEqual(projects["."].Dependencies, []string{"libs/ok"}) {
		t.Fatalf("projects %+v", result.Projects)
	}
	if app := metadata(t, projects["."]); app.PHPUnit != nil {
		t.Fatalf("a malformed configuration is read: %+v", app.PHPUnit)
	}
}

func slicesEqualUnordered(t *testing.T, got, want []string) {
	t.Helper()
	count := map[string]int{}
	for _, g := range got {
		count[g]++
	}
	for _, w := range want {
		count[w]--
	}
	for k, n := range count {
		if n != 0 {
			t.Fatalf("diagnostics %q, want %q (%s)", got, want, k)
		}
	}
}

func TestCandidatesOfAMissingDirectory(t *testing.T) {
	paths, diags := Candidates(filepath.Join(t.TempDir(), "missing"))
	if len(paths) != 0 || len(diags) != 0 {
		t.Fatalf("paths %v diagnostics %v", paths, diags)
	}
	result := Probe(t.TempDir(), wsproto.ProbeRequest{Paths: []string{"../x", ".", "."}})
	if len(result.Projects) != 0 {
		t.Fatalf("projects %+v", result.Projects)
	}
}
