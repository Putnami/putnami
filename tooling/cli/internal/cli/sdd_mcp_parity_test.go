package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The extraction acceptance: for each of the four SDD
// MCP tools, `sdd.<name>` must return the bytes core's `<name>` returned, except
// `sdd.list_features`, whose recordings hold its own bounded page (below).
//
// # The oracle is recorded, because the oracle is gone
//
// Same story as sdd_extraction_parity_test.go beside it, and the same
// disposition. Until Task 8 this file called core's tool and the extension's
// tool on one server and compared the two answers; Task 8 deletes core's four,
// so the last thing they did was answer the seventeen calls it then had, and their
// answers were written into testdata/sdd-parity/mcp/ in the SAME commit that
// deleted them. What is asserted did not change; where the expected bytes live
// did.
//
// There is deliberately no -update flag: nothing but the implementation under
// test could rewrite these files, and a self-regenerated oracle is not one.
//
// The `list_features` recordings, including the `limit` and `cursor` cases,
// hold the tool's own contract rather than a core answer: one bounded page of
// short entries, as tooling/sdd-extension/doc/04-mcp-tools.md documents. They
// change only with that contract.
//
// # The extension side goes through the real server
//
// One mcp.Server, built by newMCPServer — production's wiring, not a copy. The
// extension's four tools are registered by the REAL discovery path, because the
// fixture workspace declares the extension and the shipped manifest is what
// discovery reads. So the comparison covers the whole chain an agent triggers:
// discovery, the extension-tool validator, request construction (including the
// workspace view on the wire), the subprocess, and the result decode.
//
// One thing is a stand-in, and only one: the tools' `command`. The shipped
// manifest names {extensionRuntime}, whose resolution compiles the extension
// into the machine's artifact store; the fixture replaces it with a binary this
// test compiles, exactly as the interactive parity test does. That the real
// {extensionRuntime} path resolves is asserted separately, in internal/mcp.

// mcpParityCase is one tool invocation, compared against its recorded answer.
type mcpParityCase struct {
	// name identifies the subtest AND, through paritySlug, the file holding the
	// recorded answer. core is the name the built-in tool had; the extension's
	// name is core's under the D4 rename table.
	name string
	core string
	// arguments is the tools/call arguments object as a client sends it: JSON
	// text, verbatim for both sides. Written as the wire rather than as a Go map
	// because that is what it is — an agent harness sends an object, and a
	// literal keeps the two calls provably identical.
	arguments string
	// wantError states whether the recorded answer reports a tool error, so a
	// case that stopped exercising its failure path is visible instead of
	// passing because nothing failed.
	wantError bool
}

// sddMCPToolRename is D4's table: core's (now removed) name → the extension's.
var sddMCPToolRename = map[string]string{
	"list_features":   "sdd.list_features",
	"feature_context": "sdd.feature_context",
	"list_specs":      "sdd.list_specs",
	"spec_context":    "sdd.spec_context",
}

// TestSDDMCPToolsMatchTheRecordedCoreTools is the acceptance: same fixture
// workspace, same arguments, the same content blocks and isError verdict the
// core tools produced.
func TestSDDMCPToolsMatchTheRecordedCoreTools(t *testing.T) {
	t.Parallel()
	limitParityConcurrency(t)
	root := mcpParityWorkspace(t)
	srv := newMCPServer(root, wsproto.Load(root), "test")

	for _, tool := range mcpParityCases() {
		t.Run(tool.name, func(t *testing.T) {
			blocks, isError := callParityTool(t, srv, sddMCPToolRename[tool.core], tool.arguments)

			recorded := readRecordedMCPAnswer(t, paritySlug(tool.name))
			if recorded.isError != tool.wantError {
				t.Errorf("the recorded answer reports isError=%v, but this case claims %v — the table and the recording disagree",
					recorded.isError, tool.wantError)
			}
			if isError != recorded.isError {
				t.Errorf("isError: extension %v, recorded core %v", isError, recorded.isError)
			}
			if len(blocks) != len(recorded.blocks) {
				t.Fatalf("content blocks: extension %d, recorded core %d\n--- recorded ---\n%s\n--- extension ---\n%s",
					len(blocks), len(recorded.blocks), strings.Join(recorded.blocks, "\n"), strings.Join(blocks, "\n"))
			}
			for i := range recorded.blocks {
				if got := normalizeParityBytes(blocks[i], root); got != recorded.blocks[i] {
					t.Errorf("content block %d differs from the recorded core answer.\n--- recorded ---\n%s\n--- extension ---\n%s",
						i, recorded.blocks[i], got)
				}
			}
		})
	}
}

