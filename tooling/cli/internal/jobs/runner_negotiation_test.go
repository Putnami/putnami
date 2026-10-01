package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	registryproto "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func negotiationTestInvocation(t *testing.T, extraEnv []string) *jobInvocation {
	t.Helper()
	ws := &workspace.Workspace{
		Name:   "test-workspace",
		Root:   t.TempDir(),
		Config: &wsproto.Config{},
	}
	job := &ScheduledJob{
		Project:   &workspace.Project{Name: "my-app", Path: "packages/my-app"},
		Extension: &extension.ExtensionDescription{Name: "@putnami/go", Path: ws.Root},
		JobDef: &extension.JobDefinition{
			Name:          "serve",
			ExtensionName: "@putnami/go",
			Command:       "true",
		},
	}
	return buildJobInvocation(ws, job, BuildJobContext(ws, job, nil, nil, nil), extraEnv, "")
}

// envValues returns every value assigned to key, in order. os/exec keeps the
// LAST duplicate, so the tail is what the subprocess actually observes.
func envValues(env []string, key string) []string {
	prefix := key + "="
	var values []string
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			values = append(values, strings.TrimPrefix(entry, prefix))
		}
	}
	return values
}

// TestJobEnv_AdvertisesRuntimeEventVersion is the CLI half of the negotiation
// seam. Without this advertisement no first-party extension can ever emit
// typed readiness; with it, an extension's SDK resolves the same version
// through the shared helper in protocols/runtime.
func TestJobEnv_AdvertisesRuntimeEventVersion(t *testing.T) {
	inv := negotiationTestInvocation(t, nil)

	values := envValues(inv.env, runtimeproto.AcceptedVersionEnv)
	if len(values) == 0 {
		t.Fatalf("%s is absent from the job environment", runtimeproto.AcceptedVersionEnv)
	}
	advertised := values[len(values)-1]
	if got := runtimeproto.NegotiatedVersion(advertised); got != runtimeproto.MaxKnownProtocolVersion {
		t.Errorf("advertised %q resolves to version %d, want %d",
			advertised, got, runtimeproto.MaxKnownProtocolVersion)
	}
	// The advertisement must be true: what the CLI claims to accept is what
	// parseRawEvent actually parses.
	if !runtimeproto.IsKnownProtocolVersion(runtimeproto.NegotiatedVersion(advertised)) {
		t.Error("the CLI advertised a version it cannot parse")
	}
}

// TestJobEnv_AdvertisesAbsoluteCLIExecutable pins the launcher handoff used by
// extension-owned SDK seams that need to call back into the CLI. A locally
// bootstrapped CLI is not necessarily named `putnami` on PATH, so the child must
// receive the absolute executable that actually spawned it.
func TestJobEnv_AdvertisesAbsoluteCLIExecutable(t *testing.T) {
	t.Setenv(registryproto.CLIExecutableEnv, "inherited-must-not-win")
	inv := negotiationTestInvocation(t, []string{registryproto.CLIExecutableEnv + "=caller-must-not-win"})

	values := envValues(inv.env, registryproto.CLIExecutableEnv)
	if len(values) != 1 {
		t.Fatalf("%s values = %v, want one authoritative CLI value", registryproto.CLIExecutableEnv, values)
	}
	got := values[len(values)-1]
	if !filepath.IsAbs(got) {
		t.Fatalf("%s = %q, want an absolute path", registryproto.CLIExecutableEnv, got)
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve current CLI executable: %v", err)
	}
	if got != want {
		t.Fatalf("%s = %q, want current executable %q", registryproto.CLIExecutableEnv, got, want)
	}
}

// TestJobEnv_PrependsCLIExecutableDirectoryToPATH keeps existing extensions
// working while they migrate to PUTNAMI_CLI_EXECUTABLE. Some extension-owned
// orchestration still invokes the historical bare `putnami` name; a locally
// bootstrapped CLI is outside the caller's PATH, so its trusted directory must
// lead the child PATH without discarding the caller's toolchain entries.
func TestJobEnv_PrependsCLIExecutableDirectoryToPATH(t *testing.T) {
	inheritedPath := t.TempDir()
	t.Setenv("PATH", inheritedPath)

	inv := negotiationTestInvocation(t, nil)
	values := envValues(inv.env, "PATH")
	if len(values) != 1 {
		t.Fatalf("PATH values = %v, want one authoritative value", values)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve current CLI executable: %v", err)
	}
	parts := filepath.SplitList(values[0])
	if len(parts) < 2 {
		t.Fatalf("PATH = %q, want CLI directory followed by inherited PATH", values[0])
	}
	if got, want := parts[0], filepath.Dir(executable); got != want {
		t.Fatalf("PATH first entry = %q, want CLI directory %q", got, want)
	}
	if got := parts[len(parts)-1]; got != inheritedPath {
		t.Fatalf("PATH last entry = %q, want inherited path %q preserved", got, inheritedPath)
	}
}

// TestJobEnv_AdvertisementOverridesInheritedValue covers the nested-run case
// (putnami testing putnami): an outer CLI's advertisement is inherited through
// os.Environ(), and this process's own claim must win, because the stream is
// parsed by THIS process.
func TestJobEnv_AdvertisementOverridesInheritedValue(t *testing.T) {
	t.Setenv(runtimeproto.AcceptedVersionEnv, "1")

	inv := negotiationTestInvocation(t, nil)
	values := envValues(inv.env, runtimeproto.AcceptedVersionEnv)
	if len(values) < 2 {
		t.Fatalf("expected the inherited value and this CLI's own, got %v", values)
	}
	if got := runtimeproto.NegotiatedVersion(values[len(values)-1]); got != runtimeproto.MaxKnownProtocolVersion {
		t.Errorf("effective advertisement resolves to %d, want %d — the inherited value must not win",
			got, runtimeproto.MaxKnownProtocolVersion)
	}
}

