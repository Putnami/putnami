package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
)

const (
	testGuidanceBody              = "This is a Putnami workspace.\n- Rule one.\n"
	checkListsConcreteURIs        = "the-server-lists-each-workspace-resource-by-a-concrete-uri"
	checkGuidanceRendersEachRead  = "the-guidance-resource-renders-the-block-body-on-every-read"
	checkResourceReturnsToolBytes = "a-map-or-project-resource-returns-its-tools-bytes"
	checkListingPreparesNoGraph   = "listing-resources-prepares-no-workspace-graph"
	checkUnknownIsInvalidParams   = "an-unknown-or-unavailable-resource-is-invalid-params"
	checkResourcesFollowWorkspace = "project-resources-follow-the-current-project-set"
	checkNegotiation              = "initialize-answers-with-a-version-the-server-speaks"
	checkDiscover                 = "server-discover-is-method-not-found"
	checkRelayListMatchesServer   = "the-relay-and-the-server-speak-the-same-versions"
)

// resourceServer builds a server over the shared fixture with the three
// workspace builders injected. The map and context fakes echo their arguments,
// so a byte-for-byte match with the tool proves both paths reach one closure.
func resourceServer(t *testing.T) *Server {
	t.Helper()
	dir := fixtureWorkspace(t)
	return NewServer(Options{
		WorkspaceRoot: dir,
		Config:        wsproto.Load(dir),
		ServerVersion: "test",
		WorkspaceGuidance: func(context.Context) string {
			return testGuidanceBody
		},
		WorkspaceMap: func(_ context.Context, section, project string) (any, error) {
			return fakeWorkspaceMap{
				Document:   fakeMapDocument{Workspace: "fixture"},
				Provenance: fakeMapScope{Section: section, Project: project},
			}, nil
		},
		AgentContext: func(_ context.Context, selector string) (any, error) {
			return struct {
				Project string `json:"project"`
			}{Project: selector}, nil
		},
	})
}

// testReply is the part of a JSON-RPC response these tests read.
type testReply struct {
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Resources       []struct {
			URI string `json:"uri"`
		} `json:"resources"`
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"contents"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type testURIParams struct {
	URI string `json:"uri"`
}

type testToolCall struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments"`
}

// call sends one request on a fresh session and returns its reply.
func call(t *testing.T, srv *Server, method string, params any) testReply {
	t.Helper()
	body, err := json.Marshal(req(1, method, params))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), bytes.NewReader(append(body, '\n')), &out); err != nil {
		t.Fatal(err)
	}
	var reply testReply
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &reply); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	return reply
}

func listedURIs(t *testing.T, srv *Server) []string {
	t.Helper()
	var uris []string
	for _, item := range call(t, srv, "resources/list", nil).Result.Resources {
		uris = append(uris, item.URI)
	}
	return uris
}

// readResourceText returns the text and MIME type of a resources/read answer.
func readResourceText(t *testing.T, srv *Server, uri string) (text, mime string) {
	t.Helper()
	reply := call(t, srv, "resources/read", testURIParams{URI: uri})
	if reply.Error != nil || len(reply.Result.Contents) != 1 {
		t.Fatalf("read %s = %+v, want one content item", uri, reply)
	}
	item := reply.Result.Contents[0]
	if item.URI != uri {
		t.Errorf("content uri = %v, want %s", item.URI, uri)
	}
	return item.Text, item.MimeType
}

// toolText returns the first text block of a tools/call answer.
func toolText(t *testing.T, srv *Server, name string, args map[string]string) string {
	t.Helper()
	reply := call(t, srv, "tools/call", testToolCall{Name: name, Arguments: args})
	if reply.Error != nil || reply.Result.IsError || len(reply.Result.Content) == 0 {
		t.Fatalf("%s = %+v, want a text result", name, reply)
	}
	return reply.Result.Content[0].Text
}

// assertCode fails unless the reply is a JSON-RPC error with the code.
func assertCode(t *testing.T, what string, reply testReply, code int) {
	t.Helper()
	if reply.Error == nil || reply.Error.Code != code {
		t.Errorf("%s = %+v, want error code %d", what, reply, code)
	}
}