// recordedMCPAnswer is one frozen tool call: its verdict and its content
// blocks, each block ending in the newline the recording frames it with.
type recordedMCPAnswer struct {
	isError bool
	blocks  []string
}

// recordedMCPBlockMarker opens one content block on a line of its own. The
// recording added exactly one newline after each block's bytes — never a
// conditional one — so stripping exactly one here round-trips a block that ends
// in a newline and one that does not, without a length header.
const recordedMCPBlockMarker = "--- block "

func readRecordedMCPAnswer(t *testing.T, name string) recordedMCPAnswer {
	t.Helper()
	path := filepath.Join(recordedParityDir(), "mcp", name+".txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the recorded answer %s: %v\n"+
			"There is no regenerating this file: the core tool that produced it was removed.", path, err)
	}

	var answer recordedMCPAnswer
	var current *strings.Builder
	closeBlock := func() {
		if current == nil {
			return
		}
		answer.blocks = append(answer.blocks, strings.TrimSuffix(current.String(), "\n"))
		current = nil
	}
	for index, line := range strings.SplitAfter(string(data), "\n") {
		switch {
		case index == 0:
			verdict, parseErr := strconv.ParseBool(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "isError")))
			if parseErr != nil {
				t.Fatalf("%s does not start with an `isError <bool>` line: %v", path, parseErr)
			}
			answer.isError = verdict
		case strings.HasPrefix(line, recordedMCPBlockMarker):
			closeBlock()
			current = &strings.Builder{}
		case current != nil:
			current.WriteString(line)
		case strings.TrimSpace(line) != "":
			t.Fatalf("%s carries text before its first block marker: %q", path, line)
		}
	}
	closeBlock()
	if len(answer.blocks) == 0 {
		t.Fatalf("%s records no content block, so a comparison against it would prove nothing", path)
	}
	return answer
}

// mcpParityCases exercises each tool's whole argument surface: the unnarrowed
// call, every narrowing the schema declares, and the failures — because a
// failure's SHAPE is part of the payload contract (a core tool that fails with a
// report attached returns two content blocks, one that fails on an argument
// returns one).
func mcpParityCases() []mcpParityCase {
	return []mcpParityCase{
		{name: "list_features", core: "list_features"},
		{name: "list_features query", core: "list_features", arguments: `{"query":"invoice"}`},
		{
			name: "list_features projects", core: "list_features",
			arguments: `{"projects":["@acme/billing"]}`,
		},
		{
			name: "list_features impacted", core: "list_features",
			arguments: `{"impacted":true}`,
		},
		{
			name: "list_features unknown project", core: "list_features",
			arguments: `{"projects":["@acme/nope"]}`,
			wantError: true,
		},
		{
			// The two narrowings are one question asked twice, and the refusal is
			// the orchestrator's now: it resolves the selection before the
			// extension is spawned.
			name: "list_features projects and impacted", core: "list_features",
			arguments: `{"projects":["@acme/billing"],"impacted":true}`,
			wantError: true,
		},
		{
			name: "list_features limit", core: "list_features",
			arguments: `{"limit":1}`,
		},
		{
			name: "list_features cursor", core: "list_features",
			arguments: `{"limit":1,"cursor":"YWZ0ZXI6YmlsbGluZy9pbnZvaWNl"}`,
		},
		{
			name: "list_features limit above the maximum", core: "list_features",
			arguments: `{"limit":500}`,
			wantError: true,
		},
		{
			name: "list_features rejects an unknown argument", core: "list_features",
			arguments: `{"project":"@acme/billing"}`,
			wantError: true,
		},
		{
			name: "feature_context", core: "feature_context",
			arguments: `{"feature":"billing/invoice"}`,
		},
		{
			name: "feature_context unknown feature", core: "feature_context",
			arguments: `{"feature":"billing/nope"}`,
			wantError: true,
		},
		{
			name: "feature_context without a feature", core: "feature_context",
			arguments: `{}`,
			wantError: true,
		},
		{name: "list_specs", core: "list_specs"},
		{
			name: "list_specs projects", core: "list_specs",
			arguments: `{"projects":["@acme/billing"]}`,
		},
		{
			name: "list_specs impacted", core: "list_specs",
			arguments: `{"impacted":true}`,
		},
		{
			name: "spec_context", core: "spec_context",
			arguments: `{"feature":"billing/invoice"}`,
		},
		{
			// An authored feature with no spec: a report AND a verdict, which is
			// the two-content-block failure shape.
			name: "spec_context without a spec", core: "spec_context",
			arguments: `{"feature":"shipping/labels"}`,
			wantError: true,
		},
		{
			name: "spec_context unknown feature", core: "spec_context",
			arguments: `{"feature":"billing/nope"}`,
			wantError: true,
		},
		{
			name: "spec_context without a feature", core: "spec_context",
			arguments: `{}`,
			wantError: true,
		},
	}
}

