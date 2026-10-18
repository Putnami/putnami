package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestPrepareJobInvocationResolvesToolchainBeforeBuildingEnvironment(t *testing.T) {
	workspaceRoot := t.TempDir()
	putnamiHome := filepath.Join(t.TempDir(), "putnami-home")
	realRoot := filepath.Join(putnamiHome, "tools", "compiler-1.2.3", "root")
	realBinary := writeRuntimeTool(t, filepath.Join(realRoot, "bin", "compiler"), "1.2.3")
	realBinary, err := filepath.EvalSymlinks(realBinary)
	if err != nil {
		t.Fatal(err)
	}
	realRoot = filepath.Dir(filepath.Dir(realBinary))
	wrongBin := t.TempDir()
	writeRuntimeTool(t, filepath.Join(wrongBin, "compiler"), "9.9.9")
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	t.Setenv("PUTNAMI_HOME", putnamiHome)
	t.Setenv("PATH", wrongBin)

	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{{
		From: extensionproto.RuntimeToolchainCandidatePutnamiHome,
		Path: "tools/compiler-{version}/root/bin/compiler",
	}}
	ext := runtimeToolchainExtension(requirement)
	project := &workspace.Project{ID: "/provider", Name: "provider", Path: "."}
	ws := workspace.NewWorkspace(workspaceRoot, nil, []*workspace.Project{project})
	job := &ScheduledJob{
		Project:   project,
		Extension: ext,
		JobDef: &model.JobDefinition{
			Name: "generate", Kind: "command", Command: "/bin/true", Toolchains: []string{"compiler"},
		},
	}

	_, cancel, invocation, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer func() { _ = os.Remove(invocation.contextFile) }()
	if got := envLastValue(invocation.env, "COMPILER_ROOT"); got != realRoot {
		t.Fatalf("invocation compiler root = %q, want %q", got, realRoot)
	}
	paths := filepath.SplitList(envLastValue(invocation.env, "PATH"))
	if len(paths) == 0 || filepath.Clean(paths[0]) != filepath.Dir(realBinary) {
		t.Fatalf("invocation PATH = %q, want the resolved compiler first", envLastValue(invocation.env, "PATH"))
	}
	// The mismatching ambient directory stays reachable — it carries other
	// tools — but it can no longer answer the bare name.
	if !slices.Contains(paths, wrongBin) {
		t.Fatalf("invocation PATH = %q, dropped the ambient directory entirely", envLastValue(invocation.env, "PATH"))
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "minimal-child-environment",
		"the-pinned-tool-answers-every-bare-name-lookup")
}

func TestPrepareJobInvocationResolvedToolchainPrecedesMismatchedCLISibling(t *testing.T) {
	workspaceRoot := t.TempDir()
	putnamiHome := filepath.Join(t.TempDir(), "putnami-home")
	toolName := fmt.Sprintf("putnami-runtime-tool-%d", os.Getpid())
	resolvedBinary := writeRuntimeTool(t, filepath.Join(putnamiHome, "tools", "compiler-1.2.3", "bin", toolName), "1.2.3")
	resolvedBinary, err := filepath.EvalSymlinks(resolvedBinary)
	if err != nil {
		t.Fatal(err)
	}
	cliExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A file named like the pinned tool beside the CLI. Nothing starts it: the
	// one candidate is below PUTNAMI_HOME.
	cliSibling := toolchain.WithExecutableSuffix(runtime.GOOS, filepath.Join(filepath.Dir(cliExecutable), toolName))
	writeExecutable(t, cliSibling, "9.9.9\n")
	t.Cleanup(func() { _ = os.Remove(cliSibling) })
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	t.Setenv("PUTNAMI_HOME", putnamiHome)

	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{{
		From: extensionproto.RuntimeToolchainCandidatePutnamiHome,
		Path: "tools/compiler-{version}/bin/" + toolName,
	}}
	ext := runtimeToolchainExtension(requirement)
	project := &workspace.Project{ID: "/provider", Name: "provider", Path: "."}
	ws := workspace.NewWorkspace(workspaceRoot, nil, []*workspace.Project{project})
	job := &ScheduledJob{
		Project:   project,
		Extension: ext,
		JobDef: &model.JobDefinition{
			Name: "generate", Kind: "command", Command: "/bin/true", Toolchains: []string{"compiler"},
		},
	}

	_, cancel, invocation, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer func() { _ = os.Remove(invocation.contextFile) }()
	paths := filepath.SplitList(envLastValue(invocation.env, "PATH"))
	if len(paths) == 0 || filepath.Clean(paths[0]) != filepath.Dir(resolvedBinary) {
		t.Fatalf("invocation PATH = %q, want the resolved compiler first", envLastValue(invocation.env, "PATH"))
	}
	// The directory beside the CLI carries a mismatching copy of the pinned
	// name. It stays on PATH — the CLI lives there — but the resolved compiler
	// precedes it, so the bare name never reaches the sibling. The exact CLI
	// process stays addressable through its explicit variable either way.
	resolvedIndex := slices.Index(paths, filepath.Dir(resolvedBinary))
	cliIndex := slices.Index(paths, filepath.Dir(cliExecutable))
	if resolvedIndex < 0 || cliIndex < 0 || resolvedIndex >= cliIndex {
		t.Fatalf("invocation PATH = %q, want the resolved compiler before the CLI directory",
			envLastValue(invocation.env, "PATH"))
	}
	if got := envLastValue(invocation.env, registryproto.CLIExecutableEnv); got != cliExecutable {
		t.Fatalf("CLI executable variable = %q, want %q", got, cliExecutable)
	}
}

func TestPrepareJobInvocationFailsBeforeEnvironmentForWrongRequiredPin(t *testing.T) {
	workspaceRoot := t.TempDir()
	wrongBin := t.TempDir()
	writeRuntimeTool(t, filepath.Join(wrongBin, "compiler"), "9.9.9")
	writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
	t.Setenv("PATH", wrongBin)

	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	project := &workspace.Project{ID: "/provider", Name: "provider", Path: "."}
	ws := workspace.NewWorkspace(workspaceRoot, nil, []*workspace.Project{project})
	job := &ScheduledJob{
		Project:   project,
		Extension: ext,
		JobDef: &model.JobDefinition{
			Name: "generate", Kind: "command", Command: "/bin/true", Toolchains: []string{"compiler"},
		},
	}

	_, _, _, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `no candidate matched locked version "1.2.3"`) {
		t.Fatalf("wrong-pin invocation error = %v", err)
	}
}