func TestResourcesListIncludesTheWorkspaceResources(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkListsConcreteURIs)
	uris := listedURIs(t, resourceServer(t))
	want := []string{
		workspaceContextURI,
		"putnami://workspace/guidance",
		"putnami://workspace/map",
		"putnami://workspace/map/projects",
		"putnami://workspace/map/dependencies",
		"putnami://workspace/map/intercalls",
		"putnami://workspace/map/apis",
		"putnami://workspace/map/config-keys",
		"putnami://workspace/map/schemas",
		"putnami://workspace/map/docs",
		"putnami://projects/%2Fpackages%2Fapp/context",
		"putnami://projects/%2Fpackages%2Flib/context",
	}
	if !reflect.DeepEqual(uris, want) {
		t.Errorf("resources/list = %v\nwant %v", uris, want)
	}
}

func TestWorkspaceMapSectionsMatchTheToolSchema(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkListsConcreteURIs)
	var enum []string
	for _, tool := range newFixtureServer(t).toolDefs() {
		if tool.Name != "workspace_map" {
			continue
		}
		var schema struct {
			Properties struct {
				Section struct {
					Enum []string `json:"enum"`
				} `json:"section"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		enum = schema.Properties.Section.Enum
	}
	want := append(append([]string(nil), workspaceMapSections...), "all")
	if !reflect.DeepEqual(enum, want) {
		t.Errorf("workspace_map section enum = %v, want the resource sections plus all: %v", enum, want)
	}
}

func TestGuidanceResourceReturnsTheInjectedBody(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkGuidanceRendersEachRead)
	text, mime := readResourceText(t, resourceServer(t), workspaceGuidanceURI)
	if text != testGuidanceBody || mime != "text/markdown" {
		t.Errorf("guidance = (%q, %q), want the injected body as text/markdown", text, mime)
	}
}

func TestGuidanceResourceRendersOnEveryRead(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkGuidanceRendersEachRead)
	dir := fixtureWorkspace(t)
	reads := 0
	srv := NewServer(Options{
		WorkspaceRoot: dir,
		WorkspaceGuidance: func(context.Context) string {
			reads++
			return strings.Repeat("x", reads)
		},
	})
	first, _ := readResourceText(t, srv, workspaceGuidanceURI)
	second, _ := readResourceText(t, srv, workspaceGuidanceURI)
	if first != "x" || second != "xx" {
		t.Errorf("reads = (%q, %q), want a fresh render for each", first, second)
	}
}

func TestMapResourcesReturnTheToolBytes(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkResourceReturnsToolBytes)
	srv := resourceServer(t)
	cases := map[string]map[string]string{
		"putnami://workspace/map": {"section": "all"},
	}
	for _, section := range workspaceMapSections {
		cases["putnami://workspace/map/"+section] = map[string]string{"section": section}
	}
	for uri, args := range cases {
		text, mime := readResourceText(t, srv, uri)
		if want := toolText(t, srv, "workspace_map", args); text != want {
			t.Errorf("%s = %s\nwant the tool's bytes %s", uri, text, want)
		}
		if mime != "application/json" {
			t.Errorf("%s mimeType = %q, want application/json", uri, mime)
		}
	}
}

func TestProjectContextResourceReturnsTheToolBytes(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkResourceReturnsToolBytes)
	srv := resourceServer(t)
	for _, id := range []string{"/packages/app", "/packages/lib"} {
		text, _ := readResourceText(t, srv, projectContextURI(id))
		if want := toolText(t, srv, "agent_context", map[string]string{"project": id}); text != want {
			t.Errorf("%s context = %s\nwant the tool's bytes %s", id, text, want)
		}
	}
}

func TestProjectContextURIEscapesTheCanonicalID(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkListsConcreteURIs)
	for _, id := range []string{"/packages/app", "/a b/c%d", "/tooling/cli"} {
		uri := projectContextURI(id)
		if strings.Contains(strings.TrimSuffix(strings.TrimPrefix(uri, projectContextPrefix), projectContextSuffix), "/") {
			t.Errorf("%q escapes to %q, which keeps a raw slash in the id segment", id, uri)
		}
		if _, err := url.Parse(uri); err != nil {
			t.Errorf("%q is not a valid URI: %v", uri, err)
		}
		if got, ok := projectIDFromContextURI(uri); !ok || got != id {
			t.Errorf("projectIDFromContextURI(%q) = (%q, %v), want %q", uri, got, ok, id)
		}
	}
}

func TestUnknownWorkspaceResourcesFailWithInvalidParams(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkUnknownIsInvalidParams)
	srv := resourceServer(t)
	for _, uri := range []string{
		"putnami://workspace/map/endpoints",
		"putnami://workspace/map/all",
		"putnami://workspace/guidance/extra",
		"putnami://projects/%2Fnope/context",
		"putnami://projects//packages/app/context",
		"putnami://projects/%2Fpackages%2Fapp",
		"putnami://workspace/unknown",
	} {
		assertCode(t, "read "+uri, call(t, srv, "resources/read", testURIParams{URI: uri}), codeInvalidParams)
	}
}

// A nil builder leaves its resources out of the list, and a read of the URI
// fails like any unknown URI. The list never offers what a read refuses.
func TestNilBuildersLeaveTheirResourcesOut(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkUnknownIsInvalidParams)
	srv := newFixtureServer(t)
	if uris := listedURIs(t, srv); !reflect.DeepEqual(uris, []string{workspaceContextURI}) {
		t.Errorf("resources/list = %v, want only %s", uris, workspaceContextURI)
	}
	for _, uri := range []string{workspaceGuidanceURI, workspaceMapURI, "putnami://workspace/map/projects", projectContextURI("/packages/app")} {
		assertCode(t, "read "+uri, call(t, srv, "resources/read", testURIParams{URI: uri}), codeInvalidParams)
	}
}

func TestMapResourceReportsBuilderFailureAsAnInternalError(t *testing.T) {
	t.Parallel()
	dir := fixtureWorkspace(t)
	srv := NewServer(Options{
		WorkspaceRoot: dir,
		WorkspaceMap: func(context.Context, string, string) (any, error) {
			return nil, errors.New("no provider view")
		},
	})
	reply := call(t, srv, "resources/read", testURIParams{URI: workspaceMapURI})
	assertCode(t, "read of the map", reply, codeInternalError)
	if reply.Error != nil && !strings.Contains(reply.Error.Message, "no provider view") {
		t.Errorf("error = %q, want the builder message", reply.Error.Message)
	}
}

func TestListingResourcesPreparesNoWorkspaceView(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkListingPreparesNoGraph)
	dir := fixtureWorkspace(t)
	prepared := 0
	srv := NewServer(Options{
		WorkspaceRoot:        dir,
		PrepareWorkspaceView: func(context.Context) error { prepared++; return nil },
		AgentContext:         func(context.Context, string) (any, error) { return struct{}{}, nil },
	})
	if uris := listedURIs(t, srv); len(uris) != 3 {
		t.Fatalf("resources/list = %v, want the context resource and two project contexts", uris)
	}
	if prepared != 0 {
		t.Errorf("resources/list prepared the workspace view %d times, want 0", prepared)
	}
	readResourceText(t, srv, projectContextURI("/packages/app"))
	if prepared != 1 {
		t.Errorf("a project-context read prepared the view %d times, want 1, as the tool does", prepared)
	}
}

func TestInitializeNegotiatesTheProtocolVersion(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-protocol-version", checkNegotiation)
	cases := []struct {
		name      string
		requested string
		want      string
	}{
		{"2024-11-05", "2024-11-05", "2024-11-05"},
		{"2025-03-26", "2025-03-26", "2025-03-26"},
		{"2025-06-18", "2025-06-18", "2025-06-18"},
		{"2025-11-25", "2025-11-25", "2025-11-25"},
		{"newer revision", "2026-07-28", "2025-11-25"},
		{"older revision", "2024-01-01", "2025-11-25"},
		{"not a version", "latest", "2025-11-25"},
		{"missing", "", "2025-11-25"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := struct {
				ProtocolVersion string `json:"protocolVersion,omitempty"`
				ClientInfo      struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"clientInfo"`
			}{ProtocolVersion: tc.requested}
			reply := call(t, newFixtureServer(t), "initialize", params)
			if reply.Error != nil || reply.Result.ProtocolVersion != tc.want {
				t.Errorf("initialize = %+v, want protocolVersion %s", reply, tc.want)
			}
		})
	}
}

