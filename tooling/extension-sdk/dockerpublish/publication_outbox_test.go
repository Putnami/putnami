package dockerpublish

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	extproto "go.putnami.dev/protocol/extension"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/registrycred"
)

// refusingTransport fails and records every request sent through the default
// HTTP and registry transports.
type refusingTransport struct {
	mu       sync.Mutex
	requests []string
}

func (r *refusingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request.Method+" "+request.URL.String())
	r.mu.Unlock()
	return nil, errors.New("registry request during an outbox publication")
}

func (r *refusingTransport) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

// refuseRegistryRequests replaces the default transports with a recorder that
// fails every request, and returns a function that restores them.
func refuseRegistryRequests(t *testing.T) (*refusingTransport, func()) {
	t.Helper()
	refusing := &refusingTransport{}
	oldHTTP, oldRemote := http.DefaultTransport, remote.DefaultTransport
	http.DefaultTransport, remote.DefaultTransport = refusing, refusing
	restored := false
	restore := func() {
		if !restored {
			http.DefaultTransport, remote.DefaultTransport = oldHTTP, oldRemote
			restored = true
		}
	}
	t.Cleanup(restore)
	return refusing, restore
}

// refuseCredentials makes the credential seam fail the test when it is asked.
func refuseCredentials(t *testing.T) {
	t.Helper()
	old := registrycred.ResolveToken
	registrycred.ResolveToken = func(host string) (string, string) {
		t.Errorf("credential seam asked for %q during an outbox publication", host)
		return "", ""
	}
	t.Cleanup(func() { registrycred.ResolveToken = old })
}

// readOneOCIMember reads the outbox descriptor at root and returns its only
// member, which must be an OCI member.
func readOneOCIMember(t *testing.T, root string) (*publicationoutbox.Outbox, extproto.OutboxMember) {
	t.Helper()
	outbox, err := publicationoutbox.Read(root)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	if len(outbox.Descriptor.Members) != 1 {
		t.Fatalf("outbox members = %+v, want one", outbox.Descriptor.Members)
	}
	member := outbox.Descriptor.Members[0]
	if member.Ecosystem != extproto.OutboxEcosystemOCI || member.OCI == nil || member.NPM != nil || member.Go != nil {
		t.Fatalf("outbox member = %+v, want one OCI member", member)
	}
	return outbox, member
}

