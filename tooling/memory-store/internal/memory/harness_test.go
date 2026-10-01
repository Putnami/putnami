package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

// The provider is driven through collab.Serve with the request a routed call
// carries — the orchestrator's normalization included — so every answer here
// passed the same request and result validation a real call does.

const feature = "tooling/memory-store"

// workspaceName is what the orchestrator fills into identity.workspace.
const workspaceName = "proof"

type harness struct {
	t         *testing.T
	workspace string
	settings  string
	provider  *Provider
	clock     *time.Time
	selection *extension.ToolSelection
}

func newHarness(t *testing.T, workspace, settings string) *harness {
	t.Helper()
	clock := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	h := &harness{t: t, workspace: workspace, settings: settings, clock: &clock}
	h.provider = &Provider{Now: func() time.Time { return *h.clock }}
	return h
}

// fresh is another provider process on the same store: nothing is shared
// with h but the store.
func (h *harness) fresh() *harness {
	other := newHarness(h.t, h.workspace, h.settings)
	*other.clock = *h.clock
	return other
}

func (h *harness) tick(d time.Duration) { *h.clock = h.clock.Add(d) }

// do calls one operation the way the router does: the identity names the
// workspace, and a list request has an explicit page.
func (h *harness) do(operation string, arguments any) (collab.Response, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return collab.Response{}, err
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return collab.Response{}, err
	}
	identity, _ := document["identity"].(map[string]any)
	if identity == nil {
		identity = map[string]any{}
	}
	if identity["workspace"] == nil {
		identity["workspace"] = workspaceName
	}
	document["identity"] = identity
	if operation == collab.OperationContext || operation == collab.OperationSearch {
		page, _ := document["page"].(map[string]any)
		if page == nil {
			page = map[string]any{}
		}
		if page["size"] == nil {
			page["size"] = collab.DefaultPageSize
		}
		document["page"] = page
	}
	encoded, err = json.Marshal(document)
	if err != nil {
		return collab.Response{}, err
	}
	request := extension.ToolCallRequest{
		Name:          "memory-store.memory." + operation,
		Arguments:     encoded,
		WorkspaceRoot: h.workspace,
		Provider:      &extension.ToolProviderCall{Contract: collab.ContractMemory, Version: 1, Operation: operation},
		Agent:         &extension.AgentIdentity{ClientName: "contract-suite"},
	}
	if h.settings != "" {
		request.Provider.Settings = json.RawMessage(h.settings)
	}
	if operation == collab.OperationContext || operation == collab.OperationSearch {
		request.Selection = h.selection
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return collab.Response{}, err
	}
	var out bytes.Buffer
	if err := collab.Serve(context.Background(), bytes.NewReader(payload), &out, h.provider.Handlers()); err != nil {
		return collab.Response{}, err
	}
	var result extension.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return collab.Response{}, err
	}
	response, diags := collab.ParseResponse([]byte(result.Content[0].Text))
	if diags != nil {
		return collab.Response{}, fmt.Errorf("invalid response %s: %v", result.Content[0].Text, diags)
	}
	if result.IsError != (response.Outcome != collab.OutcomeOK) {
		return collab.Response{}, fmt.Errorf("isError %v disagrees with outcome %s", result.IsError, response.Outcome)
	}
	if response.Outcome == collab.OutcomeOK {
		if _, diags := collab.ParseResult(collab.ContractMemory, 1, operation, response.Result); diags != nil {
			return collab.Response{}, fmt.Errorf("the %s result violates the contract: %v", operation, diags)
		}
	}
	return *response, nil
}

func (h *harness) call(operation string, arguments any) collab.Response {
	h.t.Helper()
	response, err := h.do(operation, arguments)
	if err != nil {
		h.t.Fatal(err)
	}
	return response
}

func (h *harness) ok(operation string, arguments, into any) {
	h.t.Helper()
	response := h.call(operation, arguments)
	if response.Outcome != collab.OutcomeOK {
		h.t.Fatalf("memory.%s %s: %s %+v", operation, mustJSON(h.t, arguments), response.Outcome, response.Error)
	}
	if err := json.Unmarshal(response.Result, into); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) fails(operation string, arguments any, outcome collab.Outcome) collab.Error {
	h.t.Helper()
	response := h.call(operation, arguments)
	if response.Outcome != outcome {
		h.t.Fatalf("memory.%s %s: outcome %s, want %s (%+v)", operation, mustJSON(h.t, arguments), response.Outcome, outcome, response.Error)
	}
	return *response.Error
}

// mission reads a mission, which must exist.
func (h *harness) mission(name string) collab.MemoryRecord {
	h.t.Helper()
	return h.missionIn(name, nil)
}

