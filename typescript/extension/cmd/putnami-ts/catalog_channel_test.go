package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// candidateChannel is an immutable channel a tagged publish creates.
const candidateChannel = "tooling-v0.4.0"

// channelRegistry is an npm registry that answers a packument per package and
// records the packages it was asked for.
type channelRegistry struct {
	*httptest.Server
	mu    sync.Mutex
	asked []string
}

// newChannelRegistry serves distTags[<package>] as the package's dist-tags. A
// package it does not know answers 404.
func newChannelRegistry(t *testing.T, distTags map[string]map[string]string) *channelRegistry {
	t.Helper()
	registry := &channelRegistry{}
	registry.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		registry.mu.Lock()
		registry.asked = append(registry.asked, name)
		registry.mu.Unlock()
		tags, ok := distTags[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"dist-tags": tags})
	}))
	t.Cleanup(registry.Close)
	original := npmMetadataClient
	t.Cleanup(func() { npmMetadataClient = original })
	npmMetadataClient = registry.Client()
	return registry
}

func (r *channelRegistry) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

// channelSeedContext is the job context of a workspace install that received
// channel as the putnami-channel job option and registry as the @putnami scope
// registry. An empty channel sends no option.
func channelSeedContext(t *testing.T, dir, channel, registry string) *pctx.Context {
	t.Helper()
	ctx := &pctx.Context{WorkspaceRoot: dir, Params: pctx.Params{}}
	if channel != "" {
		ctx.Params["putnami-channel"] = mustMarshal(t, channel)
	}
	if registry != "" {
		withRegistries(t, ctx, `{"npm":{"scopes":{"@putnami":`+string(mustMarshal(t, registry))+`}}}`)
	}
	return ctx
}

// refuseNPMMetadataRequests fails the test on any dist-tag request.
func refuseNPMMetadataRequests(t *testing.T) {
	t.Helper()
	original := npmMetadataClient
	t.Cleanup(func() { npmMetadataClient = original })
	npmMetadataClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected npm metadata request: %s", r.URL)
		return nil, errors.New("no npm metadata request is expected")
	})}
}

func starterWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"smoke","private":true,"workspaces":["webapp"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMember(t, dir, "webapp", `{
		"name": "webapp",
		"dependencies": {"@putnami/web": "catalog:", "@putnami/ui": "catalog:", "react": "^19.2.3"}
	}`)
	return dir
}

// On a channel other than latest, every missing entry is the exact version the
// channel's dist-tag names for that package: never the channel name, and never
// the version latest names, whether latest names an older release or nothing.
func TestEnsureWorkspaceCatalog_SeedsTheExactVersionOfTheChannel(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "a-channel-seeds-its-exact-dist-tag-version")
	dir := starterWorkspace(t)
	registry := newChannelRegistry(t, map[string]map[string]string{
		// latest names an older release of one package and nothing for the other.
		"@putnami/web": {"latest": "0.3.0", candidateChannel: "0.4.0-20260902173000-cafe1234"},
		"@putnami/ui":  {candidateChannel: "0.4.1"},
	})

	ctx := channelSeedContext(t, dir, candidateChannel, registry.URL)
	if err := ensureWorkspaceCatalog(ctx, jsonl.New()); err != nil {
		t.Fatalf("ensureWorkspaceCatalog: %v", err)
	}

	catalog := readCatalog(t, dir)
	want := map[string]string{"@putnami/web": "0.4.0-20260902173000-cafe1234", "@putnami/ui": "0.4.1"}
	for name, version := range want {
		if catalog[name] != version {
			t.Errorf("catalog[%s] = %q, want the exact version %q of %s", name, catalog[name], version, candidateChannel)
		}
	}
	if len(catalog) != len(want) {
		t.Errorf("catalog = %v, want only the two referenced @putnami packages", catalog)
	}
	asked := registry.requests()
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"@putnami/ui", "@putnami/web"}) {
		t.Errorf("packuments asked = %v, want one per missing package", asked)
	}
}

