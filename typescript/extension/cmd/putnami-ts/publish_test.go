package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestBuildNPMPublishArgs(t *testing.T) {
	tests := []struct {
		name     string
		access   string
		registry string
		want     []string
	}{
		{"no flags", "", "", []string{"publish"}},
		{"with access", "public", "", []string{"publish", "--access", "public"}},
		{"with registry", "", "https://npm.pkg.github.com", []string{"publish", "--registry", "https://npm.pkg.github.com"}},
		{"all flags (otp via env, not argv)", "restricted", "https://registry.npmjs.org", []string{"publish", "--access", "restricted", "--registry", "https://registry.npmjs.org"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildNPMPublishArgs(tt.access, tt.registry)
			if len(got) != len(tt.want) {
				t.Fatalf("buildNPMPublishArgs() = %v (len %d), want %v (len %d)", got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("buildNPMPublishArgs()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestNpmAuthKey(t *testing.T) {
	tests := []struct {
		registry string
		want     string
	}{
		{"https://npm.putnami.dev", "npm_config_//npm.putnami.dev/:_authToken"},
		{"https://npm.pkg.github.com/", "npm_config_//npm.pkg.github.com/:_authToken"},
		{"http://localhost:4873", "npm_config_//localhost:4873/:_authToken"},
		{"npm.putnami.dev", "npm_config_//npm.putnami.dev/:_authToken"},
	}
	for _, tt := range tests {
		t.Run(tt.registry, func(t *testing.T) {
			if got := npmAuthKey(tt.registry); got != tt.want {
				t.Errorf("npmAuthKey(%q) = %q, want %q", tt.registry, got, tt.want)
			}
		})
	}
}

func TestHostFromURL(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"https://npm.putnami.dev", "npm.putnami.dev"},
		{"http://localhost:4873", "localhost:4873"},
		{"https://npm.pkg.github.com/some/path", "npm.pkg.github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := hostFromURL(tt.raw); got != tt.want {
				t.Errorf("hostFromURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRunPublishNpm_SkipsNoProject(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	ctx.Project.Name = ""
	status, _, err := runPublishNpm(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "SKIP" {
		t.Errorf("status = %q, want SKIP", status)
	}
}

func TestRunPublishNpm_DryRun(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{
		"npm":     json.RawMessage(`true`),
		"dry-run": json.RawMessage(`true`),
	}
	// Stage a pre-built npm package the publisher reads.
	npmDir := filepath.Join(dir, ".putnami", "out", "project", "package", "npm")
	if err := os.MkdirAll(npmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(npmDir, "package.json"), []byte(`{"name":"@test/pkg","version":"1.2.3-abc1234"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	status, data, err := runPublishNpm(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if data["dryRun"] != true {
		t.Errorf("expected dryRun=true in result, got %+v", data)
	}
	if data["version"] != "1.2.3-abc1234" {
		t.Errorf("version = %v, want 1.2.3-abc1234", data["version"])
	}
}

func TestRunPublishNpm_ManagedDryRunDoesNotClaimPublication(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{
		"dry-run":        json.RawMessage(`true`),
		"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg"),
	}
	stageNPMFixture(t, dir)

	var status string
	events := captureEvents(t, func() {
		status, _, _ = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	for _, event := range events {
		if event["kind"] == "published" {
			t.Fatalf("dry run emitted publication proof: %#v", event)
		}
	}
}

func TestRunPublishNpm_ManagedStaysPrivateAndVerifiesExactTarball(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{
		"channel":        json.RawMessage(`"canary"`),
		"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg"),
	}
	stageNPMFixture(t, dir)
	calls := mockManagedNPM(t, false, false)

	var status string
	var data map[string]any
	var runErr error
	events := captureEvents(t, func() {
		status, data, runErr = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if runErr != nil || status != "OK" {
		t.Fatalf("publish status=%q err=%v", status, runErr)
	}
	digest, _ := data["artifactDigest"].(string)
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 || data["digestVerified"] != true {
		t.Fatalf("unverified/malformed digest data: %#v", data)
	}
	if calls.total != 1 { // local pack only; every registry operation is bounded direct HTTP
		t.Fatalf("npm call count = %d, want 1", calls.total)
	}
	if calls.publish != 1 || len(calls.publishPayload.DistTags) != 0 {
		t.Fatalf("managed npm upload mutated a channel: publishes=%d payload=%#v", calls.publish, calls.publishPayload)
	}
	if calls.anonymousPack != 0 {
		t.Fatalf("anonymous registry downloads = %d, want 0", calls.anonymousPack)
	}
	if calls.remotePack != 0 {
		t.Fatalf("managed verification delegated %d remote downloads to npm/Node, want 0", calls.remotePack)
	}
	if calls.remoteVerify != 2 {
		t.Fatalf("managed direct verification calls = %d, want preflight + post-upload", calls.remoteVerify)
	}
	if calls.unisolated != 0 {
		t.Fatalf("managed npm subprocesses without isolated user/global config = %d, want 0", calls.unisolated)
	}
	published := findEvent(events, func(event map[string]any) bool { return event["kind"] == "published" })
	if published == nil || published["registry"] != "npm" || published["name"] != "@test/pkg" || published["version"] != "1.2.3-r42" || published["artifactDigest"] != digest || published["digestVerified"] != true || published["dryRun"] != false {
		t.Fatalf("published event is not exact verified npm proof: %#v", published)
	}
	if len(calls.tokenHosts) != 1 || calls.tokenHosts[0] != "registry.npmjs.org" {
		t.Fatalf("registry credential hosts = %#v, want [registry.npmjs.org]", calls.tokenHosts)
	}
}

func TestRunPublishNpm_ManagedUsesTheDeclaredPublishRegistryForAuthenticatedVerification(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	// A repository .npmrc names another registry. It is not consulted: the
	// workspace declaration is the only source of the publish endpoint.
	if err := os.WriteFile(filepath.Join(dir, ".npmrc"), []byte("registry=https://fallback.example.test\n@test:registry=https://npmrc.example.test/ignored/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withRegistries(t, ctx, `{"npm":{"publish":"https://npm.example.test/scoped/"}}`)
	calls := mockManagedNPM(t, false, false)

	status, _, err := runPublishNpm(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("scoped registry publish status=%q err=%v", status, err)
	}
	want := "https://npm.example.test/scoped"
	if calls.publishRegistry != want || calls.remoteRegistry != want {
		t.Fatalf("registry drift: publish=%q verify=%q, want %q", calls.publishRegistry, calls.remoteRegistry, want)
	}
	if len(calls.tokenHosts) != 1 || calls.tokenHosts[0] != "npm.example.test" {
		t.Fatalf("registry token hosts = %v, want [npm.example.test]", calls.tokenHosts)
	}
}

func TestRunPublishNpm_ManagedRejectsUnsafeRegistryBeforeMutation(t *testing.T) {
	tests := []struct {
		name       string
		registry   string
		registries string
		wantError  string
	}{
		{name: "invalid explicit URL", registry: "not a registry URL", wantError: "managed npm registry"},
		{name: "credential-bearing explicit URL", registry: "https://publisher:secret@npm.example.test", wantError: "managed npm registry"},
		{name: "credential-bearing declared registry", registries: `{"npm":{"publish":"https://publisher:secret@npm.example.test"}}`, wantError: "registries.npm.publish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := makeTestCtx(t)
			ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
			if tt.registry != "" {
				ctx.Params["registry"] = json.RawMessage(fmt.Sprintf("%q", tt.registry))
			}
			if tt.registries != "" {
				withRegistries(t, ctx, tt.registries)
			}
			calls := mockManagedNPM(t, false, false)

			var status string
			var err error
			events := captureEvents(t, func() {
				status, _, err = runPublishNpm(ctx, jsonl.New(), nil)
			})
			if status != "FAILED" || err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("unsafe registry status=%q err=%v", status, err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(fmt.Sprint(events), "secret") {
				t.Fatalf("unsafe registry leaked URL userinfo: err=%v events=%#v", err, events)
			}
			if calls.total != 0 || len(calls.tokenHosts) != 0 {
				t.Fatalf("unsafe registry reached mutation/auth: npm=%d tokenHosts=%v", calls.total, calls.tokenHosts)
			}
		})
	}
}

func TestRunPublishNpm_ManagedRejectsTarballPublishConfigBeforeLease(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{
		"registry":       json.RawMessage(`"https://npm.example.test"`),
		"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg"),
	}
	stageNPMFixture(t, dir)
	npmDir := filepath.Join(dir, ".putnami", "out", "project", "package", "npm")
	pkg := map[string]any{
		"name": "@test/pkg", "version": "1.2.3-r42",
		"publishConfig": map[string]any{
			"registry": "https://registry.npmjs.org", "access": "public", "tag": "latest",
		},
	}
	pkgData, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(npmDir, "package.json"), pkgData, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := mockManagedNPM(t, false, false)

	status, _, runErr := runPublishNpm(ctx, jsonl.New(), nil)
	if status != "FAILED" || runErr == nil || !strings.Contains(runErr.Error(), "publishConfig") {
		t.Fatalf("unsafe publishConfig status=%q err=%v", status, runErr)
	}
	if calls.total != 0 || len(calls.tokenHosts) != 0 {
		t.Fatalf("unsafe publishConfig reached npm/the credential seam: npm=%d hosts=%#v", calls.total, calls.tokenHosts)
	}
}

func TestRunPublishNpm_ManagedRequiresACredentialForTheHost(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{
		"registry":       json.RawMessage(`"https://registry.npmjs.org"`),
		"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg"),
	}
	stageNPMFixture(t, dir)
	calls := mockManagedNPM(t, false, false)
	npmResolveRegistryToken = func(host string) (string, string) {
		calls.tokenHosts = append(calls.tokenHosts, host)
		return "", "no npm credential for this registry"
	}

	var status string
	var runErr error
	events := captureEvents(t, func() {
		status, _, runErr = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if status != "FAILED" || runErr == nil || !strings.Contains(runErr.Error(), "no npm credential for this registry") {
		t.Fatalf("missing credential status=%q err=%v", status, runErr)
	}
	if calls.total != 0 {
		t.Fatalf("managed publish reached npm without a lease: %d calls", calls.total)
	}
	if len(calls.tokenHosts) != 1 || calls.tokenHosts[0] != "registry.npmjs.org" {
		t.Fatalf("third-party credential hosts = %#v, want [registry.npmjs.org]", calls.tokenHosts)
	}
	if findEvent(events, func(event map[string]any) bool { return event["kind"] == "published" }) != nil {
		t.Fatalf("missing lease emitted publication evidence: %#v", events)
	}
}

func TestManagedNPMConfigEnvNamesRemovesAmbientAuthAndConfig(t *testing.T) {
	got := managedNPMConfigEnvNames([]string{
		"PATH=/bin", "npm_config_registry=https://registry.npmjs.org",
		"NPM_CONFIG_USERCONFIG=/tmp/repo.npmrc", "NPM_TOKEN=secret",
		"NODE_AUTH_TOKEN=secret", "PUTNAMI_CLOUD_TOKEN=cloud-secret",
		"NODE_OPTIONS=--require=/tmp/preload.js", "NODE_PATH=/tmp/modules",
		"HTTP_PROXY=http://proxy.invalid", "https_proxy=http://proxy.invalid",
		"ALL_PROXY=socks5://proxy.invalid", "NO_PROXY=localhost",
		"PUTNAMI_KEEP=value",
	})
	want := map[string]bool{
		"npm_config_registry": true, "NPM_CONFIG_USERCONFIG": true,
		"NPM_TOKEN": true, "NODE_AUTH_TOKEN": true,
		"PUTNAMI_CLOUD_TOKEN": true, "NODE_OPTIONS": true, "NODE_PATH": true,
		"HTTP_PROXY": true, "https_proxy": true, "ALL_PROXY": true, "NO_PROXY": true,
	}
	if len(got) != len(want) {
		t.Fatalf("ambient npm vars removed = %v, want %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("unexpected environment removal %q in %v", name, got)
		}
	}
}

func TestManagedNPMConfigDoesNotGiveBearerToLocalPack(t *testing.T) {
	config, cleanup, err := newManagedNPMConfig(map[string]string{
		"npm_config_//npm.example.test/:_authToken": "target-token",
	})
	if err != nil {
		t.Fatalf("new managed npm config: %v", err)
	}
	defer cleanup()
	if len(config.env) != 0 {
		t.Fatalf("local npm pack inherited managed auth env: %#v", config.env)
	}
}

func TestSendManagedNPMPublishUsesAuthenticatedTaglessPut(t *testing.T) {
	var method, escapedPath, authorization, contentType string
	var received managedNPMPublishPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		escapedPath = r.URL.EscapedPath()
		authorization = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode managed npm request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	payload := managedNPMPublishPayload{
		Name:        "@test/pkg",
		Versions:    map[string]json.RawMessage{"1.2.3-r42": json.RawMessage(`{"name":"@test/pkg","version":"1.2.3-r42"}`)},
		Attachments: map[string]managedNPMAttachment{"pkg-1.2.3-r42.tgz": {ContentType: "application/octet-stream", Data: "dGd6", Length: 3}},
		DistTags:    map[string]string{},
	}
	if err := sendManagedNPMPublish(server.URL, "target-token", payload); err != nil {
		t.Fatalf("send managed npm publish: %v", err)
	}
	if method != http.MethodPut || escapedPath != "/@test%2Fpkg" {
		t.Fatalf("managed npm endpoint = %s %s, want PUT /@test%%2Fpkg", method, escapedPath)
	}
	if authorization != "Bearer target-token" || contentType != "application/json" {
		t.Fatalf("managed npm headers authorization=%q content-type=%q", authorization, contentType)
	}
	if received.Name != payload.Name || len(received.DistTags) != 0 || len(received.Versions) != 1 || len(received.Attachments) != 1 {
		t.Fatalf("managed npm wire payload = %#v, want exact tagless payload", received)
	}
}

func TestSendManagedNPMPublishIgnoresAmbientProxyAndRedactsEchoedToken(t *testing.T) {
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
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)

	payload := managedNPMPublishPayload{
		Name:        "@test/pkg",
		Versions:    map[string]json.RawMessage{"1.2.3-r42": json.RawMessage(`{"name":"@test/pkg","version":"1.2.3-r42"}`)},
		Attachments: map[string]managedNPMAttachment{"pkg-1.2.3-r42.tgz": {ContentType: "application/octet-stream", Data: "dGd6", Length: 3}},
		DistTags:    map[string]string{},
	}
	err := sendManagedNPMPublish("http://registry.example.invalid", "target-token", payload)
	if err == nil {
		t.Fatal("managed npm publish error = nil, want unreachable target")
	}
	if proxyRequests != 0 || proxyAuthorization != "" {
		t.Fatalf("ambient proxy observed managed request/token: requests=%d authorization=%q", proxyRequests, proxyAuthorization)
	}
	if strings.Contains(err.Error(), "target-token") {
		t.Fatalf("managed npm error leaked token: %v", err)
	}
}

func TestSendManagedNPMPublishRedactsTokenFromRegistryError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "reflected target-token", http.StatusForbidden)
	}))
	defer server.Close()
	payload := managedNPMPublishPayload{
		Name:        "@test/pkg",
		Versions:    map[string]json.RawMessage{"1.2.3-r42": json.RawMessage(`{"name":"@test/pkg","version":"1.2.3-r42"}`)},
		Attachments: map[string]managedNPMAttachment{"pkg-1.2.3-r42.tgz": {ContentType: "application/octet-stream", Data: "dGd6", Length: 3}},
		DistTags:    map[string]string{},
	}
	err := sendManagedNPMPublish(server.URL, "target-token", payload)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("managed npm registry error = %v, want 403", err)
	}
	if strings.Contains(err.Error(), "target-token") {
		t.Fatalf("managed npm registry error leaked reflected token: %v", err)
	}
}

// The registry's error body names the cause (a stale ownership copy looks the
// same as a refusal by status alone), so the error carries a bounded,
// single-line excerpt of it with every bearer credential redacted.
func TestManagedNPMRegistryErrorsShowBoundedRedactedBody(t *testing.T) {
	cause := `{"code":"distribution.local_access.stale","error":"package ownership data on this registry is stale; retry shortly"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(cause + "\x1b[31m\r\n echoed " + r.Header.Get("Authorization") +
			" raw target-token and Bearer other-secret " + strings.Repeat("x", 1000)))
	}))
	defer server.Close()
	payload := managedNPMPublishPayload{
		Name:        "@test/pkg",
		Versions:    map[string]json.RawMessage{"1.2.3-r42": json.RawMessage(`{"name":"@test/pkg","version":"1.2.3-r42"}`)},
		Attachments: map[string]managedNPMAttachment{"pkg-1.2.3-r42.tgz": {ContentType: "application/octet-stream", Data: "dGd6", Length: 3}},
		DistTags:    map[string]string{},
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("x")))
	_, probeErr := probeManagedNPMArtifact(server.URL, "target-token", "@test/pkg", "1.2.3-r42", digest, 1)
	for name, err := range map[string]error{
		"publish": sendManagedNPMPublish(server.URL, "target-token", payload),
		"probe":   probeErr,
	} {
		if err == nil {
			t.Fatalf("%s error = nil, want the registry 503", name)
		}
		message := err.Error()
		if !strings.Contains(message, "503 Service Unavailable: "+cause) {
			t.Fatalf("%s error = %q, want the status and the registry's cause", name, message)
		}
		if strings.Contains(message, "target-token") || strings.Contains(message, "other-secret") {
			t.Fatalf("%s error leaked a bearer credential: %q", name, message)
		}
		if !strings.Contains(message, "Bearer [redacted]") || strings.ContainsAny(message, "\x1b\r\n") {
			t.Fatalf("%s error = %q, want redacted bearers and no control characters", name, message)
		}
		_, excerpt, _ := strings.Cut(message, "Service Unavailable: ")
		if got := len([]rune(excerpt)); got > managedNPMRegistryExcerptLimit+len("...") {
			t.Fatalf("%s excerpt has %d characters, want at most %d", name, got, managedNPMRegistryExcerptLimit)
		}
	}
}

func TestProbeManagedNPMArtifactUsesAuthenticatedBoundedTarballRead(t *testing.T) {
	artifact := []byte("immutable npm tarball bytes")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(artifact))
	var gotPath, gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write(artifact)
	}))
	defer server.Close()

	found, err := probeManagedNPMArtifact(server.URL+"/npm", "target-token", "@test/pkg", "1.2.3-r42", digest, int64(len(artifact)))
	if err != nil || !found {
		t.Fatalf("probe managed npm artifact found=%t err=%v", found, err)
	}
	if gotPath != "/npm/@test/pkg/-/pkg-1.2.3-r42.tgz" || gotAuthorization != "Bearer target-token" {
		t.Fatalf("managed npm tarball request path=%q authorization=%q", gotPath, gotAuthorization)
	}

	found, err = probeManagedNPMArtifact(server.URL+"/npm", "target-token", "@test/pkg", "1.2.3-r42", digest, int64(len(artifact)-1))
	if found || err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("overlong managed npm artifact found=%t err=%v", found, err)
	}
}

func TestProbeManagedNPMArtifactRejectsRedirectAndErrorBody(t *testing.T) {
	var redirected int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected++
		_, _ = w.Write([]byte("target-token"))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("x")))
	found, err := probeManagedNPMArtifact(redirect.URL, "target-token", "@test/pkg", "1.2.3-r42", digest, 1)
	if found || err == nil || !strings.Contains(err.Error(), "302") || strings.Contains(err.Error(), "target-token") {
		t.Fatalf("redirect probe found=%t err=%v", found, err)
	}
	if redirected != 0 {
		t.Fatalf("managed npm verification followed %d redirects", redirected)
	}

	errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "reflected target-token", http.StatusForbidden)
	}))
	defer errorServer.Close()
	found, err = probeManagedNPMArtifact(errorServer.URL, "target-token", "@test/pkg", "1.2.3-r42", digest, 1)
	if found || err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "target-token") {
		t.Fatalf("error-body probe found=%t err=%v", found, err)
	}
}

