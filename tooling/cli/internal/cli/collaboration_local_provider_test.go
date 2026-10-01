package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The shipped local provider (@putnami/local-collaboration), resolved from a
// workspace binding and called through both entry paths against one store.
//
// The provider is compiled from its source here, the way its bin/prepare
// compiles it (GOWORK=off, CGO off), and its shipped manifest is written with
// {extensionRuntime} replaced by that binary: everything else — discovery,
// binding, routing, the provider's own validation and store — is the real
// thing.

// localProviderRuntime compiles the local provider into a test directory.
func localProviderRuntime(t *testing.T) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), pkgmeta.ExecutableName(runtime.GOOS, "putnami-local-collaboration"))
	cmd := exec.Command("go", "build", "-o", output, "./cmd/putnami-local-collaboration")
	cmd.Dir = filepath.Join("..", "..", "..", "local-collaboration")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the local provider:\n%s", out)
	}
	return output
}

func localProviderWorkspace(t *testing.T) string {
	t.Helper()
	shipped, err := os.ReadFile(filepath.Join("..", "..", "..", "local-collaboration", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	resolved := strings.ReplaceAll(string(shipped), "{extensionRuntime}", jsonStringContent(t, localProviderRuntime(t)))
	extensionRoot := filepath.Join(t.TempDir(), "local-collaboration")
	writeParityFile(t, extensionRoot, "putnami.extension.json", resolved)

	root := t.TempDir()
	writeParityFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"local-proof","includes":["packages/app"],`+
		`"extensions":[`+mustJSONString(t, extensionRoot)+`],`+
		`"options":{"collaboration":{`+
		`"tasks":{"provider":"@putnami/local-collaboration","version":1},`+
		`"proposals":{"provider":"@putnami/local-collaboration","version":1,"settings":{"repository":"acme/app"}}}}}`)
	writeParityFile(t, filepath.Join(root, "packages", "app"), "putnami.json", `{"name":"app","type":"library"}`)
	workspace.InvalidateLoadCache(root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.RefreshSnapshot(ws, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	return root
}

func TestTheLocalProviderAnswersBothEntryPaths(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "one-route-two-entry-paths", "the-cli-and-mcp-return-the-same-envelope")
	root := localProviderWorkspace(t)

	// The CLI records a task and a proposal.
	stdout, stderr, code := runCollabCommand(t, root, "", "tasks", "create", "--output=json",
		"--input", `{"title":"Route the collaboration contracts","labels":["epic/42"],"idempotencyKey":"42:task-2"}`)
	if code != ExitSuccess {
		t.Fatalf("tasks create: code %d\n%s\n%s", code, stdout, stderr)
	}
	var created collab.TaskCreateResult
	if err := json.Unmarshal(decodeCollabEnvelope(t, stdout).Result, &created); err != nil || !created.Created {
		t.Fatalf("tasks create: %s", stdout)
	}
	stdout, _, code = runCollabCommand(t, root, "", "proposals", "upsert", "--output=json",
		"--input", `{"change":{"base":"main","head":"fix-issue-42","headCommit":"55e2a8e42"},"title":"feat: collaboration providers"}`)
	upsert := decodeCollabEnvelope(t, stdout)
	var proposal collab.ProposalUpsertResult
	if err := json.Unmarshal(upsert.Result, &proposal); err != nil || code != ExitSuccess || !proposal.Created ||
		proposal.Proposal.Change.Repository != "acme/app" {
		t.Fatalf("proposals upsert: %s", stdout)
	}

	// MCP reads the same store, through the same binding.
	srv := newMCPServer(root, wsproto.Load(root), "collaboration-test")
	tools := map[string]bool{}
	for _, tool := range listParityTools(t, srv) {
		tools[tool.Name] = true
	}
	for _, want := range []string{"tasks.capabilities", "tasks.create", "tasks.find", "proposals.status", "proposals.review"} {
		if !tools[want] {
			t.Errorf("tools/list lacks %s", want)
		}
	}
	for _, absent := range []string{"proposals.merge", "tasks.claim", "memory.context", "local-collaboration.tasks.find"} {
		if tools[absent] {
			t.Errorf("tools/list advertises %s", absent)
		}
	}
	ref, _ := json.Marshal(created.Task.Ref)
	blocks, isError := callParityTool(t, srv, "tasks.get", `{"ref":`+string(ref)+`}`)
	got := decodeCollabEnvelope(t, blocks[0])
	var task collab.TaskResult
	if err := json.Unmarshal(got.Result, &task); err != nil || isError || task.Task.Title != "Route the collaboration contracts" ||
		got.Provider == nil || got.Provider.Name != "@putnami/local-collaboration" {
		t.Fatalf("MCP tasks.get: %s", blocks[0])
	}
	blocks, _ = callParityTool(t, srv, "proposals.status", `{"ref":`+mustMarshal(t, proposal.Proposal.Ref)+`}`)
	var status collab.ProposalStatusResult
	if err := json.Unmarshal(decodeCollabEnvelope(t, blocks[0]).Result, &status); err != nil ||
		status.Checks.State != collab.ChecksStateUnsupported || status.Proposal.Ref != proposal.Proposal.Ref {
		t.Fatalf("MCP proposals.status: %s", blocks[0])
	}

	// A retried upsert through MCP finds the CLI's proposal instead of
	// creating a second one.
	blocks, _ = callParityTool(t, srv, "proposals.upsert",
		`{"change":{"repository":"acme/app","base":"main","head":"fix-issue-42"},"title":"feat: collaboration providers"}`)
	var again collab.ProposalUpsertResult
	if err := json.Unmarshal(decodeCollabEnvelope(t, blocks[0]).Result, &again); err != nil || again.Created ||
		again.Proposal.Ref != proposal.Proposal.Ref {
		t.Fatalf("MCP proposals.upsert: %s", blocks[0])
	}

	// Discovery says what the local provider cannot do.
	stdout, _, code = runCollabCommand(t, root, "", "proposals", "capabilities", "--output=json")
	var capabilities collab.Capabilities
	if err := json.Unmarshal(decodeCollabEnvelope(t, stdout).Result, &capabilities); err != nil || code != ExitSuccess {
		t.Fatalf("capabilities: %s", stdout)
	}
	for _, op := range capabilities.Operations {
		if op.Name == collab.OperationMerge && op.Supported {
			t.Error("the local provider offers no merge")
		}
		if op.Name == collab.OperationStatus && !strings.Contains(op.Description, "no hosted checks") {
			t.Errorf("status does not disclose the absence of hosted checks: %q", op.Description)
		}
	}
	stdout, _, _ = runCollabCommand(t, root, "", "memory", "capabilities", "--output=json")
	if err := json.Unmarshal(decodeCollabEnvelope(t, stdout).Result, &capabilities); err != nil || capabilities.Status != collab.BindingStatusUnbound {
		t.Fatalf("memory capabilities: %s", stdout)
	}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