// assertLayoutHolds checks that the outbox layout holds the manifest digest.
func assertLayoutHolds(t *testing.T, outbox *publicationoutbox.Outbox, member extproto.OutboxMember) string {
	t.Helper()
	layoutDir, err := outbox.Layout(member.OCI.Layout)
	if err != nil {
		t.Fatalf("outbox layout: %v", err)
	}
	image, err := oci.LoadFromLayout(layoutDir)
	if err != nil {
		t.Fatalf("load the outbox layout: %v", err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digest.String() != member.OCI.Digest {
		t.Fatalf("outbox layout digest = %s, want the descriptor digest %s", digest, member.OCI.Digest)
	}
	return layoutDir
}

func assertNoPublicationRecords(t *testing.T, outputPath, projectDir string) {
	t.Helper()
	if _, err := os.Stat(pkgmeta.PublishedImageManifestPath(outputPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("published-image.json stat = %v, want absent: the engine records the published image", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".gen", "version.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf(".gen/version.json stat = %v, want absent under the outbox", err)
	}
}

func assertNoPublicationEvents(t *testing.T, events []map[string]any) {
	t.Helper()
	for _, kind := range []string{extproto.PublishedMemberEventKind, "published"} {
		if found := eventsOfKind(events, kind); len(found) != 0 {
			t.Fatalf("%s events = %+v, want none under the outbox", kind, found)
		}
	}
}

// A managed OCI publication under the outbox packs the layout, its repository,
// its manifest digest and its tags, and sends no registry request. The engine
// pushes the layout with oci.PushLayout and records the published image.
func TestDockerPublishWritesTheOutboxForTheManagedRegistry(t *testing.T) {
	t.Run("workload image", func(t *testing.T) {
		fixture := newDockerPublishFixture(t)
		fixture.ctx.Params["registries"] = json.RawMessage(`{"oci":{"publish":"` + managedOCIRegistry + `/putnami"}}`)
		fixture.ctx.Identity = &job.TaskIdentity{Project: job.ProjectIdentity{ID: "/apps/api"}}
		broker := newPrivateOCITestBroker(t, "")
		t.Setenv(privateOCIRegistryURLEnv, broker.server.URL+"/oci")
		outboxRoot := filepath.Join(t.TempDir(), "outbox")
		t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)
		refuseCredentials(t)
		refusing, restore := refuseRegistryRequests(t)

		var status string
		var data map[string]any
		var err error
		events := captureEvents(t, func() {
			status, data, err = Publish(fixture.ctx, jsonl.New(), nil)
		})
		restore()
		if err != nil || status != "OK" || data["packed"] != true {
			t.Fatalf("Publish() = (%q, %+v, %v), want a packed OK", status, data, err)
		}
		if requests := refusing.recorded(); len(requests) != 0 {
			t.Fatalf("registry requests = %v, want none", requests)
		}
		if calls := broker.recordedCalls(); len(calls) != 0 {
			t.Fatalf("broker calls = %+v, want none", calls)
		}
		if calls := fixture.registry.recordedCalls(); len(calls) != 0 {
			t.Fatalf("registry calls = %+v, want none", calls)
		}
		assertNoPublicationEvents(t, events)
		assertNoPublicationRecords(t, fixture.ctx.OutputPath, filepath.Join(fixture.ctx.WorkspaceRoot, fixture.ctx.Project.Path))

		outbox, member := readOneOCIMember(t, outboxRoot)
		wantCoordinate := "putnami/" + fixture.repository
		if member.Coordinate != wantCoordinate || member.Version != fixture.version || member.Project != "/apps/api" {
			t.Fatalf("outbox member identity = %+v, want %s@%s for /apps/api", member, wantCoordinate, fixture.version)
		}
		if member.OCI.Repository != managedOCIRegistry+"/"+wantCoordinate || member.OCI.Digest != fixture.digest ||
			!slices.Equal(member.OCI.Tags, []string{fixture.version}) {
			t.Fatalf("outbox OCI = %+v, want the managed repository, the packaged digest and the version tag", member.OCI)
		}
		layoutDir := assertLayoutHolds(t, outbox, member)

		// The descriptor is what the engine pushes: the layout, the digest and the
		// tags it names publish the image to a registry.
		pushed, err := oci.PushLayout(context.Background(), layoutDir, oci.LayoutTarget{
			Repository: fixture.host + "/" + fixture.repository, Digest: member.OCI.Digest, Tags: member.OCI.Tags,
		}, "")
		if err != nil || pushed.Digest != fixture.digest {
			t.Fatalf("PushLayout(outbox layout) = (%+v, %v), want the packaged digest", pushed, err)
		}
		if tags := registryTags(t, fixture.host, fixture.repository); !slices.Equal(tags, []string{fixture.version}) {
			t.Fatalf("registry tags after the engine push = %v, want the version only", tags)
		}
	})

	t.Run("image project", func(t *testing.T) {
		ctx, manifest := newManagedImagePublishFixture(t)
		plan := imageReleasePlan(ctx)
		bindImageReleasePlan(t, ctx, plan)
		writeJSONFile(t, filepath.Join(pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "docker"), "manifest.json"), manifest)
		broker := newPrivateOCITestBroker(t, "")
		t.Setenv(privateOCIRegistryURLEnv, broker.server.URL+"/oci")
		outboxRoot := filepath.Join(t.TempDir(), "outbox")
		t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)
		refuseCredentials(t)
		refusing, restore := refuseRegistryRequests(t)

		var status string
		var data map[string]any
		var err error
		events := captureEvents(t, func() {
			status, data, err = Publish(ctx, jsonl.New(), nil)
		})
		restore()
		if err != nil || status != "OK" || data["packed"] != true {
			t.Fatalf("Publish() = (%q, %+v, %v), want a packed OK", status, data, err)
		}
		if requests := refusing.recorded(); len(requests) != 0 {
			t.Fatalf("registry requests = %v, want none", requests)
		}
		if calls := broker.recordedCalls(); len(calls) != 0 {
			t.Fatalf("broker calls = %+v, want none", calls)
		}
		assertNoPublicationEvents(t, events)
		assertNoPublicationRecords(t, ctx.OutputPath, filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path))

		outbox, member := readOneOCIMember(t, outboxRoot)
		planned := plan.Members[0]
		if member.Coordinate != planned.Coordinate || member.Version != planned.Version || member.Project != planned.ProjectID {
			t.Fatalf("outbox member identity = %+v, want the planned member %+v", member, planned)
		}
		if member.OCI.Repository != managedOCIRegistry+"/"+planned.Coordinate || member.OCI.Digest != manifest.Digest || len(member.OCI.Tags) != 0 {
			t.Fatalf("outbox OCI = %+v, want the managed repository at the packaged digest and no tag", member.OCI)
		}
		assertLayoutHolds(t, outbox, member)
	})

	t.Run("dry run packs nothing", func(t *testing.T) {
		fixture := newDockerPublishFixture(t)
		fixture.ctx.Params["registries"] = json.RawMessage(`{"oci":{"publish":"` + managedOCIRegistry + `/putnami"}}`)
		fixture.ctx.Identity = &job.TaskIdentity{Project: job.ProjectIdentity{ID: "/apps/api"}}
		outboxRoot := filepath.Join(t.TempDir(), "outbox")
		t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)
		refuseCredentials(t)

		status, data, err := Publish(fixture.ctx, jsonl.New(), []string{"--dry-run"})
		if err != nil || status != "OK" || data["dryRun"] != true {
			t.Fatalf("Publish(--dry-run) = (%q, %+v, %v), want a dry-run OK", status, data, err)
		}
		if _, err := os.Stat(outboxRoot); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("outbox stat = %v, want no outbox written by a dry run", err)
		}
	})

	t.Run("a managed workload without a layout candidate fails closed", func(t *testing.T) {
		fixture := newDockerPublishFixture(t)
		fixture.ctx.Params["registries"] = json.RawMessage(`{"oci":{"publish":"` + managedOCIRegistry + `/putnami"}}`)
		fixture.ctx.Identity = &job.TaskIdentity{Project: job.ProjectIdentity{ID: "/apps/api"}}
		packageDir := pkgmeta.PackageOutputDir(fixture.ctx.WorkspaceRoot, fixture.ctx.Project.Path, "docker")
		writeJSONFile(t, filepath.Join(packageDir, "manifest.json"), pkgmeta.DockerManifest{
			Image: "team/app", Tags: []string{"team/app:" + fixture.contentTag}, Version: "0.0.0-stale",
		})
		outboxRoot := filepath.Join(t.TempDir(), "outbox")
		t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)
		refuseCredentials(t)

		status, _, err := Publish(fixture.ctx, jsonl.New(), nil)
		if err == nil || status != "FAILED" {
			t.Fatalf("Publish(daemon candidate) = (%q, %v), want FAILED", status, err)
		}
		if _, err := os.Stat(outboxRoot); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("outbox stat = %v, want no outbox written", err)
		}
	})

	t.Run("another registry ignores the outbox", func(t *testing.T) {
		fixture := newDockerPublishFixture(t)
		outboxRoot := filepath.Join(t.TempDir(), "outbox")
		t.Setenv(extproto.PublicationOutboxEnv, outboxRoot)

		var status string
		var err error
		events := captureEvents(t, func() {
			status, _, err = Publish(fixture.ctx, jsonl.New(), nil)
		})
		if err != nil || status != "OK" {
			t.Fatalf("Publish() = (%q, %v), want OK", status, err)
		}
		if tags := registryTags(t, fixture.host, fixture.repository); len(tags) != 2 {
			t.Fatalf("registry tags = %v, want the content tag and the version pushed by the job", tags)
		}
		if members := eventsOfKind(events, extproto.PublishedMemberEventKind); len(members) != 1 {
			t.Fatalf("published-member events = %+v, want one", members)
		}
		if _, err := os.Stat(outboxRoot); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("outbox stat = %v, want no outbox written for another registry", err)
		}
	})
}
