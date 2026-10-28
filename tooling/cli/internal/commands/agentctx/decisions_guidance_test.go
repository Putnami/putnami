package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

const seededDecisions = `{
  "$schema": "https://putnami.dev/schemas/putnami-decisions.json",
  "protocolVersion": 1,
  "decisions": [
    {
      "id": "D-002",
      "statement": "every pull request names what it deletes",
      "settled": "2026-09-03",
      "settledBy": "fdumay",
      "reviewOnly": true
    },
    {
      "id": "D-001",
      "statement": "serverless workloads scale to zero",
      "settled": "2026-09-03",
      "settledBy": "fdumay",
      "check": {
        "kind": "json-value",
        "files": ["**/infra/requirements.json"],
        "pointer": "/scaling/minInstances",
        "rule": "equals",
        "value": 0
      }
    }
  ]
}`

func writeDecisions(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, featureproto.DecisionsFilename), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBlockNamesTheDecisionFilesAndListsNoDecision pins the block half of the
// decision guidance: one line names the decisions.json files, and no decision
// statement or id enters the block, so a new decision regenerates nothing.
func TestBlockNamesTheDecisionFilesAndListsNoDecision(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "settled-decisions-guidance", "the-block-names-the-decision-files-and-lists-no-decision")
	dir := t.TempDir()
	writeDecisions(t, dir, seededDecisions)

	line := decisionsGuidanceForWorkspace(dir)
	if want := "- Settled decisions are recorded in `decisions.json` files: the root one binds every project, a project's own binds that project."; !strings.HasPrefix(line, want) {
		t.Fatalf("the block line does not start with %q:\n%s", want, line)
	}
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Fatalf("the block line is not one line:\n%q", line)
	}
	block := guidanceBlock("lint,test,build,validate", line)
	for _, banned := range []string{"D-001", "D-002", "serverless", "settled 2026"} {
		if strings.Contains(block, banned) {
			t.Errorf("the block lists a decision (%q):\n%s", banned, block)
		}
	}
	if _, _, intact, found := findGuidanceBlock(block); !found || !intact {
		t.Fatal("a block carrying the decisions line is not its own intact block")
	}
}

