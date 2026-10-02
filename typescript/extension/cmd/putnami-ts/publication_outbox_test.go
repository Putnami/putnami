package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/registrycred"
)

// outboxTarballBytes are the bytes the bun pack fake writes.
const outboxTarballBytes = "immutable npm tarball bytes"

// countingRegistry is an npm registry that answers 500 to every request and
// counts them.
func countingRegistry(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "no request is expected", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server.URL, &requests
}

// stubOutboxNPMPack lets a managed publication pack with bun and fails the
// test on any other subprocess or credential request. The managed upload and
// probe keep their real HTTP implementations, so a request would reach the
// registry under test.
func stubOutboxNPMPack(t *testing.T) {
	t.Helper()
	originalRun, originalToken, originalBun, originalGOOS := npmExecRun, npmResolveRegistryToken, resolveBunBin, npmGOOS
	originalSeam := registrycred.ResolveToken
	t.Cleanup(func() {
		npmExecRun, npmResolveRegistryToken, resolveBunBin, npmGOOS = originalRun, originalToken, originalBun, originalGOOS
		registrycred.ResolveToken = originalSeam
	})
	npmGOOS = "linux"
	resolveBunBin = func() (string, error) { return managedTestBun, nil }
	refuse := func(host string) (string, string) {
		t.Errorf("credential requested for %q during an outbox publication", host)
		return "", ""
	}
	npmResolveRegistryToken = refuse
	registrycred.ResolveToken = refuse
	npmExecRun = func(name string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if name != managedTestBun || len(args) < 2 || args[0] != "pm" || args[1] != "pack" {
			t.Errorf("subprocess %s %v ran during an outbox publication; only the bun pack may run", name, args)
			return &exec.Result{Success: false, ExitCode: 1}, nil
		}
		destination := flagValue(args, "--destination")
		if err := os.WriteFile(filepath.Join(destination, "artifact.tgz"), []byte(outboxTarballBytes), 0o600); err != nil {
			t.Fatalf("stage the packed tarball: %v", err)
		}
		return &exec.Result{Success: true}, nil
	}
}

// managedOutboxNPMContext is a release-set npm member whose registry is the
// counting registry, under a malformed private broker that a credentialed
// publication would refuse, with the outbox named.
func managedOutboxNPMContext(t *testing.T) (*pctx.Context, string, string, *atomic.Int64) {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	registry, requests := countingRegistry(t)
	withRegistries(t, ctx, `{"npm":{"publish":"`+registry+`"}}`)
	t.Setenv(privateNPMRegistryURLEnv, "http://localhost:41234/npm")
	outbox := filepath.Join(t.TempDir(), "outbox")
	t.Setenv(extproto.PublicationOutboxEnv, outbox)
	return ctx, dir, outbox, requests
}

func assertNoPublishedEvents(t *testing.T, events []map[string]any) {
	t.Helper()
	for _, event := range events {
		if event["kind"] == "published" || event["kind"] == extproto.PublishedMemberEventKind {
			t.Fatalf("outbox publication emitted %v: %#v", event["kind"], event)
		}
	}
}

