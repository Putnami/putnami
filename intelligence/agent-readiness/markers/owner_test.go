package markers

import (
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/history"
	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
)

func TestGuardedSkipsLeaveTheReliableSignal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{"database binding", []string{
			"\tres, err := testprovider.Provision(ctx, opts)",
			"\tif errors.Is(err, testprovider.ErrSkip) {",
			"\t\tt.Skip(\"no usable test database binding (mode=skip)\")",
		}, true},
		{"missing shell", []string{
			"\tif _, err := exec.LookPath(\"/bin/sh\"); err != nil {",
			"\t\tt.Skip(\"/bin/sh unavailable on this platform\")",
		}, true},
		{"platform check", []string{"\tif runtime.GOOS == \"windows\" {", "\t\tt.Skip(\"symlinks\")"}, true},
		{"one-line platform check", []string{"\tif runtime.GOOS == \"plan9\" { t.Skip(\"no fork\") }"}, true},
		{"opt-in variable", []string{
			"\tdsn := os.Getenv(\"MIGRATE_E2E_ADMIN_DSN\")",
			"\tif dsn == \"\" {",
			"\t\tt.Skip(\"set MIGRATE_E2E_ADMIN_DSN to run the local bootstrap\")",
		}, true},
		{"pytest skipif on the platform", []string{"@pytest.mark.skipif(sys.platform == \"win32\", reason=\"posix only\")"}, true},
		{"pytest skipif over lines", []string{
			"@pytest.mark.skipif(",
			"    not HAS_EMAIL_VALIDATOR,",
			"    reason=\"email-validator not installed\",",
			")",
		}, true},
		{"python dependency probe", []string{"    if shutil.which(\"git\") is None:", "        pytest.skip(\"git is not installed\")"}, true},
		{"junit platform annotation", []string{"    @DisabledOnOs(OS.WINDOWS)"}, true},
		{"unconditional flaky skip", []string{"func TestLedger(t *testing.T) { t.Skip(\"flaky\") }"}, false},
		{"unconditional reason naming a dependency", []string{"\tt.Skip(\"requires docker\")"}, false},
		{"condition on CI", []string{"\tif os.Getenv(\"CI\") != \"\" {", "\t\tt.Skip(\"fails on CI\")"}, false},
		{"condition naming a flaky test", []string{"\tif runtime.GOOS == \"darwin\" {", "\t\tt.Skip(\"flaky on macOS\")"}, false},
		{"short mode is not a guard", []string{"\tif testing.Short() {", "\t\tt.Skip(\"skipping in short mode\")"}, false},
		{"guard closed above", []string{"\tif runtime.GOOS == \"windows\" {", "\t}", "\tt.Skip(\"not written yet\")"}, false},
		{"blank line ends the search", []string{"\tif runtime.GOOS == \"windows\" {", "", "\tt.Skip(\"not written yet\")"}, false},
		{"retries always count", []string{"\tif process.platform === 'win32' {", "  retries: 2,"}, false},
		{"focused tests always count", []string{"if (process.platform === 'win32') {", "  it.only('works', () => {})"}, false},
		{"junit unconditional", []string{"    @Disabled(\"until the dependency lands\")"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GuardedSkip(tc.lines, len(tc.lines)-1-trailing(tc.lines)); got != tc.want {
				t.Fatalf("GuardedSkip(%q) = %v, want %v", tc.lines, got, tc.want)
			}
		})
	}
	// pytest.importorskip skips the module when the import fails. The signal
	// pattern never matched it, so it never counted.
	if strings.Contains(SignalPattern, "importorskip") || GuardedSkip([]string{"np = pytest.importorskip(\"numpy\")"}, 0) {
		t.Fatal("pytest.importorskip must stay outside the signal")
	}
}

// trailing counts the lines after a multi-line statement's first line: the
// skipif case puts the skip on its first line.
func trailing(lines []string) int {
	for i, line := range lines {
		if guardSkip.MatchString(line) || strings.Contains(line, "retries:") || strings.Contains(line, ".only(") {
			return len(lines) - 1 - i
		}
	}
	return 0
}

