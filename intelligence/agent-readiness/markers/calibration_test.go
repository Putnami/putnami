package markers

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
)

// computationOf builds the markers' input from paths and their contents, the
// way the collector reads a repository at HEAD.
func computationOf(paths []string, contents map[string]string) *computation {
	sort.Strings(paths)
	files := make([]gitrepo.File, 0, len(paths))
	for _, file := range paths {
		files = append(files, gitrepo.File{Path: file})
	}
	data := map[string][]byte{}
	for file, content := range contents {
		data[file] = []byte(content)
	}
	read := func(wanted []string) map[string][]byte {
		out := map[string][]byte{}
		for _, file := range wanted {
			if content, ok := data[file]; ok {
				out[file] = content
			}
		}
		return out
	}
	return newComputation(Input{Files: files, Layout: areas.Detect(paths, read), Contents: data})
}

// TestReliableSignalCountsOnlyTestsRunnersAndCI keeps production source out
// of the test-signal count, even when its text resembles retry or focus syntax.
func TestReliableSignalCountsOnlyTestsRunnersAndCI(t *testing.T) {
	for file, want := range map[string]bool{
		"control/libs/engine/invoker_propagation.go":              false,
		"app/javascript/mastodon/features/emoji/database.ts":      false,
		"deploy/istio/virtual-service.yaml":                       false,
		"control/libs/engine/invoker_test.go":                     true,
		"spec/models/account_spec.rb":                             true,
		"web/playwright.config.ts":                                true,
		"jest.config.js":                                          true,
		"pytest.ini":                                              true,
		".github/workflows/ci.yml":                                true,
		".github/setup/action.yml":                                true,
		".gitlab-ci.yml":                                          true,
		"app/javascript/mastodon/features/emoji/database.test.ts": true,
	} {
		if got := SignalSource(file); got != want {
			t.Errorf("SignalSource(%q) = %v, want %v", file, got, want)
		}
	}

	c := computationOf([]string{".github/workflows/ci.yml", "go.mod"}, map[string]string{
		".github/workflows/ci.yml": "on: pull_request\njobs: {t: {steps: [{run: go test ./...}]}}",
	})
	c.Signals = []gitrepo.Match{
		{Path: "app/javascript/mastodon/features/emoji/database.ts", Line: 50},
		{Path: "control/libs/engine/invoker_propagation.go", Line: 137},
		{Path: "control/libs/engine/invoker_test.go", Line: 12},
		{Path: "web/playwright.config.ts", Line: 4},
	}
	c.SignalsAdded = map[string]int{
		"app/javascript/mastodon/features/emoji/database.ts": 1,
		"control/libs/engine/invoker_propagation.go":         1,
		"control/libs/engine/invoker_test.go":                1,
		"web/playwright.config.ts":                           1,
	}
	m := c.reliableSignal(-1)
	if m.State != contract.MarkerExists || m.Value == nil || *m.Value != 2 {
		t.Fatalf("reliable signal = %+v, want exists with 2 lines", m)
	}
	if want := "control/libs/engine/invoker_test.go:12,web/playwright.config.ts:4"; strings.Join(m.Evidence.Sample, ",") != want {
		t.Fatalf("sample = %v, want %s", m.Evidence.Sample, want)
	}
	if !strings.Contains(m.Evidence.Command, "-- control/libs/engine/invoker_test.go web/playwright.config.ts |") {
		t.Fatalf("command %q must name the files it counts", m.Evidence.Command)
	}
	if got := SignalFiles(c.Signals); strings.Join(got, ",") != "control/libs/engine/invoker_test.go,web/playwright.config.ts" {
		t.Fatalf("SignalFiles = %v, want the test and the runner configuration only", got)
	}

	c.Signals = []gitrepo.Match{{Path: "control/libs/engine/invoker_propagation.go", Line: 137}}
	c.SignalsAdded = map[string]int{"control/libs/engine/invoker_propagation.go": 3}
	if m := c.reliableSignal(-1); m.State != contract.MarkerEnforced || *m.Value != 0 {
		t.Fatalf("production matches alone = %+v, want enforced with 0", m)
	}
}

