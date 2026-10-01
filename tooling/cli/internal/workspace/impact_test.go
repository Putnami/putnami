package workspace

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The evidence renders as one line that names the paths and says why they
// selected nothing, and degrades to a count rather than a wall of text when a
// root-wide edit lands. Nothing unattributed renders as nothing at all: a
// caller must be able to print the result unconditionally.
func TestImpactedSelection_UnownedRootFilesExplanation(t *testing.T) {
	got := ImpactedSelection{UnownedRootFiles: []string{"bun.lock", "go.work.sum"}}.UnownedRootFilesExplanation()
	if !strings.Contains(got, "bun.lock, go.work.sum") {
		t.Errorf("explanation %q does not name the changed root paths", got)
	}
	if !strings.Contains(got, "belong to no project") {
		t.Errorf("explanation %q does not state why they selected nothing", got)
	}

	capped := ImpactedSelection{
		UnownedRootFiles: []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7"},
	}.UnownedRootFilesExplanation()
	if !strings.Contains(capped, "(+2 more)") {
		t.Errorf("explanation %q does not cap the list at %d with a count", capped, unownedRootFilesShown)
	}
	if strings.Contains(capped, "r7") {
		t.Errorf("explanation %q names a path past the cap", capped)
	}

	if empty := (ImpactedSelection{}).UnownedRootFilesExplanation(); empty != "" {
		t.Errorf("explanation with nothing unattributed = %q, want empty", empty)
	}
}

// traceFixtureSelection is a hand-built selection whose trace has every seed
// and edge kind once, so the rendering can be checked line for line: three
// changed files of which one reached nobody, two seeded projects, and three
// propagated ones. Projects are in workspace order, which is the order every
// list below follows.
func traceFixtureSelection() ImpactedSelection {
	ids := []string{"/tooling/extension-sdk", "/tooling/cli", "/go/extension", "/typescript/extension", "/apps/web"}
	projects := make([]*Project, 0, len(ids))
	for _, id := range ids {
		projects = append(projects, &Project{ID: id})
	}
	return ImpactedSelection{
		Projects:     projects,
		ChangedFiles: []string{"tooling/extension-sdk/sdk.go", ".github/CODEOWNERS", ".agents/skills/fix/SKILL.md"},
		Trace: ImpactTrace{
			Seeds: map[string][]ImpactSeed{
				"/tooling/cli":           {{File: ".agents/skills/fix/SKILL.md", Kind: ImpactSeedDeclaredInput, Via: "../../.agents/skills/** !**/*.snap"}},
				"/tooling/extension-sdk": {{File: "tooling/extension-sdk/sdk.go", Kind: ImpactSeedPathOwner, Via: "tooling/extension-sdk"}},
			},
			Edges: map[string]ImpactEdge{
				"/go/extension":         {From: "/tooling/extension-sdk", Kind: ImpactEdgeDependency},
				"/typescript/extension": {From: "/tooling/extension-sdk", Kind: ImpactEdgeDependency},
				"/apps/web":             {From: "/go/extension", Kind: ImpactEdgeExtensionConsumer},
			},
			Scopes: map[string][]string{"/apps/web": {"/go/extension", "/typescript/extension"}},
		},
	}
}