func TestDefaultProtocolVersionIsTheNewestSupported(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-protocol-version", checkNegotiation)
	if got := supportedProtocolVersions[len(supportedProtocolVersions)-1]; got != defaultProtocolVersion {
		t.Errorf("newest supported version = %s, want the default %s", got, defaultProtocolVersion)
	}
}

// Claude Code sends server/discover (revision 2026-07-28) first and falls back
// to initialize on "method not found". The server keeps answering that way.
func TestServerDiscoverIsMethodNotFound(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-protocol-version", checkDiscover)
	assertCode(t, "server/discover", call(t, newFixtureServer(t), "server/discover", nil), codeMethodNotFound)
}

// The server is long-lived. A project added to or removed from the workspace
// after the first request changes the list and the read, as it changes the
// agent_context tool, without a change to the recorded index file.
func TestProjectResourcesFollowTheCurrentProjectSet(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-workspace-resources", checkResourcesFollowWorkspace)
	dir := fixtureWorkspace(t)
	srv := NewServer(Options{
		WorkspaceRoot: dir,
		AgentContext: func(_ context.Context, selector string) (any, error) {
			return struct {
				Project string `json:"project"`
			}{Project: selector}, nil
		},
	})
	added := projectContextURI("/packages/extra")
	if uris := listedURIs(t, srv); slices.Contains(uris, added) {
		t.Fatalf("resources/list = %v, want no extra project yet", uris)
	}

	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib","packages/extra"]}`)
	write("packages/extra/putnami.json", `{"name":"extra","type":"library"}`)
	if uris := listedURIs(t, srv); !slices.Contains(uris, added) {
		t.Fatalf("resources/list = %v, want the added project listed", uris)
	}
	if reply := call(t, srv, "resources/read", testURIParams{URI: added}); reply.Error != nil {
		t.Errorf("read of the added project = %+v, want its context", reply)
	}

	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/extra"]}`)
	removed := projectContextURI("/packages/lib")
	if uris := listedURIs(t, srv); slices.Contains(uris, removed) {
		t.Fatalf("resources/list = %v, want the removed project gone", uris)
	}
	assertCode(t, "read of the removed project", call(t, srv, "resources/read", testURIParams{URI: removed}), codeInvalidParams)
}

// The putnamiw relay answers initialize while the CLI builds, then hands the
// replayed initialize to this server. The two lists must agree, or the client
// meets a version the relay never told it. The test reads the relay's list and
// its fallback from the script.
func TestRelayVersionListMatchesTheServer(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-protocol-version", checkRelayListMatchesServer)
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "putnamiw"))
	if err != nil {
		t.Fatal(err)
	}
	const date = `\d{4}-\d{2}-\d{2}`
	listLine := regexp.MustCompile(`(?m)^\s*(` + date + `(?: \| ` + date + `)*)\) version=`).FindSubmatch(script)
	if listLine == nil {
		t.Fatal("putnamiw has no supported-version case line in mcp_relay_while_preparing")
	}
	relay := strings.Split(string(listLine[1]), " | ")
	if !reflect.DeepEqual(relay, supportedProtocolVersions) {
		t.Errorf("putnamiw supports %v, the server supports %v", relay, supportedProtocolVersions)
	}
	fallback := regexp.MustCompile(`(?m)^\s*version="(` + date + `)"$`).FindSubmatch(script)
	if fallback == nil || string(fallback[1]) != defaultProtocolVersion {
		t.Errorf("putnamiw falls back to %q, want %q", fallback, defaultProtocolVersion)
	}
}
