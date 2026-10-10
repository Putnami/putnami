package cloudcli

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

// statusEntryOrder is the order `cloud status` prints its lines in: the
// public root order, restricted to the entries that have a state.
var statusEntryOrder = []string{
	"whoami", "token", "registries", "packages", "channels", "ci", "cache",
	"source", "config", "secrets", "db", "env",
}

// lockedTransport serializes the fake transport: the status probes run in
// parallel and the fake records every request.
type lockedTransport struct {
	mu    sync.Mutex
	inner http.RoundTripper
}

func (l *lockedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.RoundTrip(req)
}

type statusOutput struct {
	State   clicore.StatusState  `json:"state"`
	Entries []clicore.StatusNode `json:"entries"`
}

// runCloudStatus runs `cloud status`. When it fails in a structured mode, the
// failure envelope is written to the output, as the CLI host does.
func runCloudStatus(t *testing.T, env map[string]string, args ...string) ([]string, error) {
	t.Helper()
	var output []string
	ioctx := IO{
		Env:    env,
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: &http.Client{Transport: &lockedTransport{inner: newFakeTransport(t)}},
		Now:    fixedNow,
	}
	err := RunCommand("status", ioctx, args)
	if err != nil && slices.Contains(args, "--output=json") {
		clicore.WriteErrorResult(err, map[string]any{"output": "json"}, ioctx)
	}
	return output, err
}

func entryIDs(nodes []clicore.StatusNode) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	return ids
}

// TestCloudStatusPrintsEveryLineWhenSignedOut: without a session or a link,
// the whoami line fails and the command exits 1; the workspace lines are
// unknown with the reason, and the registries line, set up per machine, still
// answers.
func TestCloudStatusPrintsEveryLineWhenSignedOut(t *testing.T) {
	env := hometest.Env(t.TempDir(), map[string]string{"PUTNAMI_AUTH_URL": testBaseURL, "PUTNAMI_WORKSPACE_ROOT": t.TempDir()})
	output, err := runCloudStatus(t, env, "--output=json")
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("cloud status error = %v, want exit %d", err, clicore.ExitFailure)
	}
	assertContains(t, err.Error(), "workspace is failing: whoami failing")
	var got statusOutput
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if ids := entryIDs(got.Entries); !slices.Equal(ids, statusEntryOrder) {
		t.Fatalf("entries = %v, want %v", ids, statusEntryOrder)
	}
	if got.State != clicore.StatusFailing {
		t.Fatalf("state = %s, want failing", got.State)
	}
	byID := map[string]clicore.StatusNode{}
	for _, node := range got.Entries {
		byID[node.ID] = node
		if !clicore.ValidStatusState(node.State) {
			t.Fatalf("entry %s state = %q, outside the vocabulary", node.ID, node.State)
		}
	}
	if whoami := byID["whoami"]; whoami.State != clicore.StatusFailing || whoami.Fix != "putnami cloud login" || len(whoami.Children) != 2 {
		t.Fatalf("whoami = %+v, want failing with the login command", whoami)
	}
	if registries := byID["registries"]; strings.Contains(registries.Detail, "sign-in") {
		t.Fatalf("registries = %+v, want the machine's own answer", registries)
	}
	for _, id := range statusEntryOrder[1:] {
		if id == "registries" {
			continue
		}
		node := byID[id]
		if node.State != clicore.StatusUnknown || !strings.Contains(node.Detail, "sign-in") || node.Fix != statusMore[id] {
			t.Fatalf("%s = %+v, want unknown naming the sign-in and %q", id, node, statusMore[id])
		}
	}
}

func TestCloudStatusTextNamesTheCommandThatShowsMore(t *testing.T) {
	env := hometest.Env(t.TempDir(), map[string]string{"PUTNAMI_AUTH_URL": testBaseURL, "PUTNAMI_WORKSPACE_ROOT": t.TempDir()})
	output, err := runCloudStatus(t, env)
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("cloud status error = %v, want exit %d", err, clicore.ExitFailure)
	}
	lines := strings.Split(strings.Join(output, "\n"), "\n")
	if len(lines) != len(statusEntryOrder)+1 {
		t.Fatalf("status printed %d lines, want a header and %d entries:\n%s", len(lines), len(statusEntryOrder), strings.Join(lines, "\n"))
	}
	if fields := strings.Fields(lines[0]); !slices.Equal(fields, []string{"STATE", "ENTRY", "DETAIL", "NEXT"}) {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "failing   whoami") || !strings.HasSuffix(lines[1], "putnami cloud login") {
		t.Fatalf("whoami line = %q", lines[1])
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "unknown   env") || !strings.HasSuffix(last, "putnami cloud env status") {
		t.Fatalf("env line = %q", last)
	}
	for _, id := range statusEntryOrder {
		if statusMore[id] == "" {
			t.Fatalf("entry %s names no command that shows more", id)
		}
	}
}

