package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const (
	contextFeature                 = "cli/context-mcp-discovery"
	contextRequirement             = "mcp-context-change-notice"
	checkDigestCoversInputs        = "the-digest-covers-each-guidance-input"
	checkNoticeNamesChanges        = "the-notice-names-each-change-and-what-to-read"
	checkNextToolResultReports     = "the-next-tool-result-reports-a-change-once"
	checkNothingChangedNoNotice    = "nothing-changed-adds-no-notice"
	checkResourceReadLeavesNotice  = "a-resource-read-leaves-the-notice-for-the-next-tool-result"
	checkReloadNotifiesBefore      = "an-extension-set-change-reloads-and-notifies-before-the-response"
	checkListsFollowReload         = "the-lists-after-a-reload-follow-the-new-extension-set"
	checkCapabilitiesListChanged   = "the-capabilities-declare-list-changed"
	checkConcurrentReportsOnceSafe = "concurrent-requests-report-a-change-once-without-a-data-race"
	checkFoundProjectJoinsSilently = "a-project-a-load-finds-later-joins-without-a-notice"
	checkIncludesAndBindingReload  = "an-includes-or-collaboration-change-reloads-and-notifies"
	checkEditDuringReloadLoads     = "an-edit-during-a-reload-is-loaded-by-the-next-request"
	checkRemovedToolSaysListAgain  = "a-call-to-a-tool-a-reload-removed-says-to-list-the-tools-again"
	checkNothingWrittenAfterServe  = "nothing-is-written-after-the-session-ends"
)

const contextDecisionsOne = `{
  "protocolVersion": 1,
  "decisions": [
    {"id": "D-001", "statement": "every pull request names what it deletes", "settled": "2026-09-03", "settledBy": "fdumay", "reviewOnly": true}
  ]
}`

const contextDecisionsTwo = `{
  "protocolVersion": 1,
  "decisions": [
    {"id": "D-001", "statement": "every pull request names what it deletes", "settled": "2026-09-03", "settledBy": "fdumay", "reviewOnly": true},
    {"id": "D-002", "statement": "every GitHub artifact is written in English", "settled": "2026-09-11", "settledBy": "fdumay", "reviewOnly": true}
  ]
}`

// contextFixture is a workspace with every kind of guidance input: the three
// root documents, a root and a project decision registry, and one extension
// loaded by path. tool is the command its tools run.
type contextFixture struct {
	dir  string
	tool string
}