// TestReliableSignalSampleLeavesGuardedSkipsOut checks that a guarded skip
// at HEAD leaves the sample even when its file also carries a counted one.
func TestReliableSignalSampleLeavesGuardedSkipsOut(t *testing.T) {
	c := computationOf([]string{".github/workflows/ci.yml", "go.mod", "store/store_test.go"}, map[string]string{
		".github/workflows/ci.yml": "on: pull_request\njobs: {t: {steps: [{run: go test ./...}]}}",
	})
	c.SignalContents = map[string][]byte{"store/store_test.go": []byte(strings.Join([]string{
		"package store",
		"func TestA(t *testing.T) {",
		"\tif runtime.GOOS == \"windows\" {",
		"\t\tt.Skip(\"symlinks\")",
		"\t}",
		"\tt.Skip(\"flaky\")",
		"}",
	}, "\n"))}
	c.Signals = []gitrepo.Match{{Path: "store/store_test.go", Line: 4}, {Path: "store/store_test.go", Line: 6}}
	c.SignalsAdded = map[string]int{"store/store_test.go": 1}
	if m := c.reliableSignal(-1); m.State != contract.MarkerExists || strings.Join(m.Evidence.Sample, ",") != "store/store_test.go:6" {
		t.Fatalf("reliable signal = %+v, want the flaky skip only", m)
	}
}

func TestJavaBuildsPinTheToolchain(t *testing.T) {
	enforcer := "<project><build><plugins><plugin><artifactId>maven-enforcer-plugin</artifactId>" +
		"<configuration><rules><requireUpperBoundDeps/></rules></configuration></plugin></plugins></build></project>"
	mavenCI := "on:\n  pull_request:\njobs:\n  test:\n    steps:\n      - run: mvn -B verify\n"
	gradleCI := "on:\n  pull_request:\njobs:\n  test:\n    steps:\n      - run: ./gradlew build\n"
	for _, tc := range []struct {
		name     string
		contents map[string]string
		want     contract.MarkerState
		sample   string
	}{
		{"Maven wrapper alone", map[string]string{
			"pom.xml": "<project/>", ".mvn/wrapper/maven-wrapper.properties": "distributionUrl=x", ".github/workflows/ci.yml": mavenCI,
		}, contract.MarkerExists, ".mvn/wrapper/maven-wrapper.properties"},
		{"Gradle wrapper alone", map[string]string{
			"build.gradle": "plugins { id 'java' }", "gradle/wrapper/gradle-wrapper.properties": "distributionUrl=x", ".github/workflows/ci.yml": gradleCI,
		}, contract.MarkerExists, "gradle/wrapper/gradle-wrapper.properties"},
		{"dependency-track Enforcer rule in CI", map[string]string{
			"pom.xml": enforcer, ".github/workflows/ci.yml": mavenCI,
		}, contract.MarkerEnforced, "pom.xml,.github/workflows/ci.yml"},
		{"Enforcer rule without CI", map[string]string{"pom.xml": enforcer}, contract.MarkerExists, "pom.xml"},
		{"Enforcer without a locking rule", map[string]string{
			"pom.xml": "<project><artifactId>maven-enforcer-plugin</artifactId><requireJavaVersion/></project>", ".github/workflows/ci.yml": mavenCI,
		}, contract.MarkerAbsent, ""},
		{"Gradle lockfile in CI", map[string]string{
			"build.gradle": "plugins { id 'java' }", "gradle.lockfile": "x=1", "gradle/wrapper/gradle-wrapper.properties": "d", ".github/workflows/ci.yml": gradleCI,
		}, contract.MarkerEnforced, "gradle.lockfile,gradle/wrapper/gradle-wrapper.properties,.github/workflows/ci.yml"},
		{"Gradle dependency locking in CI", map[string]string{
			"build.gradle.kts": "dependencyLocking { lockAllConfigurations() }", ".github/workflows/ci.yml": gradleCI,
		}, contract.MarkerEnforced, "build.gradle.kts,.github/workflows/ci.yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := make([]string, 0, len(tc.contents))
			for file := range tc.contents {
				paths = append(paths, file)
			}
			m := computationOf(paths, tc.contents).pinnedToolchain()
			if m.State != tc.want || strings.Join(m.Evidence.Sample, ",") != tc.sample {
				t.Fatalf("pinned toolchain = %+v, want %s with %q", m, tc.want, tc.sample)
			}
		})
	}
	files := []gitrepo.File{{Path: "build.gradle.kts"}, {Path: "gradle/wrapper/gradle-wrapper.properties"}, {Path: "lib/deep/build.gradle"}}
	if wanted := Wanted(files); !slices.Contains(wanted, "build.gradle.kts") {
		t.Fatalf("Wanted = %v, want the Gradle script", wanted)
	}
	if dated := Dated(files, areas.Layout{}); !slices.Contains(dated, "gradle/wrapper/gradle-wrapper.properties") || !slices.Contains(dated, "build.gradle.kts") {
		t.Fatalf("Dated = %v, want the wrapper and the Gradle script", dated)
	}
}

