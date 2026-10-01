package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
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

// TestDecisionsGuidanceRendersEveryDecision covers contract point 4 plus
// the reason the checked half is rendered too: an agent that can see a check
// exists does not spend a cycle proposing a change the gate will reject.
func TestDecisionsGuidanceRendersEveryDecision(t *testing.T) {
	dir := t.TempDir()
	writeDecisions(t, dir, seededDecisions)

	rendered := decisionsGuidanceForWorkspace(dir)
	for _, want := range []string{
		"Settled decisions are recorded in `decisions.json` files: the root one binds every project",
		"D-001 — serverless workloads scale to zero (settled 2026-09-03, `validate` enforces it)",
		"D-002 — every pull request names what it deletes (settled 2026-09-03, review-only, held by a human reviewer)",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered guidance is missing %q:\n%s", want, rendered)
		}
	}
	// Canonical order, not authored order: the same registry always renders the
	// same bytes, so a regeneration that changes nothing changes no file.
	if strings.Index(rendered, "D-001") > strings.Index(rendered, "D-002") {
		t.Fatalf("decisions are not rendered in canonical id order:\n%s", rendered)
	}
	if !strings.HasSuffix(rendered, "\n") {
		t.Fatalf("rendered guidance does not end with a newline:\n%q", rendered)
	}

	block := guidanceBlock("lint,test,build,validate", rendered)
	if _, _, intact, found := findGuidanceBlock(block); !found || !intact {
		t.Fatal("a block carrying decisions is not its own intact block")
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

// TestWriteAgentEntrypointsCarriesTheSettledDecisions proves the injection
// end to end: the generated AGENTS.md a workspace receives names what that
// workspace has settled.
func TestWriteAgentEntrypointsCarriesTheSettledDecisions(t *testing.T) {
	dir := t.TempDir()
	writeDecisions(t, dir, seededDecisions)
	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("WriteAgentEntrypoints: %v", err)
	}
	agent := readGuidance(t, dir, agentEntrypointPath)
	for _, want := range []string{
		"D-001 — serverless workloads scale to zero (settled 2026-09-03, `validate` enforces it)",
		"D-002 — every pull request names what it deletes (settled 2026-09-03, review-only, held by a human reviewer)",
	} {
		if !strings.Contains(agent, want) {
			t.Errorf("generated AGENTS.md is missing %q:\n%s", want, agent)
		}
	}
	// A second run over its own output changes nothing: the derivation is a
	// pure function of the committed registry.
	before := agent
	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("second WriteAgentEntrypoints: %v", err)
	}
	if after := readGuidance(t, dir, agentEntrypointPath); after != before {
		t.Fatalf("a second generation rewrote the entrypoint:\n%s\n%s", before, after)
	}
}

const projectDecisionsRegistry = `{"protocolVersion":1,"decisions":[
	{"id":"APP-1","statement":"the app never listens on a fixed port","settled":"2026-09-20","settledBy":"fdumay","reviewOnly":true}]}`

// TestDecisionsGuidanceNamesProjectRegistries keeps a project's decisions out
// of the guidance every agent reads: the block names where the project
// registry is and how many decisions it holds, never their statements.
func TestDecisionsGuidanceNamesProjectRegistries(t *testing.T) {
	root := makeLoadableFixture(t)
	writeDecisions(t, root, seededDecisions)
	writeDecisions(t, filepath.Join(root, "svc", "app"), projectDecisionsRegistry)

	rendered := decisionsGuidanceForWorkspace(root)
	if !strings.Contains(rendered, "D-001 — serverless workloads scale to zero") {
		t.Fatalf("the root decisions are missing:\n%s", rendered)
	}
	if !strings.HasSuffix(rendered, "  - Project decisions, returned by `putnami.context` with their project: `svc/app/decisions.json` (1).\n") {
		t.Fatalf("the project registry is not named:\n%s", rendered)
	}
	if strings.Contains(rendered, "APP-1") {
		t.Fatalf("a project decision leaked into the workspace guidance:\n%s", rendered)
	}

	// A workspace whose only registry is a project's still gets the rule and
	// the pointer.
	if err := os.Remove(filepath.Join(root, featureproto.DecisionsFilename)); err != nil {
		t.Fatal(err)
	}
	rendered = decisionsGuidanceForWorkspace(root)
	if !strings.HasPrefix(rendered, "- Settled decisions are recorded in") || !strings.Contains(rendered, "`svc/app/decisions.json` (1)") {
		t.Fatalf("a project-only workspace lost the rule or the pointer:\n%s", rendered)
	}
}

// TestAgentContextReturnsTheProjectDecisions is the other half: the agent
// orienting on a project receives that project's decisions, and only those.
func TestAgentContextReturnsTheProjectDecisions(t *testing.T) {
	root := makeLoadableFixture(t)
	writeDecisions(t, root, seededDecisions)
	writeDecisions(t, filepath.Join(root, "svc", "app"), projectDecisionsRegistry)

	app, err := BuildAgentContextResult(root, fixtureVersion, "/svc/app")
	if err != nil {
		t.Fatalf("BuildAgentContextResult: %v", err)
	}
	if app.Decisions == nil || app.Decisions.Registry != "svc/app/decisions.json" ||
		len(app.Decisions.Decisions) != 1 || app.Decisions.Decisions[0].ID != "APP-1" {
		t.Fatalf("app decisions = %+v, want APP-1 from svc/app/decisions.json", app.Decisions)
	}
	lib, err := BuildAgentContextResult(root, fixtureVersion, "/svc/lib")
	if err != nil {
		t.Fatalf("BuildAgentContextResult: %v", err)
	}
	if lib.Decisions != nil {
		t.Fatalf("a project without a registry received decisions: %+v", lib.Decisions)
	}
}