func (f contextFixture) write(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pathExtensionManifest declares the @putnami/notes extension with one
// read-only tool per name, each running command.
func pathExtensionManifest(command string, tools ...string) string {
	return namedExtensionManifest("@putnami/notes", command, tools...)
}

// namedExtensionManifest declares an extension with one read-only tool per
// name, each running command.
func namedExtensionManifest(extensionName, command string, tools ...string) string {
	declared := make([]string, 0, len(tools))
	for _, name := range tools {
		declared = append(declared, fmt.Sprintf(`%q:{
    "description":"Answer %s.",
    "inputSchema":{"type":"object","properties":{},"additionalProperties":false},
    "annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},
    "_meta":{"putnami.dev/contract":{"access":"read","readOnly":true,"supportsDryRun":false}},
    "command":%q
  }`, name, name, command))
	}
	return `{
  "name":` + strconv.Quote(extensionName) + `,
  "cliContract": 4,
  "commands":{"` + strings.TrimPrefix(extensionName, "@putnami/") + `-support":{"run":[{"id":"noop","task":"noop"}]}},
  "tasks":{"noop":{"kind":"command","command":"true"}},
  "tools":{` + strings.Join(declared, ",") + `}
}`
}

func newContextFixture(t *testing.T) contextFixture {
	t.Helper()
	f := contextFixture{dir: fixtureWorkspace(t)}
	program := fixtureproc.Write(t, filepath.Join(f.dir, "extensions", "notes", "bin", "notes"), fixtureproc.Program{Stdout: toolResult("notes")})
	f.tool = "{extensionRoot}/bin/" + filepath.Base(program)
	f.write(t, "putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib"],"extensions":["/extensions/notes"]}`)
	f.write(t, "putnami.lock.json", `{"version":5,"extensions":{},"templates":{}}`)
	f.write(t, "putnami.ci.json", `{"version":3}`)
	f.write(t, "decisions.json", contextDecisionsOne)
	f.write(t, "packages/lib/decisions.json", contextDecisionsOne)
	f.write(t, "extensions/notes/putnami.extension.json", pathExtensionManifest(f.tool, "notes.read"))
	f.write(t, "extensions/notes/AI.md", "# Notes\n")
	workspace.InvalidateLoadCache(f.dir)
	recordWorkspaceIndex(t, f.dir)
	return f
}

// server builds a server over the fixture with the guidance builder injected
// and the reload closure the CLI shell injects: the configuration read again.
func (f contextFixture) server(t *testing.T, reloads *int) *Server {
	t.Helper()
	return NewServer(Options{
		WorkspaceRoot:     f.dir,
		Config:            wsproto.Load(f.dir),
		ServerVersion:     "test",
		WorkspaceGuidance: func(context.Context) string { return testGuidanceBody },
		AgentContext: func(_ context.Context, selector string) (any, error) {
			return map[string]string{"project": selector}, nil
		},
		ReloadExtensions: func(context.Context) ExtensionSelection {
			if reloads != nil {
				*reloads++
			}
			return ExtensionSelection{Config: wsproto.Load(f.dir)}
		},
	})
}

// sessionLine is one line a session writes: a notification names its method,
// and a response carries its id and the parts of a result these tests read.
type sessionLine struct {
	Method string `json:"method"`
	ID     *int   `json:"id"`
	Result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Contents []struct {
			Text string `json:"text"`
		} `json:"contents"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Resources []struct {
			URI string `json:"uri"`
		} `json:"resources"`
		Capabilities serverCaps `json:"capabilities"`
		IsError      bool       `json:"isError"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

// contextSession runs one Serve loop over the requests and returns every line
// it writes, notifications included, in order.
func contextSession(t *testing.T, srv *Server, requests ...any) []sessionLine {
	t.Helper()
	var in bytes.Buffer
	for _, request := range requests {
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		in.Write(body)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), &in, &out); err != nil {
		t.Fatal(err)
	}
	var lines []sessionLine
	for _, raw := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var line sessionLine
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// toolCallTexts returns the text blocks of the tools/call response with id.
func toolCallTexts(t *testing.T, lines []sessionLine, id int) []string {
	t.Helper()
	for _, line := range lines {
		if line.ID == nil || *line.ID != id {
			continue
		}
		if line.Error != nil {
			t.Fatalf("response %d is an error: %s", id, line.Error.Message)
		}
		texts := make([]string, 0, len(line.Result.Content))
		for _, block := range line.Result.Content {
			texts = append(texts, block.Text)
		}
		return texts
	}
	t.Fatalf("no response with id %d in %+v", id, lines)
	return nil
}

func listProjectsCall(id int) any {
	return req(id, "tools/call", testToolCall{Name: "list_projects", Arguments: map[string]string{}})
}

func methodsOf(lines []sessionLine) []string {
	methods := make([]string, 0, len(lines))
	for _, line := range lines {
		switch {
		case line.Method != "":
			methods = append(methods, line.Method)
		case line.ID != nil:
			methods = append(methods, fmt.Sprintf("response %d", *line.ID))
		default:
			methods = append(methods, "response without id")
		}
	}
	return methods
}

// Each kind of guidance input changes the digest, and a file that is not an
// input does not.
func TestContextDigestCoversEachGuidanceInput(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkDigestCoversInputs)
	f := newContextFixture(t)
	srv := f.server(t, nil)

	previous := srv.readContextInputs().digest
	for _, edit := range []struct{ name, rel, content string }{
		{"workspace configuration", "putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib"],"extensions":["/extensions/notes"],"quiet":true}`},
		{"lock", "putnami.lock.json", `{"version":5,"extensions":{"@putnami/go":{"version":"0.3.1"}},"templates":{}}`},
		{"CI document", "putnami.ci.json", `{"version":3,"commands":[]}`},
		{"root decisions", "decisions.json", contextDecisionsTwo},
		{"project decisions", "packages/lib/decisions.json", contextDecisionsTwo},
		{"path extension manifest", "extensions/notes/putnami.extension.json", pathExtensionManifest(f.tool, "notes.read", "notes.write")},
		{"path extension guide", "extensions/notes/AI.md", "# Notes\n\nNew rule.\n"},
	} {
		f.write(t, edit.rel, edit.content)
		current := srv.readContextInputs().digest
		if current == previous {
			t.Errorf("a change to the %s (%s) left the digest unchanged", edit.name, edit.rel)
		}
		previous = current
	}

	f.write(t, "packages/app/README.md", "# App\n")
	if srv.readContextInputs().digest != previous {
		t.Error("a change to a file that is no guidance input changed the digest")
	}
}

