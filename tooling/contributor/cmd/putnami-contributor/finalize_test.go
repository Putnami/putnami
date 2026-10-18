package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

// finalizeRequirement names the requirement the tests below prove, in the
// feature languageFeature names.
const finalizeRequirement = "change-finalize-parity"

// TestTheManifestDeclaresChangeFinalize pins the launch: an interactive
// subcommand that needs a workspace, every flag the core command takes, and
// an uncached task that starts in the workspace root with a deadline longer
// than the default wait for hosted checks.
func TestTheManifestDeclaresChangeFinalize(t *testing.T) {
	t.Parallel()
	manifest := readManifest(t)
	subcommand, declared := manifest.CommandGroups["change"].Subcommands["finalize"]
	if !declared || subcommand.Command != "change-finalize" || !subcommand.Interactive || subcommand.RunsWithoutWorkspace() {
		t.Fatalf("change finalize is %+v, want an interactive change-finalize that needs a workspace", subcommand)
	}
	flags := map[string]string{}
	for name, flag := range manifest.Commands["change-finalize"].Flags {
		flags[name] = flag.Type
	}
	want := map[string]string{"base": "string", "branch": "string", "title": "string", "body-file": "string",
		"task-source": "string", "task-id": "string", "verification-file": "string", "workflow-file": "string",
		"gate": "string", "proof": "string", "draft": "boolean", "partial-delivery": "boolean"}
	if !maps.Equal(flags, want) {
		t.Errorf("change-finalize flags %v, want %v", flags, want)
	}
	task := manifest.Tasks["change-finalize"]
	if task.Command != "{extensionRuntime}" || !slices.Equal(task.Args, []string{"finalize"}) || task.Cwd != "{workspaceRoot}" {
		t.Errorf("change-finalize runs %s %v in %q, want {extensionRuntime} finalize in {workspaceRoot}", task.Command, task.Args, task.Cwd)
	}
	if task.Cache == nil || task.Cache.IsEnabled() {
		t.Error("change-finalize is cacheable; a publication writes to the providers, which no cache key covers")
	}
	// The default policy waits 60 minutes for hosted checks.
	if task.TimeoutMs < 75*60*1000 {
		t.Errorf("change-finalize times out after %d ms, before the default 60-minute wait for hosted checks ends", task.TimeoutMs)
	}
}

// providerHost serves the host RPC channel on a Unix socket, as putnami
// serves it for a launch: collaboration.call from an in-memory provider with
// one task and hosted checks that pass on the proposal's head,
// workspace.owners from a table, and a refusal for every verification.report.
// It counts the connections it accepts and records every request.
type providerHost struct {
	socket, token string
	owners        map[string][]wire.OwningProject

	mu          sync.Mutex
	connections int
	requests    []wire.Request
	proposal    *collab.Proposal
}

func newProviderHost(t *testing.T, owners map[string][]wire.OwningProject) *providerHost {
	t.Helper()
	// A Unix socket path is limited to about 100 bytes: the temporary
	// directory of a test is longer on macOS.
	dir, err := os.MkdirTemp("/tmp", "pf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	h := &providerHost{socket: filepath.Join(dir, "host.sock"), token: "launch-token", owners: owners}
	listener, err := net.Listen("unix", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.connections++
			h.mu.Unlock()
			go h.serve(conn)
		}
	}()
	return h
}

func (h *providerHost) env() []string {
	return []string{wire.SocketEnv + "=" + h.socket, wire.TokenEnv + "=" + h.token}
}

func (h *providerHost) seen() (int, []wire.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connections, slices.Clone(h.requests)
}

func (h *providerHost) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		frame, err := wire.ReadFrame(reader, wire.MaxFrameBytes)
		if err != nil {
			return
		}
		var request wire.Request
		if err := json.Unmarshal(frame, &request); err != nil {
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, request)
		result, refusal := h.answer(request)
		h.mu.Unlock()
		response := wire.Response{JSONRPC: wire.JSONRPCVersion, ID: request.ID, Error: refusal}
		if refusal == nil {
			response.Result, _ = json.Marshal(result)
		}
		reply, err := wire.EncodeFrame(response)
		if err != nil {
			return
		}
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}
}

