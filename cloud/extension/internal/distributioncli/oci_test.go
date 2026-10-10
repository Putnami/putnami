package distributioncli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

const testManifestPayload = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
	`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` +
	"1111111111111111111111111111111111111111111111111111111111111111" + `","size":2},` +
	`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:` +
	"2222222222222222222222222222222222222222222222222222222222222222" + `","size":3}]}`

var (
	testConfigDigest   = "sha256:" + strings.Repeat("1", 64)
	testLayerDigest    = "sha256:" + strings.Repeat("2", 64)
	testManifestDigest = "sha256:" + strings.Repeat("d", 64)
)

// testRepo is the single OCI repository every test in this file exercises.
const testRepo = "ns/app"

// serveManifest registers the manifest GET route for testRepo, answering every
// reference with the fixed test payload + digest.
func serveManifest(mux *http.ServeMux) *int {
	calls := new(int)
	mux.HandleFunc("GET /v2/"+testRepo+"/manifests/{reference}", func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		w.Header().Set("Docker-Content-Digest", testManifestDigest)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write([]byte(testManifestPayload))
	})
	return calls
}

// serveCapabilities advertises the given APIs on the probe route. It states
// the JSON content type the registry states (go.putnami.dev/http's JSON
// response), which the generated client requires to accept the declared body.
func serveCapabilities(mux *http.ServeMux, apis ...string) {
	mux.HandleFunc("GET /v2/_putnami/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"apis": apis})
	})
}

// TestOCICopyImageFastPath: when the registry advertises copy/v1, the copy is
// one server-side POST — no blob mounts, no manifest PUTs.
func TestOCICopyImageFastPath(t *testing.T) {
	var copyReq map[string]any
	var gotAuth string
	var mounts, manifestPuts int

	mux := http.NewServeMux()
	serveCapabilities(mux, "tag-digest/v1", "copy/v1")
	serveManifest(mux)
	mux.HandleFunc("POST /v2/_putnami/copy", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&copyReq)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /v2/{ns}/{name}/blobs/uploads", func(w http.ResponseWriter, _ *http.Request) {
		mounts++
		w.WriteHeader(201)
	})
	mux.HandleFunc("PUT /v2/{ns}/{name}/manifests/{reference}", func(w http.ResponseWriter, _ *http.Request) {
		manifestPuts++
		w.WriteHeader(201)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := ociCopyImage(context.Background(), srv.Client(), srv.URL, "jwt-token",
		ociRef{Repo: "ns/app", Tag: "latest"}, ociRef{Repo: "ns2/app2", Tag: "stable"}, "")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !res.FastPath {
		t.Error("expected the fast path to be used")
	}
	if gotAuth != "Bearer jwt-token" {
		t.Errorf("auth = %q, want Bearer jwt-token", gotAuth)
	}
	if copyReq["source"] != "ns/app" || copyReq["destination"] != "ns2/app2" || copyReq["digest"] != testManifestDigest {
		t.Errorf("copy request = %v", copyReq)
	}
	tags, _ := copyReq["tags"].([]any)
	if len(tags) != 1 || tags[0] != "stable" {
		t.Errorf("copy tags = %v, want [stable]", copyReq["tags"])
	}
	if mounts != 0 || manifestPuts != 0 {
		t.Errorf("fast path leaked into the standard API: mounts=%d manifestPuts=%d", mounts, manifestPuts)
	}
}

// TestOCICopyImageFallback: a registry without the capability route (probe
// 404s) is driven through the standard API — one cross-repo mount per
// referenced blob plus a manifest PUT, no fast-path calls.
func TestOCICopyImageFallback(t *testing.T) {
	var mounted []string
	var mountFrom string
	var putBody []byte
	var putCT string
	var fastPathCalls int

	mux := http.NewServeMux()
	// No capabilities route: the probe 404s → baseline-only.
	serveManifest(mux)
	mux.HandleFunc("POST /v2/_putnami/copy", func(w http.ResponseWriter, _ *http.Request) {
		fastPathCalls++
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /v2/ns2/app2/blobs/uploads", func(w http.ResponseWriter, r *http.Request) {
		mounted = append(mounted, r.URL.Query().Get("mount"))
		mountFrom = r.URL.Query().Get("from")
		w.WriteHeader(201)
	})
	mux.HandleFunc("PUT /v2/ns2/app2/manifests/{reference}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("reference") != "stable" {
			t.Errorf("manifest PUT reference = %q, want stable", r.PathValue("reference"))
		}
		putCT = r.Header.Get("Content-Type")
		putBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(201)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := ociCopyImage(context.Background(), srv.Client(), srv.URL, "jwt-token",
		ociRef{Repo: "ns/app", Tag: "latest"}, ociRef{Repo: "ns2/app2", Tag: "stable"}, "")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if res.FastPath {
		t.Error("fallback copy reported fast_path=true")
	}
	if fastPathCalls != 0 {
		t.Errorf("fallback still called the fast path %d times", fastPathCalls)
	}
	if !slices.Equal(mounted, []string{testConfigDigest, testLayerDigest}) {
		t.Errorf("mounted = %v, want [config layer]", mounted)
	}
	if mountFrom != "ns/app" {
		t.Errorf("mount from = %q, want ns/app", mountFrom)
	}
	if string(putBody) != testManifestPayload {
		t.Errorf("manifest PUT body = %q, want the source payload verbatim", string(putBody))
	}
	if putCT != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("manifest PUT Content-Type = %q", putCT)
	}
}

