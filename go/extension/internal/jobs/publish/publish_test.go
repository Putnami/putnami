package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/releaseplan"
	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	gomod "go.putnami.dev/protocol/gomod"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/sdk/extension/releaseset"
)

func TestRun_NoChannelSkips(t *testing.T) {
	status, _, err := Run(&pctx.Context{}, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "SKIP" {
		t.Errorf("status = %q, want SKIP for an invocation with no channel flag", status)
	}
}

func TestGoModule_MissingConfig(t *testing.T) {
	t.Setenv("GO_REGISTRY_URL", "")
	ctx := &pctx.Context{Project: pctx.Project{Name: "mod", Path: "mod"}}
	status, _, err := goModule(ctx, jsonl.New(), nil)
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED when no registry URL", status)
	}
	if err == nil {
		t.Error("expected a non-nil error when registries.go.origin / GO_REGISTRY_URL is missing")
	}
}

func TestGoModule_DryRun(t *testing.T) {
	// An explicit token avoids the cloud seam shell-out (which runs before the
	// dry-run short-circuit).
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "test-token")
	var registryCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		registryCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	dir := t.TempDir()
	projectPath := "mod"
	pkgDir := filepath.Join(dir, ".putnami", "out", projectPath, "package")
	if err := os.MkdirAll(filepath.Join(pkgDir, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "go", "channel.json"),
		[]byte(`{"version":"1.0.0","channels":["go"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "go", "module.json"),
		[]byte(`{"modulePath":"go.putnami.dev/mod","version":"1.0.0","zipPath":"/tmp/x.zip","modPath":"/tmp/go.mod"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	declareGoOrigin(t, dir, server.URL)
	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		Project:       pctx.Project{Name: "mod", Path: projectPath},
		Params:        pctx.Params{"dry-run": json.RawMessage(`true`)},
	}
	var status string
	var data map[string]any
	var err error
	output := capturePublishOutput(t, func() {
		status, data, err = goModule(ctx, jsonl.New(), nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if data["dryRun"] != true {
		t.Errorf("expected dryRun=true, got %+v", data)
	}
	if data["modulePath"] != "go.putnami.dev/mod" {
		t.Errorf("modulePath = %v, want go.putnami.dev/mod", data["modulePath"])
	}
	for _, event := range parsePublishEvents(t, output) {
		if event["kind"] == "published" {
			t.Errorf("dry run emitted kind=published proof: %+v", event)
		}
		if event["kind"] == "package" && event["digestVerified"] != false {
			t.Errorf("dry-run package claimed digest verification: %+v", event)
		}
	}
	if registryCalls != 0 {
		t.Fatalf("dry run made %d registry calls, including a possible release", registryCalls)
	}
}

func TestGoModule_RejectsAndRedactsRegistryUserinfo(t *testing.T) {
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "test-token")
	dir := t.TempDir()
	projectPath := "mod"
	pkgDir := filepath.Join(dir, ".putnami", "out", projectPath, "package")
	if err := os.MkdirAll(filepath.Join(pkgDir, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "go", "channel.json"),
		[]byte(`{"version":"1.0.0","channels":["go"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "go", "module.json"),
		[]byte(`{"modulePath":"go.putnami.dev/mod","version":"1.0.0","zipPath":"/tmp/x.zip","modPath":"/tmp/go.mod"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	declareGoOrigin(t, dir, "https://publisher:secret@go.putnami.dev")
	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		Project:       pctx.Project{Name: "mod", Path: projectPath},
		Params:        pctx.Params{"dry-run": json.RawMessage(`true`)},
	}
	var status string
	var runErr error
	output := capturePublishOutput(t, func() {
		status, _, runErr = goModule(ctx, jsonl.New(), nil)
	})
	if status != "FAILED" || runErr == nil || !strings.Contains(runErr.Error(), "registry URL") {
		t.Fatalf("credential-bearing registry status=%q err=%v", status, runErr)
	}
	if strings.Contains(runErr.Error(), "secret") || strings.Contains(output, "secret") {
		t.Fatalf("registry URL userinfo leaked: err=%v output=%s", runErr, output)
	}
}

func TestGoModule_RejectsRemoteHTTPBeforeLeaseInDryRunAndPublish(t *testing.T) {
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "")
	originalResolver := registrycred.ResolveToken
	var leaseCalls int
	registrycred.ResolveToken = func(string) (string, string) {
		leaseCalls++
		return "", "no credential for this registry"
	}
	t.Cleanup(func() { registrycred.ResolveToken = originalResolver })

	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, "mod")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(workspace, "module.zip")
	modPath := filepath.Join(workspace, "version.mod")
	if err := os.WriteFile(zipPath, []byte("private zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modPath, []byte("module go.putnami.dev/mod\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeGoPublishMetadata(t, workspace, "v1.2.3", zipPath, modPath)
	declareGoOrigin(t, workspace, "http://registry.example")

	for _, dryRun := range []bool{true, false} {
		t.Run(fmt.Sprintf("dry-run=%t", dryRun), func(t *testing.T) {
			params := pctx.Params{}
			if dryRun {
				params["dry-run"] = json.RawMessage(`true`)
			}
			ctx := &pctx.Context{
				WorkspaceRoot: workspace,
				Project:       pctx.Project{Name: "go.putnami.dev/mod", Path: "mod", FullPath: projectRoot},
				Params:        params,
			}
			status, _, err := goModule(ctx, jsonl.New(), nil)
			if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "HTTPS") {
				t.Fatalf("remote HTTP dryRun=%t status=%q err=%v, want preflight HTTPS failure", dryRun, status, err)
			}
		})
	}
	if leaseCalls != 0 {
		t.Fatalf("remote HTTP reached registry lease resolution %d times, want 0", leaseCalls)
	}
}

func TestValidateGoRegistryURLAllowsHTTPOnlyForLoopback(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:8080/base/",
		"http://registry.localhost:8080/base/",
		"http://127.0.0.1:8080/base/",
		"http://[::1]:8080/base/",
	} {
		if _, err := validateGoRegistryURL(raw); err != nil {
			t.Errorf("validateGoRegistryURL(%q): %v", raw, err)
		}
	}
	if _, err := validateGoRegistryURL("http://registry.example/base/"); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("remote HTTP validation err=%v, want HTTPS failure", err)
	}
}

func TestGoModule_PrivateBearerIgnoresAmbientProxy(t *testing.T) {
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "target-token")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	var proxyRequests int
	var proxyAuthorization string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests++
		proxyAuthorization = r.Header.Get("Authorization")
		http.Error(w, "reflected target-token", http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)

	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, "mod")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(workspace, "module.zip")
	modPath := filepath.Join(workspace, "version.mod")
	if err := os.WriteFile(zipPath, []byte("private zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modPath, []byte("module go.putnami.dev/mod\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeGoPublishMetadata(t, workspace, "v1.2.3", zipPath, modPath)
	declareGoOrigin(t, workspace, "https://registry.example.invalid")
	ctx := &pctx.Context{
		WorkspaceRoot: workspace,
		Project:       pctx.Project{Name: "go.putnami.dev/mod", Path: "mod", FullPath: projectRoot},
		Params:        pctx.Params{},
	}
	var status string
	var runErr error
	output := capturePublishOutput(t, func() {
		status, _, runErr = goModule(ctx, jsonl.New(), nil)
	})
	if status != "FAILED" || runErr == nil {
		t.Fatalf("unreachable private registry status=%q err=%v", status, runErr)
	}
	if proxyRequests != 0 || proxyAuthorization != "" {
		t.Fatalf("ambient proxy observed Go registry request/token: requests=%d authorization=%q", proxyRequests, proxyAuthorization)
	}
	if strings.Contains(runErr.Error(), "target-token") || strings.Contains(output, "target-token") {
		t.Fatalf("Go registry failure leaked bearer: err=%v output=%s", runErr, output)
	}
}

func TestGoRegistryHTTPClientDisablesProxyAndRedirects(t *testing.T) {
	client, err := newGoRegistryHTTPClient()
	if err != nil {
		t.Fatalf("new Go registry client: %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("Go registry transport = %#v, want direct transport with Proxy=nil", client.Transport)
	}
	request, err := http.NewRequest(http.MethodGet, "https://other.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.CheckRedirect(request, nil); !errors.Is(got, http.ErrUseLastResponse) {
		t.Fatalf("Go registry redirect policy = %v, want http.ErrUseLastResponse", got)
	}
}

func TestManagedGoModuleDryRunUsesPlanWithoutPackageArtifacts(t *testing.T) {
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "test-token")
	var registryCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		registryCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, "mod")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module go.putnami.dev/mod\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	declareGoOrigin(t, workspace, server.URL)
	params := managedGoBootstrapParams(t)
	params["dry-run"] = json.RawMessage(`true`)
	ctx := &pctx.Context{
		WorkspaceRoot: workspace,
		Project:       pctx.Project{Name: "go.putnami.dev/mod", Path: "mod", FullPath: projectRoot},
		Params:        params,
	}

	var status string
	var data map[string]any
	var err error
	output := capturePublishOutput(t, func() {
		status, data, err = goModule(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("managed dry run = (%q, %+v, %v), want OK", status, data, err)
	}
	if data["modulePath"] != "go.putnami.dev/mod" || data["version"] != "v1.2.3-canary.1" || data["dryRun"] != true {
		t.Fatalf("managed dry-run result = %+v", data)
	}
	var packages int
	for _, event := range parsePublishEvents(t, output) {
		if event["kind"] == "published" {
			t.Fatalf("managed dry run emitted publication proof: %+v", event)
		}
		if event["kind"] == "package" && event["registry"] == "go" {
			packages++
			if event["name"] != "go.putnami.dev/mod" || event["version"] != "v1.2.3-canary.1" || event["dryRun"] != true || event["digestVerified"] != false {
				t.Fatalf("managed dry-run package event = %+v", event)
			}
		}
	}
	if packages != 1 {
		t.Fatalf("managed dry run emitted %d Go package events, want 1: %s", packages, output)
	}
	if registryCalls != 0 {
		t.Fatalf("managed dry run made %d registry calls", registryCalls)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, ".putnami", "out", "mod", "package", "go", "module.json")); !os.IsNotExist(statErr) {
		t.Fatalf("managed dry run unexpectedly required or created module.json: %v", statErr)
	}
}

func TestFullBootstrapGoModuleStaysPrivateWhenSparseIsOff(t *testing.T) {
	ctx, zipBytes, goModBytes, digest := managedGoPublishTestContext(t)
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "")

	originalTokenResolver := registrycred.ResolveToken
	var hosts []string
	registrycred.ResolveToken = func(host string) (string, string) {
		hosts = append(hosts, host)
		return "test-token", ""
	}
	t.Cleanup(func() { registrycred.ResolveToken = originalTokenResolver })

	var releases, anonymousReads, zipReads, modReads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == gomod.BlobUploadPath("go.putnami.dev/mod"):
			assertBearer(t, r)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"digest":%q}`, digest)
		case r.Method == http.MethodPut && r.URL.Path == gomod.VersionPath("go.putnami.dev/mod", "v1.2.3-canary.1"):
			assertBearer(t, r)
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"visibility"`) {
				t.Errorf("repository-controlled publish request asserted visibility: %s", body)
			}
			request, diagnostics := gomod.ParseAndValidatePublishVersionRequest(body)
			if len(diagnostics) > 0 || request == nil {
				t.Errorf("private bootstrap publish request = %+v diagnostics=%v", request, diagnostics)
			} else if request.DistTag != "" {
				t.Errorf("private bootstrap independently advanced dist tag %q", request.DistTag)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".zip"):
			if r.Header.Get("Authorization") == "" {
				anonymousReads++
			} else {
				assertBearer(t, r)
			}
			zipReads++
			_, _ = w.Write(zipBytes)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".mod"):
			if r.Header.Get("Authorization") == "" {
				anonymousReads++
			} else {
				assertBearer(t, r)
			}
			modReads++
			_, _ = w.Write(goModBytes)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			releases++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected registry request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	declareGoOrigin(t, ctx.WorkspaceRoot, server.URL)
	ctx.Params = managedGoBootstrapParams(t)

	originalDownload := goModDownload
	var smokeProxy string
	goModDownload = func(_ context.Context, proxy, _, modulePath, version string) (string, error) {
		smokeProxy = proxy
		if modulePath != "go.putnami.dev/mod" || version != "v1.2.3-canary.1" {
			t.Errorf("private smoke coordinate = %s@%s", modulePath, version)
		}
		return "", nil
	}
	t.Cleanup(func() { goModDownload = originalDownload })

	output := capturePublishOutput(t, func() {
		status, _, err := goModule(ctx, jsonl.New(), nil)
		if status != "OK" || err != nil {
			t.Fatalf("private full bootstrap = (%q, %v), want OK", status, err)
		}
	})
	published := findPublishedGoEvent(t, output)
	if published["artifactDigest"] != digest || published["digestVerified"] != true {
		t.Fatalf("private bootstrap publication evidence = %+v", published)
	}
	if len(hosts) != 1 {
		t.Fatalf("private bootstrap credential hosts = %#v, want exactly one", hosts)
	}
	if releases != 0 || anonymousReads != 0 {
		t.Fatalf("private bootstrap release/anonymous calls = %d/%d, want 0/0", releases, anonymousReads)
	}
	if zipReads != 1 || modReads != 1 {
		t.Fatalf("authenticated immutable reads zip/mod = %d/%d, want 1/1", zipReads, modReads)
	}
	wantProxy := strings.Replace(server.URL, "://", "://x-access-token:test-token@", 1) + ",off"
	if smokeProxy != wantProxy {
		t.Fatalf("private smoke GOPROXY = %q, want authenticated origin-only %q", smokeProxy, wantProxy)
	}
	if strings.Contains(smokeProxy, "proxy.golang.org") || strings.Contains(smokeProxy, "sum.golang.org") {
		t.Fatalf("private smoke leaked a public Go service into GOPROXY: %q", smokeProxy)
	}
}