// answer answers one request. The caller holds h.mu.
func (h *providerHost) answer(request wire.Request) (any, *wire.Error) {
	if request.Token != h.token {
		return nil, &wire.Error{Code: wire.CodeUnauthorized, Message: "refused"}
	}
	switch request.Method {
	case wire.MethodWorkspaceOwners:
		var params wire.WorkspaceOwnersParams
		if json.Unmarshal(request.Params, &params) != nil {
			return nil, &wire.Error{Code: wire.CodeInvalidParams, Message: "invalid params"}
		}
		result := wire.WorkspaceOwnersResult{Owners: []wire.PathOwners{}}
		for _, path := range params.Paths {
			projects := h.owners[path]
			if projects == nil {
				projects = []wire.OwningProject{}
			}
			result.Owners = append(result.Owners, wire.PathOwners{Path: path, Projects: projects})
		}
		return result, nil
	case wire.MethodCollaborationCall:
		var params wire.CollaborationCallParams
		if json.Unmarshal(request.Params, &params) != nil {
			return nil, &wire.Error{Code: wire.CodeInvalidParams, Message: "invalid params"}
		}
		return h.collaboration(params), nil
	}
	return nil, &wire.Error{Code: wire.CodeNotVerified, Message: "no report is corroborated"}
}

// fixtureTask is the one task the in-memory provider holds.
var fixtureTask = collab.Task{Ref: collab.Ref{Source: "local:fixture", ID: "T-1"}, Revision: "r1",
	Title: "Publish the parser", State: collab.TaskStateInProgress}

// collaboration is the in-memory provider: bound proposals and tasks, one
// task, and at most one proposal. The caller holds h.mu.
func (h *providerHost) collaboration(params wire.CollaborationCallParams) collab.Envelope {
	answer := func(value any) collab.Envelope {
		data, _ := json.Marshal(value)
		return collab.Envelope{Contract: params.Contract, Operation: params.Operation, Outcome: collab.OutcomeOK, Result: data}
	}
	if params.Operation == collab.OperationCapabilities {
		names := []string{collab.OperationFind, collab.OperationUpsert, collab.OperationStatus}
		if params.Contract == collab.ContractTasks {
			names = []string{collab.OperationGet}
		}
		operations := make([]collab.OperationCapability, 0, len(names))
		for _, name := range names {
			operations = append(operations, collab.OperationCapability{Name: name, Supported: true})
		}
		return answer(collab.Capabilities{Status: collab.BindingStatusBound, Operations: operations})
	}
	switch params.Contract + "." + params.Operation {
	case "tasks.get":
		return answer(collab.TaskResult{Task: fixtureTask})
	case "proposals.find":
		var items []collab.Proposal
		if h.proposal != nil {
			items = append(items, *h.proposal)
		}
		return answer(collab.ProposalListResult{Items: items})
	case "proposals.upsert":
		var input collab.ProposalUpsertInput
		_ = json.Unmarshal(params.Input, &input)
		proposal := collab.Proposal{Ref: collab.Ref{Source: "local:fixture", ID: "P-1"}, Revision: "r1", Change: input.Change,
			Title: input.Title, Body: input.Body, State: collab.ProposalStateOpen}
		proposal.Change.Repository = "acme/app"
		if input.Draft != nil && *input.Draft {
			proposal.State = collab.ProposalStateDraft
		}
		h.proposal = &proposal
		return answer(collab.ProposalUpsertResult{Proposal: proposal, Created: true})
	case "proposals.status":
		proposal := *h.proposal
		return answer(collab.ProposalStatusResult{Proposal: proposal, Checks: collab.Checks{State: collab.ChecksStatePassing,
			Commit: proposal.Change.HeadCommit, Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}})
	}
	return collab.Envelope{Contract: params.Contract, Operation: params.Operation, Outcome: collab.OutcomeUnsupported,
		Error: &collab.Error{Message: "unexpected call"}}
}

// finalizeRepo is a workspace whose origin is a local bare repository. Its
// main branch holds one featureless project, lib; the branch topic is
// checked out.
type finalizeRepo struct {
	root, origin, home string
}