// A managed npm member under the outbox is packed: the descriptor names the
// tarball and the staged package.json, and the job asks for no credential,
// spawns nothing but the pack and sends no request to the registry.
func TestManagedNPMPublishWritesTheOutboxAndUploadsNothing(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "managed-publication-packs-only", "the-managed-route-packs-into-the-outbox-and-uploads-nothing")
	ctx, dir, outboxRoot, requests := managedOutboxNPMContext(t)
	stubOutboxNPMPack(t)

	var status string
	var data map[string]any
	var err error
	events := captureEvents(t, func() {
		status, data, err = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" || data["packed"] != true {
		t.Fatalf("runPublishNpm() = (%q, %#v, %v), want a packed OK", status, data, err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("registry requests = %d, want none", got)
	}
	assertNoPublishedEvents(t, events)

	outbox, err := publicationoutbox.Read(outboxRoot)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	if len(outbox.Descriptor.Members) != 1 {
		t.Fatalf("outbox members = %+v, want one", outbox.Descriptor.Members)
	}
	member := outbox.Descriptor.Members[0]
	if member.Ecosystem != extproto.OutboxEcosystemNPM || member.Coordinate != "@test/pkg" ||
		member.Version != "1.2.3-r42" || member.Project != "/project" || member.NPM == nil || member.Go != nil || member.OCI != nil {
		t.Fatalf("outbox member = %+v, want the planned npm member", member)
	}
	declared, err := npmRegistriesFrom(ctx.Params)
	if err != nil || member.NPM.Registry != declared.Publish {
		t.Fatalf("outbox npm registry = %q, want the declared %q (%v)", member.NPM.Registry, declared.Publish, err)
	}
	tarball, err := outbox.ReadFile(member.NPM.Tarball)
	if err != nil || string(tarball) != outboxTarballBytes {
		t.Fatalf("outbox tarball = (%q, %v), want the packed bytes", tarball, err)
	}
	if want := fmt.Sprintf("sha256:%x", sha256.Sum256(tarball)); data["artifactDigest"] != want {
		t.Fatalf("artifactDigest = %v, want the outbox tarball digest %s", data["artifactDigest"], want)
	}
	manifest, err := outbox.ReadFile(member.NPM.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := os.ReadFile(filepath.Join(dir, ".putnami", "out", "project", "package", "npm", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest) != string(staged) {
		t.Fatalf("outbox manifest = %s, want the staged package.json %s", manifest, staged)
	}
}

// The npm member names the managed registry the job resolved, normalized as the
// managed rules require: --registry over registries.npm.publish over npm's own
// registry. The engine uploads there and nowhere else.
func TestManagedNPMOutboxNamesTheResolvedRegistry(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "managed-publication-packs-only", "the-managed-route-packs-into-the-outbox-and-uploads-nothing")
	for name, tc := range map[string]struct {
		registry   string
		registries string
		want       string
	}{
		"the declared registry":   {registries: `{"npm":{"publish":"HTTPS://npm.acme.dev/team/"}}`, want: "https://npm.acme.dev/team"},
		"the --registry override": {registry: "https://override.acme.dev/", registries: `{"npm":{"publish":"https://npm.acme.dev"}}`, want: "https://override.acme.dev"},
		"npm's own registry":      {want: defaultNPMRegistry},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, _, outboxRoot, requests := managedOutboxNPMContext(t)
			stubOutboxNPMPack(t)
			delete(ctx.Params, "registries")
			if tc.registries != "" {
				withRegistries(t, ctx, tc.registries)
			}
			if tc.registry != "" {
				ctx.Params["registry"] = json.RawMessage(fmt.Sprintf("%q", tc.registry))
			}
			if status, _, err := runPublishNpm(ctx, jsonl.New(), nil); err != nil || status != "OK" {
				t.Fatalf("runPublishNpm() = (%q, %v), want a packed OK", status, err)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("registry requests = %d, want none", got)
			}
			outbox, err := publicationoutbox.Read(outboxRoot)
			if err != nil {
				t.Fatalf("read the outbox: %v", err)
			}
			if got := outbox.Descriptor.Members[0].NPM.Registry; got != tc.want {
				t.Fatalf("outbox npm registry = %q, want %q", got, tc.want)
			}
		})
	}
}