// A package the channel does not name fails the seed, names the package, the
// channel and the registry, and leaves the manifest as it was: the seed never
// reads latest in place of the channel.
func TestEnsureWorkspaceCatalog_AChannelThatNamesNoVersionFailsAndWritesNothing(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "a-channel-never-falls-back-to-latest")
	dir := starterWorkspace(t)
	before, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry := newChannelRegistry(t, map[string]map[string]string{
		"@putnami/web": {"latest": "0.3.0", candidateChannel: "0.4.0"},
		"@putnami/ui":  {"latest": "0.3.0"},
	})

	ctx := channelSeedContext(t, dir, candidateChannel, registry.URL)
	err = ensureWorkspaceCatalog(ctx, jsonl.New())
	if err == nil {
		t.Fatal("ensureWorkspaceCatalog succeeded for a package the channel does not name")
	}
	for _, want := range []string{"@putnami/ui", candidateChannel, strings.TrimPrefix(registry.URL, "http://")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	after, readErr := os.ReadFile(filepath.Join(dir, "package.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(before) {
		t.Errorf("a failed channel seed rewrote package.json:\n%s", after)
	}
}

// No option, latest and stable all seed the "latest" dist-tag and ask the
// registry nothing, as an install without a channel does.
func TestEnsureWorkspaceCatalog_LatestSeedsTheDistTagWithoutARequest(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "latest-seeds-the-latest-dist-tag-without-a-request")
	for _, channel := range []string{"", "latest", "stable"} {
		t.Run("channel="+channel, func(t *testing.T) {
			dir := starterWorkspace(t)
			refuseNPMMetadataRequests(t)

			ctx := channelSeedContext(t, dir, channel, "http://127.0.0.1:1")
			if err := ensureWorkspaceCatalog(ctx, jsonl.New()); err != nil {
				t.Fatalf("ensureWorkspaceCatalog: %v", err)
			}
			catalog := readCatalog(t, dir)
			if catalog["@putnami/web"] != "latest" || catalog["@putnami/ui"] != "latest" {
				t.Errorf("catalog = %v, want both entries seeded with latest", catalog)
			}
		})
	}
}

// An entry the workspace already pins is kept on a channel too, and costs no
// request: the channel seeds what is missing and moves nothing.
func TestEnsureWorkspaceCatalog_AChannelKeepsExistingEntries(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "an-existing-entry-is-left-alone")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"private":true,"workspaces":["webapp"],"catalog":{"@putnami/web":"0.2.0","@putnami/ui":"0.2.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMember(t, dir, "webapp", `{"name":"webapp","dependencies":{"@putnami/web":"catalog:","@putnami/ui":"catalog:"}}`)
	refuseNPMMetadataRequests(t)

	ctx := channelSeedContext(t, dir, candidateChannel, "http://127.0.0.1:1")
	if err := ensureWorkspaceCatalog(ctx, jsonl.New()); err != nil {
		t.Fatalf("ensureWorkspaceCatalog: %v", err)
	}
	catalog := readCatalog(t, dir)
	if catalog["@putnami/web"] != "0.2.0" || catalog["@putnami/ui"] != "0.2.0" {
		t.Errorf("catalog = %v, want the pinned entries kept", catalog)
	}
}

// The workspace install hands the channel it received to the catalog seed and
// stops before bun when the channel resolves nothing: bun never installs the
// starter on another channel.
func TestWorkspaceInstall_StopsBeforeBunWhenTheChannelSeedFails(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "a-channel-never-falls-back-to-latest")
	ctx, dir := membershipTree(t)
	ctx.WorkspaceProjects = membership()
	writeProbeFile(t, filepath.Join(dir, "web", "package.json"), `{"name":"web","dependencies":{"@putnami/web":"catalog:"}}`)
	registry := newChannelRegistry(t, map[string]map[string]string{"@putnami/web": {"latest": "0.3.0"}})
	ctx.Params = channelSeedContext(t, dir, candidateChannel, registry.URL).Params

	run := installRecordingWorkspaces(t, ctx)
	if run.err == nil || run.status != "FAILED" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want FAILED with the seed error", run.status, run.err)
	}
	if !strings.Contains(run.err.Error(), candidateChannel) || !strings.Contains(run.err.Error(), "@putnami/web") {
		t.Errorf("error %q does not name the channel and the package", run.err)
	}
	if run.installed {
		t.Error("bun install ran after the channel seed failed")
	}
	if catalog := readCatalog(t, dir); len(catalog) != 0 {
		t.Errorf("catalog = %v, want no entry seeded", catalog)
	}
}

// With the channel's dist-tag published, the install seeds the exact version
// before bun reads the manifest.
func TestWorkspaceInstall_SeedsTheChannelBeforeBunInstall(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "a-channel-seeds-its-exact-dist-tag-version")
	ctx, dir := membershipTree(t)
	ctx.WorkspaceProjects = membership()
	writeProbeFile(t, filepath.Join(dir, "web", "package.json"), `{"name":"web","dependencies":{"@putnami/web":"catalog:"}}`)
	registry := newChannelRegistry(t, map[string]map[string]string{
		"@putnami/web": {"latest": "0.3.0", candidateChannel: "0.4.0"},
	})
	ctx.Params = channelSeedContext(t, dir, candidateChannel, registry.URL).Params

	var atInstall map[string]string
	mockBunResolution(t)
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if len(args) > 0 && args[0] == "install" {
			atInstall = readCatalog(t, dir)
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})
	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	if atInstall["@putnami/web"] != "0.4.0" {
		t.Errorf("catalog when bun install started = %v, want @putnami/web at 0.4.0", atInstall)
	}
}
