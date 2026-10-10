package cloudcli

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	registry "go.putnami.dev/protocol/registry"
)

// TestCredentialProviderTaskStartsTheNativeRuntime pins how the engine starts
// the registry credential provider, which receives the hosted run's credential:
// the native runtime with the credential-provider argument, like the cache
// provider, never a launcher script and never a cached task.
func TestCredentialProviderTaskStartsTheNativeRuntime(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest struct {
		Commands map[string]struct {
			Run []struct {
				Task string `json:"task"`
			} `json:"run"`
		} `json:"commands"`
		Tasks map[string]struct {
			Command string          `json:"command"`
			Args    []string        `json:"args"`
			Cwd     string          `json:"cwd"`
			Cache   json.RawMessage `json:"cache"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}
	command, ok := manifest.Commands["credential-provider"]
	if !ok || len(command.Run) != 1 || command.Run[0].Task != "cloud-credential-provider" {
		t.Fatalf("credential-provider command = %+v, want one step running cloud-credential-provider", command)
	}
	task, ok := manifest.Tasks["cloud-credential-provider"]
	if !ok {
		t.Fatal("manifest declares no cloud-credential-provider task")
	}
	if task.Command != "{extensionRuntime}" || !reflect.DeepEqual(task.Args, []string{"credential-provider"}) || task.Cwd != "{workspaceRoot}" {
		t.Fatalf("cloud-credential-provider = %+v, want {extensionRuntime} credential-provider in {workspaceRoot}", task)
	}
	if string(task.Cache) != "false" {
		t.Fatal("cloud-credential-provider must declare cache:false")
	}

	// The exact args reach the provider and it answers the RPC without a run
	// credential: initialize, publish absence, shutdown.
	stdout := serveRuntimeOnce(t, task.Args, IO{Env: map[string]string{"PUTNAMI_WORKSPACE_ROOT": t.TempDir()}}, strings.Join([]string{
		`{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"capabilities":["credential-v1"]}}`,
		`{"protocolVersion":1,"id":2,"op":"credential","payload":{"purpose":"publish"}}`,
		`{"protocolVersion":1,"id":3,"op":"shutdown"}`,
	}, "\n")+"\n")
	ops := []registry.CredentialOp{registry.CredentialOpInitialize, registry.CredentialOpCredential, registry.CredentialOpShutdown}
	lines := strings.Split(strings.TrimSpace(string(stdout)), "\n")
	if len(lines) != len(ops) {
		t.Fatalf("provider wrote %q, want %d answers", stdout, len(ops))
	}
	for i, line := range lines {
		response, err := registry.ParseCredentialResponse([]byte(line), ops[i])
		if err != nil || !response.OK || response.ID != int64(i+1) {
			t.Fatalf("answer %d = %s (%v)", i+1, line, err)
		}
	}
	if lines[1] != `{"protocolVersion":1,"id":2,"ok":true,"payload":{}}` {
		t.Fatalf("publish answer = %s, want absence", lines[1])
	}
}

// TestCredentialProviderReadsTheSignedInUsersRegistryBearer drives the whole
// developer-machine path through the runtime: a signed-in user of a linked
// checkout gets one distribution bearer for the four registry markers, and
// stdout carries nothing but protocol answers.
func TestCredentialProviderReadsTheSignedInUsersRegistryBearer(t *testing.T) {
	_, env := registryTokenFixture(t)
	capture := &userTokenCapture{}
	client := userTokenClient(t, capture, "go npm oci put")
	var stderr strings.Builder
	session := startRuntime(t, []string{"credential-provider"}, IO{
		Env: env, Client: client, Now: fixedNow,
		Stdout: func(line string) { t.Errorf("the provider wrote %q through IO.Stdout", line) },
		Stderr: func(line string) { stderr.WriteString(line) },
	})
	session.send(`{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"capabilities":["credential-v1"]}}`)
	if response := session.next(registry.CredentialOpInitialize); !response.OK {
		t.Fatalf("initialize = %+v", response)
	}
	session.send(`{"protocolVersion":1,"id":2,"op":"credential","payload":{"purpose":"read"}}`)
	response := session.next(registry.CredentialOpCredential)
	result, err := registry.ParseCredentialResult(response.Payload)
	if !response.OK || err != nil || result.Credential == nil {
		t.Fatalf("read = %+v (%v)", response, err)
	}
	want := registry.Credential{
		Bearer:    capture.bearer,
		ExpiresAt: fixedNow().Add(5 * time.Minute).UTC().Format(time.RFC3339),
		Hosts:     []string{"go.putnami.dev", "npm.putnami.dev", "oci.putnami.dev", "put.putnami.dev"},
	}
	if !reflect.DeepEqual(*result.Credential, want) {
		t.Fatalf("credential hosts %v expiry %s, want hosts %v expiry %s", result.Credential.Hosts, result.Credential.ExpiresAt, want.Hosts, want.ExpiresAt)
	}
	if capture.requestedScope != "go npm oci put" || capture.clientID != distributioncli.DefaultRegistryTokenClientID {
		t.Fatalf("minted scope %q for client %q", capture.requestedScope, capture.clientID)
	}
	session.send(`{"protocolVersion":1,"id":3,"op":"shutdown"}`)
	session.next(registry.CredentialOpShutdown)
	if code := session.wait(); code != ExitSuccess {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stderr.String(), capture.bearer) {
		t.Fatal("stderr carries the bearer")
	}
}

// runtimeSession is one RunMain call whose stdin and stdout are pipes the test
// writes and reads line by line, as the engine does.
type runtimeSession struct {
	t     *testing.T
	stdin *os.File
	lines *bufio.Scanner
	code  chan int
}

func startRuntime(t *testing.T, argv []string, ioctx IO) *runtimeSession {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin, oldStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	session := &runtimeSession{t: t, stdin: inW, lines: bufio.NewScanner(outR), code: make(chan int, 1)}
	session.lines.Buffer(make([]byte, 0, 4<<10), registry.MaxCredentialLineBytes)
	go func() {
		code := RunMain(argv, ioctx)
		_ = outW.Close()
		session.code <- code
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		session.wait()
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = inR.Close()
		_ = outR.Close()
	})
	return session
}

func (s *runtimeSession) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.stdin, line+"\n"); err != nil {
		s.t.Fatalf("write: %v", err)
	}
}

func (s *runtimeSession) next(op registry.CredentialOp) *registry.CredentialResponse {
	s.t.Helper()
	if !s.lines.Scan() {
		s.t.Fatalf("the provider closed stdout: %v", s.lines.Err())
	}
	response, err := registry.ParseCredentialResponse(s.lines.Bytes(), op)
	if err != nil {
		s.t.Fatalf("answer %s: %v", s.lines.Bytes(), err)
	}
	return response
}

func (s *runtimeSession) wait() int {
	select {
	case code := <-s.code:
		s.code <- code
		return code
	case <-time.After(10 * time.Second):
		s.t.Fatal("the provider did not exit")
	}
	return -1
}
