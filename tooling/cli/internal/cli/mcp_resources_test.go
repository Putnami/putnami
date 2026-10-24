package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
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

// serveResourceRequests runs the real server, as production wires it, over the
// requests and returns the decoded responses by id.
func serveResourceRequests(t *testing.T, root string, requests ...string) map[int]testJSONRPCEnvelope {
	t.Helper()
	srv := newMCPServer(root, wsproto.Load(root), "test")
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

// The guidance resource of the real server equals the block body that
// `context generate` writes, decisions and gate line included, and it follows
// the workspace's documents on the next read.
func TestMCPGuidanceResourceEqualsTheBlockContextGenerateWrites(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", "the-server-lists-and-reads-the-guidance-map-and-project-resources")
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
	if !strings.Contains(written, "D-001 — every pull request names what it deletes") {
		t.Fatalf("the written block carries no decision:\n%s", written)
	}

	read := func(id int) string {
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

	if got := read(2); got != written {
		t.Errorf("guidance resource differs from the written block body\nresource:\n%s\nblock:\n%s", got, written)
	}
}