// TestMCPParityCasesCoverEveryTool keeps the table above honest: a tool missing
// from it would make this file's acceptance a partial one while still passing.
func TestMCPParityCasesCoverEveryTool(t *testing.T) {
	t.Parallel()
	covered := map[string]bool{}
	for _, tool := range mcpParityCases() {
		covered[tool.core] = true
	}
	for core := range sddMCPToolRename {
		if !covered[core] {
			t.Errorf("%s is in the D4 rename table and in no parity case", core)
		}
	}
}

// TestSDDMCPToolsAreAdvertisedAndAcceptedByTheValidator is the other half of
// the acceptance: the five extension tools must reach tools/list, and core's
// four must not.
//
// Reaching it is not a formality. Registration drops a tool that is not
// dot-namespaced, one whose annotations and contract metadata disagree, one
// whose executable is absent, and one whose name a core tool already owns — all
// silently, because tools/list has no place to report a descriptor it refused.
// That last rule is why the absence check below is not cosmetic: a core tool
// that survived removal would not merely duplicate the extension's, it would
// SHADOW it, and every assertion in this file would then be measuring core
// against its own recording. `sdd.architecture_context` has no core
// predecessor and no recorded oracle, so this advertisement check is the one
// place the CLI proves the validator accepts it at all.
func TestSDDMCPToolsAreAdvertisedAndAcceptedByTheValidator(t *testing.T) {
	t.Parallel()
	limitParityConcurrency(t)
	root := mcpParityWorkspace(t)
	srv := newMCPServer(root, wsproto.Load(root), "test")

	advertised := map[string]advertisedTool{}
	for _, tool := range listParityTools(t, srv) {
		advertised[tool.Name] = tool
	}

	extensions := []string{"sdd.architecture_context"}
	for core, extension := range sddMCPToolRename {
		if _, found := advertised[core]; found {
			t.Errorf("the built-in tool %q is still advertised; an earlier change removed it in favor of %q "+
				"— and a core name always wins over an extension's, so this would shadow the extension entirely",
				core, extension)
		}
		extensions = append(extensions, extension)
	}

	for _, extension := range extensions {
		tool, found := advertised[extension]
		if !found {
			t.Errorf("extension tool %q was not advertised; the validator refused it", extension)
			continue
		}
		for _, hint := range []struct {
			name string
			got  *bool
			want bool
		}{
			{"readOnlyHint", tool.Annotations.ReadOnlyHint, true},
			{"destructiveHint", tool.Annotations.DestructiveHint, false},
			{"idempotentHint", tool.Annotations.IdempotentHint, true},
			{"openWorldHint", tool.Annotations.OpenWorldHint, false},
		} {
			if hint.got == nil || *hint.got != hint.want {
				t.Errorf("tool %q %s = %v, want %v", extension, hint.name, hint.got, hint.want)
			}
		}
		contract := tool.Meta.Contract
		if contract.Access != "read" || !contract.ReadOnly || contract.SupportsDryRun {
			t.Errorf("tool %q contract = %+v, want a read-only tool with no dry run", extension, contract)
		}
	}
}