// TestOCICopyImageFallbackMountRefused: when the registry refuses a mount
// (202 upload session), the blob is pulled from the source repo and re-pushed
// monolithically — the only leg that moves bytes.
func TestOCICopyImageFallbackMountRefused(t *testing.T) {
	blobBytes := []byte("layer-bytes")
	var uploaded [][]byte

	mux := http.NewServeMux()
	serveManifest(mux)
	mux.HandleFunc("POST /v2/ns2/app2/blobs/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mount") != "" {
			w.WriteHeader(202) // mount refused → upload session fallback
			return
		}
		body, _ := io.ReadAll(r.Body)
		uploaded = append(uploaded, body)
		w.WriteHeader(201)
	})
	mux.HandleFunc("GET /v2/ns/app/blobs/{digest}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blobBytes)
	})
	mux.HandleFunc("PUT /v2/ns2/app2/manifests/{reference}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(201)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := ociCopyImage(context.Background(), srv.Client(), srv.URL, "jwt-token",
		ociRef{Repo: "ns/app", Digest: testManifestDigest}, ociRef{Repo: "ns2/app2", Tag: "stable"}, ""); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if len(uploaded) != 2 { // config + layer, both refused mounts
		t.Fatalf("uploads = %d, want 2", len(uploaded))
	}
	for _, body := range uploaded {
		if string(body) != string(blobBytes) {
			t.Errorf("uploaded blob = %q, want the pulled source bytes", string(body))
		}
	}
}

// TestOCIRetagImageFastPath: with tag-digest/v1 advertised and a digest-pinned
// ref, retag is a single fast-path POST — no manifest round trips at all.
func TestOCIRetagImageFastPath(t *testing.T) {
	var tagReq map[string]any
	mux := http.NewServeMux()
	serveCapabilities(mux, "tag-digest/v1")
	manifestGets := serveManifest(mux)
	mux.HandleFunc("POST /v2/_putnami/tag-digest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&tagReq)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := ociRetagImage(context.Background(), srv.Client(), srv.URL, "jwt-token",
		ociRef{Repo: "ns/app", Digest: testManifestDigest}, []string{"stable", "v2"}, "")
	if err != nil {
		t.Fatalf("retag: %v", err)
	}
	if !res.FastPath {
		t.Error("expected the fast path to be used")
	}
	if tagReq["repository"] != "ns/app" || tagReq["digest"] != testManifestDigest {
		t.Errorf("tag-digest request = %v", tagReq)
	}
	tags, _ := tagReq["tags"].([]any)
	if len(tags) != 2 || tags[0] != "stable" || tags[1] != "v2" {
		t.Errorf("tag-digest tags = %v, want [stable v2]", tagReq["tags"])
	}
	if *manifestGets != 0 {
		t.Errorf("digest-pinned fast-path retag fetched the manifest %d times, want 0", *manifestGets)
	}
}