// The notice names each change in plain words, groups the changes by what to
// read, and points at a listed resource, a file, or nothing.
func TestContextNoticeNamesEachChangeAndWhatToRead(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkNoticeNamesChanges)
	snapshot := func(files map[string]contextInput) contextSnapshot {
		for key, input := range files {
			if input.data != nil {
				input.sum = sha256.Sum256(input.data)
			}
			files[key] = input
		}
		return contextSnapshot{files: files, digest: snapshotDigest(files)}
	}
	retained := func(kind contextInputKind, owner, content string) contextInput {
		return contextInput{kind: kind, owner: owner, data: []byte(content)}
	}
	hashed := func(kind contextInputKind, owner, content string) contextInput {
		return contextInput{kind: kind, owner: owner, sum: sha256.Sum256([]byte(content))}
	}
	from := snapshot(map[string]contextInput{
		"putnami.workspace.json":                  retained(inputWorkspaceConfig, "", `{"extensions":["/extensions/notes","@putnami/go"]}`),
		"putnami.lock.json":                       retained(inputLock, "", `{"extensions":{"@putnami/go":{"version":"0.3.0"},"@putnami/old":{"version":"1.0.0"}}}`),
		"putnami.ci.json":                         hashed(inputCI, "", "v1"),
		"decisions.json":                          retained(inputDecisions, "", contextDecisionsOne),
		"packages/lib/decisions.json":             retained(inputDecisions, "/packages/lib", contextDecisionsTwo),
		"extensions/notes/putnami.extension.json": hashed(inputExtensionManifest, "extensions/notes", "m1"),
		"extensions/notes/AI.md":                  hashed(inputExtensionGuide, "extensions/notes", "g1"),
	})
	to := snapshot(map[string]contextInput{
		"putnami.workspace.json":                  retained(inputWorkspaceConfig, "", `{"extensions":["/extensions/notes","@putnami/go"],"disable":{"extensions":["@putnami/old"]}}`),
		"putnami.lock.json":                       retained(inputLock, "", `{"extensions":{"@putnami/go":{"version":"0.3.1"}}}`),
		"putnami.ci.json":                         hashed(inputCI, "", "v2"),
		"decisions.json":                          retained(inputDecisions, "", contextDecisionsTwo),
		"packages/lib/decisions.json":             retained(inputDecisions, "/packages/lib", contextDecisionsOne),
		"extensions/notes/putnami.extension.json": hashed(inputExtensionManifest, "extensions/notes", "m2"),
		"extensions/notes/AI.md":                  hashed(inputExtensionGuide, "extensions/notes", "g2"),
	})
	goGuide := extensionGuidanceURI("@putnami/go", "0.3.1")
	targets := noticeTargets{guidance: true, projectContext: true, reload: true, guides: map[string]bool{goGuide: true}}
	got := renderContextNotice(describeContextChanges(from, to, targets), reloadedBetween(from, to, targets))
	want := strings.Join([]string{
		"Putnami notice: the workspace context changed.",
		"- D-002 added; putnami.ci.json changed. Read putnami://workspace/guidance",
		"- D-002 removed in packages/lib/decisions.json. Read putnami://projects/%2Fpackages%2Flib/context",
		"- @putnami/go 0.3.1 installed, replacing 0.3.0. Read putnami://extension-guidance/AI.md?name=%40putnami%2Fgo&version=0.3.1",
		"- @putnami/old removed; @putnami/old disabled.",
		"- extensions/notes/putnami.extension.json changed. Read extensions/notes/putnami.extension.json",
		"- extensions/notes/AI.md changed. Read extensions/notes/AI.md",
		"- Putnami reloaded its extension tools and guides; list the tools again.",
	}, "\n")
	if got != want {
		t.Fatalf("notice =\n%s\nwant\n%s", got, want)
	}

	// Without the guidance and agent-context builders, the notice points at
	// the files, because the server lists no resource for them.
	bare := renderContextNotice(describeContextChanges(from, to, noticeTargets{}), false)
	for _, line := range []string{
		"- D-002 added. Read decisions.json",
		"- putnami.ci.json changed. Read putnami.ci.json",
		"- D-002 removed in packages/lib/decisions.json. Read packages/lib/decisions.json",
	} {
		if !strings.Contains(bare, line+"\n") {
			t.Errorf("notice without builders lacks %q:\n%s", line, bare)
		}
	}
	if strings.Contains(bare, "reloaded") {
		t.Errorf("a server that cannot reload claims a reload:\n%s", bare)
	}

	// A change no listed resource shows points at its file: a change the
	// notice cannot name in detail, a version constraint, and an extension
	// the lock installs without a listed guide, such as a dependency of a
	// declared extension.
	only := func(key string, input contextInput) contextSnapshot {
		return snapshot(map[string]contextInput{key: input})
	}
	for _, tc := range []struct {
		from, to contextSnapshot
		want     string
	}{
		{only("putnami.workspace.json", retained(inputWorkspaceConfig, "", `{"quiet":true}`)), only("putnami.workspace.json", retained(inputWorkspaceConfig, "", `{"quiet":false}`)),
			"- putnami.workspace.json changed. Read putnami.workspace.json"},
		{only("putnami.workspace.json", retained(inputWorkspaceConfig, "", `{"extensions":{"@putnami/go":"^0.3"}}`)), only("putnami.workspace.json", retained(inputWorkspaceConfig, "", `{"extensions":{"@putnami/go":"^0.4"}}`)),
			"- the @putnami/go version constraint changed. Read putnami.workspace.json"},
		{only("putnami.lock.json", retained(inputLock, "", `{"cli":{"version":"1"}}`)), only("putnami.lock.json", retained(inputLock, "", `{"cli":{"version":"2"}}`)),
			"- putnami.lock.json changed. Read putnami.lock.json"},
		{only("decisions.json", retained(inputDecisions, "", contextDecisionsOne)), only("decisions.json", retained(inputDecisions, "", "{not json")),
			"- decisions.json changed. Read putnami://workspace/guidance"},
		{snapshot(map[string]contextInput{}), only("putnami.lock.json", retained(inputLock, "", `{"extensions":{"@putnami/go":{"version":"0.3.1"}}}`)),
			"- @putnami/go 0.3.1 installed. Read " + goGuide},
		{snapshot(map[string]contextInput{}), only("putnami.lock.json", retained(inputLock, "", `{"extensions":{"@putnami/dev":{"version":"0.3.1"}}}`)),
			"- @putnami/dev 0.3.1 installed. Read putnami.lock.json"},
	} {
		notice := renderContextNotice(describeContextChanges(tc.from, tc.to, targets), false)
		if !strings.HasSuffix(notice, "\n"+tc.want) {
			t.Errorf("notice =\n%s\nwant it to end with %q", notice, tc.want)
		}
	}

	// A line names at most eight changes, then counts the rest.
	var many []contextChange
	for i := 1; i <= 11; i++ {
		many = append(many, contextChange{text: fmt.Sprintf("D-%03d added", i), read: workspaceGuidanceURI})
	}
	capped := renderContextNotice(many, false)
	if want := "\n- D-001 added; D-002 added; D-003 added; D-004 added; D-005 added; D-006 added; D-007 added; D-008 added; 3 more. Read putnami://workspace/guidance"; !strings.HasSuffix(capped, want) {
		t.Errorf("notice =\n%s\nwant it to end with %q", capped, want)
	}
}