// A release-set member is reported once per publication, new or reused. The
// second attempt is answered 409 Conflict by the registry: the version is
// immutable, so the run re-verifies the bytes the registry serves against the
// zip it built and reports the member with that same verified digest. That is
// what makes re-running a partially failed release safe — the coordinator gets
// a complete set instead of a missing member.
func TestSparseGoModuleStaysPrivateAndIdempotent(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "member-event-per-publication", "conflict-with-equal-digest-is-a-reuse")
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "")
	originalTokenResolver := registrycred.ResolveToken
	var tokenHosts []string
	registrycred.ResolveToken = func(host string) (string, string) {
		// The host is the whole request: no package, no action, no workspace.
		tokenHosts = append(tokenHosts, host)
		return "test-token", ""
	}
	t.Cleanup(func() { registrycred.ResolveToken = originalTokenResolver })
	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, "mod")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module go.putnami.dev/mod\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zipBytes := []byte("exact immutable go module zip bytes")
	goModBytes := []byte("module go.putnami.dev/mod\n\ngo 1.25\n")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(zipBytes))
	zipPath := filepath.Join(workspace, "module.zip")
	modPath := filepath.Join(workspace, "version.mod")
	if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modPath, goModBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	writeGoPublishMetadata(t, workspace, "v1.2.3-canary.1", zipPath, modPath)

	var uploads, publishes, releases, zipDownloads, modDownloads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == gomod.BlobUploadPath("go.putnami.dev/mod"):
			assertBearer(t, r)
			uploads++
			body, _ := io.ReadAll(r.Body)
			if string(body) != string(zipBytes) {
				t.Errorf("uploaded bytes = %q, want exact zip", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"digest":%q}`, digest)
		case r.Method == http.MethodPut && r.URL.Path == gomod.VersionPath("go.putnami.dev/mod", "v1.2.3-canary.1"):
			assertBearer(t, r)
			publishes++
			body, _ := io.ReadAll(r.Body)
			request, diagnostics := gomod.ParseAndValidatePublishVersionRequest(body)
			if len(diagnostics) > 0 || request == nil {
				t.Errorf("invalid publish request: %v", diagnostics)
			} else if request.DistTag != "" {
				t.Errorf("managed Go publish independently advanced dist tag %q", request.DistTag)
			}
			if publishes == 1 {
				w.WriteHeader(http.StatusCreated)
			} else {
				w.WriteHeader(http.StatusConflict)
			}
		case r.Method == http.MethodPost && r.URL.Path == gomod.ReleasePath("go.putnami.dev/mod", "v1.2.3-canary.1"):
			releases++
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/go.putnami.dev/mod/@v/v1.2.3-canary.1.zip":
			zipDownloads++
			assertBearer(t, r)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(zipBytes)
		case r.Method == http.MethodGet && r.URL.Path == "/go.putnami.dev/mod/@v/v1.2.3-canary.1.mod":
			assertBearer(t, r)
			modDownloads++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(goModBytes)
		default:
			t.Errorf("unexpected registry request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	originalDownload := goModDownload
	var smokeProxies []string
	goModDownload = func(_ context.Context, proxy, _, _, _ string) (string, error) {
		smokeProxies = append(smokeProxies, proxy)
		return "", nil
	}
	defer func() { goModDownload = originalDownload }()

	declareGoOrigin(t, workspace, server.URL)
	ctx := &pctx.Context{
		WorkspaceRoot: workspace,
		Project:       pctx.Project{Name: "go.putnami.dev/mod", Path: "mod", FullPath: projectRoot},
		Params:        managedGoSparseParams(t),
	}
	for attempt := 1; attempt <= 2; attempt++ {
		var status string
		var data map[string]any
		var err error
		output := capturePublishOutput(t, func() {
			status, data, err = goModule(ctx, jsonl.New(), nil)
		})
		if err != nil || status != "OK" {
			t.Fatalf("attempt %d = (%q, %v), want OK", attempt, status, err)
		}
		if data["artifactDigest"] != digest || data["digestVerified"] != true {
			t.Errorf("attempt %d result lacks verified digest: %+v", attempt, data)
		}
		published := findPublishedGoEvent(t, output)
		if published["registry"] != "go" || published["name"] != "go.putnami.dev/mod" ||
			published["version"] != "v1.2.3-canary.1" || published["artifactDigest"] != digest ||
			published["digestVerified"] != true || published["dryRun"] != false {
			t.Errorf("attempt %d published event = %+v", attempt, published)
		}
		if _, tagged := published["tags"]; tagged {
			t.Errorf("managed release-set event carried independently-updated tags: %+v", published)
		}
		// Attempt 2 is the reuse: same digest, same member, marked as reused so
		// a reader can tell a fresh upload from a verified re-run.
		if reused, marked := published["digestReused"]; marked != (attempt == 2) || (marked && reused != true) {
			t.Errorf("attempt %d digestReused = %v (present=%v), want present only on the conflict attempt", attempt, reused, marked)
		}
		if reused, marked := data["digestReused"]; marked != (attempt == 2) || (marked && reused != true) {
			t.Errorf("attempt %d result digestReused = %v (present=%v)", attempt, reused, marked)
		}
		member := findPublishedGoMemberEvent(t, output)
		if member.Ecosystem != "go" || member.Coordinate != "go.putnami.dev/mod" ||
			member.Version != "v1.2.3-canary.1" || member.ArtifactDigest != digest {
			t.Errorf("attempt %d published member = %+v", attempt, member)
		}
	}
	if uploads != 2 || publishes != 2 || releases != 0 || zipDownloads != 2 || modDownloads != 2 {
		t.Errorf("registry calls uploads/publishes/releases/zip/mod downloads = %d/%d/%d/%d/%d, want 2/2/0/2/2", uploads, publishes, releases, zipDownloads, modDownloads)
	}
	if len(tokenHosts) != 2 {
		t.Fatalf("registry credential hosts = %#v, want one per publish attempt", tokenHosts)
	}
	if len(smokeProxies) != 2 {
		t.Fatalf("managed smoke proxies = %v, want two attempts", smokeProxies)
	}
	wantProxy := strings.Replace(server.URL, "://", "://x-access-token:test-token@", 1) + ",off"
	for attempt, proxy := range smokeProxies {
		if proxy != wantProxy {
			t.Errorf("attempt %d private sparse smoke proxy = %q, want %q", attempt+1, proxy, wantProxy)
		}
	}
}

// An unmanaged Go publish uploads the module and nothing else: it never calls
// the managed release endpoint, and — since a channel is no longer a publish
// input — it no longer writes a registry dist-tag either. Advancing a channel
// is the release set's one transaction, after every member has a digest.
func TestLegacyGoModuleDoesNotCallManagedReleaseOrWriteADistTag(t *testing.T) {
	var releaseCalls, anonymousReads int
	ctx, zipBytes, goModBytes, digest := managedGoPublishTestContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == gomod.BlobUploadPath("go.putnami.dev/mod"):
			assertBearer(t, r)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"digest":%q}`, digest)
		case r.Method == http.MethodPut && r.URL.Path == gomod.VersionPath("go.putnami.dev/mod", "v1.2.3-canary.1"):
			assertBearer(t, r)
			body, _ := io.ReadAll(r.Body)
			request, diagnostics := gomod.ParseAndValidatePublishVersionRequest(body)
			if len(diagnostics) > 0 || request == nil || request.DistTag != "" {
				t.Errorf("legacy publish request = %+v diagnostics=%v", request, diagnostics)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".zip"):
			assertBearer(t, r)
			if r.Header.Get("Authorization") == "" {
				anonymousReads++
			}
			_, _ = w.Write(zipBytes)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".mod"):
			assertBearer(t, r)
			_, _ = w.Write(goModBytes)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			releaseCalls++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected registry request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	declareGoOrigin(t, ctx.WorkspaceRoot, server.URL)
	// The channel parameter is deliberately still supplied: a stale caller must
	// not be able to move a registry marker through it.
	ctx.Params = pctx.Params{"channel": json.RawMessage(`"canary"`)}
	stubGoModuleDownload(t)

	var status string
	var runErr error
	output := capturePublishOutput(t, func() {
		status, _, runErr = goModule(ctx, jsonl.New(), nil)
	})
	if status != "OK" || runErr != nil {
		t.Fatalf("legacy goModule = (%q, %v), want OK", status, runErr)
	}
	findPublishedGoEvent(t, output)
	if releaseCalls != 0 || anonymousReads != 0 {
		t.Fatalf("legacy calls release/anonymous = %d/%d, want 0/0", releaseCalls, anonymousReads)
	}
}

func assertBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if authorization := r.Header.Get("Authorization"); authorization != "Bearer test-token" {
		t.Errorf("authorization = %q, want publisher bearer", authorization)
	}
}

func TestUploadZipBlobRejectsRegistryDigestMismatch(t *testing.T) {
	zipBytes := []byte("uploaded")
	zipPath := filepath.Join(t.TempDir(), "module.zip")
	if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"digest":"sha256:%s"}`, strings.Repeat("f", 64))
	}))
	defer server.Close()
	if _, err := uploadZipBlob(server.Client(), jsonl.New(), server.URL, "token", "go.putnami.dev/mod", zipPath); err == nil || !strings.Contains(err.Error(), "does not match uploaded zip digest") {
		t.Fatalf("upload mismatch error = %v", err)
	}
}

func TestUploadZipBlobDoesNotReflectBearerFromHostileDigest(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "module.zip")
	if err := os.WriteFile(zipPath, []byte("uploaded"), 0o644); err != nil {
		t.Fatal(err)
	}
	bearer := digestForTest('f')
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"digest":%q}`, bearer)
	}))
	defer server.Close()

	var uploadErr error
	output := capturePublishOutput(t, func() {
		_, uploadErr = uploadZipBlob(server.Client(), jsonl.New(), server.URL, bearer, "go.putnami.dev/mod", zipPath)
	})
	if uploadErr == nil || !strings.Contains(uploadErr.Error(), "does not match uploaded zip digest") {
		t.Fatalf("hostile digest upload error = %v", uploadErr)
	}
	if strings.Contains(uploadErr.Error(), bearer) || strings.Contains(output, bearer) {
		t.Fatalf("hostile registry digest reflected publisher bearer: err=%v output=%s", uploadErr, output)
	}
}

