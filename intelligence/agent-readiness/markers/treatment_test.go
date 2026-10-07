package markers

import (
	"strings"
	"testing"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/history"
	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
)

// now is the collection time of these fixtures, in Unix seconds.
const now = 1_000 * secondsPerDay

// ago returns the Unix time the given number of days before now.
func ago(days int64) int64 { return now - days*secondsPerDay }

// enforcedDays reads a marker's EnforcedDays, -1 when it is not set.
func enforcedDays(m contract.Marker) float64 {
	if m.EnforcedDays == nil {
		return -1
	}
	return *m.EnforcedDays
}

// TestContributorsCountPeopleAndAgents pins recover.ownership's count: each
// person, as a non-agent author or a Co-Authored-By trailer, once, and each
// agent a commit credits. A person who runs an agent under their own name,
// without a trailer, counts once.
func TestContributorsCountPeopleAndAgents(t *testing.T) {
	people := func(emails ...string) map[string]bool {
		out := map[string]bool{}
		for _, email := range emails {
			out[email] = true
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		activity Activity
		want     int
	}{
		{"person and agent trailer", Activity{Authors: people("alice@acme.example"), Agents: people("claude")}, 2},
		{"agent author only", Activity{Authors: people("noreply@anthropic.com"), Agents: people("claude")}, 1},
		{"person running an agent without a trailer", Activity{Authors: people("alice@acme.example")}, 1},
		{"author who is also a co-author", Activity{Authors: people("alice@acme.example"), CoAuthors: people("alice@acme.example")}, 1},
		{"author, co-author and agent", Activity{Authors: people("alice@acme.example"), CoAuthors: people("bob@acme.example"), Agents: people("claude")}, 3},
		{"nobody", Activity{}, 0},
	} {
		if got := tc.activity.Contributors(); got != tc.want {
			t.Errorf("%s: Contributors = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestMeasureCreditsAgentCommits pins that Measure counts the commits that
// credit an agent, and those a later commit reverted, for the repository
// and for each area they changed, with the people their trailers credit.
func TestMeasureCreditsAgentCommits(t *testing.T) {
	layout := areas.Detect([]string{"a/go.mod", "b/go.mod", "c/go.mod"}, func([]string) map[string][]byte { return nil })
	repo, perArea := Measure(layout, []history.Commit{
		{Author: "alice@acme.example", Time: 4, Agents: []string{"claude"}, Reverted: true, Files: []history.FileChange{{Path: "a/x.go"}}},
		{Author: "noreply@anthropic.com", Time: 3, Agents: []string{"claude"}, Files: []history.FileChange{{Path: "b/y.go"}}},
		{Author: "198982749+copilot@users.noreply.github.com", Time: 2, Agents: []string{"copilot"}, CoAuthors: []string{"bob@acme.example"}, Files: []history.FileChange{{Path: "c/z.go"}}},
		{Author: "alice@acme.example", Time: 1, Files: []history.FileChange{{Path: "a/x.go"}}},
	}, nil)
	if repo.AgentCommits != 3 || repo.AgentReverted != 1 || repo.Contributors() != 4 {
		t.Fatalf("repository = %+v, want 3 agent commits, 1 reverted, 4 contributors", repo)
	}
	a, b, c := perArea[layout.Assign("a/x.go")], perArea[layout.Assign("b/y.go")], perArea[layout.Assign("c/z.go")]
	if a.AgentCommits != 1 || a.AgentReverted != 1 || a.Contributors() != 2 {
		t.Fatalf("area a = %+v, want 1 agent commit, reverted, and 2 contributors", a)
	}
	if b.AgentCommits != 1 || b.AgentReverted != 0 || b.Contributors() != 1 {
		t.Fatalf("area b = %+v, want 1 agent commit and the agent alone", b)
	}
	// A Copilot commit with a human Co-Authored-By trailer has two
	// contributors: the agent and the person.
	if c.AgentCommits != 1 || !c.CoAuthors["bob@acme.example"] || c.Contributors() != 2 {
		t.Fatalf("area c = %+v, want copilot and bob", c)
	}

	// An Activity built by hand has no map yet: credit makes one.
	var bare Activity
	bare.credit(history.Commit{Agents: []string{"cursor"}, CoAuthors: []string{"bob@acme.example"}})
	if bare.AgentCommits != 1 || !bare.Agents["cursor"] || !bare.CoAuthors["bob@acme.example"] {
		t.Fatalf("bare activity = %+v, want cursor and bob credited", bare)
	}
}

// TestInstructionsHeldSinceTheOldestFileNamingTheCommands pins
// understand.instructions' EnforcedDays: the age of the oldest instruction
// file that names both the build and the test commands. An older file that
// names neither does not count.
func TestInstructionsHeldSinceTheOldestFileNamingTheCommands(t *testing.T) {
	naming := "Run `putnami lint,test,build --impacted` before you push.\n"
	paths := []string{"AGENTS.md", "CLAUDE.md", ".github/copilot-instructions.md", "putnami.workspace.json", "putnami.ci.json", "tooling/cli/putnami.json", "tooling/cli/main.go"}
	contents := map[string]string{
		"AGENTS.md":                       naming,
		".github/copilot-instructions.md": naming,
		"CLAUDE.md":                       "Be concise.\n",
		"putnami.workspace.json":          "{}",
		"putnami.ci.json":                 `{"commands":["lint","test","build"]}`,
		"tooling/cli/putnami.json":        `{"name":"tooling/cli"}`,
	}
	c := computationOf(paths, contents)
	c.Now = now
	c.Dates = gitrepo.FileDates{Added: map[string]int64{"AGENTS.md": ago(120), ".github/copilot-instructions.md": ago(50), "CLAUDE.md": ago(300)}}
	if m := c.instructions(-1); m.State != contract.MarkerEnforced || enforcedDays(m) != 120 {
		t.Fatalf("instructions = %+v, want enforced for 120 days", m)
	}
	contents["AGENTS.md"] = "Be concise.\n"
	contents[".github/copilot-instructions.md"] = "Be kind.\n"
	c = computationOf(paths, contents)
	c.Now = now
	if m := c.instructions(-1); m.State != contract.MarkerExists || m.EnforcedDays != nil {
		t.Fatalf("instructions without commands = %+v, want exists without enforcedDays", m)
	}
}

// TestAreaDocsHeldSinceBothTheReadmeAndTheCheck pins understand.area-docs'
// EnforcedDays: the later of the README's addition and the docs CI job's.
func TestAreaDocsHeldSinceBothTheReadmeAndTheCheck(t *testing.T) {
	paths := []string{"README.md", ".github/workflows/docs.yml"}
	contents := map[string]string{".github/workflows/docs.yml": "on: pull_request\njobs: {d: {steps: [{run: lychee --offline .}]}}"}
	for _, tc := range []struct {
		name        string
		readme, job int64
		want        float64
	}{
		{"CI added after the README", 200, 40, 40},
		{"README added after the CI", 30, 200, 30},
	} {
		c := computationOf(paths, contents)
		c.Now = now
		c.Dates = gitrepo.FileDates{Added: map[string]int64{"README.md": ago(tc.readme), ".github/workflows/docs.yml": ago(tc.job)}}
		if m := c.areaDocs(-1); m.State != contract.MarkerEnforced || enforcedDays(m) != tc.want {
			t.Errorf("%s: area docs = %+v, want enforced for %v days", tc.name, m, tc.want)
		}
	}
	c := computationOf([]string{"README.md"}, nil)
	if m := c.areaDocs(-1); m.State != contract.MarkerExists || m.EnforcedDays != nil {
		t.Fatalf("area docs without a check = %+v, want exists without enforcedDays", m)
	}
}

// TestDeclaredAreasHeldSinceTheOldestBuildGraph pins bound.declared-areas'
// EnforcedDays: the age of the oldest manifest or file a build-graph tool
// reads. A workspace manifest no graph tool reads does not count.
func TestDeclaredAreasHeldSinceTheOldestBuildGraph(t *testing.T) {
	dates := gitrepo.FileDates{Added: map[string]int64{"go.work": ago(150), "turbo.json": ago(60), "pnpm-workspace.yaml": ago(400)}}
	c := newComputation(Input{
		Now:   now,
		Files: []gitrepo.File{{Path: "go.work"}, {Path: "pnpm-workspace.yaml"}, {Path: "turbo.json"}},
		Layout: areas.Layout{Manifests: []areas.Manifest{
			{Kind: "go.work", Path: "go.work"}, {Kind: "pnpm", Path: "pnpm-workspace.yaml"},
		}},
		Dates: dates,
	})
	m := c.declaredAreas()
	if m.State != contract.MarkerEnforced || enforcedDays(m) != 150 {
		t.Fatalf("declared areas = %+v, want enforced for 150 days", m)
	}
	if !strings.Contains(m.Evidence.Command, "go.work") || !strings.Contains(m.Evidence.Command, "turbo.json") || strings.Contains(m.Evidence.Command, "pnpm") {
		t.Fatalf("command %q must date the build graph only", m.Evidence.Command)
	}
	c = newComputation(Input{
		Now:    now,
		Files:  []gitrepo.File{{Path: "pnpm-workspace.yaml"}},
		Layout: areas.Layout{Manifests: []areas.Manifest{{Kind: "pnpm", Path: "pnpm-workspace.yaml"}}},
		Dates:  dates,
	})
	if m := c.declaredAreas(); m.State != contract.MarkerExists || m.EnforcedDays != nil {
		t.Fatalf("declared areas without a graph tool = %+v, want exists without enforcedDays", m)
	}
	if dated := Dated([]gitrepo.File{{Path: "go.work"}, {Path: "turbo.json"}}, areas.Layout{Manifests: []areas.Manifest{{Kind: "go.work", Path: "go.work"}}}); len(dated) != 2 {
		t.Fatalf("Dated = %v, want the build graph dated", dated)
	}
}

// TestOwnershipHeldSinceTheCodeownersFile pins recover.ownership's
// EnforcedDays, the age of the CODEOWNERS file, and its value, the
// contributors people and agents together.
func TestOwnershipHeldSinceTheCodeownersFile(t *testing.T) {
	paths := []string{".github/CODEOWNERS", "api/go.mod", "api/main.go"}
	c := computationOf(paths, map[string]string{".github/CODEOWNERS": "/api/ @acme/api\n"})
	c.Now = now
	c.Dates = gitrepo.FileDates{Added: map[string]int64{".github/CODEOWNERS": ago(120)}}
	c.Repo = Activity{Authors: map[string]bool{"alice@acme.example": true}, Agents: map[string]bool{"claude": true}}
	api := c.Layout.Assign("api/main.go")
	c.Areas = make([]Activity, len(c.Layout.Areas))
	c.Areas[api] = Activity{Commits: 1, Authors: map[string]bool{"alice@acme.example": true}}
	if m := c.ownership(-1); m.State != contract.MarkerEnforced || enforcedDays(m) != 120 || *m.Value != 2 {
		t.Fatalf("repository ownership = %+v, want enforced for 120 days with 2 contributors", m)
	}
	if m := c.ownership(api); m.State != contract.MarkerEnforced || enforcedDays(m) != 120 || *m.Value != 1 {
		t.Fatalf("api ownership = %+v, want enforced for 120 days with 1 contributor", m)
	}
}

// changes builds first-parent changes, newest first, one every two days:
// true lands through a pull request, false is a direct push.
func changes(viaPR ...bool) []history.Change {
	out := make([]history.Change, 0, len(viaPR))
	for i, pr := range viaPR {
		out = append(out, history.Change{Time: ago(int64(2 * (i + 1))), Lines: 10, ViaPullRequest: pr})
	}
	return out
}

// repeat returns n copies of v.
func repeat(v bool, n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// TestSmallChangesReadTheNewestChanges pins recover.small-changes' state and
// EnforcedDays: the state is enforced when 90% of the newest ten changes
// landed through pull requests, and EnforcedDays reaches back to the oldest
// change before the newest-first share first falls below 90%, or to the
// window's start or the repository's birth when it never does.
func TestSmallChangesReadTheNewestChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		viaPR []bool
		born  int64
		state contract.MarkerState
		days  float64
	}{
		// Ten pull requests, then three pushes: the run keeps 10 of 11 and
		// stops at the twelfth, so it reaches the 11th change, 22 days old.
		{"newest ten through pull requests", append(repeat(true, 10), repeat(false, 3)...), 0, contract.MarkerEnforced, 22},
		// Older pull requests bring the share back to 50 of 53, but the run
		// ended at the three pushes.
		{"pushes between pull requests", append(append(repeat(true, 10), repeat(false, 3)...), repeat(true, 40)...), 0, contract.MarkerEnforced, 22},
		{"newest change pushed, nine of ten", append([]bool{false}, repeat(true, 9)...), 0, contract.MarkerEnforced, 90},
		{"two pushes among the newest ten", append(append([]bool{false, true, false}, repeat(true, 7)...), repeat(true, 10)...), 0, contract.MarkerExists, -1},
		{"whole window, repository born inside it", repeat(true, 5), ago(30), contract.MarkerEnforced, 30},
		{"whole window, repository older than it", repeat(true, 5), ago(400), contract.MarkerEnforced, 90},
		{"no pull request", repeat(false, 3), 0, contract.MarkerAbsent, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newComputation(Input{Now: now, Born: tc.born, Changes: changes(tc.viaPR...)})
			m := c.smallChanges()
			if m.State != tc.state || enforcedDays(m) != tc.days {
				t.Fatalf("small changes = %+v (enforcedDays %v), want %s for %v days", m, enforcedDays(m), tc.state, tc.days)
			}
		})
	}
}

// TestReliableSignalStateReadsHead pins that verify.reliable-signal's state
// reads HEAD while its value counts the window: an unguarded match still at
// HEAD keeps it at exists though the window added none, and guarded matches
// alone make it enforced though the window added one.
func TestReliableSignalStateReadsHead(t *testing.T) {
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

	c.Signals = []gitrepo.Match{{Path: "store/store_test.go", Line: 6}}
	c.SignalsAdded = nil
	if m := c.reliableSignal(-1); m.State != contract.MarkerExists || *m.Value != 0 || strings.Join(m.Evidence.Sample, ",") != "store/store_test.go:6" {
		t.Fatalf("old unguarded skip = %+v, want exists with 0 added lines", m)
	}

	c.Signals = []gitrepo.Match{{Path: "store/store_test.go", Line: 4}}
	c.SignalsAdded = map[string]int{"store/store_test.go": 1}
	if m := c.reliableSignal(-1); m.State != contract.MarkerEnforced || *m.Value != 1 {
		t.Fatalf("guarded skip alone = %+v, want enforced with 1 added line", m)
	}
}