// TestCloudStatusProbesTheLinkedWorkspace: signed in and linked, every remote
// line runs against the control plane. The fake answers 404 for the domain
// routes, so those lines are unknown; every line still prints.
func TestCloudStatusProbesTheLinkedWorkspace(t *testing.T) {
	previous := statusProbeTimeout
	statusProbeTimeout = 5 * time.Second
	t.Cleanup(func() { statusProbeTimeout = previous })
	home := t.TempDir()
	root := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, root)
	env := hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL, "PUTNAMI_WORKSPACE_ROOT": root})
	output, err := runCloudStatus(t, env, "--output=json")
	if err != nil && clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("cloud status: %v", err)
	}
	var got statusOutput
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if ids := entryIDs(got.Entries); !slices.Equal(ids, statusEntryOrder) {
		t.Fatalf("entries = %v, want %v", ids, statusEntryOrder)
	}
	whoami := got.Entries[0]
	session, _ := whoami.Child("whoami.session")
	link, _ := whoami.Child("whoami.workspace")
	if whoami.State != clicore.StatusOK || !strings.HasPrefix(session.Detail, "signed in") {
		t.Fatalf("whoami = %+v, want ok", whoami)
	}
	if link.State != clicore.StatusOK || !strings.Contains(link.Detail, "ws-acme") {
		t.Fatalf("whoami link = %+v, want linked to ws-acme", link)
	}
	for _, node := range got.Entries {
		if !clicore.ValidStatusState(node.State) || node.Detail == "" {
			t.Fatalf("entry %+v has no state or detail", node)
		}
	}
}

func TestCloudStatusRefusesArgumentsAndTheDeploymentTable(t *testing.T) {
	env := hometest.Env(t.TempDir(), map[string]string{"PUTNAMI_WORKSPACE_ROOT": t.TempDir()})
	for _, args := range [][]string{{"prod"}, {"--provenance"}, {"--health"}} {
		if _, err := runCloudStatus(t, env, args...); clicore.ExitCode(err) != ExitUsage {
			t.Fatalf("cloud status %v error = %v, want a usage error", args, err)
		}
	}
	_, err := runCloudStatus(t, env, "--provenance")
	assertContains(t, err.Error(), "putnami cloud env status --provenance")
	output, err := runCloudStatus(t, env, "help")
	if err != nil || !strings.Contains(strings.Join(output, "\n"), "cloud <entry> status") {
		t.Fatalf("cloud status help = %v, %q", err, output)
	}
}

// TestRunStatusProbesBoundsEachProbe: a probe that does not answer in time or
// panics is unknown, the others keep their answer, and probes never write to
// stdout.
func TestRunStatusProbesBoundsEachProbe(t *testing.T) {
	previous := statusProbeTimeout
	statusProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { statusProbeTimeout = previous })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var stdout, stderr []string
	ioctx := IO{
		Stdout: func(line string) { stdout = append(stdout, line) },
		Stderr: func(line string) { stderr = append(stderr, line) },
	}
	nodes := runStatusProbes([]remoteStatusProbe{
		{"ci", func(probeIO IO) clicore.StatusNode {
			probeIO.Stdout("probe output")
			probeIO.Stderr("probe warning")
			return clicore.StatusNode{ID: "ci", Title: "ci", State: clicore.StatusOK, Detail: "healthy"}
		}},
		{"db", func(IO) clicore.StatusNode {
			<-release
			return clicore.StatusNode{ID: "db", State: clicore.StatusOK}
		}},
		{"env", func(IO) clicore.StatusNode { panic("boom") }},
	}, ioctx)

	if nodes[0].State != clicore.StatusOK || nodes[0].Detail != "healthy" {
		t.Fatalf("ci = %+v, want its own answer", nodes[0])
	}
	if nodes[1].State != clicore.StatusUnknown || !strings.Contains(nodes[1].Detail, "no answer within") || nodes[1].Fix != "putnami cloud db status" {
		t.Fatalf("db = %+v, want unknown after the timeout", nodes[1])
	}
	if nodes[2].State != clicore.StatusUnknown || !strings.Contains(nodes[2].Detail, "boom") {
		t.Fatalf("env = %+v, want unknown naming the panic", nodes[2])
	}
	if len(stdout) != 0 || !slices.Equal(stderr, []string{"probe warning"}) {
		t.Fatalf("stdout = %q, stderr = %q, want only the warning on stderr", stdout, stderr)
	}
}