func TestManagedGoRegistryResponsesAreBoundedAndSanitized(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "module.zip")
	if err := os.WriteFile(zipPath, []byte("zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("blob error body is not exposed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "sensitive-blob-detail")
		}))
		defer server.Close()
		output := capturePublishOutput(t, func() {
			_, err := uploadZipBlob(server.Client(), jsonl.New(), server.URL, "token", "go.putnami.dev/mod", zipPath)
			if err == nil || !strings.Contains(err.Error(), "blob upload returned 401") {
				t.Fatalf("blob upload error = %v", err)
			}
		})
		if strings.Contains(output, "sensitive-blob-detail") {
			t.Fatalf("blob diagnostic exposed response body: %s", output)
		}
	})

	t.Run("blob success response is bounded", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxGoRegistryResponseBytes+1))
		}))
		defer server.Close()
		if _, err := uploadZipBlob(server.Client(), jsonl.New(), server.URL, "token", "go.putnami.dev/mod", zipPath); err == nil || !strings.Contains(err.Error(), "response exceeds") {
			t.Fatalf("oversized blob response error = %v", err)
		}
	})

	t.Run("version error body is not exposed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "sensitive-version-detail")
		}))
		defer server.Close()
		output := capturePublishOutput(t, func() {
			_, err := publishVersion(server.Client(), jsonl.New(), server.URL, "token", "go.putnami.dev/mod", "v1.2.3", "module go.putnami.dev/mod\n", digestForTest('a'), true)
			if err == nil || !strings.Contains(err.Error(), "publish returned 403") {
				t.Fatalf("publish error = %v", err)
			}
		})
		if strings.Contains(output, "sensitive-version-detail") {
			t.Fatalf("version diagnostic exposed response body: %s", output)
		}
	})
}