// advertisedTool is the slice of a tools/list descriptor this file asserts on:
// the name an agent calls, the four safety hints its harness reads, and the
// contract metadata the extension-tool validator requires. Declared rather than
// walked as a generic map, so a renamed member fails to decode instead of
// reading as a zero value that happens to match.
type advertisedTool struct {
	Name        string `json:"name"`
	Annotations struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
		IdempotentHint  *bool `json:"idempotentHint"`
		OpenWorldHint   *bool `json:"openWorldHint"`
	} `json:"annotations"`
	Meta struct {
		Contract struct {
			Access         string `json:"access"`
			ReadOnly       bool   `json:"readOnly"`
			SupportsDryRun bool   `json:"supportsDryRun"`
		} `json:"putnami.dev/contract"`
	} `json:"_meta"`
}

// TestSDDMCPToolsAnswerFromTheWorkspaceOnTheWire is the measurement that
// justifies the wire change, kept as an assertion.
//
// The failure it guards is not a crash. An extension handed a workspace ROOT and
// nothing else answers every one of these tools perfectly well — with a
// confidently EMPTY payload: zero features, zero specs, `valid: true`. A
// byte-comparison against core would catch that only because core answers
// differently; this states the property directly, so a future change that
// starved the tool of its membership fails with a message that says so.
func TestSDDMCPToolsAnswerFromTheWorkspaceOnTheWire(t *testing.T) {
	t.Parallel()
	limitParityConcurrency(t)
	root := mcpParityWorkspace(t)
	srv := newMCPServer(root, wsproto.Load(root), "test")

	blocks, isError := callParityTool(t, srv, "sdd.list_features", "")
	if isError {
		t.Fatalf("sdd.list_features failed: %s", strings.Join(blocks, "\n"))
	}
	var catalog struct {
		Selection struct {
			Mode         string   `json:"mode"`
			Scoped       bool     `json:"scoped"`
			Projects     []string `json:"projects"`
			ProjectCount int      `json:"projectCount"`
		} `json:"selection"`
		Features []struct {
			ID string `json:"id"`
		} `json:"features"`
	}
	if err := json.Unmarshal([]byte(blocks[0]), &catalog); err != nil {
		t.Fatalf("decode the catalog: %v\n%s", err, blocks[0])
	}
	if len(catalog.Features) == 0 {
		t.Fatal("the catalog is empty; the tool answered from a workspace it could not see")
	}
	if catalog.Selection.Mode != "all" || catalog.Selection.Scoped {
		t.Errorf("an unnarrowed call resolved to %+v, want the unscoped whole-workspace projection", catalog.Selection)
	}
	if catalog.Selection.ProjectCount != 2 || len(catalog.Selection.Projects) != 0 {
		t.Errorf("the call covered %+v, want the count of both fixture projects and no id list", catalog.Selection)
	}

	// And a narrowed call must actually narrow — through the orchestrator's
	// resolver, since the extension has none.
	narrowed, isError := callParityTool(t, srv, "sdd.list_features",
		`{"projects":["@acme/billing"]}`)
	if isError {
		t.Fatalf("the narrowed call failed: %s", strings.Join(narrowed, "\n"))
	}
	if err := json.Unmarshal([]byte(narrowed[0]), &catalog); err != nil {
		t.Fatalf("decode the narrowed catalog: %v", err)
	}
	if !catalog.Selection.Scoped || len(catalog.Selection.Projects) != 1 {
		t.Errorf("a --projects call resolved to %+v, want one scoped project", catalog.Selection)
	}
}

// --- driving the server ------------------------------------------------------