// TestJobEnv_AdvertisementOverridesExtraEnvironment keeps the parser contract
// truthful: caller-provided task environment cannot downgrade the stream to a
// version this CLI rejects.
func TestJobEnv_AdvertisementOverridesExtraEnvironment(t *testing.T) {
	inv := negotiationTestInvocation(t, []string{runtimeproto.AcceptedVersionEnv + "=1"})

	values := envValues(inv.env, runtimeproto.AcceptedVersionEnv)
	if got := runtimeproto.NegotiatedVersion(values[len(values)-1]); got != runtimeproto.MaxKnownProtocolVersion {
		t.Errorf("effective advertisement resolves to %d, want %d", got, runtimeproto.MaxKnownProtocolVersion)
	}
}

// TestRunJob_RuntimeEventAdvertisementOverridesTaskEnvironment exercises the
// full failure mode: a task and its caller both request v1, then the task emits
// the version it actually observes. Before the CLI-owned advertisement was
// final, the v1 result was dropped and exit 0 synthesized a misleading success.
func TestRunJob_RuntimeEventAdvertisementOverridesTaskEnvironment(t *testing.T) {
	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws", Config: &wsproto.Config{}}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/site", Name: "site", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/widget", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/widget",
			Name:          "build",
			Kind:          "command",
			TimeoutMs:     unboundedJobTimeoutMs,
			Env: map[string]string{
				runtimeproto.AcceptedVersionEnv: "1",
			},
		},
	}
	// The task emits its events at the version it observes.
	version := "$" + runtimeproto.AcceptedVersionEnv
	fixtureTask(t, job.JobDef, fixtureScript{
		{"print-env", `{"v":` + version + `,"type":"summary","message":"negotiated=` + version + `"}`},
		{"print-env", `{"v":` + version + `,"type":"result","data":{"status":"success"}}`},
	})

	result, err := RunJob(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if len(result.Events) != 2 {
		t.Fatalf("events = %+v, want the summary and result emitted at the CLI's version", result.Events)
	}

	wantSummary := "negotiated=" + strconv.Itoa(runtimeproto.MaxKnownProtocolVersion)
	if got := result.Events[0]; got.Version != runtimeproto.MaxKnownProtocolVersion || got.Type != EventTypeSummary || got.Message != wantSummary {
		t.Errorf("summary event = %+v, want v=%d type=%q message=%q", got, runtimeproto.MaxKnownProtocolVersion, EventTypeSummary, wantSummary)
	}
	if got := result.Events[1]; got.Version != runtimeproto.MaxKnownProtocolVersion || got.Type != EventTypeResult {
		t.Errorf("result event = %+v, want v=%d type=%q", got, runtimeproto.MaxKnownProtocolVersion, EventTypeResult)
	}
}

// TestRunJob_NegotiatesReadinessEndToEnd closes the loop through a REAL
// subprocess: the CLI advertises, the job reads the advertisement back out of
// its own environment and answers with a v2 stream carrying a readiness event,
// and the CLI parses all of it. The two halves are unit-tested apart; this is
// the one place they meet, and it is where an env-plumbing regression (a
// dropped advertisement, a renamed variable) would actually show up.
func TestRunJob_NegotiatesReadinessEndToEnd(t *testing.T) {
	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws", Config: &wsproto.Config{}}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/site", Name: "site", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/widget", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/widget",
			Name:          "serve",
			Kind:          "command",
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	// The job echoes the negotiated version back as a summary, then emits the
	// readiness event it is only allowed to emit at v2.
	fixtureTask(t, job.JobDef, fixtureScript{
		{"print-env", `{"v":2,"type":"summary","message":"negotiated=$` + runtimeproto.AcceptedVersionEnv + `"}`},
		{"print", `{"v":2,"type":"ready","data":{"target":"server","endpoints":[{"scheme":"http","host":"localhost","port":8080}]}}`},
		{"print", `{"v":2,"type":"result","data":{"status":"success"}}`},
	})

	result, err := RunJob(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success — the v2 result event must survive parsing", result.Status)
	}

	wantSummary := "negotiated=" + strconv.Itoa(runtimeproto.MaxKnownProtocolVersion)
	var sawSummary, sawReady bool
	for _, event := range result.Events {
		if event.Version != runtimeproto.ProtocolVersion2 {
			t.Errorf("event %q carried v = %d, want %d", event.Type, event.Version, runtimeproto.ProtocolVersion2)
		}
		switch event.Type {
		case EventTypeSummary:
			if event.Message == wantSummary {
				sawSummary = true
			} else {
				t.Errorf("job saw %q, want %q", event.Message, wantSummary)
			}
		case EventTypeReady:
			sawReady = true
			ready, readyErr := runtimeproto.ExtractReadyData(event.Data)
			if readyErr != nil {
				t.Fatalf("readiness payload: %v", readyErr)
			}
			if len(ready.Endpoints) != 1 || ready.Endpoints[0].URL() != "http://localhost:8080" {
				t.Errorf("readiness endpoints = %+v", ready.Endpoints)
			}
		}
	}
	if !sawSummary {
		t.Error("the job never observed the CLI's advertisement")
	}
	if !sawReady {
		t.Errorf("the readiness event never reached the CLI; events = %+v", result.Events)
	}
}