// A change reaches the next tool result as one last text block, after the
// tool's own content, and the tool result after that carries none.
func TestContextChangeReachesTheNextToolResultOnce(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkNextToolResultReports)
	f := newContextFixture(t)
	srv := f.server(t, nil)

	f.write(t, "decisions.json", contextDecisionsTwo)
	lines := contextSession(t, srv, listProjectsCall(1), listProjectsCall(2))
	if got := methodsOf(lines); strings.Join(got, ",") != "response 1,response 2" {
		t.Fatalf("lines = %v, want two responses and no notification: no extension changed", got)
	}
	first := toolCallTexts(t, lines, 1)
	if len(first) != 2 {
		t.Fatalf("first tool result has %d blocks, want the tool's own and the notice: %q", len(first), first)
	}
	if !json.Valid([]byte(first[0])) {
		t.Errorf("the tool's own content is not first: %q", first[0])
	}
	want := "Putnami notice: the workspace context changed.\n- D-002 added. Read putnami://workspace/guidance"
	if first[1] != want {
		t.Errorf("notice = %q, want %q", first[1], want)
	}
	if second := toolCallTexts(t, lines, 2); len(second) != 1 {
		t.Errorf("the second tool result repeats the notice: %q", second)
	}

	// A later change is reported once too, in a later session of the same
	// server.
	f.write(t, "putnami.ci.json", `{"version":3,"commands":[]}`)
	later := contextSession(t, srv, listProjectsCall(3), listProjectsCall(4))
	if texts := toolCallTexts(t, later, 3); len(texts) != 2 || !strings.Contains(texts[1], "- putnami.ci.json changed. Read putnami://workspace/guidance") {
		t.Errorf("the change to putnami.ci.json is not reported: %q", texts)
	}
	if texts := toolCallTexts(t, later, 4); len(texts) != 1 {
		t.Errorf("the change to putnami.ci.json is reported twice: %q", texts)
	}
}

// With no change to an input, a tool result carries the tool's content only.
func TestNothingChangedAddsNoNotice(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkNothingChangedNoNotice)
	f := newContextFixture(t)
	srv := f.server(t, nil)

	lines := contextSession(t, srv, listProjectsCall(1))
	f.write(t, "packages/app/README.md", "# App\n")
	lines = append(lines, contextSession(t, srv, listProjectsCall(2))...)
	for _, id := range []int{1, 2} {
		if texts := toolCallTexts(t, lines, id); len(texts) != 1 {
			t.Errorf("tool result %d carries a notice with nothing changed: %q", id, texts)
		}
	}
	if got := methodsOf(lines); strings.Join(got, ",") != "response 1,response 2" {
		t.Errorf("lines = %v, want two responses and no notification", got)
	}
}