// callParityTool runs one tools/call and returns its content blocks with the
// isError verdict. Blocks are returned as text, unmodified, because the whole
// claim is that the two sides produce the same bytes.
func callParityTool(t *testing.T, srv mcpServerUnderTest, name, arguments string) ([]string, bool) {
	t.Helper()
	params := `{"name":` + mustJSONString(t, name)
	if arguments != "" {
		params += `,"arguments":` + arguments
	}
	result := parityRPC(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}}`)
	if len(result.Content) == 0 {
		t.Fatalf("tools/call %s returned no content", name)
	}
	blocks := make([]string, 0, len(result.Content))
	for _, block := range result.Content {
		blocks = append(blocks, block.Text)
	}
	return blocks, result.IsError
}

func listParityTools(t *testing.T, srv mcpServerUnderTest) []advertisedTool {
	t.Helper()
	result := parityRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if len(result.Tools) == 0 {
		t.Fatal("tools/list returned no tools")
	}
	return result.Tools
}

// mcpServerUnderTest is the slice of *mcp.Server this file drives: one Serve
// loop over a request buffer. Naming it keeps the helpers readable without
// importing the server type's whole surface into every signature.
type mcpServerUnderTest interface {
	Serve(ctx context.Context, in io.Reader, out io.Writer) error
}

// parityRPC runs ONE request through a fresh Serve loop and returns its result
// object.
//
// One request per loop, deliberately: Serve returns when its input is exhausted,
// so a loop per call keeps each answer's framing unambiguous and keeps a tool
// that writes a notification from being read as another call's reply.
func parityRPC(t *testing.T, srv mcpServerUnderTest, request string) parityResult {
	t.Helper()
	var in bytes.Buffer
	in.WriteString(request)
	in.WriteByte('\n')

	var out bytes.Buffer
	if err := srv.Serve(context.Background(), &in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	scanner := bufio.NewScanner(&out)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var response parityResponse
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatalf("unmarshal response %q: %v", line, err)
		}
		if len(response.Error) > 0 {
			t.Fatalf("JSON-RPC error for %s: %s", request, response.Error)
		}
		if response.Result != nil {
			var result parityResult
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatalf("unmarshal result %q: %v", response.Result, err)
			}
			return result
		}
	}
	t.Fatalf("no result for %s", request)
	return parityResult{}
}

// parityResponse and parityResult are the JSON-RPC frames this file reads, as
// declared shapes. A transport error is kept raw so the message a failure prints
// is whatever the server actually sent.
type parityResponse struct {
	Error  json.RawMessage `json:"error"`
	Result json.RawMessage `json:"result"`
}

type parityResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool             `json:"isError"`
	Tools   []advertisedTool `json:"tools"`
}

// --- the fixture -------------------------------------------------------------

// mcpParityWorkspace is the interactive parity fixture with the extension
// DECLARED, so discovery finds it the way a user's workspace would.
//
// The extension is written as a directory holding one file — the shipped
// manifest with {extensionRuntime} replaced by the compiled binary. Nothing else
// of the extension has to be there: a tool call runs an executable and reads no
// other file, and copying the Go module would mean copying its relative replace
// directives too.
func mcpParityWorkspace(t *testing.T) string {
	t.Helper()
	root := parityWorkspace(t)
	runtimePath := parityExtensionRuntime(t)

	shipped, err := os.ReadFile(filepath.Join("..", "..", "..", "sdd-extension", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read the shipped manifest: %v", err)
	}
	resolved := strings.ReplaceAll(string(shipped), "{extensionRuntime}", jsonStringContent(t, runtimePath))
	if resolved == string(shipped) {
		t.Fatal("the shipped manifest names no {extensionRuntime}; this fixture no longer substitutes anything")
	}
	extensionRoot := filepath.Join(root, ".sdd-extension")
	writeParityFile(t, extensionRoot, "putnami.extension.json", resolved)

	// Declared by ABSOLUTE path, which discovery's first probe for an explicit
	// reference accepts. A workspace-relative reference would work too; the
	// absolute form states that the directory is not a workspace project and
	// must not be discovered as one.
	writeParityFile(t, root, "putnami.workspace.json",
		`{"name":"sdd-parity","includes":["billing","shipping"],"extensions":[`+
			mustJSONString(t, extensionRoot)+`]}`)
	workspace.InvalidateLoadCache(root)
	return root
}

func mustJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %q: %v", value, err)
	}
	return string(encoded)
}

// jsonStringContent is value encoded for use inside a JSON string literal,
// without the surrounding quotes, so a Windows path substituted into a manifest
// keeps its backslashes as escapes instead of breaking the document.
func jsonStringContent(t *testing.T, value string) string {
	t.Helper()
	return strings.TrimSuffix(strings.TrimPrefix(mustJSONString(t, value), `"`), `"`)
}
