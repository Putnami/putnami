package cloudcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	deliverycli "go.putnami.dev/cloud/extension/internal/deliverycli"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
)

// TestInstallLeavesTheSessionModeToTheRun runs install on a fresh checkout of a
// linked workspace, then starts the cache provider the way a run does. Install
// records no mode when none is requested, so the provider picks minimal on a
// hosted run and full on a developer machine. A file that already records a
// mode keeps it, and the provider uses it on either.
func TestInstallLeavesTheSessionModeToTheRun(t *testing.T) {
	spectest.Proves(t, "cloud/cache-provider-configuration", "an-unrequested-mode-is-left-to-the-run",
		"install-writes-no-mode-and-the-run-picks-minimal-when-hosted-else-full")
	for _, tc := range []struct {
		name     string
		recorded string
		hosted   bool
		want     cache.Mode
	}{
		{name: "hosted run, no cache file", hosted: true, want: cache.ModeMinimal},
		{name: "developer machine, no cache file", want: cache.ModeFull},
		{name: "hosted run, file records full", recorded: "full", hosted: true, want: cache.ModeFull},
		{name: "developer machine, file records toplevel", recorded: "toplevel", want: cache.ModeToplevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearCacheOverrides(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PUTNAMI_CLOUD_TOKEN", "")
			ingest := ""
			if tc.hosted {
				ingest = "https://ingest.invalid"
			}
			t.Setenv(deliverycli.SessionReporterIngestURLEnv, ingest)

			workspaceRoot := linkedWorkspace(t)
			if tc.recorded != "" {
				if err := os.MkdirAll(filepath.Join(workspaceRoot, ".putnami"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), map[string]any{
					"enabled": true, "url": "https://cache.invalid", "mode": tc.recorded,
				})
			}
			env := hometest.Env(home, map[string]string{"PUTNAMI_WORKSPACE_ROOT": workspaceRoot})
			if err := RunCommand("install", IO{
				Env:    env,
				Stdout: func(string) {},
				Stderr: func(string) {},
				Client: newFakeTransport(t).client(),
				Now:    fixedNow,
			}, nil); err != nil {
				t.Fatalf("install: %v", err)
			}

			var written map[string]any
			readJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), &written)
			if got, _ := written["mode"].(string); got != tc.recorded {
				t.Fatalf("cache.json mode = %q, want %q: install writes a mode only when one is requested and keeps a recorded one", got, tc.recorded)
			}

			if got := providerSessionMode(t, env); got != tc.want {
				t.Fatalf("session mode = %q, want %q", got, tc.want)
			}
		})
	}
}

// linkedWorkspace is a workspace root whose manifest links a Cloud workspace
// and that holds no .putnami/cache.json, as a fresh hosted checkout does.
func linkedWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeJSONFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{
		"name": "local",
		"options": map[string]any{
			"@putnami/cloud": map[string]any{
				"workspace": map[string]any{
					"version":           1,
					"control_plane_url": testBaseURL,
					"workspace_id":      "ws-acme",
					"environment":       "prod",
				},
			},
		},
	})
	return root
}

// providerSessionMode starts the cache provider runtime, sends the initialize
// a run sends with provider-configuration, and returns the mode it resolved.
func providerSessionMode(t *testing.T, env map[string]string) cache.Mode {
	t.Helper()
	payload, err := cache.MarshalPayload(&cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: t.TempDir(),
		Capabilities:    []string{cache.CapabilityProviderConfiguration},
	})
	if err != nil {
		t.Fatal(err)
	}
	initialize, err := json.Marshal(&cache.ProviderRequest{
		ProtocolVersion: cache.ProviderProtocolMinVersion, ID: 1, Op: cache.OpInitialize, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := string(initialize) + "\n" + `{"protocolVersion":1,"id":2,"op":"shutdown"}` + "\n"
	stdout := serveRuntimeOnce(t, []string{"cache-provider"}, IO{Env: env}, requests)

	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var response cache.ProviderResponse
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("provider wrote a non-JSON line %q: %v", scanner.Bytes(), err)
		}
		if response.ID != 1 {
			continue
		}
		if !response.OK {
			t.Fatalf("initialize not OK: %+v", response.Error)
		}
		result, diags := cache.ParseAndValidateInitializeResult(response.Payload)
		if result == nil {
			t.Fatalf("invalid initialize result: %v", diags)
		}
		return result.Mode
	}
	t.Fatalf("no initialize response in %s", stdout)
	return ""
}