func digestForTest(char byte) string {
	return "sha256:" + strings.Repeat(string(char), 64)
}

func TestVerifyPublishedZipRejectsDownloadedDigestMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("different registry bytes"))
	}))
	defer server.Close()
	want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("uploaded bytes")))
	err := verifyPublishedZip(server.Client(), server.URL, "", "go.putnami.dev/mod", "v1.2.3", want)
	if err == nil || !strings.Contains(err.Error(), "does not match uploaded digest") {
		t.Fatalf("download mismatch error = %v", err)
	}
}

func TestVerifyPublishedModRejectsDownloadedBytesMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("module go.putnami.dev/other\n"))
	}))
	defer server.Close()
	err := verifyPublishedMod(server.Client(), server.URL, "", "go.putnami.dev/mod", "v1.2.3", []byte("module go.putnami.dev/mod\n"))
	if err == nil || !strings.Contains(err.Error(), "does not match submitted bytes") {
		t.Fatalf("go.mod mismatch error = %v", err)
	}
}

func TestManagedGoModuleSkipsInheritedUpstreamBeforeUpload(t *testing.T) {
	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, "upstream")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module go.putnami.dev/upstream\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{
		WorkspaceRoot: workspace,
		Project:       pctx.Project{Name: "go.putnami.dev/upstream", Path: "upstream", FullPath: projectRoot},
		Params:        sparsePublishPlanParams(t),
	}
	status, _, err := goModule(ctx, jsonl.New(), nil)
	if err != nil || status != "SKIP" {
		t.Fatalf("unchanged upstream publish = (%q, %v), want SKIP before registry configuration", status, err)
	}
}

func managedGoPublishTestContext(t *testing.T) (*pctx.Context, []byte, []byte, string) {
	t.Helper()
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "test-token")
	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, "mod")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	goModBytes := []byte("module go.putnami.dev/mod\n\ngo 1.25\n")
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), goModBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	zipBytes := []byte("exact immutable go module zip bytes")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(zipBytes))
	zipPath := filepath.Join(workspace, "module.zip")
	modPath := filepath.Join(workspace, "version.mod")
	if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modPath, goModBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	writeGoPublishMetadata(t, workspace, "v1.2.3-canary.1", zipPath, modPath)
	return &pctx.Context{
		WorkspaceRoot: workspace,
		Project:       pctx.Project{Name: "go.putnami.dev/mod", Path: "mod", FullPath: projectRoot},
	}, zipBytes, goModBytes, digest
}

func stubGoModuleDownload(t *testing.T) {
	t.Helper()
	originalDownload := goModDownload
	goModDownload = func(_ context.Context, _, _, _, _ string) (string, error) { return "", nil }
	t.Cleanup(func() { goModDownload = originalDownload })
}

func managedGoBootstrapParams(t *testing.T) pctx.Params {
	t.Helper()
	plan := releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channels:        []string{"canary"},
		Heads:           map[string]*distribution.ChannelHead{"canary": nil},
		Members: []releaseset.PlannedMember{{
			Ecosystem: releaseplan.GoEcosystem, Coordinate: "go.putnami.dev/mod", Version: "v1.2.3-canary.1",
			Dependencies: []distribution.ReleaseSetDependency{}, Selected: true, ProjectID: "/mod",
			SourceRevision: goPublishTestRevision, SelectionFingerprint: digestForTest('9'),
		}},
	}
	raw, err := json.Marshal(&plan)
	if err != nil {
		t.Fatal(err)
	}
	return pctx.Params{
		releaseset.ContextParamName: raw,
		"channel":                   json.RawMessage(`"canary"`),
	}
}

func managedGoSparseParams(t *testing.T) pctx.Params {
	t.Helper()
	base := &distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members: []distribution.ReleaseSetMember{{
			Ecosystem: releaseplan.GoEcosystem, Coordinate: "go.putnami.dev/mod", Version: "v1.2.3-canary.0",
			ArtifactDigest: digestForTest('a'), Dependencies: []distribution.ReleaseSetDependency{},
			SourceRevision: goPublishTestRevision, SelectionFingerprint: digestForTest('1'),
		}},
	}
	ref, diagnostics := distribution.DeriveReleaseSetRef(base)
	if len(diagnostics) > 0 {
		t.Fatalf("derive managed sparse base: %v", diagnostics)
	}
	plan := releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channels:        []string{"canary"},
		Heads: map[string]*distribution.ChannelHead{
			"canary": {Ref: ref, Generation: 3, ReleaseSet: base},
		},
		Members: []releaseset.PlannedMember{{
			Ecosystem: releaseplan.GoEcosystem, Coordinate: "go.putnami.dev/mod", Version: "v1.2.3-canary.1",
			Dependencies: []distribution.ReleaseSetDependency{}, Selected: true, ProjectID: "/mod",
			SourceRevision: goPublishTestRevision, SelectionFingerprint: digestForTest('9'),
		}},
	}
	raw, err := json.Marshal(&plan)
	if err != nil {
		t.Fatal(err)
	}
	return pctx.Params{
		releaseset.ContextParamName: raw,
		"channel":                   json.RawMessage(`"canary"`),
	}
}

