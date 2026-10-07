package markers

import (
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/history"
	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
)

func TestIsTest(t *testing.T) {
	for file, want := range map[string]bool{
		"a/b_test.go": true, "a/test_b.py": true, "a/b_test.py": true, "a/b.test.ts": true, "a/b.spec.tsx": true,
		"a/b_spec.rb": true, "a/BTest.java": true, "a/BTests.java": true, "a/BTest.kt": true, "infra/x.tftest.hcl": true,
		"a/__tests__/b.js": true, "a/tests/b.rs": true, "e2e/login.ts": true,
		"a/b.go": false, "a/testing.go": false, "a/contest.py": false,
		"a/tsconfig.spec.json": false, "a/test/env.test.yaml": false, "a/test/__snapshots__/b.spec.ts.snap": false,
		"a/test/.keep": false, ".gitlab/backend/test/lint.yml": false, ".vscode/command.spec.code-snippets": false,
		".github/actions/test/run.sh": false,
	} {
		if got := IsTest(file); got != want {
			t.Errorf("IsTest(%q) = %v, want %v", file, got, want)
		}
	}
}

func TestCovers(t *testing.T) {
	for _, tc := range []struct {
		pattern, dir string
		want         bool
	}{
		{"*", "a/b", true},
		{"/**", ".", true},
		{"/packages/checkout/", "packages/checkout", true},
		{"/packages/", "packages/checkout", true},
		{"packages/checkout/**", "packages/checkout", true},
		{"docs/", "site/docs", true},
		{"/docs/", "site/docs", false},
		{"/packages/ui/", "packages/checkout", false},
		{"*.js", "packages/ui", false},
		{"/packages/", ".", false},
	} {
		if got := covers(tc.pattern, tc.dir); got != tc.want {
			t.Errorf("covers(%q, %q) = %v, want %v", tc.pattern, tc.dir, got, tc.want)
		}
	}
	rules := []ownerRule{{pattern: "*", owned: true, line: 1}, {pattern: "/legacy/", owned: false, line: 2}}
	if _, ok := owner(rules, "legacy"); ok {
		t.Fatal("a later rule without owners must remove ownership")
	}
	if rule, ok := owner(rules, "api"); !ok || rule.line != 1 {
		t.Fatalf("owner(api) = %+v, %v", rule, ok)
	}
}

func TestMedian(t *testing.T) {
	changes := func(lines ...int) []history.Change {
		out := make([]history.Change, 0, len(lines))
		for _, n := range lines {
			out = append(out, history.Change{Lines: n})
		}
		return out
	}
	for _, tc := range []struct {
		lines []int
		want  float64
	}{{nil, 0}, {[]int{5}, 5}, {[]int{9, 1, 4}, 4}, {[]int{10, 1, 3, 20}, 6.5}} {
		if got := Median(changes(tc.lines...)); got != tc.want {
			t.Errorf("Median(%v) = %v, want %v", tc.lines, got, tc.want)
		}
	}
}

func TestEvidenceKeepsTheContractBounds(t *testing.T) {
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, strings.Repeat("x", 20)+"/file.go")
	}
	if command := fileList("git ls-files --", many); len(command) > maxCommandBytes {
		t.Fatalf("command is %d bytes", len(command))
	}
	got := evidence("git status", []string{"a b.go", "ok.go:3", "/abs.go", "a.go", "b.go", "c.go", "d.go", "e.go"})
	if len(got.Sample) != maxSample || got.Sample[0] != "ok.go:3" {
		t.Fatalf("sample = %v", got.Sample)
	}
	if long := signalCommand(strings.Repeat("d", 120)+"/", []string{strings.Repeat("f", 200) + "_test.go"}); len(long) > maxCommandBytes {
		t.Fatalf("signal command is %d bytes", len(long))
	}
}

func TestMeasureCountsCrossAreaChangesOutsideTheRoot(t *testing.T) {
	layout := areas.Detect([]string{"a/go.mod", "b/go.mod", "README.md"}, func([]string) map[string][]byte { return nil })
	repo, perArea := Measure(layout, []history.Commit{
		{Author: "x", Time: 3, Files: []history.FileChange{{Path: "a/x.go"}, {Path: "README.md"}}},
		{Author: "y", Time: 2, Files: []history.FileChange{{Path: "a/x.go"}, {Path: "b/y_test.go"}}},
		{Author: "y", Time: 1, Files: []history.FileChange{{Path: "README.md"}}},
	}, nil)
	if repo.Commits != 3 || repo.AreaCommits != 2 || repo.CrossArea != 1 || len(repo.Authors) != 2 || repo.LastCode != 3 {
		t.Fatalf("repository activity = %+v", repo)
	}
	a := perArea[layout.Assign("a/x.go")]
	if a.Commits != 2 || a.CrossArea != 1 || a.CodeCommits != 2 || a.CodeWithTest != 0 {
		t.Fatalf("area a = %+v", a)
	}
	if root := perArea[layout.Assign("README.md")]; root.CrossArea != 0 || root.Commits != 1 {
		t.Fatalf("root = %+v", root)
	}
}

