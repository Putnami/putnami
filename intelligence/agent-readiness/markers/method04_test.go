package markers

import (
	"slices"
	"strconv"
	"testing"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/history"
	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
)

// landed builds a pull request change landed the given days ago.
func landed(number, days int64, title string, files ...string) history.Change {
	subject := title + " (#" + strconv.FormatInt(number, 10) + ")"
	return history.Change{
		Hash: "c0ffee" + strconv.FormatInt(number, 10), Time: ago(days), Subject: subject, Title: subject,
		Files: files, ViaPullRequest: true, Lines: 10,
	}
}

// TestContainedChangesCountReverts pins recover.contained-changes' value:
// the share of the changes landed a week or more ago that a revert undid
// within a week. A fix that does not revert does not count.
func TestContainedChangesCountReverts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []history.Change
		want    float64
	}{
		{"no revert", []history.Change{
			landed(3, 10, "feat: c", "c.go"),
			landed(2, 20, "feat: b", "b.go"),
			landed(1, 30, "feat: a", "a.go"),
		}, 0},
		{"reverted within a week", func() []history.Change {
			changes := []history.Change{landed(2, 20, "feat: b", "b.go"), landed(1, 30, "feat: a", "a.go")}
			changes[1].RevertedAt = ago(28)
			return changes
		}(), 0.5},
		{"reverted after a week", func() []history.Change {
			changes := []history.Change{landed(2, 20, "feat: b", "b.go"), landed(1, 30, "feat: a", "a.go")}
			changes[1].RevertedAt = ago(20)
			return changes
		}(), 0},
		{"fix touching its code within a week", []history.Change{
			landed(2, 27, "fix: guard a nil map", "a.go"),
			landed(1, 30, "feat: a", "a.go"),
		}, 0},
		{"change younger than a week does not count yet", func() []history.Change {
			changes := []history.Change{landed(2, 3, "feat: b", "b.go"), landed(1, 30, "feat: a", "a.go")}
			changes[0].RevertedAt = ago(2)
			return changes
		}(), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newComputation(Input{Now: now, Changes: tc.changes})
			m := c.containedChanges()
			if m.Value == nil || *m.Value != tc.want {
				t.Fatalf("contained changes = %+v, want value %v", m, tc.want)
			}
			if m.State != contract.MarkerEnforced || m.Evidence.Command != containedCommand {
				t.Fatalf("contained changes = %+v, want enforced with the evidence command", m)
			}
		})
	}
}

// TestContainedChangesShareThePullRequestState pins that the state and the
// days are the pull request practice's, as recover.small-changes reads it.
func TestContainedChangesShareThePullRequestState(t *testing.T) {
	for _, viaPR := range [][]bool{repeat(true, 12), repeat(false, 3), append([]bool{false, true, false}, repeat(true, 7)...)} {
		c := newComputation(Input{Now: now, Changes: changes(viaPR...)})
		small, contained := c.smallChanges(), c.containedChanges()
		if small.State != contained.State || enforcedDays(small) != enforcedDays(contained) {
			t.Fatalf("contained %+v and small %+v disagree on the practice", contained, small)
		}
	}
}