// The block explains a selection that is too LARGE the way the unowned-root
// line explains one that is too small: a header with the counts, then the
// evidence — which file claimed which project and how, then which edge pulled
// each of the others in.
func TestImpactedSelection_TraceExplanation_RendersHeaderSeedsThenEdges(t *testing.T) {
	got := traceFixtureSelection().TraceExplanation()
	want := []string{
		"  --impacted: 3 changed file(s) reached 2 project(s) directly; propagation added 3 (dependency 2, extension-consumer 1, 1 task-scoped)",
		"    tooling/extension-sdk/sdk.go → /tooling/extension-sdk  [path-owner tooling/extension-sdk]",
		"    .agents/skills/fix/SKILL.md → /tooling/cli  [declared-input ../../.agents/skills/** !**/*.snap]",
		"    /go/extension ← /tooling/extension-sdk  [dependency]",
		"    /typescript/extension ← /tooling/extension-sdk  [dependency]",
		"    /apps/web ← /go/extension  [extension-consumer; tasks /go/extension, /typescript/extension]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("explanation:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// When propagation added nothing the header ends after "directly", and a
	// kind at zero is not listed.
	seedsOnly := traceFixtureSelection()
	seedsOnly.Trace.Edges, seedsOnly.Trace.Scopes = nil, nil
	if got := seedsOnly.TraceExplanation(); got[0] != "  --impacted: 3 changed file(s) reached 2 project(s) directly" {
		t.Errorf("header without propagation = %q", got[0])
	}
	if len(seedsOnly.TraceExplanation()) != 3 {
		t.Errorf("seeds-only explanation = %q, want the header and two seed lines", seedsOnly.TraceExplanation())
	}
}

func TestImpactedSelection_TraceExplanation_CapsSeedsAndEdges(t *testing.T) {
	s := ImpactedSelection{Trace: ImpactTrace{Seeds: map[string][]ImpactSeed{}, Edges: map[string]ImpactEdge{}}}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("/seed%d", i)
		s.Projects = append(s.Projects, &Project{ID: id})
		s.ChangedFiles = append(s.ChangedFiles, fmt.Sprintf("seed%d/file.go", i))
		s.Trace.Seeds[id] = []ImpactSeed{{File: fmt.Sprintf("seed%d/file.go", i), Kind: ImpactSeedPathOwner, Via: fmt.Sprintf("seed%d", i)}}
	}
	for i := 0; i < 13; i++ {
		id := fmt.Sprintf("/edge%d", i)
		s.Projects = append(s.Projects, &Project{ID: id})
		s.Trace.Edges[id] = ImpactEdge{From: "/seed0", Kind: ImpactEdgeDependency}
	}

	got := s.TraceExplanation()

	if want := "  --impacted: 7 changed file(s) reached 7 project(s) directly; propagation added 13 (dependency 13)"; got[0] != want {
		t.Errorf("header = %q, want %q", got[0], want)
	}
	// header + 5 seeds + overflow + 10 edges + overflow
	if len(got) != 1+impactTraceSeedsShown+1+impactTraceEdgesShown+1 {
		t.Fatalf("explanation has %d lines: %q", len(got), got)
	}
	if got[1+impactTraceSeedsShown] != "    … +2 more seed(s)" {
		t.Errorf("seed overflow line = %q", got[1+impactTraceSeedsShown])
	}
	if last := got[len(got)-1]; last != "    … +3 more edge(s); the `impacted` MCP tool returns every reason." {
		t.Errorf("edge overflow line = %q", last)
	}
	for _, line := range got {
		if strings.Contains(line, "seed6/") || strings.Contains(line, "/edge12 ") {
			t.Errorf("line %q names an entry past the cap", line)
		}
	}
}