// TestCrossAreaIsEnforcedOnlyWithImpactAwareCI: the treatment is CI that
// checks the dependents on pull requests, so it makes the marker enforced
// whatever the share (method 0.3); the scorer reads the share for L4.
func TestCrossAreaIsEnforcedOnlyWithImpactAwareCI(t *testing.T) {
	layout := areas.Detect([]string{"a/go.mod", "b/go.mod"}, func([]string) map[string][]byte { return nil })
	for _, tc := range []struct {
		name, ci string
		cross    int
		want     contract.MarkerState
	}{
		{"low share, full CI", "on: pull_request\nrun: go test ./...\n", 1, contract.MarkerExists},
		{"low share, Turborepo filter", "on: pull_request\nrun: turbo run test --filter=...[origin/main]\n", 1, contract.MarkerEnforced},
		{"low share, Nx affected", "on: pull_request\nrun: npx nx affected -t test\n", 1, contract.MarkerEnforced},
		{"high share, impact-aware CI", "on: pull_request\nrun: putnami test --impacted\n", 2, contract.MarkerEnforced},
		{"high share, full CI", "on: pull_request\nrun: go test ./...\n", 2, contract.MarkerExists},
		{"impact-aware CI off pull requests", "on: push\nrun: putnami test --impacted\n", 1, contract.MarkerExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newComputation(Input{
				Files:    []gitrepo.File{{Path: ".github/workflows/ci.yml"}, {Path: "a/go.mod"}, {Path: "b/go.mod"}},
				Layout:   layout,
				Repo:     Activity{Commits: 4, AreaCommits: 4, CrossArea: tc.cross},
				Contents: map[string][]byte{".github/workflows/ci.yml": []byte(tc.ci)},
			})
			m := c.crossArea(-1)
			if m.State != tc.want {
				t.Fatalf("state = %s, want %s", m.State, tc.want)
			}
			if tc.want == contract.MarkerEnforced && (len(m.Evidence.Sample) != 1 || m.Evidence.Sample[0] != ".github/workflows/ci.yml") {
				t.Fatalf("sample = %v, want the CI file", m.Evidence.Sample)
			}
		})
	}
}

// TestMonorepoConfigurationBelowTheRootCounts reads a repository shaped like
// a GitLab monorepo: linters, lockfiles and pins live in the top-level
// workspaces, and the jobs live in files .gitlab-ci.yml includes.
func TestMonorepoConfigurationBelowTheRootCounts(t *testing.T) {
	paths := []string{
		".gitlab-ci.yml", ".gitlab/test.yml", "frontend/yarn.lock", "frontend/.tool-versions", "frontend/web/eslint.config.mjs",
		"backend/pnpm-lock.yaml", "backend/biome.json", "ai/pyproject.toml", "vendor/x/.golangci.yml", "a/testdata/ws/biome.json",
	}
	sort.Strings(paths)
	files := make([]gitrepo.File, 0, len(paths))
	for _, file := range paths {
		files = append(files, gitrepo.File{Path: file})
	}
	c := newComputation(Input{
		Files:  files,
		Layout: areas.Detect(paths, func([]string) map[string][]byte { return nil }),
		Contents: map[string][]byte{
			".gitlab-ci.yml":    []byte("include:\n  - local: .gitlab/test.yml\n"),
			".gitlab/test.yml":  []byte("lint:\n  script: yarn install --frozen-lockfile && yarn lint\ntest:\n  script: bazel test //...\n"),
			"ai/pyproject.toml": []byte("[project]\nname = \"ai\"\n[tool.ruff]\nline-length = 100\n"),
		},
	})
	static := c.staticChecks()
	if static.State != contract.MarkerEnforced || strings.Join(static.Evidence.Sample, ",") != "ai/pyproject.toml,backend/biome.json,frontend/web/eslint.config.mjs,.gitlab/test.yml" {
		t.Fatalf("static checks = %+v", static)
	}
	pinned := c.pinnedToolchain()
	if pinned.State != contract.MarkerEnforced || strings.Join(pinned.Evidence.Sample, ",") != "backend/pnpm-lock.yaml,frontend/yarn.lock,frontend/.tool-versions" {
		t.Fatalf("pinned toolchain = %+v", pinned)
	}
	if jobs := c.testJobs(); len(jobs) != 1 || jobs[0].Path != ".gitlab/test.yml" {
		t.Fatalf("test jobs = %+v", jobs)
	}
}