func TestPlatformAndShortModeGuardSkips(t *testing.T) {
	short := []string{"\tif testing.Short() {", "\t\tt.Skip(\"skipping in short mode\")"}
	flakyShort := []string{"\tif testing.Short() {", "\t\tt.Skip(\"flaky in short mode\")"}
	plain := []string{"\tt.Skip(\"needs a daemon\")"}
	flaky := []string{"\tt.Skip(\"flaky\")"}
	constrained := []byte("//go:build linux && amd64\n\npackage store\n")
	tagged := []byte("//go:build integration\n\npackage store\n")
	for _, tc := range []struct {
		name  string
		file  string
		ci    string
		body  map[string][]byte
		lines []string
		want  bool
	}{
		{"short mode, CI runs the full suite", "store/a_test.go", "go test ./...", nil, short, false},
		{"short mode, CI passes -short", "store/a_test.go", "go test -short ./...", nil, short, true},
		{"short mode naming a flaky test", "store/a_test.go", "go test ./...", nil, flakyShort, true},
		{"negated short mode fires on the full run", "store/a_test.go", "go test ./...", nil, []string{"\tif !testing.Short() {", "\t\tt.Skip(\"slow path\")"}, true},
		{"short mode, GOFLAGS passes -short", "store/a_test.go", "GOFLAGS=-short go test ./...", nil, short, true},
		{"short mode, CI passes -short=false", "store/a_test.go", "go test -short=false ./...", nil, short, false},
		{"short mode, CI runs no Go test", "store/a_test.go", "npm test", nil, short, true},
		{"platform file name", "store/a_windows_test.go", "go test ./...", nil, plain, false},
		{"platform architecture file name", "store/a_linux_arm64_test.go", "go test ./...", nil, plain, false},
		{"platform file naming a flaky test", "store/a_windows_test.go", "go test ./...", nil, flaky, true},
		{"platform build line", "store/a_test.go", "go test ./...", map[string][]byte{"store/a_test.go": constrained}, plain, false},
		{"build tag naming no platform", "store/a_test.go", "go test ./...", map[string][]byte{"store/a_test.go": tagged}, plain, true},
		{"file name that only looks like a platform", "store/windowsx_test.go", "go test ./...", nil, plain, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []string{".github/workflows/ci.yml", tc.file}
			contents := map[string][]byte{".github/workflows/ci.yml": []byte("on: pull_request\njobs: {t: {steps: [{run: " + tc.ci + "}]}}")}
			signal := NewSignalContext(files, contents, tc.body)
			if got := signal.Counts(tc.file, tc.lines, len(tc.lines)-1); got != tc.want {
				t.Fatalf("Counts(%s, %q) = %v, want %v", tc.file, tc.lines, got, tc.want)
			}
		})
	}
	if !CountsSignal("store/a_test.go", short, 1) {
		t.Fatal("with no repository fact, a short-mode skip still counts")
	}
}