// TestDecisionsGuidanceRendersNothingWithoutAUsableRegistry is the byte-
// identity rule: a workspace with no registry, an empty one, or one Putnami
// cannot read gets exactly the block it had before the registry existed. A
// malformed file must not fail `putnami context generate` — the decision gate
// is where a bad registry is reported.
func TestDecisionsGuidanceRendersNothingWithoutAUsableRegistry(t *testing.T) {
	for name, contents := range map[string]string{
		"absent":         "",
		"empty registry": `{"protocolVersion":1,"decisions":[]}`,
		"not json":       `{"protocolVersion":1,`,
		"wrong version":  `{"protocolVersion":2,"decisions":[]}`,
		"invalid entry":  `{"protocolVersion":1,"decisions":[{"id":"D-1","statement":"a","settled":"2026-1-2","settledBy":"x","reviewOnly":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if contents != "" {
				writeDecisions(t, dir, contents)
			}
			if rendered := decisionsGuidanceForWorkspace(dir); rendered != "" {
				t.Fatalf("rendered %q, want nothing", rendered)
			}
			if got, want := guidanceBlock("lint", ""), guidanceBlock("lint", decisionsGuidanceForWorkspace(dir)); got != want {
				t.Fatalf("the block is not byte-identical to a registry-less one:\n%s\n%s", got, want)
			}
			// No empty section and no dangling bullet: the body is the same six
			// lines a workspace without a registry has always had.
			if lines := strings.Count(guidanceBody("lint", ""), "\n"); lines != 6 {
				t.Fatalf("block body has %d lines, want exactly 6", lines)
			}
		})
	}
}

// TestWriteAgentEntrypointsNamesTheDecisionFiles proves the injection end to
// end: the generated AGENTS.md a workspace receives names the decisions.json
// files and lists none of the decisions.
func TestWriteAgentEntrypointsNamesTheDecisionFiles(t *testing.T) {
	dir := t.TempDir()
	writeDecisions(t, dir, seededDecisions)
	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("WriteAgentEntrypoints: %v", err)
	}
	agent := readGuidance(t, dir, agentEntrypointPath)
	if !strings.Contains(agent, "- Settled decisions are recorded in `decisions.json` files") {
		t.Errorf("generated AGENTS.md does not name the decision files:\n%s", agent)
	}
	if strings.Contains(agent, "D-001") || strings.Contains(agent, "D-002") {
		t.Errorf("generated AGENTS.md lists a decision:\n%s", agent)
	}
	// A decision added to a registry that already holds one changes no byte of
	// the entrypoint.
	writeDecisions(t, dir, strings.Replace(seededDecisions, `"decisions": [`, `"decisions": [
    {"id": "D-003", "statement": "a third decision", "settled": "2026-09-04", "settledBy": "fdumay", "reviewOnly": true},`, 1))
	before := agent
	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("second WriteAgentEntrypoints: %v", err)
	}
	if after := readGuidance(t, dir, agentEntrypointPath); after != before {
		t.Fatalf("an added decision rewrote the entrypoint:\n%s\n%s", before, after)
	}

	// The registry's last decision going away removes the line, and its first
	// decision coming back restores it: those two changes regenerate the block.
	if err := os.Remove(filepath.Join(dir, featureproto.DecisionsFilename)); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("third WriteAgentEntrypoints: %v", err)
	}
	if without := readGuidance(t, dir, agentEntrypointPath); without == before || strings.Contains(without, "Settled decisions are recorded") {
		t.Fatalf("removing the registry left the line in the block:\n%s", without)
	}
	writeDecisions(t, dir, seededDecisions)
	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("fourth WriteAgentEntrypoints: %v", err)
	}
	if again := readGuidance(t, dir, agentEntrypointPath); again != before {
		t.Fatalf("the first decision did not restore the line:\n%s", again)
	}
}

const projectDecisionsRegistry = `{"protocolVersion":1,"decisions":[
	{"id":"APP-1","statement":"the app never listens on a fixed port","settled":"2026-09-20","settledBy":"fdumay","reviewOnly":true}]}`

// TestBlockLineKeepsProjectRegistriesOut keeps a project's decisions out of
// the line every agent reads: the line names the decisions.json files and
// never a project registry, and a workspace whose only registry is a
// project's still gets it.
func TestBlockLineKeepsProjectRegistriesOut(t *testing.T) {
	root := makeLoadableFixture(t)
	writeDecisions(t, root, seededDecisions)
	writeDecisions(t, filepath.Join(root, "svc", "app"), projectDecisionsRegistry)

	line := decisionsGuidanceForWorkspace(root)
	if !strings.HasPrefix(line, "- Settled decisions are recorded in") {
		t.Fatalf("the block line is missing:\n%s", line)
	}
	if strings.Contains(line, "svc/app") || strings.Contains(line, "APP-1") {
		t.Fatalf("the block line names a project registry:\n%s", line)
	}

	if err := os.Remove(filepath.Join(root, featureproto.DecisionsFilename)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(decisionsGuidanceForWorkspace(root), "- Settled decisions are recorded in") {
		t.Fatalf("a project-only workspace lost the block line")
	}
}

// TestProjectDecisionsAreThoseOfTheProjectRegistry: a project's decisions are
// those of the registry in its own directory, and only those.
func TestProjectDecisionsAreThoseOfTheProjectRegistry(t *testing.T) {
	root := makeLoadableFixture(t)
	writeDecisions(t, root, seededDecisions)
	writeDecisions(t, filepath.Join(root, "svc", "app"), projectDecisionsRegistry)

	app := projectDecisions(root, "svc/app")
	if app == nil || app.Registry != "svc/app/decisions.json" ||
		len(app.Decisions) != 1 || app.Decisions[0].ID != "APP-1" {
		t.Fatalf("app decisions = %+v, want APP-1 from svc/app/decisions.json", app)
	}
	if lib := projectDecisions(root, "svc/lib"); lib != nil {
		t.Fatalf("a project without a registry received decisions: %+v", lib)
	}
	if rootScope := projectDecisions(root, ""); rootScope != nil {
		t.Fatalf("the workspace root is not a project scope: %+v", rootScope)
	}
}