func TestManagedNPMHTTPClientDisablesProxyAndRedirects(t *testing.T) {
	client, err := newManagedNPMHTTPClient()
	if err != nil {
		t.Fatalf("new managed npm HTTP client: %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("managed npm transport = %#v, want direct transport with Proxy=nil", client.Transport)
	}
	request, err := http.NewRequest(http.MethodGet, "https://other.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.CheckRedirect(request, nil); !errors.Is(got, http.ErrUseLastResponse) {
		t.Fatalf("managed npm redirect policy = %v, want http.ErrUseLastResponse", got)
	}
}

func TestBuildManagedNPMPublishPayloadStripsPublishConfigFromWire(t *testing.T) {
	dir := t.TempDir()
	manifest := []byte(`{"name":"@test/pkg","version":"1.2.3-r42","publishConfig":{"provenance":true}}`)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(dir, "pkg.tgz")
	if err := os.WriteFile(artifact, []byte("tgz"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload, err := buildManagedNPMPublishPayload(dir, artifact, "@test/pkg", "1.2.3-r42")
	if err != nil {
		t.Fatalf("build managed npm payload: %v", err)
	}
	var wireManifest map[string]json.RawMessage
	if err := json.Unmarshal(payload.Versions["1.2.3-r42"], &wireManifest); err != nil {
		t.Fatalf("decode wire manifest: %v", err)
	}
	if _, present := wireManifest["publishConfig"]; present {
		t.Fatalf("managed npm wire manifest retained repository publishConfig: %s", payload.Versions["1.2.3-r42"])
	}
}

func TestRunPublishNpm_ManagedRepositoryAccessCannotControlVisibility(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{
		"access":         json.RawMessage(`"public"`),
		"registry":       json.RawMessage(`"https://npm.example.test"`),
		"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg"),
	}
	stageNPMFixture(t, dir)
	calls := mockManagedNPM(t, false, false)

	var status string
	var runErr error
	events := captureEvents(t, func() {
		status, _, runErr = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if status != "OK" || runErr != nil {
		t.Fatalf("managed publish status=%q err=%v", status, runErr)
	}
	if calls.publish != 1 || len(calls.publishPayload.DistTags) != 0 {
		t.Fatalf("repository access parameter controlled managed visibility/channel: %#v", calls.publishPayload)
	}
	if calls.remotePack != 0 || calls.remoteVerify != 2 || calls.anonymousPack != 0 {
		t.Fatalf("registry verification npm/direct/anonymous=%d/%d/%d, want 0/2/0", calls.remotePack, calls.remoteVerify, calls.anonymousPack)
	}
	if findEvent(events, func(event map[string]any) bool { return event["kind"] == "published" }) == nil {
		t.Fatalf("private verified publish omitted evidence: %#v", events)
	}
}

func TestRunPublishNpm_ManagedAlreadyPublishedRetryVerifiesWithoutUpload(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	calls := mockManagedNPM(t, true, false)

	var status string
	var data map[string]any
	var err error
	events := captureEvents(t, func() {
		status, data, err = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("retry status=%q err=%v", status, err)
	}
	if calls.publish != 0 {
		t.Fatalf("already-published retry uploaded %d artifacts", calls.publish)
	}
	if calls.remotePack != 0 || calls.remoteVerify != 1 || calls.anonymousPack != 0 {
		t.Fatalf("already-published retry verification npm/direct/anonymous=%d/%d/%d, want 0/1/0", calls.remotePack, calls.remoteVerify, calls.anonymousPack)
	}
	if data["alreadyPublished"] != true || data["digestVerified"] != true {
		t.Fatalf("retry did not report verified reuse: %#v", data)
	}
	if findEvent(events, func(event map[string]any) bool {
		return event["kind"] == "published" && event["digestVerified"] == true
	}) == nil {
		t.Fatalf("verified private retry did not emit publication evidence: %#v", events)
	}
}

// A publication reports one release-set member per artifact, on the fresh and
// on the reused path alike: the set is assembled from what EXISTS in the
// registry under a verified digest, not from what this session uploaded.
func TestRunPublishNpm_ManagedEmitsAPublishedMemberOnBothPaths(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "member-event-per-publication", "reuse-reports-the-existing-digest")

	for _, alreadyPublished := range []bool{false, true} {
		ctx, dir := makeTestCtx(t)
		ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
		stageNPMFixture(t, dir)
		mockManagedNPM(t, alreadyPublished, false)

		var status string
		var data map[string]any
		events := captureEvents(t, func() {
			status, data, _ = runPublishNpm(ctx, jsonl.New(), nil)
		})
		if status != "OK" {
			t.Fatalf("alreadyPublished=%v status=%q", alreadyPublished, status)
		}
		member := findEvent(events, func(event map[string]any) bool {
			return event["kind"] == extproto.PublishedMemberEventKind
		})
		if member == nil {
			t.Fatalf("alreadyPublished=%v emitted no published member: %#v", alreadyPublished, events)
		}
		if member["ecosystem"] != "npm" || member["coordinate"] != "@test/pkg" || member["version"] != "1.2.3-r42" {
			t.Fatalf("member identity = %#v", member)
		}
		digest, _ := member["artifactDigest"].(string)
		if digest == "" || digest != data["artifactDigest"] {
			t.Fatalf("member digest %q does not match the verified digest %v", digest, data["artifactDigest"])
		}
		parsed, diagnostics := extproto.ParsePublishedMember(publishedMemberJSON(t, member))
		if len(diagnostics) > 0 {
			t.Fatalf("member does not parse strictly: %v", diagnostics)
		}
		if defects := extproto.ValidatePublishedMember(parsed); len(defects) > 0 {
			t.Fatalf("member is invalid: %v", defects)
		}
	}
}

// publishedMemberJSON extracts the member fields out of a captured artifact
// event so the protocol's own strict parser can check them.
func publishedMemberJSON(t *testing.T, event map[string]any) []byte {
	t.Helper()
	member := map[string]any{}
	for _, key := range []string{"ecosystem", "coordinate", "version", "artifactDigest"} {
		member[key] = event[key]
	}
	encoded, err := json.Marshal(member)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// A dist-tag IS a channel. Neither path writes one: the managed payload carries
// an empty dist-tags map, and the unmanaged path passes no --tag, so npm is
// never told which channel this version answers to.
func TestRunPublishNpm_WritesNoDistTagOnEitherPath(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "member-event-per-publication", "no-dist-tag-is-written")

	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	calls := mockManagedNPM(t, false, false)

	if status, _, err := runPublishNpm(ctx, jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("managed publish status=%q err=%v", status, err)
	}
	if len(calls.publishPayload.DistTags) != 0 {
		t.Fatalf("managed payload carried dist-tags: %#v", calls.publishPayload.DistTags)
	}

	for _, arg := range buildNPMPublishArgs("public", "https://npm.example.test") {
		if arg == "--tag" {
			t.Fatal("unmanaged publish argv named a dist-tag")
		}
	}
}

func TestRunPublishNpm_ManagedRejectsAuthenticated404WithoutEvidence(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	calls := mockManagedNPM(t, false, false)
	calls.remoteFailure = "404 Not Found"

	var status string
	var runErr error
	events := captureEvents(t, func() {
		status, _, runErr = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if status != "FAILED" || runErr == nil || !strings.Contains(runErr.Error(), "404 Not Found") {
		t.Fatalf("authenticated 404 status=%q err=%v", status, runErr)
	}
	if calls.remotePack != 0 || calls.remoteVerify != 2 || calls.anonymousPack != 0 {
		t.Fatalf("registry verification npm/direct/anonymous=%d/%d/%d, want 0/2/0", calls.remotePack, calls.remoteVerify, calls.anonymousPack)
	}
	if findEvent(events, func(event map[string]any) bool { return event["kind"] == "published" }) != nil {
		t.Fatalf("authenticated 404 emitted publication evidence: %#v", events)
	}
}

func TestRunPublishNpm_ManagedRejectsRegistryDigestMismatch(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	mockManagedNPM(t, true, true)
	var status string
	var err error
	events := captureEvents(t, func() {
		status, _, err = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("digest mismatch status=%q err=%v", status, err)
	}
	if findEvent(events, func(event map[string]any) bool { return event["kind"] == "published" }) != nil {
		t.Fatalf("digest mismatch emitted publication evidence: %#v", events)
	}
}

func TestNPMRegistryKeyOverridesProjectScopedRegistry(t *testing.T) {
	tests := map[string]string{
		"@putnami/cloud": "npm_config_@putnami:registry",
		"@other/pkg":     "npm_config_@other:registry",
		"unscoped":       "npm_config_registry",
		"@invalid":       "npm_config_registry",
	}
	for packageName, want := range tests {
		if got := npmRegistryKey(packageName); got != want {
			t.Errorf("npmRegistryKey(%q) = %q, want %q", packageName, got, want)
		}
	}
}

func TestRunPublishNpm_RejectsMissingOrMalformedReleaseSetMember(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/other")}
	if status, _, err := runPublishNpm(ctx, jsonl.New(), nil); status != "FAILED" || err == nil || !strings.Contains(err.Error(), "no member") {
		t.Fatalf("missing member status=%q err=%v", status, err)
	}

	ctx.Params = pctx.Params{"releaseSetPlan": json.RawMessage(`{"protocolVersion":1,"namespace":"test","channel":"canary","baseRef":{"id":"rs_bad","digest":"sha256:bad"},"base":{"protocolVersion":1,"namespace":"test","members":[]},"members":[]}`)}
	if status, _, err := runPublishNpm(ctx, jsonl.New(), nil); status != "FAILED" || err == nil {
		t.Fatalf("malformed plan status=%q err=%v", status, err)
	}
}

func TestManagedDownstreamOnlyAllowsTransitiveUpstreamPackageButRejectsPublish(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	ctx.Project.Name = "@test/upstream"
	ctx.Project.Path = "upstream"
	ctx.Version = &pctx.Version{SHA: "r42", Branch: "main"}
	ctx.Params = pctx.Params{"releaseSetPlan": downstreamOnlyNPMPlan(t)}
	packagePlan, err := npmPackageReleaseSetPlan(ctx)
	if err != nil || packagePlan == nil || packagePlan.Coordinate != "@test/upstream" || packagePlan.Version != "1.0.0-r41" {
		t.Fatalf("unchanged upstream package plan=%+v err=%v", packagePlan, err)
	}
	if status, _, err := runPublishNpm(ctx, jsonl.New(), nil); status != "FAILED" || err == nil || !strings.Contains(err.Error(), "not selected") {
		t.Fatalf("unchanged upstream publish status=%q err=%v", status, err)
	}
}

func TestRunPublishNpm_FailsWhenPackageMissing(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	ctx.Params = pctx.Params{"npm": json.RawMessage(`true`)}
	// No npm package staged → publish must fail loudly, not silently no-op.
	status, _, err := runPublishNpm(ctx, jsonl.New(), nil)
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if err == nil {
		t.Error("expected a non-nil error when the npm package is missing")
	}
}

type npmPlanMemberFixture struct {
	Ecosystem          string                     `json:"ecosystem"`
	Coordinate         string                     `json:"coordinate"`
	Version            string                     `json:"version"`
	ArtifactDigest     string                     `json:"artifactDigest,omitempty"`
	Dependencies       []npmPlanDependencyFixture `json:"dependencies"`
	SourceRevision     string                     `json:"sourceRevision,omitempty"`
	ContentFingerprint string                     `json:"selectionFingerprint,omitempty"`
	Selected           bool                       `json:"selected,omitempty"`
	ProjectID          string                     `json:"projectId,omitempty"`
}

// npmPlanFixture is the release-set plan shape the orchestrator binds into a
// package or publish job: the channels this publication advances and the head
// resolved for each of them, the first being the baseline.
type npmPlanFixture struct {
	ProtocolVersion int                    `json:"protocolVersion"`
	Namespace       string                 `json:"namespace"`
	Channels        []string               `json:"channels"`
	Heads           map[string]any         `json:"heads"`
	Members         []npmPlanMemberFixture `json:"members"`
}

// npmPlanHead wraps a base snapshot as the canary channel's resolved head.
func npmPlanHead(hexDigest string, base npmReleaseSetFixture) map[string]any {
	return map[string]any{
		"canary": map[string]any{
			"ref":        map[string]string{"id": "rs_" + hexDigest, "digest": "sha256:" + hexDigest},
			"generation": 3,
			"releaseSet": base,
		},
	}
}

const npmPlanTestRevision = "8d5edb7513d93b9165ba2a7cb48466d022fc3f63"

func npmPlanFingerprint(char string) string { return "sha256:" + strings.Repeat(char, 64) }

type npmPlanDependencyFixture struct {
	Ecosystem  string `json:"ecosystem"`
	Coordinate string `json:"coordinate"`
	Version    string `json:"version"`
}

type npmReleaseSetFixture struct {
	ProtocolVersion int                    `json:"protocolVersion"`
	Namespace       string                 `json:"namespace"`
	Members         []npmPlanMemberFixture `json:"members"`
}

func oneMemberNPMPlan(t *testing.T, coordinate string) json.RawMessage {
	t.Helper()
	baseMember := npmPlanMemberFixture{
		Ecosystem: "npm", Coordinate: coordinate, Version: "1.2.3-r41",
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64), Dependencies: []npmPlanDependencyFixture{},
		SourceRevision: npmPlanTestRevision, ContentFingerprint: npmPlanFingerprint("1"),
	}
	base := npmReleaseSetFixture{ProtocolVersion: 2, Namespace: "test", Members: []npmPlanMemberFixture{baseMember}}
	canonical, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(canonical)
	hexDigest := fmt.Sprintf("%x", hash[:])
	plan := npmPlanFixture{
		ProtocolVersion: 2, Namespace: "test",
		Channels: []string{"canary"},
		Heads:    npmPlanHead(hexDigest, base),
		Members: []npmPlanMemberFixture{{
			Ecosystem: "npm", Coordinate: coordinate, Version: "1.2.3-r42",
			Dependencies: []npmPlanDependencyFixture{}, Selected: true, ProjectID: "/project",
			SourceRevision: npmPlanTestRevision, ContentFingerprint: npmPlanFingerprint("9"),
		}},
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func downstreamOnlyNPMPlan(t *testing.T) json.RawMessage {
	t.Helper()
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	dependency := npmPlanDependencyFixture{Ecosystem: "npm", Coordinate: "@test/upstream", Version: "1.0.0-r41"}
	base := npmReleaseSetFixture{ProtocolVersion: 2, Namespace: "test", Members: []npmPlanMemberFixture{
		{Ecosystem: "npm", Coordinate: "@test/downstream", Version: "1.0.0-r41", ArtifactDigest: digestA, Dependencies: []npmPlanDependencyFixture{dependency}, SourceRevision: npmPlanTestRevision, ContentFingerprint: npmPlanFingerprint("1")},
		{Ecosystem: "npm", Coordinate: "@test/upstream", Version: "1.0.0-r41", ArtifactDigest: digestB, Dependencies: []npmPlanDependencyFixture{}, SourceRevision: npmPlanTestRevision, ContentFingerprint: npmPlanFingerprint("2")},
	}}
	canonical, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(canonical)
	hexDigest := fmt.Sprintf("%x", hash[:])
	plan := npmPlanFixture{
		ProtocolVersion: 2, Namespace: "test",
		Channels: []string{"canary"},
		Heads:    npmPlanHead(hexDigest, base),
		Members: []npmPlanMemberFixture{
			{Ecosystem: "npm", Coordinate: "@test/downstream", Version: "1.0.0-r42", Dependencies: []npmPlanDependencyFixture{dependency}, Selected: true, ProjectID: "/downstream", SourceRevision: npmPlanTestRevision, ContentFingerprint: npmPlanFingerprint("9")},
			{Ecosystem: "npm", Coordinate: "@test/upstream", Version: "1.0.0-r41", ArtifactDigest: digestB, Dependencies: []npmPlanDependencyFixture{}, ProjectID: "/upstream", SourceRevision: npmPlanTestRevision, ContentFingerprint: npmPlanFingerprint("2")},
		},
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func stageNPMFixture(t *testing.T, workspaceRoot string) {
	t.Helper()
	npmDir := filepath.Join(workspaceRoot, ".putnami", "out", "project", "package", "npm")
	if err := os.MkdirAll(npmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"name": "@test/pkg", "version": "1.2.3-r42"})
	if err := os.WriteFile(filepath.Join(npmDir, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

type npmMockCalls struct {
	total           int
	publish         int
	remotePack      int
	remoteVerify    int
	anonymousPack   int
	unisolated      int
	remoteFailure   string
	publishPayload  managedNPMPublishPayload
	publishRegistry string
	remoteRegistry  string
	tokenHosts      []string
}

func mockManagedNPM(t *testing.T, alreadyPublished, mismatch bool) *npmMockCalls {
	t.Helper()
	calls := &npmMockCalls{}
	original := npmExecRun
	originalTokenResolver := npmResolveRegistryToken
	originalManagedPublish := npmSendManagedPublish
	originalManagedProbe := npmProbeManagedArtifact
	originalBun := resolveBunBin
	originalGOOS := npmGOOS
	t.Cleanup(func() {
		npmExecRun = original
		npmResolveRegistryToken = originalTokenResolver
		npmSendManagedPublish = originalManagedPublish
		npmProbeManagedArtifact = originalManagedProbe
		resolveBunBin = originalBun
		npmGOOS = originalGOOS
	})
	// The pack fake writes opaque bytes, so the managed flow runs as on a Unix
	// host on every host: TestPackNPMArtifact_WindowsHostPacksTheUnixModes
	// covers the Windows archive.
	npmGOOS = "linux"
	resolveBunBin = func() (string, error) { return managedTestBun, nil }
	npmResolveRegistryToken = func(host string) (string, string) {
		calls.tokenHosts = append(calls.tokenHosts, host)
		return "test-registry-token", ""
	}
	npmSendManagedPublish = func(registry, token string, payload managedNPMPublishPayload) error {
		calls.publish++
		calls.publishRegistry = registry
		calls.publishPayload = payload
		if token != "test-registry-token" {
			t.Fatalf("managed publish token = %q, want the host credential", token)
		}
		if len(payload.DistTags) != 0 {
			t.Fatalf("managed publish supplied dist-tags: %#v", payload.DistTags)
		}
		if len(payload.Versions) != 1 || len(payload.Attachments) != 1 {
			t.Fatalf("managed publish payload is not exact: %#v", payload)
		}
		return nil
	}
	npmProbeManagedArtifact = func(registry, token, packageName, version, _ string, _ int64) (bool, error) {
		calls.remoteVerify++
		calls.remoteRegistry = registry
		if token == "" {
			calls.anonymousPack++
		}
		if token != "test-registry-token" || packageName != "@test/pkg" || version != "1.2.3-r42" {
			t.Fatalf("managed verification target token=%q package=%q version=%q", token, packageName, version)
		}
		if !alreadyPublished && calls.remoteVerify == 1 {
			return false, nil
		}
		if calls.remoteFailure != "" {
			return false, fmt.Errorf("%s", calls.remoteFailure)
		}
		if mismatch {
			return false, fmt.Errorf("published npm artifact digest mismatch")
		}
		return true, nil
	}
	npmExecRun = func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		calls.total++
		if len(args) == 0 {
			return &exec.Result{Success: false, ExitCode: 1, Stderr: "unexpected command"}, nil
		}
		// The staged package is packed with bun, never with npm: the CI runner
		// image ships node and bun only. Any npm invocation would have to pack
		// remotely or read registry authority, which the managed path forbids.
		// The bun is the one resolveBunBin finds, as for every other bun run.
		if name == "bun" {
			t.Fatalf("bun pm pack ran the bun on PATH instead of the resolved %s", managedTestBun)
		}
		if name == managedTestBun && len(args) >= 2 && args[0] == "pm" && args[1] == "pack" {
			for _, arg := range args {
				if strings.Contains(arg, "@1.2.3-r42") {
					calls.remotePack++
					t.Fatalf("managed verification packed remotely: %v", args)
				}
			}
			if registry := flagValue(args, "--registry"); registry != "" {
				t.Fatalf("managed local pack received registry authority %q: %v", registry, args)
			}
			if !slices.Contains(args, "--ignore-scripts") {
				t.Fatalf("managed local pack runs lifecycle scripts: %v", args)
			}
			destination := flagValue(args, "--destination")
			payload := []byte("immutable npm tarball bytes")
			if err := os.WriteFile(filepath.Join(destination, "artifact.tgz"), payload, 0o644); err != nil {
				t.Fatalf("stage mock tarball: %v", err)
			}
			return &exec.Result{Success: true}, nil
		}
		if name != "npm" {
			return &exec.Result{Success: false, ExitCode: 1, Stderr: "unexpected command"}, nil
		}
		userConfig := flagValue(args, "--userconfig")
		globalConfig := flagValue(args, "--globalconfig")
		if userConfig == "" || globalConfig == "" || userConfig == globalConfig {
			calls.unisolated++
		}
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "unexpected npm subcommand"}, nil
	}
	return calls
}

// managedTestBun is the bun resolveBunBin returns under mockManagedNPM.
var managedTestBun = filepath.Join("resolved", "bun", "bin", "bun.exe")

// TestPackNPMArtifact_FailsWhenNoBunResolves pins that the pack resolves bun
// like every other bun run, and names why none was found.
func TestPackNPMArtifact_FailsWhenNoBunResolves(t *testing.T) {
	originalBun, originalRun := resolveBunBin, npmExecRun
	t.Cleanup(func() { resolveBunBin, npmExecRun = originalBun, originalRun })
	resolveBunBin = func() (string, error) { return "", errors.New("bun not found in PATH") }
	npmExecRun = func(string, []string, ...exec.Option) (*exec.Result, error) {
		t.Fatal("bun pm pack ran without a resolved bun")
		return nil, nil
	}

	_, _, _, err := packNPMArtifact(&managedNPMConfig{}, t.TempDir(), "packages/probe", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "bun not found in PATH") {
		t.Fatalf("packNPMArtifact err = %v, want the resolver's error", err)
	}
}

func flagValue(args []string, flag string) string {
	for i := range args {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// A native publication run exports a numeric-loopback broker for the npm
// registry and holds the capability itself. A managed publication must upload
// to and verify through that broker, and ask the credential seam about the
// BROKER host so the cloud answers with the run's capability instead of a user
// session the runner does not have. The declared registry stays the logical
// coordinate and the unmanaged path never sees the variable.
func TestRunPublishNpm_ManagedRoutesThroughThePrivateBroker(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "member-event-per-publication", "private-loopback-broker-carries-the-publication")
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	withRegistries(t, ctx, `{"npm":{"publish":"https://npm.example.test/"}}`)
	t.Setenv(privateNPMRegistryURLEnv, "http://127.0.0.1:41234/npm")
	calls := mockManagedNPM(t, false, false)

	status, data, err := runPublishNpm(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("broker publish status=%q err=%v", status, err)
	}
	if data["digestVerified"] != true {
		t.Fatalf("broker publication lacks verified digest: %#v", data)
	}
	const broker = "http://127.0.0.1:41234/npm"
	if calls.publishRegistry != broker || calls.remoteRegistry != broker {
		t.Fatalf("registry drift: publish=%q verify=%q, want the broker %q", calls.publishRegistry, calls.remoteRegistry, broker)
	}
	if len(calls.tokenHosts) != 1 || calls.tokenHosts[0] != "127.0.0.1:41234" {
		t.Fatalf("registry token hosts = %v, want the broker host only", calls.tokenHosts)
	}
}

func TestRunPublishNpm_ManagedRefusesAMalformedPrivateBrokerBeforeCredentials(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	withRegistries(t, ctx, `{"npm":{"publish":"https://npm.example.test/"}}`)
	t.Setenv(privateNPMRegistryURLEnv, "http://localhost:41234/npm")
	calls := mockManagedNPM(t, false, false)

	status, _, err := runPublishNpm(ctx, jsonl.New(), nil)
	if err == nil || status != "FAILED" {
		t.Fatalf("malformed broker status=%q err=%v, want fail closed", status, err)
	}
	if len(calls.tokenHosts) != 0 || calls.publish != 0 {
		t.Fatalf("malformed broker reached credentials or the registry: hosts=%v publishes=%d", calls.tokenHosts, calls.publish)
	}
}

func TestResolveManagedNPMRouteKeepsTheDirectPathWithoutABroker(t *testing.T) {
	t.Setenv(privateNPMRegistryURLEnv, "")
	endpoint, host, err := resolveManagedNPMRoute("https://npm.example.test/scoped")
	if err != nil || endpoint != "https://npm.example.test/scoped" || host != "npm.example.test" {
		t.Fatalf("resolveManagedNPMRoute() = (%q, %q, %v), want the direct path", endpoint, host, err)
	}
	t.Setenv(privateNPMRegistryURLEnv, "https://registry.npmjs.org")
	if endpoint, host, err := resolveManagedNPMRoute("https://npm.example.test"); err != nil || endpoint != "https://npm.example.test" || host != "npm.example.test" {
		t.Fatalf("remote HTTPS compatibility value changed the route: (%q, %q, %v)", endpoint, host, err)
	}
}

// TestMain strips the private publication broker variable the CI runner
// exports for the whole DAG, so the direct-registry tests above see the same
// environment on a laptop and under native publication. Tests that exercise
// the broker set the variable themselves with t.Setenv.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("PUTNAMI_REGISTRY_NPM_URL"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
