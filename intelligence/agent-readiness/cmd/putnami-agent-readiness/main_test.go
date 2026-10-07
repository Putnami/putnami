package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/intelligence/agent-readiness/internal/testrepo"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

func contextFile(t *testing.T, callerDir string, params map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "context.json")
	data, err := json.Marshal(map[string]any{
		"workspaceRoot": t.TempDir(),
		"userScope":     map[string]string{"callerDir": callerDir},
		"params":        params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStandaloneRuntimeUsesPublicContract(t *testing.T) {
	spectest.Proves(t, "intelligence/agent-readiness", "public-extension", "standalone-runtime-uses-public-contract")
	var out, errOut bytes.Buffer
	code := runMain([]string{"__putnami", "runtime-info"}, &out, &errOut, nil)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errOut.String())
	}
	var descriptor struct {
		Extension       string `json:"extension"`
		Version         string `json:"version"`
		CLIContract     int    `json:"cliContract"`
		RuntimeProtocol int    `json:"runtimeProtocol"`
		RuntimeABI      int    `json:"runtimeABI"`
	}
	if err := json.Unmarshal(out.Bytes(), &descriptor); err != nil {
		t.Fatal(err)
	}
	if descriptor.Extension != extensionName || descriptor.Version != runtimeVersion || descriptor.CLIContract != protocolcli.CurrentContract || descriptor.RuntimeProtocol == 0 || descriptor.RuntimeABI == 0 {
		t.Fatalf("descriptor=%+v", descriptor)
	}
}

func TestUserScopeCallerDirectoryAndStructuredEnvelope(t *testing.T) {
	repo := testrepo.New(t)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "" {
			t.Error("submission carried authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"8f3k2q","url":"https://putnami.app/r/8f3k2q","methodVersion":"0.1","level":"L2","shareBlocked":0.63}`)
	}))
	defer server.Close()
	t.Setenv("PUTNAMI_CLOUD_API_URL", server.URL)
	t.Setenv("PUTNAMI_CONTROL_PLANE_URL", "http://127.0.0.1:1")
	context := contextFile(t, repo, map[string]any{"output": "json", "timeout": 30})
	var out, errOut bytes.Buffer
	code := runMain([]string{"agent-readiness", "--putnamiContext", context}, &out, &errOut, server.Client())
	if code != 0 || requests != 1 || errOut.Len() != 0 {
		t.Fatalf("exit=%d requests=%d stderr=%q", code, requests, errOut.String())
	}
	var envelope struct {
		Command string `json:"command"`
		Status  string `json:"status"`
		Data    struct {
			Report  string `json:"report"`
			Commits int    `json:"commits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("stdout=%q: %v", out.String(), err)
	}
	if envelope.Command != "agent-readiness" || envelope.Status != "success" || envelope.Data.Commits != 4 || envelope.Data.Report != "https://putnami.app/r/8f3k2q" {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestStructuredErrorUsesSharedEnvelope(t *testing.T) {
	context := contextFile(t, t.TempDir(), map[string]any{"output": "json", "timeout": 0})
	var out, errOut bytes.Buffer
	code := runMain([]string{"agent-readiness", "--putnamiContext", context}, &out, &errOut, nil)
	if code != protocolcli.ExitUsage {
		t.Fatalf("exit=%d stderr=%q", code, errOut.String())
	}
	var envelope struct {
		Command string `json:"command"`
		Status  string `json:"status"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("stdout=%q: %v", out.String(), err)
	}
	if envelope.Command != "agent-readiness" || envelope.Status != "failure" || envelope.Error.Code != "usage" || !strings.Contains(errOut.String(), "--timeout") {
		t.Fatalf("envelope=%+v stderr=%q", envelope, errOut.String())
	}
}

func TestPrintPayloadDoesNotSendThroughRuntime(t *testing.T) {
	repo := testrepo.New(t)
	context := contextFile(t, repo, map[string]any{"print-payload": true})
	t.Setenv("PUTNAMI_CLOUD_API_URL", "http://127.0.0.1:1")
	var out, errOut bytes.Buffer
	if code := runMain([]string{"agent-readiness", "--putnamiContext", context}, &out, &errOut, nil); code != 0 || errOut.Len() != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}