// missionIn reads a mission of an identity, which must exist.
func (h *harness) missionIn(name string, identity map[string]any) collab.MemoryRecord {
	h.t.Helper()
	request := map[string]any{"mission": name}
	if identity != nil {
		request["identity"] = identity
	}
	var result collab.MemoryRecordResult
	h.ok(collab.OperationMission, request, &result)
	return result.Record
}

// checkpoint is a checkpoint request. expected is a revision, or "" for
// mustNotExist.
func checkpoint(mission, key, content, expected string) map[string]any {
	precondition := map[string]any{"mustNotExist": true}
	if expected != "" {
		precondition = map[string]any{"expectedRevision": expected}
	}
	return map[string]any{"mission": mission, "idempotencyKey": key, "content": content, "precondition": precondition}
}

func (h *harness) save(request map[string]any) collab.MemoryCheckpointResult {
	h.t.Helper()
	var result collab.MemoryCheckpointResult
	h.ok(collab.OperationCheckpoint, request, &result)
	return result
}

// all traverses every page of a context request of the given page size.
func (h *harness) all(request map[string]any, size int) []collab.MemoryRecord {
	h.t.Helper()
	var records []collab.MemoryRecord
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 100 {
			h.t.Fatal("the traversal does not end")
		}
		page := map[string]any{"size": size}
		if cursor != "" {
			page["cursor"] = cursor
		}
		request["page"] = page
		var result collab.MemoryListResult
		h.ok(collab.OperationContext, request, &result)
		if len(result.Items) > size {
			h.t.Fatalf("a page of %d items for size %d", len(result.Items), size)
		}
		records = append(records, result.Items...)
		if cursor = result.Page.Next; cursor == "" {
			return records
		}
	}
}

// concurrently runs n calls at once and returns their responses in order.
func concurrently(t *testing.T, n int, call func(i int) (collab.Response, error)) []collab.Response {
	t.Helper()
	responses := make([]collab.Response, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			responses[i], errs[i] = call(i)
		})
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	return responses
}

func sequenceOf(t *testing.T, revision string) int {
	t.Helper()
	head, _, found := strings.Cut(revision, ".")
	n, err := strconv.Atoi(head)
	if !found || err != nil {
		t.Fatalf("revision %q is not <sequence>.<digest>", revision)
	}
	return n
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// isolateGit keeps the tests off the machine's Git configuration: no global
// or system file is read, so signing, hooks or templates configured there
// change nothing.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("the Git backend's tests need git on PATH: %v", err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// backend is one way to bind the provider: its settings for a fresh store in
// a workspace, and how to take that store out of reach.
type backend struct {
	name string
	open func(t *testing.T, workspace string) (settings string, outage func() (restore func()))
}

func backends() []backend {
	return []backend{
		{name: "file", open: func(t *testing.T, workspace string) (string, func() func()) {
			// The store directory is replaced by a file.
			return `{"backend":"file"}`, func() func() {
				root := filepath.Join(workspace, DefaultFilePath)
				aside := root + ".aside"
				mustDo(t, os.Rename(root, aside))
				mustDo(t, os.WriteFile(root, []byte("not a store"), 0o644))
				return func() {
					mustDo(t, os.Remove(root))
					mustDo(t, os.Rename(aside, root))
				}
			}
		}},
		{name: "git", open: func(t *testing.T, _ string) (string, func() func()) {
			isolateGit(t)
			// git is no longer on PATH.
			return `{"backend":"git"}`, func() func() {
				path := os.Getenv("PATH")
				t.Setenv("PATH", t.TempDir())
				return func() { t.Setenv("PATH", path) }
			}
		}},
		{name: "git-remote", open: func(t *testing.T, _ string) (string, func() func()) {
			isolateGit(t)
			remote := filepath.Join(t.TempDir(), "memory.git")
			gitCommand(t, filepath.Dir(remote), "init", "--bare", "--quiet", remote)
			// The remote is gone.
			return `{"backend":"git","remote":` + mustJSON(t, remote) + `,"branch":"agents/memory"}`, func() func() {
				aside := remote + ".aside"
				mustDo(t, os.Rename(remote, aside))
				return func() { mustDo(t, os.Rename(aside, remote)) }
			}
		}},
	}
}

// stored renders a record document as a backend stores it.
func stored(t *testing.T, doc *store.Document) []byte {
	t.Helper()
	data, err := store.Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// forEachBackend runs one contract scenario against every backend, each on a
// fresh workspace and store.
func forEachBackend(t *testing.T, scenario func(t *testing.T, h *harness, outage func() func())) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			workspace := t.TempDir()
			settings, outage := b.open(t, workspace)
			scenario(t, newHarness(t, workspace, settings), outage)
		})
	}
}
