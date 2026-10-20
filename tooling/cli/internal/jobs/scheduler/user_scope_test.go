package scheduler

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// userScopeTestJob is the shape the CLI builds for an interactive subcommand
// that runs outside any workspace: a synthetic workspace rooted at the
// user-scope directory, its single root project, and the caller directory.
func userScopeTestJob(t *testing.T, callerDir string) (*workspace.Workspace, *ScheduledJob) {
	t.Helper()
	ws := &workspace.Workspace{Name: "user", Root: t.TempDir(), Config: &wsproto.Config{Name: "user"}}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "user", Name: "user", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@acme/audit", Path: filepath.Join(ws.Root, "ext")},
		JobDef: &extension.JobDefinition{
			Name:          "audit-run",
			ExtensionName: "@acme/audit",
			Command:       "true",
		},
	}
	if callerDir != "" {
		job.UserScope = &protocoljob.UserScope{CallerDir: callerDir}
	}
	return ws, job
}

// TestBuildJobContext_UserScopeConformsToProtocol pins the producer half of
// the userScope member: a job that runs outside a workspace emits a v2
// document the protocol's strict parser accepts, carrying the caller
// directory, while every workspace path names the user-scope directory.
func TestBuildJobContext_UserScopeConformsToProtocol(t *testing.T) {
	t.Parallel()
	callerDir := t.TempDir()
	ws, job := userScopeTestJob(t, callerDir)

	data, err := json.Marshal(BuildJobContext(ws, job, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := protocoljob.ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("user-scope context violates the job context protocol: %v\n%s", diags, data)
	}
	if parsed.UserScope == nil || parsed.UserScope.CallerDir != callerDir {
		t.Fatalf("userScope = %+v, want callerDir %q", parsed.UserScope, callerDir)
	}
	if parsed.WorkspaceRoot != ws.Root || parsed.Project.FullPath != ws.Root {
		t.Fatalf("workspaceRoot = %q, project.fullPath = %q, want the user-scope directory %q",
			parsed.WorkspaceRoot, parsed.Project.FullPath, ws.Root)
	}
}

// TestBuildJobContext_WorkspaceRunOmitsUserScope pins the other half: inside
// a workspace the member is absent, so its presence alone tells an extension
// that no workspace exists.
func TestBuildJobContext_WorkspaceRunOmitsUserScope(t *testing.T) {
	t.Parallel()
	ws, job := userScopeTestJob(t, "")
	data, err := json.Marshal(BuildJobContext(ws, job, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"userScope"`)) {
		t.Fatalf("a workspace job context carries userScope:\n%s", data)
	}
}

// TestJobInvocation_UserScopeRunsInCallerDir pins where the process runs and
// what it reads: the caller directory is the working directory and the value
// of PUTNAMI_CALLER_DIR, even when the environment inherited another value.
func TestJobInvocation_UserScopeRunsInCallerDir(t *testing.T) {
	t.Setenv(CallerDirEnv, "/inherited/outer/caller")
	callerDir := t.TempDir()
	ws, job := userScopeTestJob(t, callerDir)

	inv := buildJobInvocation(ws, job, BuildJobContext(ws, job, nil, nil, nil), nil, "")
	if inv.cwd != callerDir {
		t.Fatalf("cwd = %q, want the caller directory %q", inv.cwd, callerDir)
	}
	if got := envValues(inv.env, CallerDirEnv); len(got) != 1 || got[0] != callerDir {
		t.Fatalf("%s values = %q, want exactly [%q]", CallerDirEnv, got, callerDir)
	}
}

// TestJobInvocation_UserScopeKeepsDeclaredCwd pins that a task's own cwd
// still applies: the caller directory replaces only the default, which would
// otherwise be the user-scope directory.
func TestJobInvocation_UserScopeKeepsDeclaredCwd(t *testing.T) {
	t.Parallel()
	ws, job := userScopeTestJob(t, t.TempDir())
	job.JobDef.Cwd = "{extensionRoot}"

	inv := buildJobInvocation(ws, job, BuildJobContext(ws, job, nil, nil, nil), nil, "")
	if inv.cwd != job.Extension.Path {
		t.Fatalf("cwd = %q, want the declared extension root %q", inv.cwd, job.Extension.Path)
	}
}

// TestJobInvocation_WorkspaceRunStripsInheritedCallerDir pins that a
// workspace job never reads as a user-scope one: a value inherited from an
// outer user-scope run is removed and the job runs in its project root.
func TestJobInvocation_WorkspaceRunStripsInheritedCallerDir(t *testing.T) {
	t.Setenv(CallerDirEnv, "/inherited/outer/caller")
	ws, job := userScopeTestJob(t, "")

	inv := buildJobInvocation(ws, job, BuildJobContext(ws, job, nil, nil, nil), nil, "")
	if got := envValues(inv.env, CallerDirEnv); len(got) != 0 {
		t.Fatalf("%s reaches a workspace job: %q", CallerDirEnv, got)
	}
	if want := filepath.Join(ws.Root, "."); inv.cwd != want {
		t.Fatalf("cwd = %q, want the project root %q", inv.cwd, want)
	}
}

// interactiveTestJob is a job the CLI launches interactively: a workspace job
// that also records where the user typed the command.
func interactiveTestJob(t *testing.T, invocationDir string) (*workspace.Workspace, *ScheduledJob) {
	t.Helper()
	ws, job := userScopeTestJob(t, "")
	job.InvocationDir = invocationDir
	return ws, job
}

// TestBuildJobContext_InvocationDirConformsToProtocol pins the producer half of
// invocationDir: an interactive launch emits a v2 document the protocol's
// strict parser accepts, with no userScope, because the member says nothing
// about workspaces.
func TestBuildJobContext_InvocationDirConformsToProtocol(t *testing.T) {
	t.Parallel()
	typedIn := t.TempDir()
	ws, job := interactiveTestJob(t, typedIn)

	data, err := json.Marshal(BuildJobContext(ws, job, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := protocoljob.ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("interactive context violates the job context protocol: %v\n%s", diags, data)
	}
	if parsed.InvocationDir != typedIn {
		t.Fatalf("invocationDir = %q, want %q", parsed.InvocationDir, typedIn)
	}
	if parsed.UserScope != nil {
		t.Fatalf("userScope = %+v, want absent inside a workspace", parsed.UserScope)
	}
}

// TestBuildJobContext_PlannedJobOmitsInvocationDir pins that a job with no
// interactive launch has no member, so no cached task can see one.
func TestBuildJobContext_PlannedJobOmitsInvocationDir(t *testing.T) {
	t.Parallel()
	ws, job := interactiveTestJob(t, "")
	data, err := json.Marshal(BuildJobContext(ws, job, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"invocationDir"`)) {
		t.Fatalf("a planned job context carries invocationDir:\n%s", data)
	}
}

// TestJobInvocation_InteractiveSetsInvocationDirAndKeepsTheCwd pins the
// environment: PUTNAMI_INVOCATION_DIR is the typed directory, the inherited
// value is replaced, PUTNAMI_CALLER_DIR stays unset, and the task still runs
// in its own directory.
func TestJobInvocation_InteractiveSetsInvocationDirAndKeepsTheCwd(t *testing.T) {
	t.Setenv(InvocationDirEnv, "/inherited/outer/invocation")
	t.Setenv(CallerDirEnv, "/inherited/outer/caller")
	typedIn := t.TempDir()
	ws, job := interactiveTestJob(t, typedIn)

	inv := buildJobInvocation(ws, job, BuildJobContext(ws, job, nil, nil, nil), nil, "")
	if got := envValues(inv.env, InvocationDirEnv); len(got) != 1 || got[0] != typedIn {
		t.Fatalf("%s values = %q, want exactly [%q]", InvocationDirEnv, got, typedIn)
	}
	if got := envValues(inv.env, CallerDirEnv); len(got) != 0 {
		t.Fatalf("%s reaches a workspace job: %q", CallerDirEnv, got)
	}
	if want := filepath.Join(ws.Root, "."); inv.cwd != want {
		t.Fatalf("cwd = %q, want the project root %q", inv.cwd, want)
	}
}

// TestJobInvocation_PlannedJobStripsInheritedInvocationDir pins that a job
// that is not an interactive launch, such as a nested run, never reads the
// outer command's directory.
func TestJobInvocation_PlannedJobStripsInheritedInvocationDir(t *testing.T) {
	t.Setenv(InvocationDirEnv, "/inherited/outer/invocation")
	ws, job := interactiveTestJob(t, "")

	inv := buildJobInvocation(ws, job, BuildJobContext(ws, job, nil, nil, nil), nil, "")
	if got := envValues(inv.env, InvocationDirEnv); len(got) != 0 {
		t.Fatalf("%s reaches a planned job: %q", InvocationDirEnv, got)
	}
}

// TestJobInvocation_UserScopeLaunchCarriesBothDirectories pins the equality the
// protocol documents: outside a workspace the invocation directory equals the
// caller directory.
func TestJobInvocation_UserScopeLaunchCarriesBothDirectories(t *testing.T) {
	t.Parallel()
	callerDir := t.TempDir()
	ws, job := userScopeTestJob(t, callerDir)
	job.InvocationDir = callerDir

	inv := buildJobInvocation(ws, job, BuildJobContext(ws, job, nil, nil, nil), nil, "")
	for _, name := range []string{CallerDirEnv, InvocationDirEnv} {
		if got := envValues(inv.env, name); len(got) != 1 || got[0] != callerDir {
			t.Fatalf("%s values = %q, want exactly [%q]", name, got, callerDir)
		}
	}
}