func TestSupportingAreasMirrorTheCode(t *testing.T) {
	paths := []string{"Gemfile", "app/models/account.rb", "spec/models/account_spec.rb", "config/routes.rb", "docs/index.md"}
	layout := areas.Detect(paths, func([]string) map[string][]byte { return nil })
	app, spec, config := layout.Assign("app/models/account.rb"), layout.Assign("spec/models/account_spec.rb"), layout.Assign("config/routes.rb")
	if layout.Areas[spec].Role != contract.AreaTests || layout.Areas[app].Role != contract.AreaCode || layout.Areas[layout.Assign("docs/index.md")].Role != contract.AreaDocs {
		t.Fatalf("roles = %+v", layout.Areas)
	}
	repo, perArea := Measure(layout, []history.Commit{
		{Author: "a", Time: 4, Files: []history.FileChange{{Path: "app/models/account.rb"}, {Path: "spec/models/account_spec.rb"}}},
		{Author: "a", Time: 3, Files: []history.FileChange{{Path: "app/models/account.rb"}}},
		{Author: "a", Time: 2, Files: []history.FileChange{{Path: "app/models/account.rb"}, {Path: "config/routes.rb"}}},
		{Author: "a", Time: 1, Files: []history.FileChange{{Path: "app/models/account.rb"}, {Path: "spec/models/account_spec.rb"}, {Path: "Gemfile"}}},
	}, nil)
	if got := perArea[app]; got.CodeCommits != 4 || got.CodeWithTest != 2 || got.CrossArea != 1 {
		t.Fatalf("app = %+v, want 2 of 4 code commits with a test and 1 cross-area change", got)
	}
	if got := perArea[config]; got.CodeWithTest != 0 || got.CrossArea != 1 {
		t.Fatalf("config = %+v", got)
	}
	if repo.CrossArea != 1 || repo.AreaCommits != 4 {
		t.Fatalf("repository = %+v", repo)
	}
	if root := perArea[layout.Assign("Gemfile")]; root.Commits != 0 {
		t.Fatalf("root = %+v, want no commit: the Gemfile change came with app/", root)
	}

	c := computationOf(paths, nil)
	c.Areas = perArea
	if m := c.tests(c.Layout.Assign("app/models/account.rb")); m.State == contract.MarkerAbsent || !slices.Contains(m.Evidence.Sample, "spec/models/account_spec.rb") {
		t.Fatalf("app tests = %+v, want the spec tree to count", m)
	}
	if m := c.tests(c.Layout.Assign("docs/index.md")); m.State != contract.MarkerAbsent {
		t.Fatalf("docs tests = %+v, want absent: the spec tree tests code only", m)
	}
}