func TestAreaDocsFollowsTasks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    map[string]string
		run      string
		want     contract.MarkerState
		declared string
	}{
		{"npm script run by pnpm", map[string]string{
			"package.json": `{"scripts":{"lint:links":"lychee --offline docs"}}`,
		}, "pnpm run lint:links", contract.MarkerEnforced, "package.json"},
		{"chained scripts", map[string]string{
			"package.json": `{"scripts":{"check":"pnpm run links && pnpm test","links":"markdown-link-check README.md"}}`,
		}, "pnpm check", contract.MarkerEnforced, "package.json"},
		{"nested package script through turbo", map[string]string{
			"package.json":     `{"scripts":{}}`,
			"www/package.json": `{"scripts":{"linkcheck":"lychee ."}}`,
			"turbo.json":       `{}`,
		}, "pnpm turbo run build,linkcheck", contract.MarkerEnforced, "www/package.json"},
		{"nx target", map[string]string{
			"package.json":      `{}`,
			"docs/project.json": `{"targets":{"links":{"command":"lychee docs"}}}`,
		}, "npx nx affected -t links", contract.MarkerEnforced, "docs/project.json"},
		{"makefile target", map[string]string{
			"Makefile": "docs-check:\n\tvale docs/\n\ntest:\n\tgo test ./...\n",
		}, "make docs-check", contract.MarkerEnforced, "Makefile"},
		{"npm test shortcut", map[string]string{
			"package.json": `{"scripts":{"test":"vitest && lychee docs"}}`,
		}, "npm test", contract.MarkerEnforced, "package.json"},
		{"pnpm filter", map[string]string{
			"package.json":     `{}`,
			"www/package.json": `{"scripts":{"docs":"docusaurus build"}}`,
		}, "pnpm --filter www docs", contract.MarkerEnforced, "www/package.json"},
		{"yarn workspace", map[string]string{
			"package.json":      `{}`,
			"site/package.json": `{"scripts":{"check":"markdown-link-check README.md"}}`,
		}, "yarn workspace site check", contract.MarkerEnforced, "site/package.json"},
		{"step title that names a docs task", map[string]string{
			"package.json": `{"scripts":{"lint":"markdownlint docs"}}`,
		}, "go vet ./...\n  name: Run lint", contract.MarkerExists, ""},
		{"another tool's -t flag", map[string]string{
			"package.json": `{"scripts":{"site":"mkdocs build"}}`,
		}, "docker build -t site .", contract.MarkerExists, ""},
		{"task that runs no docs check", map[string]string{
			"package.json": `{"scripts":{"lint":"eslint ."}}`,
		}, "pnpm lint", contract.MarkerExists, ""},
		{"docs task CI never runs", map[string]string{
			"package.json": `{"scripts":{"links":"lychee .","test":"vitest"}}`,
		}, "pnpm test", contract.MarkerExists, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := []string{"README.md", ".github/workflows/ci.yml"}
			contents := map[string]string{".github/workflows/ci.yml": "on: pull_request\njobs: {c: {steps: [{run: " + tc.run + "}]}}"}
			for file, content := range tc.files {
				paths = append(paths, file)
				contents[file] = content
			}
			c := computationOf(paths, contents)
			m := c.areaDocs(-1)
			if m.State != tc.want {
				t.Fatalf("area docs = %+v, want %s", m, tc.want)
			}
			if tc.declared != "" && !slices.Contains(m.Evidence.Sample, tc.declared) {
				t.Fatalf("area docs sample = %v, want the task file %s", m.Evidence.Sample, tc.declared)
			}
		})
	}
	if !slices.Contains(Wanted([]gitrepo.File{{Path: "docs/project.json"}}), "docs/project.json") {
		t.Fatal("the collector does not read Nx project.json files")
	}
}

func TestPutnamiWorkspacePinsItsToolchain(t *testing.T) {
	c := computationOf([]string{"putnami.workspace.json", "putnami.lock.json", "putnami.ci.json"}, map[string]string{
		"putnami.ci.json": `{"commands":["lint","test","build"],"flags":["--impacted"]}`,
	})
	m := c.pinnedToolchain()
	if m.State != contract.MarkerEnforced || !slices.Contains(m.Evidence.Sample, "putnami.lock.json") {
		t.Fatalf("pinned toolchain = %+v, want enforced on putnami.lock.json", m)
	}
}

func TestDeclaredAreasWeighCodeThatExists(t *testing.T) {
	paths := []string{"go.work", "a/go.mod", "b/go.mod"}
	layout := areas.Detect(paths, func(wanted []string) map[string][]byte {
		return map[string][]byte{"go.work": []byte("go 1.24\n\nuse (\n\t./a\n\t./b\n)\n")}
	})
	head := map[string]bool{"a/x.go": true, "tools/gen.go": true, ".claude/skills/fix/finalize.sh": true}
	repo, _ := Measure(layout, []history.Commit{
		{Author: "x", Time: 3, Files: []history.FileChange{{Path: "a/x.go"}, {Path: "README.md"}, {Path: ".github/workflows/ci.yml"}, {Path: ".claude/skills/fix/finalize.sh"}}},
		{Author: "x", Time: 2, Files: []history.FileChange{{Path: "tools/gen.go"}, {Path: "docs/guide.md"}}},
		{Author: "x", Time: 1, Files: []history.FileChange{{Path: "old/gone.go"}}},
	}, func(file string) bool { return head[file] })
	if repo.FileChanges != 2 || repo.DeclaredChanges != 1 {
		t.Fatalf("file changes = %d, declared = %d, want 2 and 1", repo.FileChanges, repo.DeclaredChanges)
	}
}
