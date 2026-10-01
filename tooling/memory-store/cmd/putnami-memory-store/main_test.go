package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

const feature = "tooling/memory-store"

// providerProcessEnv turns this test binary into the provider runtime, so a
// test can start real provider processes without building one.
const providerProcessEnv = "PUTNAMI_MEMORY_STORE_TEST_PROVIDER"

func TestMain(m *testing.M) {
	if os.Getenv(providerProcessEnv) == "1" {
		os.Exit(run([]string{providerToolArg}, os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func readManifest(t *testing.T) *extension.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := extension.ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the manifest does not parse strictly: %v", diags)
	}
	return manifest
}

// TestTheManifestPassesBothGates holds the shipped manifest to the extension
// protocol's strict validation and to the collaboration offer the orchestrator
// reads when it resolves a binding: the provider offers exactly the memory
// operations this runtime answers, with truthful annotations.
func TestTheManifestPassesBothGates(t *testing.T) {
	spectest.Proves(t, feature, "truthful-manifest", "the-manifest-passes-both-gates")
	manifest := readManifest(t)
	if manifest.Name != extensionName {
		t.Fatalf("manifest name %q, runtime answers as %q", manifest.Name, extensionName)
	}
	if diags := extension.ValidateManifest(manifest); diag.HasErrors(diags) {
		t.Fatalf("the manifest fails extension validation: %v", diags)
	}
	offer, diags := collab.ReadProviderOffer(manifest.Tools)
	if len(diags) != 0 {
		t.Fatalf("the provider declarations draw diagnostics: %v", diags)
	}
	if !slices.Equal(offer.Versions(collab.ContractMemory), []int{1}) ||
		len(offer.Versions(collab.ContractTasks)) != 0 || len(offer.Versions(collab.ContractProposals)) != 0 {
		t.Fatalf("offer %v", offer)
	}
	answered := map[collab.OperationKey]bool{}
	for key := range newHandlers() {
		answered[key] = true
	}
	operations, _ := offer.Operations(collab.ContractMemory, 1)
	for name, provided := range operations {
		key := collab.OperationKey{Contract: collab.ContractMemory, Version: 1, Operation: name}
		if !answered[key] {
			t.Errorf("the manifest offers %s, which the runtime does not answer", key)
		}
		delete(answered, key)
		definition := provided.Definition
		if !slices.Equal(definition.Args, []string{providerToolArg}) || definition.Command != "{extensionRuntime}" {
			t.Errorf("%s does not route to the runtime's provider bridge", provided.Tool)
		}
		// The git backend with a remote reaches the network; one manifest
		// serves both backends, so every tool says it may.
		if !*definition.Annotations.OpenWorldHint || !strings.Contains(definition.Description, "Only the git backend with a remote reaches the network") {
			t.Errorf("%s does not disclose that it may reach outside the machine", provided.Tool)
		}
	}
	if len(answered) != 0 {
		t.Errorf("the runtime answers operations the manifest does not offer: %v", answered)
	}
	checkpoint := operations[collab.OperationCheckpoint]
	if checkpoint.Declaration.Preconditions != collab.PreconditionsAtomic || !*checkpoint.Definition.Annotations.DestructiveHint ||
		!strings.Contains(checkpoint.Definition.Description, "never records that anything passed") {
		t.Errorf("checkpoint %+v", checkpoint.Declaration)
	}
	for _, name := range []string{collab.OperationContext, collab.OperationSearch} {
		if !operations[name].Definition.WorkspaceSelection {
			t.Errorf("%s does not take the resolved selection", name)
		}
	}
}

func TestTheRuntimeHandshake(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	var info runtimeproto.Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Extension != extensionName || info.RuntimeABI != runtimeproto.RuntimeABIVersion {
		t.Fatalf("info %+v", info)
	}
}

func TestAnUnknownInvocationIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"memory", "context"}, nil, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), providerToolArg) {
		t.Fatalf("code %d, out %q, err %q", code, out.String(), errOut.String())
	}
	if code := run([]string{providerToolArg}, strings.NewReader("{"), &out, &errOut); code != 1 {
		t.Errorf("an undecodable request exited %d", code)
	}
}

func checkpointRequest(t *testing.T, workspace, settings, key string, precondition map[string]any) []byte {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{
		"mission": "race", "identity": map[string]any{"workspace": "proof"}, "idempotencyKey": key,
		"content": "written by " + key, "precondition": precondition,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(extension.ToolCallRequest{
		Name: "memory-store.memory.checkpoint", Arguments: arguments, WorkspaceRoot: workspace,
		Provider: &extension.ToolProviderCall{Contract: collab.ContractMemory, Version: 1, Operation: collab.OperationCheckpoint,
			Settings: json.RawMessage(settings)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// provide runs one provider process on request and returns its response.
func provide(request []byte) (*collab.Response, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), providerProcessEnv+"=1")
	cmd.Stdin = bytes.NewReader(request)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("provider process: %w: %s", err, stderr.String())
	}
	var result extension.ToolCallResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("provider output %q: %w", stdout.String(), err)
	}
	response, diags := collab.ParseResponse([]byte(result.Content[0].Text))
	if diags != nil {
		return nil, fmt.Errorf("provider response %s: %v", result.Content[0].Text, diags)
	}
	return response, nil
}

// TestConcurrentProviderProcessesCheckpointOnce races provider processes —
// separate operating-system processes, as concurrent agents are — on one
// store, for each backend: of the creations exactly one lands, and of the
// checkpoints against the revision it produced exactly one lands.
func TestConcurrentProviderProcessesCheckpointOnce(t *testing.T) {
	spectest.Proves(t, feature, "compare-and-set-checkpoints", "concurrent-processes-land-once")
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("the Git backend's tests need git on PATH: %v", err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	remote := filepath.Join(t.TempDir(), "memory.git")
	if out, err := exec.Command("git", "init", "--bare", "--quiet", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for name, settings := range map[string]string{
		"file":       `{"backend":"file"}`,
		"git":        `{"backend":"git"}`,
		"git-remote": `{"backend":"git","remote":` + string(mustJSON(t, remote)) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			const processes = 5
			race := func(precondition map[string]any, round string) (winner collab.MemoryRecord) {
				t.Helper()
				requests := make([][]byte, processes)
				for i := range processes {
					requests[i] = checkpointRequest(t, workspace, settings, fmt.Sprintf("%s-%d", round, i), precondition)
				}
				responses := make([]*collab.Response, processes)
				errs := make([]error, processes)
				var wg sync.WaitGroup
				for i := range processes {
					wg.Go(func() { responses[i], errs[i] = provide(requests[i]) })
				}
				wg.Wait()
				winners, conflicts := 0, 0
				for i, response := range responses {
					if errs[i] != nil {
						t.Fatal(errs[i])
					}
					switch response.Outcome {
					case collab.OutcomeOK:
						winners++
						var result collab.MemoryCheckpointResult
						if err := json.Unmarshal(response.Result, &result); err != nil {
							t.Fatal(err)
						}
						winner = result.Record
					case collab.OutcomeConflict:
						conflicts++
					default:
						t.Fatalf("a process answered %s %+v", response.Outcome, response.Error)
					}
				}
				if winners != 1 || conflicts != processes-1 {
					t.Fatalf("%s: %d processes landed and %d conflicted; exactly one lands", round, winners, conflicts)
				}
				return winner
			}
			created := race(map[string]any{"mustNotExist": true}, "create")
			updated := race(map[string]any{"expectedRevision": created.Revision}, "update")
			if !strings.HasPrefix(updated.Revision, "2.") {
				t.Fatalf("the update wrote revision %s", updated.Revision)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