func TestCrossAreaCarriesHowLongCIHasChecked(t *testing.T) {
	layout := areas.Detect([]string{"a/go.mod", "b/go.mod"}, func([]string) map[string][]byte { return nil })
	const now = 1_000 * secondsPerDay
	for _, tc := range []struct {
		name string
		ci   string
		want *float64
	}{
		{"impact-aware CI for 120 days", "on: pull_request\nrun: putnami test --impacted\n", number(120)},
		{"full CI", "on: pull_request\nrun: go test ./...\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newComputation(Input{
				Now:      now,
				Files:    []gitrepo.File{{Path: ".github/workflows/ci.yml"}, {Path: "a/go.mod"}, {Path: "b/go.mod"}},
				Layout:   layout,
				Repo:     Activity{Commits: 4, AreaCommits: 4, CrossArea: 1},
				Contents: map[string][]byte{".github/workflows/ci.yml": []byte(tc.ci)},
				Dates:    gitrepo.FileDates{Added: map[string]int64{".github/workflows/ci.yml": now - 120*secondsPerDay}},
			})
			m := c.crossArea(-1)
			if (m.EnforcedDays == nil) != (tc.want == nil) || (m.EnforcedDays != nil && *m.EnforcedDays != *tc.want) {
				t.Fatalf("cross-area = %+v, want enforcedDays %v", m, tc.want)
			}
		})
	}
}

func TestBoundaryToolsEnforceTheBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents map[string]string
		// covered is a file of an area the tool covers; outside is a file of
		// an area it does not, "" when the tool covers the repository.
		covered, outside string
	}{
		{"Putnami architecture gate", map[string]string{
			"billing/putnami.architecture.json": `{"dependsOn":[]}`,
			"billing/api/main.go":               "package main",
			"shop/web/main.go":                  "package main",
			".github/workflows/ci.yml":          "on: pull_request\njobs: {t: {steps: [{run: ./putnamiw lint,test,build,validate}]}}",
		}, "billing/api/main.go", "shop/web/main.go"},
		{"golangci-lint depguard on the module's own packages", map[string]string{
			"go.mod":                   "module acme.example/shop\n",
			".golangci.yml":            "linters:\n  enable: [depguard]\nsettings:\n  depguard:\n    rules:\n      web:\n        files: ['**/web/**']\n        deny:\n          - pkg: acme.example/shop/billing\n",
			"billing/ledger.go":        "package billing",
			"web/handler.go":           "package web",
			".github/workflows/ci.yml": "on: pull_request\njobs: {t: {steps: [{run: golangci-lint run}]}}",
		}, "web/handler.go", ""},
		{"eslint-plugin-boundaries", map[string]string{
			"eslint.config.js":         "import boundaries from 'eslint-plugin-boundaries'\nexport default [{rules: {'boundaries/element-types': 'error'}}]",
			"packages/a/package.json":  `{"name": "a"}`,
			"packages/a/src/index.ts":  "export {}",
			"packages/b/package.json":  `{"name": "b"}`,
			"packages/b/src/index.ts":  "export {}",
			".github/workflows/ci.yml": "on: pull_request\njobs: {t: {steps: [{run: pnpm lint}]}}",
		}, "packages/a/src/index.ts", ""},
		{"Nx enforce-module-boundaries", map[string]string{
			".eslintrc.json":           `{"rules": {"@nx/enforce-module-boundaries": "error"}}`,
			"libs/a/package.json":      `{"name": "a"}`,
			"libs/a/src/index.ts":      "export {}",
			"libs/b/package.json":      `{"name": "b"}`,
			"libs/b/src/index.ts":      "export {}",
			".github/workflows/ci.yml": "on: pull_request\njobs: {t: {steps: [{run: npx nx affected -t lint}]}}",
		}, "libs/a/src/index.ts", ""},
		{"ArchUnit in the module that declares it", map[string]string{
			"pom.xml":                             "<project><dependencyManagement><dependencies><dependency><artifactId>archunit-junit5</artifactId></dependency></dependencies></dependencyManagement></project>",
			"apiserver/pom.xml":                   "<project><dependencies><dependency><groupId>com.tngtech.archunit</groupId><artifactId>archunit-junit5</artifactId></dependency></dependencies></project>",
			"apiserver/src/main/java/Server.java": "class Server {}",
			"engine/pom.xml":                      "<project/>",
			"engine/src/main/java/Engine.java":    "class Engine {}",
			".github/workflows/ci.yml":            "on: pull_request\njobs: {t: {steps: [{run: mvn -B verify}]}}",
		}, "apiserver/src/main/java/Server.java", "engine/src/main/java/Engine.java"},
		{"Go internal packages", map[string]string{
			"svc/go.mod":               "module acme.example/svc\n",
			"svc/internal/store/db.go": "package store",
			"svc/main.go":              "package main",
			".github/workflows/ci.yml": "on: pull_request\njobs: {t: {steps: [{run: go test ./...}]}}",
		}, "svc/main.go", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := make([]string, 0, len(tc.contents))
			for file := range tc.contents {
				paths = append(paths, file)
			}
			c := computationOf(paths, tc.contents)
			if m := c.boundaryRules(c.Layout.Assign(tc.covered)); m.State != contract.MarkerEnforced {
				t.Fatalf("covered area = %+v, want enforced", m)
			}
			if m := c.boundaryRules(-1); m.State != contract.MarkerEnforced {
				t.Fatalf("repository = %+v, want enforced", m)
			}
			if tc.outside != "" {
				if m := c.boundaryRules(c.Layout.Assign(tc.outside)); m.State == contract.MarkerEnforced {
					t.Fatalf("uncovered area = %+v, want the tool to stay out", m)
				}
			}
			delete(tc.contents, ".github/workflows/ci.yml")
			without := computationOf(paths, tc.contents)
			if m := without.boundaryRules(without.Layout.Assign(tc.covered)); m.State != contract.MarkerExists {
				t.Fatalf("covered area without CI = %+v, want exists", m)
			}
		})
	}

	// alertmanager's depguard only bans third-party packages: it guards no
	// boundary between the repository's own areas.
	contents := map[string]string{
		"go.mod":                   "module github.com/prometheus/alertmanager\n",
		".golangci.yml":            "linters:\n  enable: [depguard]\nsettings:\n  depguard:\n    rules:\n      main:\n        deny:\n          - pkg: github.com/pkg/errors\n",
		"notify/notify.go":         "package notify",
		".github/workflows/ci.yml": "on: pull_request\njobs: {t: {steps: [{run: golangci-lint run}]}}",
	}
	paths := []string{"go.mod", ".golangci.yml", "notify/notify.go", ".github/workflows/ci.yml"}
	c := computationOf(paths, contents)
	if m := c.boundaryRules(c.Layout.Assign("notify/notify.go")); m.State != contract.MarkerAbsent {
		t.Fatalf("third-party depguard = %+v, want absent", m)
	}
	files := []gitrepo.File{{Path: ".golangci.yml"}, {Path: "billing/putnami.architecture.json"}}
	if wanted := Wanted(files); len(wanted) != 2 {
		t.Fatalf("Wanted = %v, want the golangci-lint configuration and the architecture manifest", wanted)
	}
}

// TestLongestEnforcementSpeaks keeps the oldest active boundary treatment's
// duration when a newer treatment is added.
func TestLongestEnforcementSpeaks(t *testing.T) {
	at := func(v float64) contract.Marker { return contract.Marker{State: contract.MarkerEnforced, Value: &v} }
	undated := contract.Marker{State: contract.MarkerEnforced}
	for _, tc := range []struct {
		name string
		a, b contract.Marker
		want bool
	}{
		{"older wins", at(44), at(24), true},
		{"newer loses", at(24), at(44), false},
		{"a tie keeps the first", at(24), at(24), false},
		{"dated beats undated", at(1), undated, true},
		{"undated never wins", undated, at(1), false},
	} {
		if got := longer(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: longer = %v, want %v", tc.name, got, tc.want)
		}
	}
}