func sparsePublishPlanParams(t *testing.T) pctx.Params {
	t.Helper()
	digest := func(char string) string { return "sha256:" + strings.Repeat(char, 64) }
	base := &distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members: []distribution.ReleaseSetMember{
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: "go.putnami.dev/downstream", Version: "v1.0.0", ArtifactDigest: digest("d"), Dependencies: []distribution.ReleaseSetDependency{{Ecosystem: releaseplan.GoEcosystem, Coordinate: "go.putnami.dev/upstream", Version: "v1.0.0"}}, SourceRevision: goPublishTestRevision, SelectionFingerprint: digest("1")},
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: "go.putnami.dev/upstream", Version: "v1.0.0", ArtifactDigest: digest("e"), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: goPublishTestRevision, SelectionFingerprint: digest("2")},
		},
	}
	ref, diagnostics := distribution.DeriveReleaseSetRef(base)
	if len(diagnostics) > 0 {
		t.Fatalf("derive sparse publish plan base: %v", diagnostics)
	}
	plan := releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channels:        []string{"canary"},
		Heads: map[string]*distribution.ChannelHead{
			"canary": {Ref: ref, Generation: 3, ReleaseSet: base},
		},
		Members: []releaseset.PlannedMember{
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: "go.putnami.dev/downstream", Version: "v1.1.0-canary.1", Dependencies: []distribution.ReleaseSetDependency{{Ecosystem: releaseplan.GoEcosystem, Coordinate: "go.putnami.dev/upstream", Version: "v1.0.0"}}, Selected: true, ProjectID: "/downstream", SourceRevision: goPublishTestRevision, SelectionFingerprint: digest("9")},
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: "go.putnami.dev/upstream", Version: "v1.0.0", ArtifactDigest: digest("e"), Dependencies: []distribution.ReleaseSetDependency{}, ProjectID: "/upstream", SourceRevision: goPublishTestRevision, SelectionFingerprint: digest("2")},
		},
	}
	raw, err := json.Marshal(&plan)
	if err != nil {
		t.Fatal(err)
	}
	return pctx.Params{releaseset.ContextParamName: raw}
}

