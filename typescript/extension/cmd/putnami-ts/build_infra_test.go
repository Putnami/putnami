package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/infra"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/infraagg"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Bun does not serve h2c, and the platform may default a service to HTTP/2, so
// the statement has to be explicit — an ABSENT protocols block would be read as
// "take the platform default".
func TestDisableHTTP2_SetsTheExplicitOptOut(t *testing.T) {
	rt := infra.DefaultRuntime()
	disableHTTP2(rt)

	if rt.Protocols == nil || rt.Protocols.HTTP2 == nil {
		t.Fatalf("protocols = %+v, want an explicit http2 statement", rt.Protocols)
	}
	if *rt.Protocols.HTTP2 {
		t.Error("http2 = true, want false for a Bun-backed workload")
	}
}

// An authored runtime.json asking for HTTP/2 is asking for something the
// runtime cannot do; compatibility outranks the request.
func TestDisableHTTP2_OverridesAnAuthoredRequest(t *testing.T) {
	enabled := true
	rt := &infra.Runtime{Protocols: &infra.RuntimeProtocols{HTTP2: &enabled}}

	disableHTTP2(rt)

	if rt.Protocols.HTTP2 == nil || *rt.Protocols.HTTP2 {
		t.Errorf("http2 = %v, want false even when the project asked for it", rt.Protocols.HTTP2)
	}
}

func TestDisableHTTP2_NilRuntimeIsANoOp(_ *testing.T) {
	disableHTTP2(nil) // must not panic
}

// End to end through the registered task: a TypeScript workload's emitted
// manifest and its defaults sidecar both carry the opt-out.
func TestBuildInfra_EmitsTypeScriptRuntimeCompatibility(t *testing.T) {
	root := t.TempDir()
	appRoot := filepath.Join(root, "app")
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Project: pctx.Project{
			Name:     "@example/app",
			Path:     "app",
			FullPath: appRoot,
			Type:     "application",
			DependencyClosure: []pctx.ProjectRef{
				{ID: "/app", Name: "@example/app", Path: "app", FullPath: appRoot},
			},
		},
	}

	status, data, err := buildInfra()(ctx, jsonl.NewForVersion(1), nil)
	if err != nil {
		t.Fatalf("build-infra: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if data["outcome"] != string(infraagg.OutcomeEmitted) {
		t.Fatalf("outcome = %v, want %q", data["outcome"], infraagg.OutcomeEmitted)
	}

	manifest := readRuntimeBlock(t, filepath.Join(appRoot, ".gen", "requirements.json"))
	if manifest.Protocols == nil || manifest.Protocols.HTTP2 == nil || *manifest.Protocols.HTTP2 {
		t.Errorf("aggregated runtime.protocols.http2 = %+v, want false", manifest.Protocols)
	}
	sidecar := readRuntimeFile(t, filepath.Join(appRoot, ".gen", "infra", "runtime.json"))
	if sidecar.Protocols == nil || sidecar.Protocols.HTTP2 == nil || *sidecar.Protocols.HTTP2 {
		t.Errorf("defaults sidecar runtime.protocols.http2 = %+v, want false", sidecar.Protocols)
	}
}

func readRuntimeBlock(t *testing.T, path string) *infra.Runtime {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m infra.AggregatedManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	if m.Runtime == nil {
		t.Fatalf("%s carries no runtime block", path)
	}
	return m.Runtime
}

func readRuntimeFile(t *testing.T, path string) *infra.Runtime {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var rt infra.Runtime
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return &rt
}