// TestPutnamiCommandsInInstructionsAreRecognized accepts explicit build and
// test commands in repository instructions, including wrapper invocations.
func TestPutnamiCommandsInInstructionsAreRecognized(t *testing.T) {
	paths := []string{"AGENTS.md", "putnami.workspace.json", "putnami.ci.json", "tooling/cli/putnami.json", "tooling/cli/main.go"}
	contents := map[string]string{
		"AGENTS.md": "This is a Putnami workspace.\n" +
			"- Build, test, lint, install, add dependencies, and generate with `./putnamiw` when present, otherwise `putnami`.\n" +
			"- Before declaring work complete, run `putnami lint,test,build,validate,validate-workspace --impacted --enforce-coverage` once.\n",
		"putnami.workspace.json":   "{}",
		"putnami.ci.json":          `{"commands":["lint","test","build"]}`,
		"tooling/cli/putnami.json": `{"name":"tooling/cli"}`,
	}
	if m := computationOf(paths, contents).instructions(-1); m.State != contract.MarkerEnforced {
		t.Fatalf("instructions = %+v, want enforced", m)
	}
	contents["AGENTS.md"] = "Run `./putnamiw test` before you push, and `./putnamiw build` to package.\n"
	if m := computationOf(paths, contents).instructions(-1); m.State != contract.MarkerEnforced {
		t.Fatalf("instructions through the wrapper = %+v, want enforced", m)
	}
	contents["AGENTS.md"] = "Use `./putnamiw` for every command.\n"
	if m := computationOf(paths, contents).instructions(-1); m.State != contract.MarkerExists {
		t.Fatalf("instructions naming no command = %+v, want exists", m)
	}
}

// TestMavenModulesAndLintPluginsCount recognizes Maven modules and lint
// plugins declared in pom.xml files.
func TestMavenModulesAndLintPluginsCount(t *testing.T) {
	paths := []string{
		"pom.xml", "Makefile", ".github/workflows/ci-lint.yaml",
		"common/pom.xml", "common/src/main/java/Common.java",
		"apiserver/pom.xml", "apiserver/src/main/java/Server.java",
		"alpine/pom.xml", "alpine/alpine-common/pom.xml", "alpine/alpine-common/src/main/java/Alpine.java",
		"support/net/pom.xml", "support/net/src/main/java/Net.java",
	}
	contents := map[string]string{
		"pom.xml": `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modules>
    <module>common</module>
    <module>support/net</module>
    <module>apiserver/pom.xml</module>
    <module>alpine</module>
  </modules>
  <build>
    <plugins>
      <plugin>
        <groupId>com.diffplug.spotless</groupId>
        <artifactId>spotless-maven-plugin</artifactId>
      </plugin>
    </plugins>
  </build>
</project>
`,
		"alpine/pom.xml":                 "<project><modules><module>alpine-common</module></modules></project>",
		"common/pom.xml":                 "<project><artifactId>common</artifactId></project>",
		"Makefile":                       "lint-java:\n\tmvn -q validate\n",
		".github/workflows/ci-lint.yaml": "on:\n  pull_request:\njobs:\n  lint-java:\n    steps:\n      - run: make lint-java\n",
	}
	c := computationOf(paths, contents)

	got := make([]string, 0, len(c.Layout.Areas))
	for _, area := range c.Layout.Areas {
		got = append(got, area.Path+"="+string(area.Source))
	}
	want := []string{".=inferred", "alpine=manifest", "alpine/alpine-common=manifest", "apiserver=manifest", "common=manifest", "support/net=manifest"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("areas = %v, want %v", got, want)
	}
	declared := c.declaredAreas()
	if declared.State != contract.MarkerEnforced || !slices.Contains(declared.Evidence.Sample, "pom.xml") {
		t.Fatalf("declared areas = %+v, want enforced by pom.xml", declared)
	}
	static := c.staticChecks()
	if static.State != contract.MarkerEnforced || strings.Join(static.Evidence.Sample, ",") != "pom.xml,.github/workflows/ci-lint.yaml" {
		t.Fatalf("static checks = %+v, want pom.xml enforced by the lint job", static)
	}

	contents[".github/workflows/ci-lint.yaml"] = "on:\n  pull_request:\njobs:\n  check:\n    steps:\n      - run: mvn -B spotless:check\n"
	if static := computationOf(paths, contents).staticChecks(); static.State != contract.MarkerEnforced {
		t.Fatalf("static checks with spotless:check = %+v, want enforced", static)
	}
	contents["pom.xml"] = "<project><modules><module>common</module></modules></project>"
	if static := computationOf(paths, contents).staticChecks(); static.State != contract.MarkerAbsent {
		t.Fatalf("a pom.xml without a lint plugin = %+v, want absent", static)
	}

	gitFiles := make([]gitrepo.File, 0, len(paths))
	for _, file := range paths {
		gitFiles = append(gitFiles, gitrepo.File{Path: file})
	}
	if wanted := Wanted(gitFiles); !slices.Contains(wanted, "alpine/alpine-common/pom.xml") {
		t.Fatalf("Wanted = %v, want every pom.xml", wanted)
	}
	if dated := Dated(gitFiles, c.Layout); !slices.Contains(dated, "alpine/pom.xml") || slices.Contains(dated, "alpine/alpine-common/src/main/java/Alpine.java") {
		t.Fatalf("Dated = %v, want the shallow pom.xml files", dated)
	}
}

