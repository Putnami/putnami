//go:build unix

package main

import (
	"os"
	stdexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// emptyDescriptorHelperEnv names the file the workspace-fetch helper of
// TestRunWorkspaceFetch_AHandedEmptyCredentialStartsNoCredentialChild writes
// its status to.
const emptyDescriptorHelperEnv = "PUTNAMI_TS_EMPTY_DESCRIPTOR_HELPER_RESULT"

// TestWorkspaceFetchEmptyDescriptorHelper is the workspace-fetch job process
// that TestRunWorkspaceFetch_AHandedEmptyCredentialStartsNoCredentialChild
// starts. It reads the job credential and refreshes the native one with the
// production functions; only bun is mocked. It does nothing in any other run.
func TestWorkspaceFetchEmptyDescriptorHelper(t *testing.T) {
	resultPath := os.Getenv(emptyDescriptorHelperEnv)
	if resultPath == "" {
		return
	}
	ctx := fetchUnitWorkspace(t)
	privateTempDir(t)
	mockBunResolution(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		switch {
		case slices.Equal(args, []string{"--version"}):
			return &exec.Result{Success: true, Stdout: "1.4.0\n"}, nil
		case slices.Equal(args, []string{"pm", "cache"}):
			return &exec.Result{Success: true, Stdout: cacheDir}, nil
		}
		return &exec.Result{Success: true}, nil
	})
	status, _, err := runWorkspaceFetch(ctx, jsonl.New(), nil)
	result := status
	if err != nil {
		result += " " + err.Error()
	}
	if err := os.WriteFile(resultPath, []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
}

// When the provider of a hosted run holds no read credential,
// the engine hands workspace-fetch "{}". The fetch then starts no
// `putnami cloud registry-token` child: that child is a CLI without the run
// credential. Without a handed descriptor the same fetch refreshes the native
// credential through the child.
func TestRunWorkspaceFetch_AHandedEmptyCredentialStartsNoCredentialChild(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hand        bool
		wantStarted bool
	}{
		{name: "no descriptor", wantStarted: true},
		{name: "an empty descriptor", hand: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "credential-child")
			cli := filepath.Join(dir, "putnami")
			script := "#!/bin/sh\necho \"$*\" >> '" + marker + "'\n"
			if err := os.WriteFile(cli, []byte(script), 0o755); err != nil { //nolint:gosec // an executable test double
				t.Fatal(err)
			}

			var env []string
			for _, entry := range os.Environ() {
				name, _, _ := strings.Cut(entry, "=")
				switch name {
				case extensionproto.OfflineDependenciesEnv, extensionproto.JobCredentialFDEnv, registry.CLIExecutableEnv:
					continue
				}
				env = append(env, entry)
			}
			resultPath := filepath.Join(dir, "result")
			env = append(env, emptyDescriptorHelperEnv+"="+resultPath, registry.CLIExecutableEnv+"="+cli)

			cmd := stdexec.Command(os.Args[0], "-test.run=^TestWorkspaceFetchEmptyDescriptorHelper$", "-test.count=1")
			if tc.hand {
				credential, err := os.CreateTemp(dir, "descriptor")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = credential.Close() }()
				if _, err := credential.WriteString("{}\n"); err != nil {
					t.Fatal(err)
				}
				if _, err := credential.Seek(0, 0); err != nil {
					t.Fatal(err)
				}
				cmd.ExtraFiles = make([]*os.File, credentialFD-2)
				cmd.ExtraFiles[credentialFD-3] = credential
				env = append(env, extensionproto.JobCredentialFDEnv+"="+strconv.Itoa(credentialFD))
			}
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("workspace-fetch process: %v\n%s", err, out)
			}
			if result, err := os.ReadFile(resultPath); err != nil || string(result) != "OK" {
				t.Fatalf("workspace-fetch = %q (%v), want OK", result, err)
			}

			_, err := os.Stat(marker)
			if started := err == nil; started != tc.wantStarted {
				t.Fatalf("the credential child started: %v, want %v", started, tc.wantStarted)
			}
		})
	}
}