// declareGoOrigin writes the workspace document a real workspace carries: the
// Go module origin lives in `registries.go`, keyed by ecosystem, and is the one
// place the publisher reads its endpoint from.
func declareGoOrigin(t *testing.T, workspace, origin string) {
	t.Helper()
	document := fmt.Sprintf(`{"registries":{"go":{"origin":%q}}}`, origin)
	if err := os.WriteFile(filepath.Join(workspace, "putnami.workspace.json"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGoPublishMetadata(t *testing.T, workspace, version, zipPath, modPath string) {
	t.Helper()
	packageDir := filepath.Join(workspace, ".putnami", "out", "mod", "package")
	if err := os.MkdirAll(filepath.Join(packageDir, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The go packager records its channel inside the directory it owns, so a
	// restore of that directory brings the record back with the module.
	if err := os.WriteFile(filepath.Join(packageDir, "go", "channel.json"), []byte(fmt.Sprintf(
		`{"version":%q,"channels":["go"]}`, version)), 0o644); err != nil {
		t.Fatal(err)
	}
	moduleMetadata, err := json.Marshal(map[string]string{
		"modulePath": "go.putnami.dev/mod", "version": version, "zipPath": zipPath, "modPath": modPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "go", "module.json"), moduleMetadata, 0o644); err != nil {
		t.Fatal(err)
	}
}

func capturePublishOutput(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	done := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		done <- data
	}()
	fn()
	_ = writer.Close()
	os.Stdout = original
	data := <-done
	_ = reader.Close()
	return string(data)
}

func parsePublishEvents(t *testing.T, output string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("parse JSONL event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func findPublishedGoEvent(t *testing.T, output string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, event := range parsePublishEvents(t, output) {
		if event["type"] == "artifact" && event["kind"] == "published" && event["registry"] == "go" {
			if found != nil {
				t.Fatalf("multiple published Go events: %s", output)
			}
			found = event
		}
	}
	if found == nil {
		t.Fatalf("no published Go event: %s", output)
	}
	return found
}

// findPublishedGoMemberEvent returns the one published-member event of the go
// ecosystem, read back through the protocol's own validator. The publisher
// writes the member's fields at the top level of the artifact event, so a
// consumer reads them the way this does.
func findPublishedGoMemberEvent(t *testing.T, output string) *extproto.PublishedMember {
	t.Helper()
	var found map[string]any
	for _, event := range parsePublishEvents(t, output) {
		if event["type"] != "artifact" || event["kind"] != extproto.PublishedMemberEventKind {
			continue
		}
		if found != nil {
			t.Fatalf("multiple published-member events: %s", output)
		}
		found = event
	}
	if found == nil {
		t.Fatalf("no published-member event: %s", output)
	}
	field := func(name string) string {
		value, ok := found[name].(string)
		if !ok {
			t.Fatalf("published-member event field %q = %v, want a string", name, found[name])
		}
		return value
	}
	member := &extproto.PublishedMember{
		Ecosystem:      field("ecosystem"),
		Coordinate:     field("coordinate"),
		Version:        field("version"),
		ArtifactDigest: field("artifactDigest"),
	}
	if diagnostics := extproto.ValidatePublishedMember(member); len(diagnostics) > 0 {
		t.Fatalf("published member %+v is invalid: %v", member, diagnostics)
	}
	return member
}

func TestResolveGoPublishToken_ExplicitWins(t *testing.T) {
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "")

	// The explicit kebab param wins over everything (and never reaches the cloud).
	if tok, _ := resolveGoPublishToken(pctx.Params{"go-registry-token": json.RawMessage(`"flagtok"`)}, "h"); tok != "flagtok" {
		t.Errorf("go-registry-token param should win, got %q", tok)
	}

	// PUTNAMI_REGISTRY_TOKEN wins over config.
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "envtok")
	if tok, _ := resolveGoPublishToken(pctx.Params{"goRegistryToken": json.RawMessage(`"configtok"`)}, "h"); tok != "envtok" {
		t.Errorf("PUTNAMI_REGISTRY_TOKEN should win over config, got %q", tok)
	}
}

func TestResolveGoPublishToken_AsksTheCloudByHost(t *testing.T) {
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "")
	original := registrycred.ResolveToken
	t.Cleanup(func() { registrycred.ResolveToken = original })
	registrycred.ResolveToken = func(got string) (string, string) {
		if got != "go.putnami.dev" {
			t.Fatalf("host = %q, want go.putnami.dev", got)
		}
		return "host-token", ""
	}
	if token, hint := resolveGoPublishToken(nil, "go.putnami.dev"); token != "host-token" || hint != "" {
		t.Fatalf("token = %q, hint = %q", token, hint)
	}
}

func TestGoProxyURL(t *testing.T) {
	if got, _ := goProxyURL("https://go.putnami.dev", ""); got != "https://go.putnami.dev,off" {
		t.Errorf("no token: got %q", got)
	}
	got, err := goProxyURL("https://go.putnami.dev", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://x-access-token:tok@go.putnami.dev,off" {
		t.Errorf("with token: got %q", got)
	}
}

func TestPrivateGoSmokeEnvironmentHasOneAuthenticatedOriginAndNoSumDB(t *testing.T) {
	origin := "https://x-access-token:tok@go.putnami.dev,off"
	cache := filepath.Join(t.TempDir(), "mod-cache")
	env := goModDownloadEnvironment([]string{
		"HOME=" + t.TempDir(),
		"GOPROXY=https://proxy.golang.org,direct",
		"GONOPROXY=go.putnami.dev/*",
		"GOSUMDB=sum.golang.org",
		"GOMODCACHE=/untrusted/cache",
		"PUTNAMI_CLOUD_TOKEN=cloud-secret",
		"NODE_OPTIONS=--require=/tmp/preload.js",
		"HTTP_PROXY=http://proxy.invalid",
		"https_proxy=http://proxy.invalid",
		"ALL_PROXY=socks5://proxy.invalid",
		"NO_PROXY=localhost",
	}, "/usr/bin/go", origin, cache)

	want := map[string]string{
		"GOPROXY": origin,
		// "none", not "": an empty GONOPROXY falls back to GOPRIVATE, which can
		// come from Go's env file and would route the module past the very
		// registry this job exists to exercise.
		"GONOPROXY":  "none",
		"GOSUMDB":    "off",
		"GOMODCACHE": cache,
	}
	for key, value := range want {
		prefix := key + "="
		var matches []string
		for _, entry := range env {
			if strings.HasPrefix(entry, prefix) {
				matches = append(matches, entry)
			}
		}
		if len(matches) != 1 || matches[0] != prefix+value {
			t.Errorf("private smoke %s entries = %q, want exactly [%q]", key, matches, prefix+value)
		}
	}
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{
		"proxy.golang.org", "sum.golang.org", "cloud-secret", "preload.js", "proxy.invalid",
		"PUTNAMI_CLOUD_TOKEN=", "NODE_OPTIONS=", "HTTP_PROXY=", "https_proxy=", "ALL_PROXY=", "NO_PROXY=",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("private smoke environment retained forbidden capability %q:\n%s", forbidden, joined)
		}
	}
}

func TestPrivateGoSmokeRedactsCredentialFromDownloadError(t *testing.T) {
	const token = "secret/token"
	originalDownload := goModDownload
	goModDownload = func(_ context.Context, proxy, _, _, _ string) (string, error) {
		return "raw " + token + "\nfetch " + proxy, fmt.Errorf("download failed")
	}
	t.Cleanup(func() { goModDownload = originalDownload })

	err := smokeTestGoModule("https://go.putnami.dev", token, "go.putnami.dev/mod", "v1.2.3")
	if err == nil {
		t.Fatal("smoke error = nil, want failure")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "secret%2Ftoken") ||
		strings.Count(err.Error(), "[REDACTED]") != 2 {
		t.Fatalf("smoke error did not redact registry token: %v", err)
	}
}

const goPublishTestRevision = "0123456789abcdef0123456789abcdef01234567"