// newFinalizeRepo builds the workspace. With change, topic holds one commit
// that edits lib/source.txt; without, topic has no commit ahead of main.
func newFinalizeRepo(t *testing.T, change bool) finalizeRepo {
	t.Helper()
	dir := t.TempDir()
	repo := finalizeRepo{root: filepath.Join(dir, "ws"), origin: filepath.Join(dir, "origin.git"), home: filepath.Join(dir, "home")}
	for _, path := range []string{repo.root, repo.home} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo.write(t, "putnami.workspace.json", `{"name":"finalize-fixture","options":{"@putnami/contributor":{"version":1,`+
		`"verification":{"ciGate":{"checks":["Putnami CI"],"pollSeconds":1}},"publication":{"draft":false}}}}`+"\n")
	repo.write(t, "lib/putnami.json", `{"name":"lib","featureAuthority":{"none":"Fixture plumbing with no user-facing feature."}}`+"\n")
	repo.write(t, "lib/source.txt", "first\n")
	repo.git(t, "", "init", "-q", "--bare", repo.origin)
	repo.git(t, repo.root, "init", "-q", "-b", "main")
	repo.git(t, repo.root, "config", "user.name", "Finalize Test")
	repo.git(t, repo.root, "config", "user.email", "finalize-test@example.invalid")
	repo.git(t, repo.root, "config", "commit.gpgSign", "false")
	repo.git(t, repo.root, "remote", "add", "origin", repo.origin)
	repo.git(t, repo.root, "add", "-A")
	repo.git(t, repo.root, "commit", "-q", "-m", "chore: add the fixture")
	repo.git(t, repo.root, "push", "-q", "origin", "main")
	repo.git(t, repo.root, "checkout", "-q", "-b", "topic")
	if change {
		repo.write(t, "lib/source.txt", "second\n")
		repo.git(t, repo.root, "commit", "-q", "-am", "feat: change the fixture")
	}
	return repo
}

func (r finalizeRepo) write(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(r.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r finalizeRepo) env() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null"}
}