// TestOCIRetagImageFallback: a non-2xx probe falls back to one manifest GET by
// digest plus a manifest PUT per tag.
func TestOCIRetagImageFallback(t *testing.T) {
	var putRefs []string
	mux := http.NewServeMux()
	// No capabilities route: probe 404s.
	serveManifest(mux)
	mux.HandleFunc("PUT /v2/ns/app/manifests/{reference}", func(w http.ResponseWriter, r *http.Request) {
		putRefs = append(putRefs, r.PathValue("reference"))
		w.WriteHeader(201)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := ociRetagImage(context.Background(), srv.Client(), srv.URL, "jwt-token",
		ociRef{Repo: "ns/app", Digest: testManifestDigest}, []string{"stable", "v2"}, "")
	if err != nil {
		t.Fatalf("retag: %v", err)
	}
	if res.FastPath {
		t.Error("fallback retag reported fast_path=true")
	}
	if !slices.Equal(putRefs, []string{"stable", "v2"}) {
		t.Errorf("manifest PUT refs = %v, want [stable v2]", putRefs)
	}
}

func TestParseOCIRef(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		in      string
		want    ociRef
		wantErr bool
	}{
		{in: "ns/app", want: ociRef{Repo: "ns/app"}},
		{in: "app", want: ociRef{Repo: "app"}},
		{in: "ns/app:latest", want: ociRef{Repo: "ns/app", Tag: "latest"}},
		{in: "ns/app@" + digest, want: ociRef{Repo: "ns/app", Digest: digest}},
		{in: "", wantErr: true},
		{in: "ns/app@sha256:short", wantErr: true},
		{in: ":tag", wantErr: true},
		{in: "ns/app:", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseOCIRef(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseOCIRef(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseOCIRef(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseOCIRef(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	if (ociRef{Repo: "ns/app"}).reference() != "latest" {
		t.Error("bare repo must default to the latest tag")
	}
}

// TestOCICommandUsage covers the arg validation legs that run before any
// credential resolution or network call.
func TestOCICommandUsage(t *testing.T) {
	ioctx := clicore.IO{
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: http.DefaultClient,
	}
	env := map[string]string{}

	if err := OCI(nil, []string{"unknown-verb"}, "", env, ioctx); err == nil {
		t.Error("unknown subcommand must error")
	}
	if err := OCI(nil, []string{"copy", "only-one-ref"}, "", env, ioctx); err == nil {
		t.Error("copy with one ref must error")
	}
	if err := OCI(nil, []string{"copy", "ns/a:x", "ns/b@sha256:" + strings.Repeat("a", 64)}, "", env, ioctx); err == nil {
		t.Error("copy with a digest destination must error")
	}
	if err := OCI(nil, []string{"retag", "ns/a:x"}, "", env, ioctx); err == nil {
		t.Error("retag without tags must error")
	}
}

// TestOCICopyImageFallbackRejectsIndex: against a registry without copy/v1, a
// multi-arch image index is refused up front rather than copied into a broken
// destination — the fallback cannot chase the index's child manifests.
func TestOCICopyImageFallbackRejectsIndex(t *testing.T) {
	indexPayload := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json",` +
		`"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` +
		strings.Repeat("1", 64) + `","size":1}]}`
	var manifestPuts int

	mux := http.NewServeMux()
	// No capabilities route → the probe 404s and the fallback runs.
	mux.HandleFunc("GET /v2/ns/app/manifests/{reference}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", testManifestDigest)
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		_, _ = w.Write([]byte(indexPayload))
	})
	mux.HandleFunc("PUT /v2/ns2/app2/manifests/{reference}", func(w http.ResponseWriter, _ *http.Request) {
		manifestPuts++
		w.WriteHeader(201)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := ociCopyImage(context.Background(), srv.Client(), srv.URL, "jwt-token",
		ociRef{Repo: "ns/app", Tag: "latest"}, ociRef{Repo: "ns2/app2", Tag: "stable"}, "")
	if err == nil {
		t.Fatal("expected a multi-arch index copy to be refused in the fallback")
	}
	if !strings.Contains(err.Error(), "multi-arch") {
		t.Errorf("error = %q, want it to mention multi-arch", err.Error())
	}
	if manifestPuts != 0 {
		t.Errorf("fallback wrote %d manifests for a refused index, want 0", manifestPuts)
	}
}

// ── revert ──────────────────────────────────────────────────────────────────

// TestOCIRevertTagFastPath is the CLI revert acceptance case: one server-side
// POST, the restored digest read from the server's answer (never guessed by the
// client), and the caller's provenance ref carried on the shared header.
func TestOCIRevertTagFastPath(t *testing.T) {
	var revertReq map[string]any
	var gotSourceRef string

	mux := http.NewServeMux()
	serveCapabilities(mux, "tag-digest/v1", "copy/v1", "revert/v1")
	mux.HandleFunc("POST /v2/_putnami/revert", func(w http.ResponseWriter, r *http.Request) {
		gotSourceRef = r.Header.Get(ociSourceRefHeader)
		_ = json.NewDecoder(r.Body).Decode(&revertReq)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repository": "ns/app",
			"tag":        "stable",
			"digest":     testManifestDigest,
			"reverted":   "sha256:" + strings.Repeat("e", 64),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := ociRevertTag(context.Background(), srv.Client(), srv.URL, "jwt-token",
		ociRef{Repo: "ns/app", Tag: "stable"}, "run/rollback")
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if res.Digest != testManifestDigest {
		t.Errorf("restored digest = %q, want the server's answer %q", res.Digest, testManifestDigest)
	}
	if res.Reverted != "sha256:"+strings.Repeat("e", 64) {
		t.Errorf("reverted-from digest = %q, want the server's answer", res.Reverted)
	}
	if revertReq["repository"] != "ns/app" || revertReq["tag"] != "stable" {
		t.Errorf("revert request = %v, want repository=ns/app tag=stable", revertReq)
	}
	if gotSourceRef != "run/rollback" {
		t.Errorf("%s = %q, want run/rollback", ociSourceRefHeader, gotSourceRef)
	}
}

// TestOCIRevertRequiresCapability proves revert refuses rather than inventing a
// fallback: the prior target lives only in the registry's move history.
func TestOCIRevertRequiresCapability(t *testing.T) {
	mux := http.NewServeMux()
	serveCapabilities(mux, "tag-digest/v1", "copy/v1")
	mux.HandleFunc("POST /v2/_putnami/revert", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("revert must not be attempted against a registry that does not advertise revert/v1")
		w.WriteHeader(404)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := ociRevertTag(context.Background(), srv.Client(), srv.URL, "jwt-token",
		ociRef{Repo: "ns/app", Tag: "stable"}, "")
	if err == nil {
		t.Fatal("expected an error when the registry does not advertise revert/v1")
	}
	if !strings.Contains(err.Error(), "revert/v1") {
		t.Errorf("error = %q, want it to name the missing capability", err.Error())
	}
}

// TestOCIRevertRejectsDigestRef keeps the verb's argument shape honest: a digest
// reference is immutable, so there is nothing to revert.
func TestOCIRevertRejectsDigestRef(t *testing.T) {
	err := ociRunRevert(map[string]any{}, []string{"ns/app@" + testManifestDigest}, map[string]string{}, clicore.IO{
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: http.DefaultClient,
	})
	if err == nil {
		t.Fatal("expected a usage error for a digest reference")
	}
}

// TestOCIUnknownSubcommandNamesRevert keeps the dispatch help in sync with the
// verbs the command actually implements.
func TestOCIUnknownSubcommandNamesRevert(t *testing.T) {
	err := OCI(map[string]any{}, []string{"bogus"}, "", map[string]string{}, clicore.IO{
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: http.DefaultClient,
	})
	if err == nil || !strings.Contains(err.Error(), "revert") {
		t.Fatalf("unknown-subcommand error = %v, want it to list the revert verb", err)
	}
}

// TestOCIRetagCarriesSourceRef proves the provenance header reaches both the
// fast path and the manifest-PUT fallback, so a move is attributable regardless
// of which path the registry drives.
func TestOCIRetagCarriesSourceRef(t *testing.T) {
	t.Run("fast path", func(t *testing.T) {
		var got string
		mux := http.NewServeMux()
		serveCapabilities(mux, "tag-digest/v1")
		mux.HandleFunc("POST /v2/_putnami/tag-digest", func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get(ociSourceRefHeader)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		if _, err := ociRetagImage(context.Background(), srv.Client(), srv.URL, "jwt-token",
			ociRef{Repo: "ns/app", Digest: testManifestDigest}, []string{"stable"}, "sha/abc"); err != nil {
			t.Fatalf("retag: %v", err)
		}
		if got != "sha/abc" {
			t.Errorf("%s = %q, want sha/abc", ociSourceRefHeader, got)
		}
	})

	t.Run("manifest put fallback", func(t *testing.T) {
		var got string
		mux := http.NewServeMux()
		serveCapabilities(mux)
		serveManifest(mux)
		mux.HandleFunc("PUT /v2/{ns}/{name}/manifests/{reference}", func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get(ociSourceRefHeader)
			w.WriteHeader(201)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		if _, err := ociRetagImage(context.Background(), srv.Client(), srv.URL, "jwt-token",
			ociRef{Repo: "ns/app", Digest: testManifestDigest}, []string{"stable"}, "sha/abc"); err != nil {
			t.Fatalf("retag: %v", err)
		}
		if got != "sha/abc" {
			t.Errorf("%s = %q, want sha/abc", ociSourceRefHeader, got)
		}
	})
}

// TestResolveOCICredentialsUsesMaterializedLeaseDirectly: a short-lived
// Distribution lease JWT materialized into Docker config by `cloud token
// --for oci --owner-workspace ... --package ... --materialize` is used as the
// bearer as-is. It must never be fed to the legacy api-key exchange, which
// would reject a JWT presented as grant_type api-key — the transport fails the
// test on any network call.
func TestResolveOCICredentialsUsesMaterializedLeaseDirectly(t *testing.T) {
	// HOME must be redirected too: materializeRegistryLease writes the Docker
	// config under HOME, not PUTNAMI_HOME.
	env := hometest.Env(t.TempDir(), map[string]string{"PUTNAMI_HOME": t.TempDir()})
	endpoint := RegistryEndpoint{Registry: RegistryOCI, Host: "oci.putnami.dev", URL: "https://oci.putnami.dev"}
	if err := WriteRegistriesState(env, &RegistriesState{
		Version: 1,
		Keys:    []KeyRef{{Registry: RegistryOCI, Host: endpoint.Host, URL: endpoint.URL}},
	}); err != nil {
		t.Fatal(err)
	}
	lease := workflowJWT(map[string]any{
		"aud": "distribution", "scope": "registry.package.read:oci/package/ns/app",
	})
	if err := materializeRegistryLease(env, endpoint, lease); err != nil {
		t.Fatal(err)
	}
	ioctx := clicore.IO{Client: &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected request during credential resolution: %s %s", request.Method, request.URL)
		return nil, nil
	})}}
	baseURL, token, err := resolveOCICredentials(nil, env, ioctx)
	if err != nil {
		t.Fatal(err)
	}
	if baseURL != endpoint.URL || token != lease {
		t.Fatalf("baseURL = %q, token = %q, want the materialized lease against %s unchanged", baseURL, token, endpoint.URL)
	}
}

func TestIsDistributionLeaseBearer(t *testing.T) {
	if isDistributionLeaseBearer("pkt_0123456789abcdef") {
		t.Fatal("legacy opaque pkt_* key misclassified as a lease bearer")
	}
	if isDistributionLeaseBearer("a.b.c") {
		t.Fatal("undecodable three-segment token misclassified as a lease bearer")
	}
	if !isDistributionLeaseBearer(workflowJWT(map[string]any{"aud": "distribution"})) {
		t.Fatal("distribution lease JWT not recognized as a bearer")
	}
}

// TestOCIServerAdvertisesProbesWithoutCredential pins what the capability probe
// promises now that it runs on oci-server's generated client: it reads the
// registry's advertised APIs, and it carries NO credential — the binding
// declares none, so the registry bearer the fast path uses cannot reach a route
// answered before any authorization decision was made.
func TestOCIServerAdvertisesProbesWithoutCredential(t *testing.T) {
	var authorization string
	probes := 0

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/_putnami/capabilities", func(w http.ResponseWriter, r *http.Request) {
		probes++
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apis":["tag-digest/v1","copy/v1"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if !ociServerAdvertises(context.Background(), srv.Client(), srv.URL, ociCapabilityCopyV1) {
		t.Fatal("copy/v1 is advertised but the probe did not read it")
	}
	if probes != 1 {
		t.Errorf("probes = %d, want 1", probes)
	}
	if authorization != "" {
		t.Errorf("probe Authorization = %q, want no credential on the capability probe", authorization)
	}
	if ociServerAdvertises(context.Background(), srv.Client(), srv.URL, ociCapabilityRevertV1) {
		t.Error("revert/v1 is not advertised; the probe must not claim it")
	}
}

// TestOCIServerAdvertisesDegradesToBaseline pins the probe's fail-safe
// direction: every answer it cannot trust reads as baseline-only, so the caller
// drives the standard distribution API instead of a fast path the registry
// never promised.
func TestOCIServerAdvertisesDegradesToBaseline(t *testing.T) {
	cases := []struct {
		name   string
		handle http.HandlerFunc
	}{
		{name: "no capabilities route"},
		{name: "refusal", handle: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"forbidden","message":"no"}`))
		}},
		{name: "answer outside the contract", handle: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apis":"copy/v1"}`))
		}},
		{name: "answer that is not json", handle: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(`copy/v1`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			if tc.handle != nil {
				mux.HandleFunc("GET /v2/_putnami/capabilities", tc.handle)
			}
			srv := httptest.NewServer(mux)
			defer srv.Close()

			if ociServerAdvertises(context.Background(), srv.Client(), srv.URL, ociCapabilityCopyV1) {
				t.Error("the probe claimed copy/v1 from an answer it cannot trust")
			}
		})
	}

	if ociServerAdvertises(context.Background(), http.DefaultClient, "", ociCapabilityCopyV1) {
		t.Error("the probe claimed copy/v1 without a registry to ask")
	}
}
