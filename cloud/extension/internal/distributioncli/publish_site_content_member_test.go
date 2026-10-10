package distributioncli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	distributionproto "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/sitecontent"
	"go.putnami.dev/sdk/extension/releaseset"
)

const (
	siteContentTestRevision = "0123456789abcdef0123456789abcdef01234567"
	siteContentTestVersion  = "0.0.0-20260911000000-0123456"
	siteContentTestProject  = "/docs/example.dev"
	siteContentTestCapToken = "local_broker_capability_0123456789abcdef"
)

// writeSiteContentProject lays down docs/example.dev as a site project with
// one section (platform) and a site-level README that never ships.
func writeSiteContentProject(t *testing.T, publish string) string {
	t.Helper()
	ws := t.TempDir()
	projectRoot := filepath.Join(ws, "docs", "example.dev")
	if err := os.MkdirAll(filepath.Join(projectRoot, "platform"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"putnami.json":      `{"name":"docs/example.dev","publish":[` + publish + `]}`,
		"README.md":         "# guide\n",
		"platform/index.md": "# Platform\n",
	} {
		if err := os.WriteFile(filepath.Join(projectRoot, filepath.FromSlash(name)), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func siteContentPlan(coordinate string, channels ...string) *releaseset.Plan {
	heads := make(map[string]*distributionproto.ChannelHead, len(channels))
	for _, channel := range channels {
		heads[channel] = nil
	}
	return &releaseset.Plan{
		ProtocolVersion: distributionproto.ProtocolVersion,
		Namespace:       "cloud",
		Channels:        channels,
		Heads:           heads,
		Members: []releaseset.PlannedMember{{
			Ecosystem: "put", Coordinate: coordinate, Version: siteContentTestVersion,
			Dependencies:   []distributionproto.ReleaseSetDependency{},
			SourceRevision: siteContentTestRevision, SelectionFingerprint: "sha256:" + strings.Repeat("b", 64),
			Selected: true, ProjectID: siteContentTestProject,
		}},
	}
}

// fakePutBroker stands in for the runner's loopback broker in front of
// put-server. It records every request and serves blobs, publish, and the
// manifest readback.
type fakePutBroker struct {
	mu             sync.Mutex
	requests       []string
	auths          []string
	publishBody    map[string]json.RawMessage
	storedPayload  json.RawMessage
	publishStatus  int
	readbackStored bool
}

func (b *fakePutBroker) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.requests = append(b.requests, r.Method+" "+r.URL.Path)
		b.auths = append(b.auths, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"digest": digestOf(body), "size": len(body)})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/publish"):
			_ = json.Unmarshal(body, &b.publishBody)
			b.storedPayload = b.publishBody["payload"]
			if b.publishStatus == http.StatusConflict {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"version exists"}`))
				return
			}
			var version string
			_ = json.Unmarshal(b.publishBody["version"], &version)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"package": "cloud/doc-contents-platform",
				"version": map[string]any{"id": "v1", "package_id": "p1", "version": version, "manifest_id": "m1", "state": "published", "visibility": "private"},
				"manifest": map[string]any{
					"id": "m1", "package_id": "p1", "media_type": siteContentManifestMediaType, "payload": b.storedPayload,
				},
				"channel": map[string]any{"name": "canary", "version": version},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest") && b.readbackStored:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "m1", "package_id": "p1", "media_type": siteContentManifestMediaType, "payload": b.storedPayload,
			})
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	})
}

func siteContentBrokerEnv(t *testing.T, url string) map[string]string {
	t.Helper()
	return map[string]string{
		"PUTNAMI_HOME":             t.TempDir(),
		"PUTNAMI_REGISTRY_PUT_URL": url + "/put",
		"PUTNAMI_CLOUD_TOKEN":      siteContentTestCapToken,
	}
}

func quietIO(client *http.Client) clicore.IO {
	return clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}, JSON: func(any) {}, Client: client}
}

func TestPublishSiteContentMembersPublishesThePlannedVersionOnThePlanChannel(t *testing.T) {
	broker := &fakePutBroker{}
	srv := newPutTestServer(broker.handler(t))
	defer srv.Close()
	ws := writeSiteContentProject(t, `"site-content"`)
	params := map[string]any{
		"app": "docs/example.dev", "json": true, "channel": "canary",
		releaseset.ContextParamName: siteContentPlan("cloud/doc-contents-platform", "canary"),
	}

	results, err := PublishSiteContentMembers(params, nil, ws, siteContentBrokerEnv(t, srv.URL), quietIO(srv.Client()))
	if err != nil {
		t.Fatalf("publish planned members: %v", err)
	}

	wantRequests := []string{
		"POST /put/put/cloud/doc-contents-platform/blobs",
		"POST /put/put/cloud/doc-contents-platform/publish",
	}
	if !reflect.DeepEqual(broker.requests, wantRequests) {
		t.Fatalf("broker requests = %q, want %q", broker.requests, wantRequests)
	}
	for i, auth := range broker.auths {
		if auth != "Bearer "+siteContentTestCapToken {
			t.Fatalf("request %d authorization = %q, want the local capability", i, auth)
		}
	}
	if string(broker.publishBody["version"]) != `"`+siteContentTestVersion+`"` {
		t.Fatalf("published version = %s, want the planned version", broker.publishBody["version"])
	}
	if string(broker.publishBody["channel"]) != `"canary"` {
		t.Fatalf("published channel = %s, want the plan's first channel", broker.publishBody["channel"])
	}
	var wrapper struct {
		Bundle json.RawMessage `json:"bundle"`
	}
	if err := json.Unmarshal(broker.storedPayload, &wrapper); err != nil {
		t.Fatalf("stored payload is not a site-content wrapper: %v", err)
	}
	manifest, diags := sitecontent.ParseAndValidateManifest(wrapper.Bundle)
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("bundle.json invalid: %v", diags)
	}
	if manifest.Source.Commit != siteContentTestRevision {
		t.Fatalf("bundle source commit = %q, want the planned source revision", manifest.Source.Commit)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want one member", results)
	}
	got := results[0]
	if got.Coordinate != "cloud/doc-contents-platform" || got.Version != siteContentTestVersion || got.Channel != "canary" ||
		got.ArtifactDigest != digestOfBytes(broker.storedPayload) {
		t.Fatalf("result = %+v, want the planned identity and the stored manifest digest %s", got, digestOfBytes(broker.storedPayload))
	}
}

// A retry of the same plan finds its version already stored. The stored bytes
// must match, and the channel is never moved outside the atomic publish.
func TestPublishSiteContentMembersConfirmsARetriedVersionWithoutMovingAChannel(t *testing.T) {
	broker := &fakePutBroker{publishStatus: http.StatusConflict, readbackStored: true}
	srv := newPutTestServer(broker.handler(t))
	defer srv.Close()
	ws := writeSiteContentProject(t, `"site-content"`)
	params := map[string]any{"app": "docs/example.dev", "json": true, releaseset.ContextParamName: siteContentPlan("cloud/doc-contents-platform", "canary")}

	results, err := PublishSiteContentMembers(params, nil, ws, siteContentBrokerEnv(t, srv.URL), quietIO(srv.Client()))
	if err != nil {
		t.Fatalf("retried publish: %v", err)
	}
	wantRequests := []string{
		"POST /put/put/cloud/doc-contents-platform/blobs",
		"POST /put/put/cloud/doc-contents-platform/publish",
		"GET /put/put/cloud/doc-contents-platform/versions/" + siteContentTestVersion + "/manifest",
	}
	if !reflect.DeepEqual(broker.requests, wantRequests) {
		t.Fatalf("broker requests = %q, want %q", broker.requests, wantRequests)
	}
	if len(results) != 1 || results[0].ArtifactDigest != digestOfBytes(broker.storedPayload) {
		t.Fatalf("results = %+v", results)
	}
}

func TestPublishSiteContentMembersRefusesBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]any
		wantErr string
	}{
		{"planned member without a section", map[string]any{
			"app": "docs/example.dev", "json": true,
			releaseset.ContextParamName: siteContentPlan("cloud/doc-contents-missing", "canary"),
		}, "no matching section"},
		{"channel outside the plan", map[string]any{
			"app": "docs/example.dev", "json": true, "channel": "stable",
			releaseset.ContextParamName: siteContentPlan("cloud/doc-contents-platform", "canary"),
		}, "must match"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			broker := &fakePutBroker{}
			srv := newPutTestServer(broker.handler(t))
			defer srv.Close()
			ws := writeSiteContentProject(t, `"site-content"`)
			_, err := PublishSiteContentMembers(test.params, nil, ws, siteContentBrokerEnv(t, srv.URL), quietIO(srv.Client()))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
			if len(broker.requests) != 0 {
				t.Fatalf("broker requests = %q, want none", broker.requests)
			}
		})
	}
}

func TestPublishSiteContentMembersPublishesNothingWithoutAPlanOrADeclaration(t *testing.T) {
	cases := []struct {
		name    string
		publish string
		params  map[string]any
	}{
		{"no release-set plan", `"site-content"`, map[string]any{"app": "docs/example.dev", "json": true}},
		{"project without the site-content entry", `"docker"`, map[string]any{
			"app": "docs/example.dev", "json": true,
			releaseset.ContextParamName: siteContentPlan("cloud/doc-contents-platform", "canary"),
		}},
		{"plan selects another project's member", `"site-content"`, map[string]any{
			"app": "docs/example.dev", "json": true,
			releaseset.ContextParamName: func() *releaseset.Plan {
				plan := siteContentPlan("cloud/doc-contents-platform", "canary")
				plan.Members[0].ProjectID = "/docs/other.dev"
				return plan
			}(),
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			broker := &fakePutBroker{}
			srv := newPutTestServer(broker.handler(t))
			defer srv.Close()
			ws := writeSiteContentProject(t, test.publish)
			results, err := PublishSiteContentMembers(test.params, nil, ws, siteContentBrokerEnv(t, srv.URL), quietIO(srv.Client()))
			if err != nil || results != nil {
				t.Fatalf("PublishSiteContentMembers = (%+v, %v), want a skip", results, err)
			}
			if len(broker.requests) != 0 {
				t.Fatalf("broker requests = %q, want none", broker.requests)
			}
		})
	}
}

func TestPackageSiteContentVerifiesEverySectionWithoutNetwork(t *testing.T) {
	ws := writeSiteContentProject(t, `"site-content"`)
	var got any
	ioctx := clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}, JSON: func(d any) { got = d }}
	if err := PackageSiteContent(map[string]any{"app": "docs/example.dev", "json": true}, nil, ws, ioctx); err != nil {
		t.Fatalf("package: %v", err)
	}
	if m := resultPayloadMap(t, got); m["status"] != "packaged" || m["count"] != 1 {
		t.Fatalf("package result = %v", got)
	}

	if err := os.MkdirAll(filepath.Join(ws, "docs", "example.dev", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PackageSiteContent(map[string]any{"app": "docs/example.dev", "json": true}, nil, ws, ioctx); err == nil ||
		!strings.Contains(err.Error(), "has no files") {
		t.Fatalf("empty section error = %v, want a refusal", err)
	}

	plain := writeSiteContentProject(t, `"docker"`)
	if err := PackageSiteContent(map[string]any{"app": "docs/example.dev", "json": true}, nil, plain, ioctx); err != nil {
		t.Fatalf("undeclared project must skip: %v", err)
	}
	if m := resultPayloadMap(t, got); m["status"] != "skipped" {
		t.Fatalf("undeclared project result = %v, want skipped", got)
	}
}

func TestSiteContentSectionsAndCoordinate(t *testing.T) {
	ws := writeSiteContentProject(t, `"site-content"`)
	// putnami writes .gen/ into every project; tooling state is never a section.
	if err := os.MkdirAll(filepath.Join(ws, "docs", "example.dev", ".gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "docs", "example.dev", ".gen", "version.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sections, err := SiteContentSections(filepath.Join(ws, "docs", "example.dev"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sections, []string{"platform"}) {
		t.Fatalf("sections = %v, want [platform] (site-level files are meta)", sections)
	}
	if got := SiteContentCoordinate("platform"); got != "cloud/doc-contents-platform" {
		t.Fatalf("coordinate = %q", got)
	}
}

func TestValidateSiteContentSourcesSharesPackageRejections(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		link bool
		file bool
		want string
	}{
		{name: "empty section", path: "empty", want: "has no files"},
		{name: "invalid mount", path: "Uppercase", want: "invalid mount"},
		{name: "linked page", path: "platform/link.md", link: true, want: "not a regular file"},
		{name: "invalid payload path", path: "platform/bad\\page.md", file: true, want: "invalid payload path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := writeSiteContentProject(t, `"site-content"`)
			path := filepath.Join(root, "docs", "example.dev", test.path)
			var err error
			if test.link {
				err = os.Symlink(t.TempDir(), path)
			} else if test.file {
				err = os.WriteFile(path, []byte("page"), 0o600)
			} else {
				err = os.Mkdir(path, 0o755)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, check := range []func(map[string]any, []string, string, clicore.IO) error{ValidateSiteContentSources, PackageSiteContent} {
				err := check(map[string]any{"app": "docs/example.dev"}, nil, root, quietIO(nil))
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("source contract: %v, want %q", err, test.want)
				}
			}
		})
	}
}

func TestValidateSiteContentSourcesReadsMetadataOnly(t *testing.T) {
	root := writeSiteContentProject(t, `"site-content"`)
	page := filepath.Join(root, "docs", "example.dev", "platform", "index.md")
	if err := os.Chmod(page, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(page, 0o600) })
	var got any
	ioctx := clicore.IO{
		Stdout: func(string) {}, Stderr: func(string) {}, JSON: func(data any) { got = data },
		Client: &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
			t.Fatal("source validation attempted a network request")
			return nil, nil
		})},
	}
	if err := ValidateSiteContentSources(map[string]any{"app": "docs/example.dev", "json": true}, nil, root, ioctx); err != nil {
		t.Fatalf("unreadable payload does not prevent source metadata validation: %v", err)
	}
	if result := resultPayloadMap(t, got); result["status"] != "validated" || result["files"] != 1 || result["sections"] != 1 {
		t.Fatalf("validation result = %v", got)
	}
	// Unrelated declarations never inspect their malformed source trees.
	plain := writeSiteContentProject(t, `"docker"`)
	if err := os.Mkdir(filepath.Join(plain, "docs", "example.dev", "Uppercase"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSiteContentSources(map[string]any{"app": "docs/example.dev", "json": true}, nil, plain, ioctx); err != nil {
		t.Fatal(err)
	}
	if result := resultPayloadMap(t, got); result["status"] != "skipped" {
		t.Fatalf("unrelated result = %v", got)
	}
}