// TestLocalActionsPinTheToolchain follows a workflow's local action to its
// toolchain setup declaration.
func TestLocalActionsPinTheToolchain(t *testing.T) {
	paths := []string{".nvmrc", "package.json", "pnpm-lock.yaml", ".github/workflows/main.yml", ".github/setup/action.yml"}
	workflow := "on:\n  pull_request:\njobs:\n  test:\n    steps:\n      - uses: ./.github/setup\n      - run: pnpm test\n"
	action := "runs:\n  using: composite\n  steps:\n    - uses: pnpm/action-setup@v6\n    - uses: actions/setup-node@v6\n" +
		"      with:\n        node-version-file: '.nvmrc'\n    - run: pnpm install\n      shell: bash\n"
	for _, tc := range []struct {
		name, workflow, action string
		want                   contract.MarkerState
	}{
		{"trpc", workflow, action, contract.MarkerEnforced},
		{"install opts out of the frozen lockfile", workflow, strings.ReplaceAll(action, "pnpm install", "pnpm install --no-frozen-lockfile"), contract.MarkerExists},
		{"no workflow calls the action", strings.ReplaceAll(workflow, "      - uses: ./.github/setup\n", ""), action, contract.MarkerExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := computationOf(paths, map[string]string{
				"package.json": `{"scripts":{"test":"vitest"}}`, ".github/workflows/main.yml": tc.workflow, ".github/setup/action.yml": tc.action,
			})
			if m := c.pinnedToolchain(); m.State != tc.want {
				t.Fatalf("pinned toolchain = %+v, want %s", m, tc.want)
			}
		})
	}
	files := []gitrepo.File{{Path: ".github/setup/action.yml"}, {Path: "src/action.ts"}}
	if wanted := Wanted(files); !slices.Contains(wanted, ".github/setup/action.yml") || slices.Contains(wanted, "src/action.ts") {
		t.Fatalf("Wanted = %v, want the local action", wanted)
	}
	if dated := Dated(files, areas.Layout{}); !slices.Contains(dated, ".github/setup/action.yml") {
		t.Fatalf("Dated = %v, want the local action", dated)
	}
}