// A native publication run exports a numeric-loopback broker for the Go
// registry and holds the capability itself. The publisher must send every
// registry call — blob upload, version PUT, both immutable verification reads,
// and the consumer smoke — to that broker, and ask the credential seam about
// the BROKER host so the cloud answers with the run's capability. Asking about
// go.putnami.dev fails with "unknown registry host go.putnami.dev; run
// `putnami cloud login`", and the run has no session to log in with.
func TestGoModule_PrivateBrokerCarriesEveryRegistryCallAndSkipsTheToolchainSmoke(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "member-event-per-publication", "private-loopback-broker-carries-the-publication")
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "")
	const capability = "run-capability-token"
	originalTokenResolver := registrycred.ResolveToken
	var tokenHosts []string
	registrycred.ResolveToken = func(host string) (string, string) {
		tokenHosts = append(tokenHosts, host)
		return capability, ""
	}
	t.Cleanup(func() { registrycred.ResolveToken = originalTokenResolver })

	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, "mod")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	zipBytes := []byte("exact immutable go module zip bytes")
	goModBytes := []byte("module go.putnami.dev/mod\n\ngo 1.25\n")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(zipBytes))
	zipPath := filepath.Join(workspace, "module.zip")
	modPath := filepath.Join(workspace, "version.mod")
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), goModBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modPath, goModBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	writeGoPublishMetadata(t, workspace, "v1.2.3-canary.1", zipPath, modPath)

	// The canonical origin must never be contacted: any request here fails the test.
	canonical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("canonical origin received %s %s; every call must go through the broker", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer canonical.Close()

	type brokerCall struct{ Method, Path, Authorization string }
	var calls []brokerCall
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, brokerCall{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		if !strings.HasPrefix(r.URL.Path, "/go/") || r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+capability {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/go")
		switch {
		case r.Method == http.MethodPost && path == gomod.BlobUploadPath("go.putnami.dev/mod"):
			body, _ := io.ReadAll(r.Body)
			if string(body) != string(zipBytes) {
				t.Errorf("uploaded bytes = %q, want exact zip", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"digest":%q}`, digest)
		case r.Method == http.MethodPut && path == gomod.VersionPath("go.putnami.dev/mod", "v1.2.3-canary.1"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && path == "/go.putnami.dev/mod/@v/v1.2.3-canary.1.zip":
			_, _ = w.Write(zipBytes)
		case r.Method == http.MethodGet && path == "/go.putnami.dev/mod/@v/v1.2.3-canary.1.mod":
			_, _ = w.Write(goModBytes)
		default:
			t.Errorf("unexpected broker request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer broker.Close()
	t.Setenv(privateGoRegistryURLEnv, broker.URL+"/go")

	originalDownload := goModDownload
	var smokeProxies []string
	goModDownload = func(_ context.Context, proxy, _, _, _ string) (string, error) {
		smokeProxies = append(smokeProxies, proxy)
		return "", nil
	}
	defer func() { goModDownload = originalDownload }()

	declareGoOrigin(t, workspace, canonical.URL)
	ctx := &pctx.Context{
		WorkspaceRoot: workspace,
		Project:       pctx.Project{Name: "go.putnami.dev/mod", Path: "mod", FullPath: projectRoot},
		Params:        managedGoSparseParams(t),
	}
	var status string
	var data map[string]any
	var err error
	output := capturePublishOutput(t, func() {
		status, data, err = goModule(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("goModule() = (%q, %v), want OK", status, err)
	}
	if data["artifactDigest"] != digest || data["digestVerified"] != true {
		t.Errorf("result lacks the broker-served digest: %+v", data)
	}
	member := findPublishedGoMemberEvent(t, output)
	if member.Coordinate != "go.putnami.dev/mod" || member.Version != "v1.2.3-canary.1" || member.ArtifactDigest != digest {
		t.Errorf("published member = %+v, want the logical coordinate with the broker-served digest", member)
	}

	brokerHost := strings.TrimPrefix(broker.URL, "http://")
	if len(tokenHosts) != 1 || tokenHosts[0] != brokerHost {
		t.Fatalf("credential hosts = %#v, want the broker host %q only", tokenHosts, brokerHost)
	}
	var sawUpload, sawPublish, sawZip, sawMod bool
	for _, call := range calls {
		if call.Authorization != "Bearer "+capability {
			t.Errorf("broker request %s %s authorization = %q", call.Method, call.Path, call.Authorization)
		}
		sawUpload = sawUpload || call.Method == http.MethodPost && strings.HasSuffix(call.Path, "/-/blobs/upload")
		sawPublish = sawPublish || call.Method == http.MethodPut && strings.HasSuffix(call.Path, "/@v/v1.2.3-canary.1")
		sawZip = sawZip || call.Method == http.MethodGet && strings.HasSuffix(call.Path, ".zip")
		sawMod = sawMod || call.Method == http.MethodGet && strings.HasSuffix(call.Path, ".mod")
	}
	if !sawUpload || !sawPublish || !sawZip || !sawMod {
		t.Fatalf("broker calls = %+v, want upload, version PUT, zip read, and mod read", calls)
	}
	// Go refuses to send credentials to an http:// proxy ("refusing to pass
	// credentials to insecure URL"), so the toolchain smoke cannot run against
	// the loopback broker; the authenticated .zip/.mod reads above are the
	// consumer proof on this route.
	if len(smokeProxies) != 0 {
		t.Fatalf("smoke GOPROXY = %v, want no toolchain smoke under the private broker", smokeProxies)
	}
}

func TestResolveGoPublishRouteFailsClosedAndKeepsTheDirectPath(t *testing.T) {
	const origin = "https://go.example.test"
	t.Run("absent keeps the laptop path", func(t *testing.T) {
		t.Setenv(privateGoRegistryURLEnv, "")
		route, err := resolveGoPublishRoute(origin, false)
		if err != nil || route.broker || route.endpoint != origin || route.credentialHost != "go.example.test" {
			t.Fatalf("resolveGoPublishRoute() = (%+v, %v), want the direct path", route, err)
		}
	})
	t.Run("remote HTTPS value is cloud compatibility input", func(t *testing.T) {
		t.Setenv(privateGoRegistryURLEnv, "https://go.putnami.dev")
		route, err := resolveGoPublishRoute(origin, true)
		if err != nil || route.broker || route.endpoint != origin {
			t.Fatalf("resolveGoPublishRoute() = (%+v, %v), want the direct path", route, err)
		}
	})
	t.Run("loopback broker without a plan is refused", func(t *testing.T) {
		t.Setenv(privateGoRegistryURLEnv, "http://127.0.0.1:8080/go")
		if route, err := resolveGoPublishRoute(origin, false); err == nil {
			t.Fatalf("resolveGoPublishRoute() = %+v, want refusal", route)
		}
	})
	for _, raw := range []string{"http://localhost:8080/go", "http://127.0.0.1:8080/oci", "http://127.0.0.1/go", "http://192.0.2.10:8080/go"} {
		t.Run("malformed:"+raw, func(t *testing.T) {
			t.Setenv(privateGoRegistryURLEnv, raw)
			if route, err := resolveGoPublishRoute(origin, true); err == nil {
				t.Fatalf("resolveGoPublishRoute(%q) = %+v, want fail closed", raw, route)
			}
		})
	}
}

// TestMain strips the private publication broker variable the CI runner
// exports for the whole DAG, so the direct-registry tests above see the same
// environment on a laptop and under native publication. Tests that exercise
// the broker set the variable themselves with t.Setenv. It strips the
// publication outbox for the same reason: under it, a publication without an
// explicit token would pack instead of upload.
func TestMain(m *testing.M) {
	for _, key := range []string{"PUTNAMI_REGISTRY_GOMOD_URL", extproto.PublicationOutboxEnv} {
		if err := os.Unsetenv(key); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