// Publication without a release-set member is the user's own registry and
// credentials: it publishes with npm as before and writes no outbox.
func TestNativeNPMPublishIgnoresTheOutbox(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "managed-publication-packs-only", "a-native-route-ignores-the-outbox")
	originalRun, originalToken, originalGOOS := npmExecRun, npmResolveRegistryToken, npmGOOS
	t.Cleanup(func() { npmExecRun, npmResolveRegistryToken, npmGOOS = originalRun, originalToken, originalGOOS })
	npmGOOS = "linux"
	var tokenHosts []string
	npmResolveRegistryToken = func(host string) (string, string) {
		tokenHosts = append(tokenHosts, host)
		return "", ""
	}
	var commands []string
	npmExecRun = func(name string, args []string, _ ...exec.Option) (*exec.Result, error) {
		commands = append(commands, name+" "+args[0])
		if args[0] == "view" {
			return &exec.Result{Success: false, ExitCode: 1}, nil
		}
		return &exec.Result{Success: true}, nil
	}
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"npm": json.RawMessage(`true`), "registry": json.RawMessage(`"https://npm.example.test"`)}
	stageNPMFixture(t, dir)
	outboxRoot := filepath.Join(t.TempDir(), "outbox")
	t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)

	var status string
	var err error
	events := captureEvents(t, func() {
		status, _, err = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("runPublishNpm() = (%q, %v), want OK", status, err)
	}
	if strings.Join(commands, ",") != "npm view,npm publish" {
		t.Fatalf("commands = %v, want npm view and npm publish", commands)
	}
	if len(tokenHosts) != 1 || tokenHosts[0] != "npm.example.test" {
		t.Fatalf("credential hosts = %v, want the declared registry host", tokenHosts)
	}
	if findEvent(events, func(event map[string]any) bool { return event["kind"] == "published" }) == nil {
		t.Fatalf("native publication emitted no published event: %#v", events)
	}
	if _, err := os.Stat(outboxRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("outbox stat = %v, want no outbox for a native publication", err)
	}
}

// treeState maps every path under root to its kind, mode and content digest.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			state[rel] = "dir " + info.Mode().String()
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state[rel] = fmt.Sprintf("%s %x", info.Mode(), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := treeState(t, root)
	for path, state := range after {
		if before[path] != state {
			t.Errorf("%s changed under %s: %q, then %q", path, root, before[path], state)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Errorf("%s was removed under %s", path, root)
		}
	}
}

// isolateTempDir points the process temporary directory at an empty directory
// and returns it. Call it after the test's first t.TempDir, which then keeps
// its own parent.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	temp := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(temp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temp)
	return temp
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("the job left %v in the temporary directory", names)
	}
}

