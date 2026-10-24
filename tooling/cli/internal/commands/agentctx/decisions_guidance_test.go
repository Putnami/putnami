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

// TestSettledDecisionsRendersEveryDecision covers contract point 4 plus the
// reason the checked half is rendered too: an agent that can see a check
// exists does not spend a cycle proposing a change the gate will reject.
func TestSettledDecisionsRendersEveryDecision(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "settled-decisions-guidance", "the-decision-list-renders-every-root-decision-in-canonical-order")
	dir := t.TempDir()
	writeDecisions(t, dir, seededDecisions)

	rendered := SettledDecisions(dir)
	for _, want := range []string{
		"- D-001 — serverless workloads scale to zero (settled 2026-09-03, `validate` enforces it)\n",
		"- D-002 — every pull request names what it deletes (settled 2026-09-03, review-only, held by a human reviewer)\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered decisions are missing %q:\n%s", want, rendered)
		}
	}
	// Canonical order, not authored order: the same registry always renders the
	// same bytes.
	if strings.Index(rendered, "D-001") > strings.Index(rendered, "D-002") {
		t.Fatalf("decisions are not rendered in canonical id order:\n%s", rendered)
	}
	if !strings.HasSuffix(rendered, "\n") {
		t.Fatalf("rendered decisions do not end with a newline:\n%q", rendered)
	}
}

// TestBlockNamesTheDecisionFilesAndListsNoDecision pins the block half of the
// decision guidance: one line names the decisions.json files, and no decision
// statement or id enters the block, so a new decision regenerates nothing.
func TestBlockNamesTheDecisionFilesAndListsNoDecision(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "settled-decisions-guidance", "the-block-names-the-decision-files-and-lists-no-decision")
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
			if rendered := SettledDecisions(dir); rendered != "" {
				t.Fatalf("listed %q, want nothing", rendered)
			}
			if body := WorkspaceGuidanceBody(dir); strings.Contains(body, settledDecisionsHeading) {
				t.Fatalf("the guidance resource has an empty decisions section:\n%s", body)
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
	// A decision added later changes no byte of the entrypoint.
	writeDecisions(t, dir, strings.Replace(seededDecisions, `"id": "D-002"`, `"id": "D-003"`, 1))
	// A second run over its own output changes nothing: the derivation is a
	// pure function of the committed registry's presence.
	before := agent
	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("second WriteAgentEntrypoints: %v", err)
	}
	if after := readGuidance(t, dir, agentEntrypointPath); after != before {
		t.Fatalf("a changed decision rewrote the entrypoint:\n%s\n%s", before, after)
	}
}

const projectDecisionsRegistry = `{"protocolVersion":1,"decisions":[
	{"id":"APP-1","statement":"the app never listens on a fixed port","settled":"2026-09-20","settledBy":"fdumay","reviewOnly":true}]}`

// TestSettledDecisionsNameProjectRegistries keeps a project's decisions out
// of the list every agent reads: the list names where the project registry is
// and how many decisions it holds, never their statements.
func TestSettledDecisionsNameProjectRegistries(t *testing.T) {
	root := makeLoadableFixture(t)
	writeDecisions(t, root, seededDecisions)
	writeDecisions(t, filepath.Join(root, "svc", "app"), projectDecisionsRegistry)

	rendered := SettledDecisions(root)
	if !strings.Contains(rendered, "D-001 — serverless workloads scale to zero") {
		t.Fatalf("the root decisions are missing:\n%s", rendered)
	}
	if !strings.HasSuffix(rendered, "- Project decisions, returned by `putnami.context` with their project: `svc/app/decisions.json` (1).\n") {
		t.Fatalf("the project registry is not named:\n%s", rendered)
	}
	if strings.Contains(rendered, "APP-1") {
		t.Fatalf("a project decision leaked into the workspace list:\n%s", rendered)
	}
	if strings.Contains(decisionsGuidanceForWorkspace(root), "svc/app") {
		t.Fatalf("the block line names a project registry:\n%s", decisionsGuidanceForWorkspace(root))
	}

	// A workspace whose only registry is a project's still gets the pointer and
	// the block line.
	if err := os.Remove(filepath.Join(root, featureproto.DecisionsFilename)); err != nil {
		t.Fatal(err)
	}
	rendered = SettledDecisions(root)
	if !strings.HasPrefix(rendered, "- Project decisions, returned by") || !strings.Contains(rendered, "`svc/app/decisions.json` (1)") {
		t.Fatalf("a project-only workspace lost the pointer:\n%s", rendered)
	}
	if !strings.HasPrefix(decisionsGuidanceForWorkspace(root), "- Settled decisions are recorded in") {
		t.Fatalf("a project-only workspace lost the block line")
	}
}

// TestWorkspaceGuidanceBodyHoldsTheBlockAndTheDecisions pins the guidance
// resource: the block body exactly as `context generate` writes it, then the
// settled decisions under their own heading, so a notice "D-xxx added" points
// at a resource that shows the decision.
func TestWorkspaceGuidanceBodyHoldsTheBlockAndTheDecisions(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "settled-decisions-guidance", "the-guidance-resource-holds-the-block-body-then-the-decisions")
	root := makeLoadableFixture(t)
	writeDecisions(t, root, seededDecisions)

	body := WorkspaceGuidanceBody(root)
	block := guidanceBody(gateTasksForWorkspace(root), decisionsGuidanceForWorkspace(root))
	if !strings.HasPrefix(body, block+"\n"+settledDecisionsHeading+"\n\n") {
		t.Fatalf("the resource does not start with the block body and the heading:\n%s", body)
	}
	if got, want := strings.TrimPrefix(body, block+"\n"+settledDecisionsHeading+"\n\n"), SettledDecisions(root); got != want {
		t.Fatalf("the decision section = %q, want %q", got, want)
	}
}

// TestWorkspaceGuidanceBodyFollowsTheCurrentProjectSet pins the long-lived MCP
// reader: a project added to the workspace after the first render, with the
// workspace load memo still warm, is named by the next render. Editing the
// decisions of a project the memo already holds would pass without the memo
// being dropped, so the test adds a new project to includes.
func TestWorkspaceGuidanceBodyFollowsTheCurrentProjectSet(t *testing.T) {
	root := makeLoadableFixture(t)
	writeDecisions(t, root, seededDecisions)
	if first := WorkspaceGuidanceBody(root); strings.Contains(first, "Project decisions") {
		t.Fatalf("no project registry exists yet:\n%s", first)
	}

	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["svc/app","svc/lib","svc/extra"]}`)
	write("svc/extra/putnami.json", `{"name":"example/extra"}`)
	writeDecisions(t, filepath.Join(root, "svc", "extra"), projectDecisionsRegistry)
	if second := WorkspaceGuidanceBody(root); !strings.Contains(second, "`svc/extra/decisions.json` (1)") {
		t.Fatalf("the project added after the first render is not named:\n%s", second)
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