// A resources/read notices a change but carries no notice; the next tool
// result does.
func TestAResourceReadLeavesTheNoticeForTheNextToolResult(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkResourceReadLeavesNotice)
	f := newContextFixture(t)
	srv := f.server(t, nil)

	f.write(t, "putnami.ci.json", `{"version":3,"commands":[]}`)
	lines := contextSession(t, srv,
		req(1, "resources/read", testURIParams{URI: workspaceGuidanceURI}),
		listProjectsCall(2),
		listProjectsCall(3))
	if read := lines[0].Result.Contents; len(read) != 1 || read[0].Text != testGuidanceBody {
		t.Fatalf("resources/read = %+v, want the resource alone", lines[0])
	}
	if texts := toolCallTexts(t, lines, 2); len(texts) != 2 || !strings.HasSuffix(texts[1], "- putnami.ci.json changed. Read putnami://workspace/guidance") {
		t.Errorf("the tool result after the read does not report the change: %q", texts)
	}
	if texts := toolCallTexts(t, lines, 3); len(texts) != 1 {
		t.Errorf("the change is reported twice: %q", texts)
	}
}

// A changed extension set reloads the selection and the tools, then writes
// tools/list_changed and resources/list_changed, in that order, before the
// response of the request that noticed it. The response is written once.
func TestAnExtensionSetChangeReloadsAndNotifiesBeforeTheResponse(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkReloadNotifiesBefore)
	f := newContextFixture(t)
	reloads := 0
	srv := f.server(t, &reloads)

	f.write(t, "extensions/notes/putnami.extension.json", pathExtensionManifest(f.tool, "notes.read", "notes.write"))
	lines := contextSession(t, srv, listProjectsCall(1), listProjectsCall(2))
	want := []string{notificationToolsListChanged, notificationResourcesListChanged, "response 1", "response 2"}
	if got := methodsOf(lines); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	if reloads != 1 {
		t.Errorf("reloads = %d, want 1", reloads)
	}
	texts := toolCallTexts(t, lines, 1)
	if len(texts) != 2 {
		t.Fatalf("the tool result has %d blocks, want 2: %q", len(texts), texts)
	}
	for _, line := range []string{
		"- extensions/notes/putnami.extension.json changed. Read extensions/notes/putnami.extension.json",
		"- Putnami reloaded its extension tools and guides; list the tools again.",
	} {
		if !strings.Contains(texts[1], line) {
			t.Errorf("notice lacks %q:\n%s", line, texts[1])
		}
	}

	// A change that leaves the extension set alone reloads nothing.
	f.write(t, "extensions/notes/AI.md", "# Notes\n\nNew rule.\n")
	lines = contextSession(t, srv, listProjectsCall(3))
	if got := methodsOf(lines); strings.Join(got, ",") != "response 3" {
		t.Errorf("lines = %v, want the response alone", got)
	}
	if reloads != 1 {
		t.Errorf("a guide change reloaded the extensions: reloads = %d", reloads)
	}
}