// writeFakeImageLayout stages a packaged workload image: an OCI layout the job
// copies byte for byte and the manifest that names its digest. It returns the
// digest and the layout directory.
func writeFakeImageLayout(t *testing.T, workspaceRoot, projectPath string) (string, string) {
	t.Helper()
	dockerDir := pkgmeta.PackageOutputDir(workspaceRoot, projectPath, "docker")
	layoutDir := filepath.Join(dockerDir, "oci")
	digest := "sha256:" + strings.Repeat("c", 64)
	files := [][2]string{
		{"oci-layout", `{"imageLayoutVersion":"1.0.0"}`},
		{"index.json", `{"schemaVersion":2,"manifests":[]}`},
		{filepath.Join("blobs", "sha256", strings.Repeat("c", 64)), `{"schemaVersion":2}`},
	}
	for _, file := range files {
		path := filepath.Join(layoutDir, file[0])
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(pkgmeta.DockerManifest{
		Image: "test-pkg", Tags: []string{"test-pkg:c-" + strings.Repeat("d", 64)}, Version: "0.0.0-stale",
		ContentHash: strings.Repeat("d", 64), Digest: digest, Layout: "oci", Platform: "linux/amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerDir, "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	return digest, layoutDir
}

// fileContents maps every regular file under root to its content digest.
func fileContents(t *testing.T, root string) map[string]string {
	t.Helper()
	contents := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		contents[rel] = fmt.Sprintf("%x", sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

// A packing job writes its artifacts into the outbox and nowhere else: the
// workspace, its build outputs included, is unchanged and no temporary file
// survives the job.
func TestPublishJobWritesNothingOutsideTheOutbox(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "managed-publication-packs-only", "the-job-writes-nothing-outside-the-outbox")

	t.Run("npm", func(t *testing.T) {
		ctx, dir, outboxRoot, _ := managedOutboxNPMContext(t)
		stubOutboxNPMPack(t)
		temp := isolateTempDir(t)
		before := treeState(t, dir)

		if status, _, err := runPublishNpm(ctx, jsonl.New(), nil); err != nil || status != "OK" {
			t.Fatalf("runPublishNpm() = (%q, %v), want OK", status, err)
		}
		assertTreeUnchanged(t, dir, before)
		assertEmptyDir(t, temp)
		if _, err := publicationoutbox.Read(outboxRoot); err != nil {
			t.Fatalf("read the outbox: %v", err)
		}
	})

	t.Run("docker", func(t *testing.T) {
		ctx, dir := makeTestCtx(t)
		ctx.Identity = &protocolcli.TaskIdentity{Project: protocolcli.ProjectIdentity{ID: "/project"}}
		ctx.Version = &pctx.Version{Base: "1.2.3", Full: "1.2.3-r42"}
		withRegistries(t, ctx, `{"oci":{"publish":"oci.putnami.dev/test"}}`)
		digest, packagedLayout := writeFakeImageLayout(t, dir, ctx.Project.Path)
		broker, requests := countingRegistry(t)
		t.Setenv("PUTNAMI_REGISTRY_OCI_URL", broker+"/oci")
		outboxRoot := filepath.Join(t.TempDir(), "outbox")
		t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)
		originalSeam := registrycred.ResolveToken
		t.Cleanup(func() { registrycred.ResolveToken = originalSeam })
		registrycred.ResolveToken = func(host string) (string, string) {
			t.Errorf("credential requested for %q during an outbox publication", host)
			return "", ""
		}
		temp := isolateTempDir(t)
		before := treeState(t, dir)

		if status, data, err := runPublishDocker(ctx, jsonl.New(), nil); err != nil || status != "OK" || data["packed"] != true {
			t.Fatalf("runPublishDocker() = (%q, %#v, %v), want a packed OK", status, data, err)
		}
		if got := requests.Load(); got != 0 {
			t.Fatalf("broker requests = %d, want none", got)
		}
		assertTreeUnchanged(t, dir, before)
		assertEmptyDir(t, temp)
		outbox, err := publicationoutbox.Read(outboxRoot)
		if err != nil {
			t.Fatalf("read the outbox: %v", err)
		}
		if len(outbox.Descriptor.Members) != 1 {
			t.Fatalf("outbox members = %+v, want one", outbox.Descriptor.Members)
		}
		member := outbox.Descriptor.Members[0]
		if member.OCI == nil || member.OCI.Repository != "oci.putnami.dev/test/test-pkg" || member.OCI.Digest != digest ||
			member.Coordinate != "test/test-pkg" || member.Version != "1.2.3-r42" || member.Project != "/project" {
			t.Fatalf("outbox member = %+v, want the managed image at its packaged digest", member)
		}
		layoutDir, err := outbox.Layout(member.OCI.Layout)
		if err != nil {
			t.Fatal(err)
		}
		packed, packaged := fileContents(t, layoutDir), fileContents(t, packagedLayout)
		if len(packed) != len(packaged) {
			t.Fatalf("outbox layout = %v, want the packaged layout %v", packed, packaged)
		}
		for path, digest := range packaged {
			if packed[path] != digest {
				t.Fatalf("outbox layout %s = %q, want the packaged bytes %q", path, packed[path], digest)
			}
		}
	})
}

// Without the outbox, a managed publication uploads, verifies and reports its
// member exactly as before, and nothing names an outbox.
func TestPublishWithoutTheOutboxIsUnchanged(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "managed-publication-packs-only", "without-the-outbox-publication-is-unchanged")
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	calls := mockManagedNPM(t, false, false)

	var status string
	var data map[string]any
	var err error
	events := captureEvents(t, func() {
		status, data, err = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" || data["digestVerified"] != true {
		t.Fatalf("runPublishNpm() = (%q, %#v, %v), want a verified OK", status, data, err)
	}
	if _, packed := data["packed"]; packed {
		t.Fatalf("publication without an outbox reports a pack: %#v", data)
	}
	if calls.publish != 1 || calls.remoteVerify != 2 || len(calls.tokenHosts) != 1 {
		t.Fatalf("managed publication = %d uploads, %d verifications, credential hosts %v; want 1, 2 and one host", calls.publish, calls.remoteVerify, calls.tokenHosts)
	}
	if findEvent(events, func(event map[string]any) bool { return event["kind"] == extproto.PublishedMemberEventKind }) == nil {
		t.Fatalf("managed publication emitted no published member: %#v", events)
	}
	if _, err := os.Stat(filepath.Join(dir, extproto.PublicationOutboxDescriptor)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("outbox descriptor stat = %v, want none", err)
	}
}