// The record is the block with nothing capped: every seed in the block's own
// order, every edge in selection order, the whole pattern set — the lines a
// terminal elides are exactly the ones an operator comparing two runs needs.
func TestImpactedSelection_TraceRecord_KeepsEverySeedAndEdgeInTheBlockOrder(t *testing.T) {
	s := ImpactedSelection{Trace: ImpactTrace{Seeds: map[string][]ImpactSeed{}, Edges: map[string]ImpactEdge{}}}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("/seed%d", i)
		s.Projects = append(s.Projects, &Project{ID: id})
		s.ChangedFiles = append(s.ChangedFiles, fmt.Sprintf("seed%d/file.go", i))
		s.Trace.Seeds[id] = []ImpactSeed{{File: fmt.Sprintf("seed%d/file.go", i), Kind: ImpactSeedPathOwner, Via: fmt.Sprintf("seed%d", i)}}
	}
	for i := 0; i < 13; i++ {
		id := fmt.Sprintf("/edge%d", i)
		s.Projects = append(s.Projects, &Project{ID: id})
		s.Trace.Edges[id] = ImpactEdge{From: "/seed0", Kind: ImpactEdgeDependency}
	}

	record := s.TraceRecord()
	if len(record.Seeds) != 7 || len(record.Edges) != 13 {
		t.Fatalf("record keeps %d seeds and %d edges, want all 7 and 13", len(record.Seeds), len(record.Edges))
	}
	for i, seed := range s.orderedSeeds() {
		if record.Seeds[i].Project != seed.project || record.Seeds[i].File != seed.File {
			t.Errorf("seeds[%d] = %+v, want the block's %d-th seed %s → %s", i, record.Seeds[i], i, seed.File, seed.project)
		}
	}
	if record.Edges[12] != (ImpactTraceEdge{Project: "/edge12", From: "/seed0", Kind: "dependency"}) {
		t.Errorf("last edge = %+v, want /edge12 ← /seed0", record.Edges[12])
	}

	// changedFiles, seeds and edges are present even when empty, so "none" is
	// never confused with "not recorded"; the other members are omitted.
	empty := ImpactedSelection{Baseline: "origin/main"}.TraceRecord()
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"baseline":"origin/main","changedFiles":[],"seeds":[],"edges":[],"scopes":[]}`; string(encoded) != want {
		t.Errorf("empty record = %s, want %s", encoded, want)
	}
	// The session event carries exactly those members.
	data := empty.EventData()
	if len(data) != 5 || data["baseline"] != "origin/main" {
		t.Errorf("event data = %v, want the five members of the empty record", data)
	}
}

// A declaring project can name twenty patterns. The line stays a notice: the
// evidence is cut at a pattern boundary, never mid-pattern, and the full set
// stays in the `impacted` MCP answer.
func TestImpactedSelection_TraceExplanation_ElidesALongPatternSet(t *testing.T) {
	long := "doc/** internal/cli/testdata/** scripts/** ../../**/putnami.json ../../decisions.json ../../.agents/skills/**"
	s := ImpactedSelection{
		Projects:     []*Project{{ID: "/tooling/cli"}},
		ChangedFiles: []string{"decisions.json"},
		Trace: ImpactTrace{Seeds: map[string][]ImpactSeed{
			"/tooling/cli": {{File: "decisions.json", Kind: ImpactSeedDeclaredInput, Via: long}},
		}},
	}

	line := s.TraceExplanation()[1]

	if !strings.HasSuffix(line, " …]") {
		t.Errorf("line = %q, want it to close with an elision", line)
	}
	if strings.Contains(line, "../../.agents/skills/**") {
		t.Errorf("line = %q, want the tail of the set elided", line)
	}
	shown := strings.TrimSuffix(strings.SplitN(line, "[declared-input ", 2)[1], " …]")
	if !strings.HasPrefix(long, shown) || len(shown) > impactTraceViaWidth {
		t.Errorf("shown evidence %q is not a bounded prefix of %q", shown, long)
	}
	// A pattern is never cut in half: what is shown is whole patterns.
	for _, pattern := range strings.Fields(shown) {
		if !strings.Contains(long, pattern+" ") {
			t.Errorf("shown evidence carries a truncated pattern %q", pattern)
		}
	}
	if short := (ImpactedSelection{
		Projects:     []*Project{{ID: "/a"}},
		ChangedFiles: []string{"a/x.go"},
		Trace:        ImpactTrace{Seeds: map[string][]ImpactSeed{"/a": {{File: "a/x.go", Kind: ImpactSeedPathOwner, Via: "a"}}}},
	}).TraceExplanation()[1]; short != "    a/x.go → /a  [path-owner a]" {
		t.Errorf("a short claim was rewritten: %q", short)
	}
}

func TestImpactedSelection_TraceExplanation_EmptyTraceRendersNothing(t *testing.T) {
	if got := (ImpactedSelection{ChangedFiles: []string{"bun.lock"}}).TraceExplanation(); got != nil {
		t.Errorf("explanation with no trace = %q, want nothing", got)
	}
	empty := ImpactedSelection{Trace: ImpactTrace{Seeds: map[string][]ImpactSeed{}, Edges: map[string]ImpactEdge{}}}
	if got := empty.TraceExplanation(); got != nil {
		t.Errorf("explanation with empty maps = %q, want nothing", got)
	}
}

// Seeds render in diff order first, then workspace order — the diff is what the
// operator is looking at — even though the record is keyed by project.
func TestImpactedSelection_TraceExplanation_SeedsFollowTheDiffOrder(t *testing.T) {
	s := ImpactedSelection{
		Projects:     []*Project{{ID: "/a"}, {ID: "/b"}},
		ChangedFiles: []string{"b/second.go", "a/first.go", "shared.json"},
		Trace: ImpactTrace{Seeds: map[string][]ImpactSeed{
			"/a": {{File: "a/first.go", Kind: ImpactSeedPathOwner, Via: "a"}, {File: "shared.json", Kind: ImpactSeedDeclaredInput, Via: "../shared.json"}},
			"/b": {{File: "b/second.go", Kind: ImpactSeedPathOwner, Via: "b"}, {File: "shared.json", Kind: ImpactSeedDeclaredInput, Via: "../shared.json"}},
		}},
	}
	got := s.TraceExplanation()[1:]
	want := []string{
		"    b/second.go → /b  [path-owner b]",
		"    a/first.go → /a  [path-owner a]",
		"    shared.json → /a  [declared-input ../shared.json]",
		"    shared.json → /b  [declared-input ../shared.json]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("seed lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A contract edge fires only on a moved committed contract, so its line and
// its record entry name that contract and the digest the tree holds for it:
// a run that did fan out through a client says which file made it. The
// terminal line shows twelve hex digits; the record keeps all 64. Every other
// edge kind carries neither member, in the line or on the wire.
func TestImpactedSelection_TraceNamesTheContractThatMoved(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	s := ImpactedSelection{
		Projects: []*Project{
			{ID: "/services/catalog"}, {ID: "/clients/catalog-ts"}, {ID: "/apps/storefront"},
		},
		ChangedFiles: []string{"services/catalog/schema/openapi.json"},
		Trace: ImpactTrace{
			Seeds: map[string][]ImpactSeed{
				"/services/catalog": {{File: "services/catalog/schema/openapi.json", Kind: ImpactSeedPathOwner, Via: "services/catalog"}},
			},
			Edges: map[string]ImpactEdge{
				"/clients/catalog-ts": {From: "/services/catalog", Kind: ImpactEdgeContract,
					Via: "services/catalog/schema/openapi.json", ContractSHA256: digest},
				"/apps/storefront": {From: "/clients/catalog-ts", Kind: ImpactEdgeDependency},
			},
			Scopes: map[string][]string{"/clients/catalog-ts": {"build~generate"}},
		},
	}

	got := s.TraceExplanation()
	want := []string{
		"  --impacted: 1 changed file(s) reached 1 project(s) directly; propagation added 2 (dependency 1, contract 1, 1 task-scoped)",
		"    services/catalog/schema/openapi.json → /services/catalog  [path-owner services/catalog]",
		"    /clients/catalog-ts ← /services/catalog  [contract services/catalog/schema/openapi.json sha256:0123456789ab; tasks build~generate]",
		"    /apps/storefront ← /clients/catalog-ts  [dependency]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("explanation:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	record := s.TraceRecord()
	if want := (ImpactTraceEdge{Project: "/clients/catalog-ts", From: "/services/catalog", Kind: "contract",
		Via: "services/catalog/schema/openapi.json", ContractSHA256: digest}); len(record.Edges) != 2 || record.Edges[0] != want {
		t.Fatalf("record edges = %+v, want the contract edge first as %+v", record.Edges, want)
	}
	encoded, err := json.Marshal(record.Edges)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"project":"/clients/catalog-ts","from":"/services/catalog","kind":"contract",` +
		`"via":"services/catalog/schema/openapi.json","contractSha256":"` + digest + `"},` +
		`{"project":"/apps/storefront","from":"/clients/catalog-ts","kind":"dependency"}]`; string(encoded) != want {
		t.Errorf("record edges = %s, want %s", encoded, want)
	}
}