// After a reload, tools/list and resources/list follow the new extension set:
// a tool a manifest adds is listed and callable, a tool of an extension the
// workspace drops is gone, and the guide of a newly locked extension is
// listed.
func TestTheListsAfterAReloadFollowTheNewExtensionSet(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkListsFollowReload)
	f := newContextFixture(t)
	guides := []ExtensionGuidance(nil)
	srv := NewServer(Options{
		WorkspaceRoot: f.dir,
		Config:        wsproto.Load(f.dir),
		ServerVersion: "test",
		ReloadExtensions: func(context.Context) ExtensionSelection {
			return ExtensionSelection{Config: wsproto.Load(f.dir), ExtensionGuidance: guides}
		},
	})
	registered := func() map[string]bool {
		names := make(map[string]bool)
		for _, tool := range srv.toolDefs() {
			names[tool.Name] = true
		}
		return names
	}
	if names := registered(); !names["notes.read"] || names["notes.write"] {
		t.Fatalf("tools at start = %v, want notes.read alone", names)
	}

	f.write(t, "extensions/notes/putnami.extension.json", pathExtensionManifest(f.tool, "notes.read", "notes.write"))
	lines := contextSession(t, srv,
		req(1, "resources/read", testURIParams{URI: workspaceContextURI}),
		req(2, "tools/list", nil),
		req(3, "tools/call", testToolCall{Name: "notes.write", Arguments: map[string]string{}}))
	if names := registered(); !names["notes.read"] || !names["notes.write"] {
		t.Errorf("tools after the manifest change = %v, want notes.read and notes.write", names)
	}
	listed := false
	for _, tool := range lines[len(lines)-2].Result.Tools {
		listed = listed || tool.Name == "notes.write"
	}
	if !listed {
		t.Error("tools/list after the reload does not list notes.write")
	}
	if texts := toolCallTexts(t, lines, 3); len(texts) == 0 || texts[0] != "notes" {
		t.Errorf("notes.write = %q, want the tool's answer first", texts)
	}

	guides = []ExtensionGuidance{{Name: "@putnami/go", Version: "0.3.1", Content: "# Go\n"}}
	f.write(t, "putnami.lock.json", `{"version":5,"extensions":{"@putnami/go":{"version":"0.3.1"}},"templates":{}}`)
	f.write(t, "putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib"],"extensions":["@putnami/go"]}`)
	lines = contextSession(t, srv, listProjectsCall(4), req(5, "resources/list", nil))
	if names := registered(); names["notes.read"] || names["notes.write"] {
		t.Errorf("tools after the workspace dropped the extension = %v, want no notes tool", names)
	}
	guideURI := extensionGuidanceURI("@putnami/go", "0.3.1")
	found := false
	for _, resource := range lines[len(lines)-1].Result.Resources {
		found = found || resource.URI == guideURI
	}
	if !found {
		t.Errorf("resources/list after the reload does not list %s", guideURI)
	}
	texts := toolCallTexts(t, lines, 4)
	if len(texts) != 2 || !strings.Contains(texts[1], "- @putnami/go 0.3.1 installed. Read "+guideURI) {
		t.Errorf("the notice does not point at the new guide: %q", texts)
	}
}

// initialize declares that the server sends both list-changed notifications.
func TestTheCapabilitiesDeclareListChanged(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkCapabilitiesListChanged)
	lines := contextSession(t, newFixtureServer(t), req(1, "initialize", initializeParams{ProtocolVersion: "2025-11-25"}))
	capabilities := lines[0].Result.Capabilities
	if capabilities.Tools == nil || capabilities.Resources == nil {
		t.Fatalf("capabilities = %+v, want tools and resources", capabilities)
	}
	if !capabilities.Tools.ListChanged || !capabilities.Resources.ListChanged {
		t.Errorf("capabilities = %+v %+v, want tools.listChanged and resources.listChanged true", *capabilities.Tools, *capabilities.Resources)
	}
	if capabilities.Resources.Subscribe {
		t.Error("resources.subscribe = true, want false: the server serves no subscription")
	}
}

// Requests that check the context at the same time report one change once,
// and a reload that runs beside readers of the tools and guides races with
// none of them. The race detector proves the second half when the run enables
// it.
func TestConcurrentRequestsReportAChangeOnceWithoutADataRace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkConcurrentReportsOnceSafe)
	f := newContextFixture(t)
	reloads := 0
	srv := f.server(t, &reloads)

	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				_ = srv.toolDefs()
				_, _, _ = srv.lookupTool("notes.read")
				_ = srv.guidanceResources()
				_ = srv.initializeInstructions()
				_ = srv.currentSelection()
			}
		}()
	}

	for round := range 3 {
		tools := []string{"notes.read"}
		for extra := range round + 1 {
			tools = append(tools, fmt.Sprintf("notes.extra%d", extra))
		}
		f.write(t, "extensions/notes/putnami.extension.json", pathExtensionManifest(f.tool, tools...))
		var requests sync.WaitGroup
		notices := make(chan int, 8)
		for id := range 8 {
			requests.Add(1)
			go func() {
				defer requests.Done()
				resp := srv.withNotice(srv.ok(json.RawMessage(fmt.Sprint(id)), callToolResult{Content: []toolContent{{Type: "text", Text: "{}"}}}), srv.observeContext(context.Background(), true))
				if result := resp.Result.(callToolResult); len(result.Content) == 2 {
					notices <- id
				}
			}()
		}
		requests.Wait()
		close(notices)
		if count := len(notices); count != 1 {
			t.Errorf("round %d: %d tool results carry the notice, want 1", round, count)
		}
	}
	close(done)
	readers.Wait()

	if _, ok, _ := srv.lookupTool("notes.extra2"); !ok {
		t.Error("the last manifest's tools are not registered")
	}
	if reloads != 3 {
		t.Errorf("reloads = %d, want one per changed manifest (3)", reloads)
	}
}

// A project the session sees for the first time brings its decisions.json as
// it is. Here the workspace does not load when the server starts, because two
// projects share a name, so the start tracks no project registry. Once the
// name is fixed, a tool call loads the workspace and finds the registry. No
// tool result reports its decisions as added, and a later edit to it is
// reported.
func TestAProjectAWorkspaceLoadFindsLaterJoinsWithoutANotice(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkFoundProjectJoinsSilently)
	f := newContextFixture(t)
	const appProject = `{"name":"app","type":"application","dependencies":["lib"]}`
	f.write(t, "packages/app/putnami.json", `{"name":"lib","type":"application"}`)
	workspace.InvalidateLoadCache(f.dir)
	if _, err := workspace.Load(f.dir); err == nil {
		t.Fatal("the workspace loads with two projects named lib")
	}
	srv := f.server(t, nil)

	f.write(t, "packages/app/putnami.json", appProject)
	workspace.InvalidateLoadCache(f.dir)
	lines := contextSession(t, srv, listProjectsCall(1), listProjectsCall(2))
	for _, id := range []int{1, 2} {
		if texts := toolCallTexts(t, lines, id); len(texts) != 1 {
			t.Errorf("tool result %d reports the registry of a project the load found as a change: %q", id, texts)
		}
	}

	f.write(t, "packages/lib/decisions.json", contextDecisionsTwo)
	lines = contextSession(t, srv, listProjectsCall(3))
	if texts := toolCallTexts(t, lines, 3); len(texts) != 2 || !strings.Contains(texts[1], "- D-002 added in packages/lib/decisions.json.") {
		t.Errorf("the edit to packages/lib/decisions.json is not reported: %q", texts)
	}
}

// Only the registry of a project the session sees for the first time joins
// the baseline without a notice. A decisions.json that a known project gains
// is a change: the workspace load of one tool call records it, and the next
// tool result reports it.
func TestADecisionRegistryAKnownProjectGainsIsReported(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkFoundProjectJoinsSilently)
	f := newContextFixture(t)
	srv := f.server(t, nil)

	f.write(t, "packages/app/decisions.json", contextDecisionsOne)
	lines := contextSession(t, srv, listProjectsCall(1), listProjectsCall(2))
	if texts := toolCallTexts(t, lines, 1); len(texts) != 1 {
		t.Errorf("the first tool result reports a registry no load has found yet: %q", texts)
	}
	if texts := toolCallTexts(t, lines, 2); len(texts) != 2 || !strings.Contains(texts[1], "- D-001 added in packages/app/decisions.json. Read putnami://projects/") {
		t.Errorf("the decisions.json packages/app gained is not reported: %q", texts)
	}
}

// A change to the includes or to options.collaboration changes the tools
// without changing an extension entry, so it reloads too: an included project
// that holds an extension manifest loads as an extension, and a collaboration
// binding registers the routed tools. The reload loads the workspace again,
// so it sees the new project set.
func TestAnIncludesOrCollaborationChangeReloadsAndNotifies(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkIncludesAndBindingReload)
	f := newContextFixture(t)
	reloads := 0
	srv := f.server(t, &reloads)
	listed := func(lines []sessionLine, name string) bool {
		for _, tool := range lines[len(lines)-1].Result.Tools {
			if tool.Name == name {
				return true
			}
		}
		return false
	}
	want := strings.Join([]string{notificationToolsListChanged, notificationResourcesListChanged, "response 1", "response 2"}, ",")

	drafts := fixtureproc.Write(t, filepath.Join(f.dir, "tools", "drafts", "bin", "drafts"), fixtureproc.Program{Stdout: toolResult("drafts")})
	f.write(t, "tools/drafts/putnami.json", `{"name":"drafts","type":"library"}`)
	f.write(t, "tools/drafts/putnami.extension.json", namedExtensionManifest("@putnami/drafts", "{extensionRoot}/bin/"+filepath.Base(drafts), "drafts.read"))
	f.write(t, "putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib","tools/drafts"],"extensions":["/extensions/notes"]}`)
	lines := contextSession(t, srv, listProjectsCall(1), req(2, "tools/list", nil))
	if got := strings.Join(methodsOf(lines), ","); got != want {
		t.Errorf("after an includes change, lines = %s, want %s", got, want)
	}
	if reloads != 1 || !listed(lines, "drafts.read") {
		t.Errorf("after an includes change, reloads = %d and drafts.read listed = %v, want 1 and true", reloads, listed(lines, "drafts.read"))
	}

	f.write(t, "putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib","tools/drafts"],"extensions":["/extensions/notes"],"options":{"collaboration":{"tasks":{"provider":"@acme/github","version":1}}}}`)
	lines = contextSession(t, srv, listProjectsCall(1), req(2, "tools/list", nil))
	if got := strings.Join(methodsOf(lines), ","); got != want {
		t.Errorf("after a collaboration binding change, lines = %s, want %s", got, want)
	}
	if reloads != 2 || !listed(lines, "tasks.capabilities") {
		t.Errorf("after a collaboration binding change, reloads = %d and tasks.capabilities listed = %v, want 2 and true", reloads, listed(lines, "tasks.capabilities"))
	}
}

// The tools count as built from the inputs as they were before the reload
// read them. An edit that lands while a reload runs is therefore loaded by
// the next request, not taken as already loaded.
func TestAnEditDuringAReloadIsLoadedByTheNextRequest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkEditDuringReloadLoads)
	f := newContextFixture(t)
	reloads := 0
	srv := NewServer(Options{
		WorkspaceRoot: f.dir,
		Config:        wsproto.Load(f.dir),
		ServerVersion: "test",
		ReloadExtensions: func(context.Context) ExtensionSelection {
			reloads++
			selection := ExtensionSelection{Config: wsproto.Load(f.dir)}
			if reloads == 1 {
				// The workspace disables the extension after the reload read
				// the configuration, so the first reload still registers its
				// tools.
				f.write(t, "putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib"],"extensions":["/extensions/notes"],"disable":{"extensions":["@putnami/notes"]}}`)
			}
			return selection
		},
	})

	f.write(t, "extensions/notes/putnami.extension.json", pathExtensionManifest(f.tool, "notes.read", "notes.write"))
	lines := contextSession(t, srv, listProjectsCall(1), listProjectsCall(2))
	if reloads != 2 {
		t.Fatalf("reloads = %d, want 2: the second request loads the edit the first reload missed", reloads)
	}
	if _, ok, _ := srv.lookupTool("notes.read"); ok {
		t.Error("the tools of the extension the workspace disabled are still registered")
	}
	want := []string{notificationToolsListChanged, notificationResourcesListChanged, "response 1", notificationToolsListChanged, notificationResourcesListChanged, "response 2"}
	if got := methodsOf(lines); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("lines = %v, want %v", got, want)
	}
}

