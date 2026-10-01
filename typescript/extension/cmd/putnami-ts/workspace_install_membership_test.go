package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// membershipTree is a workspace right after `putnami projects create web`: web
// is a member with a package.json, svc is a Go-only member, and the root
// package.json lists no workspaces yet. The directory "project" carries a
// package.json but is not a member.
func membershipTree(t *testing.T) (*pctx.Context, string) {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	writeProbeFile(t, filepath.Join(dir, "putnami.extension.json"), `{}`)
	writeExtensionBiomeDefault(t, dir)
	writeProbeFile(t, filepath.Join(dir, "package.json"),
		"{\n  \"name\": \"fresh\",\n  \"private\": true,\n  \"packageManager\": \"bun@1.3.14\"\n}\n")
	writeProbeFile(t, filepath.Join(dir, "web", "package.json"), `{"name":"web"}`)
	writeProbeFile(t, filepath.Join(dir, "svc", "go.mod"), "module acme/svc\n\ngo 1.25\n")
	return ctx, dir
}

// membership is the complete workspace membership as core sends it, in
// project-id order, the workspace root included.
func membership() []pctx.ProjectRef {
	return []pctx.ProjectRef{
		{ID: "/", Name: "fresh", SourceName: "fresh", Path: "."},
		{ID: "/svc", Name: "acme/svc", SourceName: "acme/svc", Path: "svc"},
		{ID: "/web", Name: "web", SourceName: "web", Path: "web"},
	}
}

// installRun is what one workspace install did.
type installRun struct {
	status string
	err    error
	// seen is the root workspaces list when `bun install` started.
	seen []string
	// installed reports whether `bun install` ran.
	installed bool
}

// installRecordingWorkspaces runs the workspace install with a bun that
// records the root workspaces it finds when `bun install` starts.
func installRecordingWorkspaces(t *testing.T, ctx *pctx.Context) installRun {
	t.Helper()
	var run installRun
	mockBunResolution(t)
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if len(args) > 0 && args[0] == "install" {
			run.installed = true
			if raw := readJSONField(t, filepath.Join(ctx.WorkspaceRoot, "package.json"), "workspaces"); len(raw) > 0 {
				if err := json.Unmarshal(raw, &run.seen); err != nil {
					t.Fatalf("workspaces at bun install = %s: %v", raw, err)
				}
			}
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})
	run.status, _, run.err = runWorkspaceInstall(ctx, jsonl.New(), nil)
	return run
}

// A project created after `putnami init` is installed by the next install:
// the root workspaces lists every member that carries a package.json when bun
// install starts. Membership decides, not the directory scan: a package.json
// outside the membership is not listed.
func TestWorkspaceInstallListsEveryMemberBeforeBunInstall(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-lists-the-members",
		"the-members-are-listed-before-bun-install")
	ctx, _ := membershipTree(t)
	ctx.WorkspaceProjects = membership()

	run := installRecordingWorkspaces(t, ctx)
	if run.err != nil || run.status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", run.status, run.err)
	}
	if !run.installed {
		t.Fatal("bun install did not run")
	}
	if !slices.Equal(run.seen, []string{"web"}) {
		t.Fatalf("workspaces when bun install started = %v, want [web]", run.seen)
	}
}

// The install writes the bytes `projects sync` writes for the same
// membership, so the two commands never rewrite each other's list.
func TestWorkspaceInstallWritesTheListProjectsSyncWrites(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-lists-the-members",
		"the-list-is-the-one-projects-sync-writes")
	installCtx, installRoot := membershipTree(t)
	installCtx.WorkspaceProjects = membership()
	_, syncRoot := membershipTree(t)

	if run := installRecordingWorkspaces(t, installCtx); run.err != nil || run.status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", run.status, run.err)
	}
	if _, err := syncTypeScriptWorkspace(syncRoot, membership(), false); err != nil {
		t.Fatal(err)
	}
	installed := readTextFile(t, filepath.Join(installRoot, "package.json"))
	synced := readTextFile(t, filepath.Join(syncRoot, "package.json"))
	if installed != synced {
		t.Fatalf("install wrote\n%s\nprojects sync writes\n%s", installed, synced)
	}

	// A second install over the listed members leaves the file untouched.
	manifest := filepath.Join(installRoot, "package.json")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(manifest, past, past); err != nil {
		t.Fatal(err)
	}
	if run := installRecordingWorkspaces(t, installCtx); run.err != nil || run.status != "OK" {
		t.Fatalf("second runWorkspaceInstall = %q, %v; want OK", run.status, run.err)
	}
	if info, err := os.Stat(manifest); err != nil || !info.ModTime().Equal(past) {
		t.Fatalf("the second install rewrote package.json (%v, %v)", info, err)
	}
}

// A context without the membership, from a CLI that does not send it, leaves
// the list as it is: an unknown membership never rewrites it.
func TestWorkspaceInstallWithoutTheMembershipLeavesWorkspacesAlone(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-lists-the-members",
		"a-context-without-the-membership-changes-nothing")
	ctx, dir := membershipTree(t)
	original := "{\n  \"name\": \"fresh\",\n  \"private\": true,\n  \"workspaces\": [\n    \"stale\"\n  ],\n  \"packageManager\": \"bun@1.3.14\"\n}\n"
	writeProbeFile(t, filepath.Join(dir, "package.json"), original)

	run := installRecordingWorkspaces(t, ctx)
	if run.err != nil || run.status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", run.status, run.err)
	}
	if !slices.Equal(run.seen, []string{"stale"}) {
		t.Fatalf("workspaces at bun install = %v, want the list left as [stale]", run.seen)
	}
	if got := readTextFile(t, filepath.Join(dir, "package.json")); got != original {
		t.Fatalf("package.json changed to\n%s", got)
	}
}

// A root workspaces member the install cannot read fails the install before
// bun runs, naming the file, rather than installing without the members.
func TestWorkspaceInstallFailsOnAnUnreadableWorkspacesMember(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-lists-the-members",
		"an-unreadable-workspaces-member-fails-the-install")
	ctx, dir := membershipTree(t)
	ctx.WorkspaceProjects = membership()
	original := "{\n  \"name\": \"fresh\",\n  \"workspaces\": 3,\n  \"packageManager\": \"bun@1.3.14\"\n}\n"
	writeProbeFile(t, filepath.Join(dir, "package.json"), original)

	run := installRecordingWorkspaces(t, ctx)
	if run.status != "FAILED" || run.err == nil || !strings.Contains(run.err.Error(), "package.json") {
		t.Fatalf("runWorkspaceInstall = %q, %v; want FAILED naming package.json", run.status, run.err)
	}
	if run.installed {
		t.Fatal("bun install ran although the members could not be listed")
	}
	if got := readTextFile(t, filepath.Join(dir, "package.json")); got != original {
		t.Fatalf("package.json changed to\n%s", got)
	}
}