func (r finalizeRepo) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = r.env()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// body writes a proposal body outside the worktree and returns its path.
func (r finalizeRepo) body(t *testing.T) string {
	t.Helper()
	path := filepath.Join(r.home, "body.md")
	if err := os.WriteFile(path, []byte("Change the fixture so the parser reads the second line.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// launch runs `finalize <args>` in the workspace root as putnami launches it,
// with the job context contextJSON when it is not empty, and extraEnv added
// to the environment.
func (r finalizeRepo) launch(t *testing.T, executable, contextJSON string, args []string, extraEnv ...string) languageResult {
	t.Helper()
	argv := append([]string{"finalize"}, args...)
	if contextJSON != "" {
		path := filepath.Join(r.home, "context.json")
		if err := os.WriteFile(path, []byte(contextJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		argv = append(argv, "--putnamiContext", path)
	}
	command := exec.Command(executable, argv...)
	command.Dir = r.root
	command.Env = append(r.env(), extraEnv...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := languageResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		result.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("launch the executable: %v", err)
	}
	return result
}

// channelShims returns a PATH entry whose git fails when it receives the host
// channel, ahead of the real git.
func channelShims(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shims := t.TempDir()
	shim := "#!/bin/sh\nif [ -n \"${" + wire.SocketEnv + "-}\" ] || [ -n \"${" + wire.TokenEnv + "-}\" ]; then\n" +
		"  echo 'git: the host channel reached git' >&2\n  exit 97\nfi\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shims, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	return shims
}

// TestFinalizeReachesTheHostOnOneConnection runs a hosted partial delivery,
// which calls the collaboration providers and workspace.owners, with a git on
// PATH that fails when it receives the channel. The run publishes on one
// connection, and every git command it starts, the push included, runs
// without the channel.
func TestFinalizeReachesTheHostOnOneConnection(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, finalizeRequirement, "one-host-channel-serves-the-run")
	spectest.Proves(t, languageFeature, finalizeRequirement, "no-git-command-receives-the-host-channel")
	executable := languageExecutable(t)
	repo := newFinalizeRepo(t, true)
	host := newProviderHost(t, map[string][]wire.OwningProject{"lib/source.txt": {{ID: "/lib", Path: "lib"}}})
	env := append(host.env(), "PATH="+channelShims(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	got := repo.launch(t, executable, `{"params":{}}`, []string{"--base", "main", "--branch", "topic",
		"--title", "feat: change the fixture", "--body-file", repo.body(t), "--task-source", fixtureTask.Ref.Source,
		"--task-id", fixtureTask.Ref.ID, "--partial-delivery", "--gate", "ci"}, env...)
	if want := `PROPOSAL_REF={"source":"local:fixture","id":"P-1"}` + "\n"; got.code != 0 || got.stdout != want || got.stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and %q", got.code, got.stdout, got.stderr, want)
	}
	connections, requests := host.seen()
	methods := map[string]int{}
	for _, request := range requests {
		methods[request.Method]++
	}
	if connections != 1 || methods[wire.MethodWorkspaceOwners] != 1 || methods[wire.MethodCollaborationCall] == 0 || len(methods) != 2 {
		t.Errorf("%d connections and methods %v, want one connection for workspace.owners and collaboration.call", connections, methods)
	}
	head := repo.git(t, repo.root, "rev-parse", "HEAD")
	if pushed := repo.git(t, repo.origin, "rev-parse", "refs/heads/topic"); pushed != head {
		t.Errorf("origin topic is %s, want the pushed head %s", pushed, head)
	}
}

// TestFinalizeDraftSeedGivesNoGitCommandTheChannel runs a draft seed, which
// commits and pushes, with a git on PATH that fails when it receives the
// channel. The seed commit and the push succeed.
func TestFinalizeDraftSeedGivesNoGitCommandTheChannel(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, finalizeRequirement, "no-git-command-receives-the-host-channel")
	executable := languageExecutable(t)
	repo := newFinalizeRepo(t, false)
	host := newProviderHost(t, nil)
	env := append(host.env(), "PATH="+channelShims(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	got := repo.launch(t, executable, `{"params":{"output":"json"}}`, []string{"--base", "main", "--branch", "topic",
		"--title", "feat: change the fixture", "--body-file", repo.body(t), "--draft"}, env...)
	var result struct {
		ProposalRef collab.Ref           `json:"proposalRef"`
		HeadSHA     string               `json:"headSHA"`
		State       collab.ProposalState `json:"state"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &result); err != nil || got.code != 0 || got.stderr != "" ||
		strings.Count(got.stdout, "\n") != 1 || result.State != collab.ProposalStateDraft {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the draft result on one line", got.code, got.stdout, got.stderr)
	}
	if subject := repo.git(t, repo.root, "log", "-1", "--format=%s", "topic"); subject != "feat: change the fixture" {
		t.Errorf("the seed commit says %q, want the title", subject)
	}
	if pushed := repo.git(t, repo.origin, "rev-parse", "refs/heads/topic"); pushed != result.HeadSHA {
		t.Errorf("origin topic is %s, want the seeded head %s", pushed, result.HeadSHA)
	}
}

// TestFinalizeThatNeverCallsTheHostNeverDials pins that a usage error and a
// refused project selection exit 2 without a connection to the channel.
func TestFinalizeThatNeverCallsTheHostNeverDials(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, finalizeRequirement, "a-usage-error-never-dials-the-host")
	executable := languageExecutable(t)
	repo := newFinalizeRepo(t, true)
	host := newProviderHost(t, nil)
	for _, tc := range []struct {
		context, stderr string
		args            []string
	}{
		{`{"params":{}}`, "change finalize: --gate accepts only ci; local proof uses --verification-file\n", []string{"--gate", "local"}},
		{"", "change finalize: --draft takes no value\n", []string{"--draft=yes"}},
		{`{"params":{"projects":"/lib"}}`, "change finalize takes no --projects selection\n", []string{"--draft"}},
	} {
		got := repo.launch(t, executable, tc.context, tc.args, host.env()...)
		if got.code != 2 || got.stdout != "" || got.stderr != tc.stderr {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want 2 and %q", tc.args, got.code, got.stdout, got.stderr, tc.stderr)
		}
	}
	if connections, _ := host.seen(); connections != 0 {
		t.Errorf("%d connections, want none", connections)
	}
}

// TestFinalizeWithoutAChannelNamesTheLaunch pins the answer of a run putnami
// did not launch interactively: exit 1, the remedy, no output, and no push.
func TestFinalizeWithoutAChannelNamesTheLaunch(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, finalizeRequirement, "one-host-channel-serves-the-run")
	executable := languageExecutable(t)
	repo := newFinalizeRepo(t, true)
	got := repo.launch(t, executable, "", []string{"--base", "main", "--branch", "topic", "--title", "feat: change the fixture",
		"--body-file", repo.body(t), "--draft"})
	want := "change finalize: this command needs an interactive launch by putnami, which opens the host channel it reaches " +
		"the collaboration providers and the workspace through; run `putnami change finalize` (a hosted run opens no channel; " +
		wire.SocketEnv + " and " + wire.TokenEnv + " not set)\n"
	if got.code != 1 || got.stdout != "" || got.stderr != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1 and %q", got.code, got.stdout, got.stderr, want)
	}
	if refs := repo.git(t, repo.origin, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Errorf("origin refs %q, want only main", refs)
	}
}