// A call to a tool that a reload in this session removed gets a tool result
// that says so and carries the notice, because a JSON-RPC error reaches no
// model. A name the server never registered stays an invalid-params error.
func TestACallToAToolAReloadRemovedSaysToListTheToolsAgain(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkRemovedToolSaysListAgain)
	f := newContextFixture(t)
	srv := f.server(t, nil)

	f.write(t, "putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib"]}`)
	lines := contextSession(t, srv,
		req(1, "tools/call", testToolCall{Name: "notes.read", Arguments: map[string]string{}}),
		req(2, "tools/call", testToolCall{Name: "notes.never", Arguments: map[string]string{}}))
	var removed, unknown sessionLine
	for _, line := range lines {
		switch {
		case line.ID != nil && *line.ID == 1:
			removed = line
		case line.ID != nil && *line.ID == 2:
			unknown = line
		}
	}
	if removed.Error != nil || !removed.Result.IsError || len(removed.Result.Content) != 2 {
		t.Fatalf("call to the removed tool = %+v, want an error tool result with the notice", removed)
	}
	if text := removed.Result.Content[0].Text; text != "The tool notes.read was removed when Putnami reloaded its extensions. List the tools again." {
		t.Errorf("removed tool text = %q", text)
	}
	if notice := removed.Result.Content[1].Text; !strings.Contains(notice, "- extension extensions/notes no longer loads.") {
		t.Errorf("the removed tool's result carries no notice of the change:\n%s", notice)
	}
	if unknown.Error == nil || unknown.Error.Code != codeInvalidParams {
		t.Errorf("call to a tool never registered = %+v, want an invalid-params error", unknown)
	}
}

// Once Serve returns, the server writes nothing, even when a later context
// check reloads.
func TestNothingIsWrittenAfterTheSessionEnds(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, contextFeature, contextRequirement, checkNothingWrittenAfterServe)
	f := newContextFixture(t)
	reloads := 0
	srv := f.server(t, &reloads)
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}

	f.write(t, "extensions/notes/putnami.extension.json", pathExtensionManifest(f.tool, "notes.read", "notes.write"))
	srv.observeContext(context.Background(), true)
	if reloads != 1 {
		t.Fatalf("reloads = %d, want 1", reloads)
	}
	if out.Len() != 0 {
		t.Errorf("the server wrote after its session ended: %q", out.String())
	}
}

// BenchmarkContextInputs measures the per-request digest over inputs the size
// of this repository's: three root documents, a root and a project decision
// registry, and twelve path extensions with a manifest and an AI.md each.
func BenchmarkContextInputs(b *testing.B) {
	dir := b.TempDir()
	write := func(rel string, size int) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	write("putnami.workspace.json", 4500)
	write("putnami.lock.json", 6200)
	write("putnami.ci.json", 1400)
	write("decisions.json", 6800)
	write("tooling/cli/decisions.json", 3000)
	srv := &Server{opts: Options{WorkspaceRoot: dir}}
	srv.changes.decisionFiles = []decisionFile{{path: "tooling/cli/decisions.json", projectID: "/tooling/cli"}}
	for i := range 12 {
		label := fmt.Sprintf("extensions/e%d", i)
		write(label+"/putnami.extension.json", 4000)
		write(label+"/AI.md", 10000)
		srv.changes.pathExtensions = append(srv.changes.pathExtensions, pathExtension{label: label, dir: filepath.Join(dir, filepath.FromSlash(label))})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if snapshot := srv.readContextInputs(); len(snapshot.files) != 29 {
			b.Fatalf("read %d inputs, want 29", len(snapshot.files))
		}
	}
}
