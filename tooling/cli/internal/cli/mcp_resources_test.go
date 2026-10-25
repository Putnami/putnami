package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const resourcesWiringDecisions = `{
  "$schema": "https://putnami.dev/schemas/putnami-decisions.json",
  "protocolVersion": 1,
  "decisions": [
    {"id": "D-001", "statement": "every pull request names what it deletes", "settled": "2026-09-03", "settledBy": "fdumay", "reviewOnly": true}
  ]
}`

const resourcesWiringDecisionsTwo = `{
  "$schema": "https://putnami.dev/schemas/putnami-decisions.json",
  "protocolVersion": 1,
  "decisions": [
    {"id": "D-001", "statement": "every pull request names what it deletes", "settled": "2026-09-03", "settledBy": "fdumay", "reviewOnly": true},
    {"id": "D-002", "statement": "every GitHub artifact is written in English", "settled": "2026-09-11", "settledBy": "fdumay", "reviewOnly": true}
  ]
}`

// serveResourceRequests runs the real server, as production wires it, over the
// requests and returns the decoded responses by id.
func serveResourceRequests(t *testing.T, root string, requests ...string) map[int]testJSONRPCEnvelope {
	t.Helper()
	srv := newMCPServer(root, "test")
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(strings.Join(requests, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	responses := map[int]testJSONRPCEnvelope{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var envelope testJSONRPCEnvelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		responses[envelope.ID] = envelope
	}
	return responses
}

// guidanceBlockBody returns the bytes between the two marker lines of the
// Putnami block in a written AGENTS.md.
func guidanceBlockBody(t *testing.T, agentsMD string) string {
	t.Helper()
	const begin, end = "<!-- putnami:guidance v2 begin sha256:", "<!-- putnami:guidance v2 end -->"
	start := strings.Index(agentsMD, begin)
	if start < 0 {
		t.Fatalf("AGENTS.md has no guidance block:\n%s", agentsMD)
	}
	bodyStart := start + strings.Index(agentsMD[start:], "\n") + 1
	bodyEnd := strings.Index(agentsMD[bodyStart:], end)
	if bodyEnd < 0 {
		t.Fatalf("AGENTS.md has no end marker:\n%s", agentsMD)
	}
	return agentsMD[bodyStart : bodyStart+bodyEnd]
}

// The guidance resource of the real server holds the block body that `context
// generate` writes, then the settled decisions. The block lists no decision, so
// a decision added between two reads changes the resource and leaves the block
// that the next `context generate` writes as it was.
func TestMCPGuidanceResourceHoldsTheBlockBodyAndTheDecisions(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", "the-guidance-resource-renders-the-block-body-on-every-read")
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app"]}`)
	write("packages/app/putnami.json", `{"name":"app","type":"application"}`)
	write("decisions.json", resourcesWiringDecisions)
	workspace.InvalidateLoadCache(root)

	if err := agentctx.WriteAgentEntrypoints(root); err != nil {
		t.Fatal(err)
	}
	agents, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	written := guidanceBlockBody(t, string(agents))
	if !strings.Contains(written, "Settled decisions are recorded in `decisions.json` files") || strings.Contains(written, "D-001") {
		t.Fatalf("the written block must name the decision files and list no decision:\n%s", written)
	}
	const heading = "\n## Settled decisions\n\n"
	const first = "- D-001 — every pull request names what it deletes (settled 2026-09-03, review-only, held by a human reviewer)\n"
	const second = "- D-002 — every GitHub artifact is written in English (settled 2026-09-11, review-only, held by a human reviewer)\n"

	read := func() string {
		t.Helper()
		responses := serveResourceRequests(t, root,
			`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`,
			`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"putnami://workspace/guidance"}}`)
		list := responses[1]
		if list.Error != nil {
			t.Fatalf("resources/list: %+v", list.Error)
		}
		var listed struct {
			Resources []struct {
				URI string `json:"uri"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(list.Result, &listed); err != nil {
			t.Fatal(err)
		}
		var uris []string
		for _, resource := range listed.Resources {
			uris = append(uris, resource.URI)
		}
		wantSections := []string{"putnami://workspace/guidance", "putnami://workspace/map"}
		for _, section := range agentctx.MapSections {
			if section != agentctx.MapSectionAll {
				wantSections = append(wantSections, "putnami://workspace/map/"+section)
			}
		}
		wantSections = append(wantSections, "putnami://projects/%2Fpackages%2Fapp/context")
		var got []string
		for _, uri := range uris {
			if strings.HasPrefix(uri, "putnami://") {
				got = append(got, uri)
			}
		}
		if !reflect.DeepEqual(got, wantSections) {
			t.Fatalf("listed putnami:// resources = %v\nwant %v", got, wantSections)
		}
		resp := responses[2]
		if resp.Error != nil {
			t.Fatalf("resources/read guidance: %+v", resp.Error)
		}
		var result struct {
			Contents []struct {
				Text     string `json:"text"`
				MimeType string `json:"mimeType"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Contents) != 1 || result.Contents[0].MimeType != "text/markdown" {
			t.Fatalf("guidance contents = %+v, want one text/markdown item", result.Contents)
		}
		return result.Contents[0].Text
	}

	if got, want := read(), written+heading+first; got != want {
		t.Errorf("guidance resource differs from the block body and the decisions\nresource:\n%s\nwant:\n%s", got, want)
	}

	write("decisions.json", resourcesWiringDecisionsTwo)
	if err := agentctx.WriteAgentEntrypoints(root); err != nil {
		t.Fatal(err)
	}
	agents, err = os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if rewritten := guidanceBlockBody(t, string(agents)); rewritten != written {
		t.Fatalf("a decision added after the first write changed the block:\n%s", rewritten)
	}
	if got, want := read(), written+heading+first+second; got != want {
		t.Errorf("after a decision was added, the guidance resource differs\nresource:\n%s\nwant:\n%s", got, want)
	}
}

// instructionsOf initializes the real server over root and returns its
// instructions.
func instructionsOf(t *testing.T, root string) string {
	t.Helper()
	responses := serveResourceRequests(t, root, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	var result struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(responses[1].Result, &result); err != nil {
		t.Fatal(err)
	}
	return result.Instructions
}

// The real server lists the settled root decisions in its initialize
// instructions, after the routing preamble, as a pointer to the guidance
// resource and one compact line per decision. It never repeats the block, and
// it names no project registry or project decision, which the resource holds.
func TestMCPInitializeInstructionsListTheSettledDecisions(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "live-extension-guidance", "the-instructions-list-the-decisions-after-the-preamble-and-before-the-guides")
	root := t.TempDir()
	for rel, content := range map[string]string{
		"putnami.workspace.json":      `{"name":"fixture","includes":["packages/app"]}`,
		"packages/app/putnami.json":   `{"name":"app","type":"application"}`,
		"decisions.json":              resourcesWiringDecisions,
		"packages/app/decisions.json": resourcesWiringDecisionsTwo,
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace.InvalidateLoadCache(root)

	instructions := instructionsOf(t, root)
	for _, want := range []string{
		"--- settled decisions ---\n\nComplete list, with each decision's date and enforcement and the project registries: resource putnami://workspace/guidance\n- D-001 — every pull request names what it deletes",
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions are missing %q:\n%s", want, instructions)
		}
	}
	for _, banned := range []string{"D-002", "This is a Putnami workspace.", "review-only", "packages/app/decisions.json", "settled 2026"} {
		if strings.Contains(instructions, banned) {
			t.Errorf("instructions carry %q:\n%s", banned, instructions)
		}
	}
}

// TestMCPContextAnswersCarryNoDiskArtifact runs the real server over the
// workspace_map and agent_context tools and their resources. Each answer
// carries the document built in memory and no diskArtifact member, and the
// session writes no map, fragment or agent-context file.
func TestMCPContextAnswersCarryNoDiskArtifact(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "mcp-rebuilds-the-same-projection-in-memory")
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app"]}`)
	write("packages/app/putnami.json", `{"name":"app","type":"application"}`)
	write("packages/app/README.md", "# App\n\nServes the fixture.\n")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	responses := serveResourceRequests(t, root,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workspace_map","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"agent_context","arguments":{"project":"app"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"putnami://workspace/map"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"putnami://projects/%2Fpackages%2Fapp/context"}}`)

	answers := map[string]string{}
	for id, tool := range map[int]string{1: "workspace_map", 2: "agent_context"} {
		var result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if responses[id].Error != nil {
			t.Fatalf("%s: %+v", tool, responses[id].Error)
		}
		if err := json.Unmarshal(responses[id].Result, &result); err != nil || result.IsError || len(result.Content) == 0 {
			t.Fatalf("%s result = %s (decode error %v)", tool, responses[id].Result, err)
		}
		answers[tool] = result.Content[0].Text
	}
	for id, uri := range map[int]string{3: "putnami://workspace/map", 4: "putnami://projects/%2Fpackages%2Fapp/context"} {
		var result struct {
			Contents []struct {
				Text string `json:"text"`
			} `json:"contents"`
		}
		if responses[id].Error != nil {
			t.Fatalf("%s: %+v", uri, responses[id].Error)
		}
		if err := json.Unmarshal(responses[id].Result, &result); err != nil || len(result.Contents) != 1 {
			t.Fatalf("%s result = %s (decode error %v)", uri, responses[id].Result, err)
		}
		answers[uri] = result.Contents[0].Text
	}

	for label, text := range answers {
		var members map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &members); err != nil {
			t.Fatalf("%s answer is not a JSON object: %v\n%s", label, err, text)
		}
		if _, present := members["document"]; !present {
			t.Errorf("%s answer carries no document: %s", label, text)
		}
		if _, present := members["diskArtifact"]; present {
			t.Errorf("%s answer still carries diskArtifact: %s", label, text)
		}
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir() && (rel == ".putnami/context-map" || d.Name() == ".gen"):
			t.Errorf("the MCP session created the directory %s", rel)
		case !d.IsDir() && (d.Name() == "repo-map.json" || d.Name() == "repo-map.md" ||
			d.Name() == "map-fragment.json" || d.Name() == "agent-context.json"):
			t.Errorf("the MCP session wrote %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMCPWorkspaceMapReusesTheSessionFragments calls workspace_map three times
// on one real server, one Serve after another, as one long session does. The
// first call builds every project's fragment, the second reuses every one, and
// after an edit the third rebuilds only the edited project's. A server that
// held no session cache would build every fragment on every call.
func TestMCPWorkspaceMapReusesTheSessionFragments(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "mcp-reuses-a-fragment-until-its-inputs-change")
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib"]}`)
	write("packages/app/putnami.json", `{"name":"app","type":"application","dependencies":["lib"]}`)
	write("packages/lib/putnami.json", `{"name":"lib","type":"library"}`)
	write("packages/lib/README.md", "# Lib\n\nShared code.\n")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	srv := newMCPServer(root, "test")
	type counts struct{ projects, built, reused int }
	call := func(id int) counts {
		t.Helper()
		request := `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"workspace_map","arguments":{}}}`
		var out bytes.Buffer
		if err := srv.Serve(context.Background(), strings.NewReader(request+"\n"), &out); err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var envelope testJSONRPCEnvelope
			if err := json.Unmarshal([]byte(line), &envelope); err != nil {
				t.Fatalf("decode %q: %v", line, err)
			}
			if envelope.ID != id {
				continue
			}
			var result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if envelope.Error != nil || json.Unmarshal(envelope.Result, &result) != nil || result.IsError || len(result.Content) == 0 {
				t.Fatalf("workspace_map call %d = %s (error %+v)", id, envelope.Result, envelope.Error)
			}
			var answer struct {
				Document struct {
					Projects []json.RawMessage `json:"projects"`
				} `json:"document"`
				Provenance struct {
					FragmentsBuilt  int `json:"fragmentsBuilt"`
					FragmentsReused int `json:"fragmentsReused"`
				} `json:"provenance"`
			}
			if err := json.Unmarshal([]byte(result.Content[0].Text), &answer); err != nil {
				t.Fatalf("decode workspace_map answer %d: %v", id, err)
			}
			return counts{len(answer.Document.Projects), answer.Provenance.FragmentsBuilt, answer.Provenance.FragmentsReused}
		}
		t.Fatalf("no response to workspace_map call %d:\n%s", id, out.String())
		return counts{}
	}

	if cold := call(1); cold.projects != 2 || cold.built != 2 || cold.reused != 0 {
		t.Fatalf("first call = %+v, want 2 projects, every fragment built", cold)
	}
	if warm := call(2); warm.built != 0 || warm.reused != 2 {
		t.Errorf("second call = %+v, want every fragment reused from the session", warm)
	}
	write("packages/lib/README.md", "# Lib\n\nShared code, edited.\n")
	if edited := call(3); edited.built != 1 || edited.reused != 1 {
		t.Errorf("call after an edit = %+v, want only the edited project's fragment rebuilt", edited)
	}
}
