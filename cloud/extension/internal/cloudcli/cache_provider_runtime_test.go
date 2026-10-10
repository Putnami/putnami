package cloudcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestCacheProviderTaskStartsTheNativeRuntime pins how the CLI starts the
// remote build-cache provider, which holds the run credential on a hosted run.
//
// A credential holder must be one native executable that the CLI starts
// directly. A shell launcher reads more files from the extension store after
// it starts, and a repository process may already have rewritten them. So the
// task runs {extensionRuntime}, the same compiled/putnami-cloud every other
// task runs, with no script in between.
//
// The second half proves that those exact args reach the provider: the runtime
// dispatches them through RunMain, as cmd/putnami-cloud does, and the process
// answers the provider RPC.
func TestCacheProviderTaskStartsTheNativeRuntime(t *testing.T) {
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
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Cwd     string   `json:"cwd"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}

	provider, ok := manifest.Commands["cache-provider"]
	if !ok || len(provider.Run) != 1 || provider.Run[0].Task != "cloud-cache-provider" {
		t.Fatalf("cache-provider command = %+v, want one step running cloud-cache-provider", provider)
	}
	task, ok := manifest.Tasks["cloud-cache-provider"]
	if !ok {
		t.Fatal("manifest declares no cloud-cache-provider task")
	}
	if task.Command != "{extensionRuntime}" {
		t.Fatalf("cloud-cache-provider command = %q, want {extensionRuntime}: a credential holder must be the native runtime, not a launcher", task.Command)
	}
	if want := []string{"cache-provider"}; !reflect.DeepEqual(task.Args, want) {
		t.Fatalf("cloud-cache-provider args = %q, want %q", task.Args, want)
	}
	if task.Cwd != "{workspaceRoot}" {
		t.Fatalf("cloud-cache-provider cwd = %q, want {workspaceRoot}", task.Cwd)
	}

	stdout := serveCacheProviderOnce(t, task.Args, `{"protocolVersion":1,"id":1,"op":"shutdown"}`+"\n")
	type providerResponse struct {
		ProtocolVersion int             `json:"protocolVersion"`
		ID              int64           `json:"id"`
		OK              bool            `json:"ok"`
		Error           json.RawMessage `json:"error"`
	}
	var responses []providerResponse
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var response providerResponse
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatalf("provider wrote a non-JSON line %q: %v", line, err)
		}
		responses = append(responses, response)
	}
	if len(responses) != 1 {
		t.Fatalf("provider wrote %d responses %s, want the one shutdown ack", len(responses), stdout)
	}
	if got := responses[0]; got.ID != 1 || !got.OK || got.ProtocolVersion != 1 || len(got.Error) != 0 {
		t.Fatalf("shutdown ack = %+v, want protocolVersion 1, id 1, ok and no error", got)
	}
}

// serveCacheProviderOnce runs RunMain with argv the way cmd/putnami-cloud does:
// the provider reads the process stdin and writes the process stdout, so both
// are swapped for pipes for the length of the call. It returns what the
// provider wrote and fails the test unless RunMain exits with ExitSuccess.
func serveCacheProviderOnce(t *testing.T, argv []string, requests string) []byte {
	t.Helper()
	return serveRuntimeOnce(t, argv, IO{Env: map[string]string{"PUTNAMI_WORKSPACE_ROOT": t.TempDir()}}, requests)
}

// serveRuntimeOnce is serveCacheProviderOnce with the IO RunMain receives.
func serveRuntimeOnce(t *testing.T, argv []string, ioctx IO, requests string) []byte {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	oldStdin, oldStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = inR.Close()
		_ = outR.Close()
	})

	if _, err := io.WriteString(inW, requests); err != nil {
		t.Fatalf("write provider requests: %v", err)
	}
	if err := inW.Close(); err != nil {
		t.Fatalf("close provider stdin: %v", err)
	}
	read := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(outR)
		read <- data
	}()

	code := RunMain(argv, ioctx)
	os.Stdin, os.Stdout = oldStdin, oldStdout
	if err := outW.Close(); err != nil {
		t.Fatalf("close provider stdout: %v", err)
	}
	stdout := <-read
	if code != ExitSuccess {
		t.Fatalf("RunMain(%q) exit = %d, want %d; stdout %s", argv, code, ExitSuccess, stdout)
	}
	return stdout
}
