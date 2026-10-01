package publish

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"go.putnami.dev/go/extension/internal/releaseplan"
	"go.putnami.dev/go/extension/internal/toolchain"
	extproto "go.putnami.dev/protocol/extension"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/gomodpublish"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/privatebroker"
	"go.putnami.dev/sdk/extension/registrycred"
)

const maxGoRegistryResponseBytes = gomodpublish.MaxResponseBytes

// privateGoRegistryURLEnv is the loopback publication broker a native
// publication run exports for the Go module registry (see privatebroker).
const privateGoRegistryURLEnv = "PUTNAMI_REGISTRY_GOMOD_URL"

// goPublishRoute is how one module publication reaches its registry: the
// endpoint every write, verification read, and smoke download is sent to, and
// the host the credential seam is asked about. The declared origin stays the
// logical publication coordinate in every message and event.
type goPublishRoute struct {
	endpoint       string
	credentialHost string
	broker         bool
}

// resolveGoPublishRoute decides the route for a module publication.
//
// Without a private broker the route is the declared origin itself and the
// seam is asked about that origin's host. Under a native publication run the
// runner exports a numeric-loopback broker: the route then targets the broker
// and asks the seam about the BROKER host, which the cloud answers with the
// run's capability; asking for the canonical host (go.putnami.dev) would look
// for a user session this process does not have. The broker only admits
// release-set members, so a loopback value without a plan fails closed rather
// than route an unplanned publication around it.
func resolveGoPublishRoute(declaredOrigin string, managed bool) (goPublishRoute, error) {
	broker, err := privatebroker.FromEnv(privateGoRegistryURLEnv, "/go")
	if err != nil {
		return goPublishRoute{}, err
	}
	if broker == nil {
		return goPublishRoute{endpoint: declaredOrigin, credentialHost: hostFromURL(declaredOrigin)}, nil
	}
	if !managed {
		return goPublishRoute{}, fmt.Errorf("private Go registry broker requires a release-set plan")
	}
	return goPublishRoute{endpoint: broker.URL, credentialHost: broker.Host, broker: true}, nil
}

// goModule publishes a Go module to a registry implementing the gomod-write
// protocol (protocol/gomod): a zip blob upload, then a version PUT, then a `go mod
// download` smoke test against the registry. Publication is private regardless
// of release-set coordination: registry writes, immutable reads, and the
// consumer smoke are all authenticated. A missing credential fails fast
// (unlike npm/docker, which fall back to native auth). Public visibility requires
// a separate, server-owned attestation contract and is deliberately not inferred
// from a repository command, channel, or release-set plan.
func goModule(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	if ctx.Project.Name == "" {
		emit.Summary("Skipped: no project context")
		return "SKIP", nil, nil
	}
	planned, err := releaseplan.ResolveGoProject(ctx)
	if err != nil {
		emit.Diagnostic("error", "Invalid Go release-set plan: "+err.Error(), "", 0)
		return "FAILED", nil, err
	}
	if planned != nil && !planned.Member.Selected {
		emit.Summary("Skipped: Go module is unchanged in release-set plan")
		return "SKIP", nil, nil
	}
	dryRun := ctx.Params.Bool("dry-run", false, "dryRun")

	// The publication endpoint is the workspace's declared `registries.go.origin`,
	// resolved through the same reader every `go` command's environment uses, so
	// the registry this job writes to is by construction the host GONOPROXY
	// routes to and the host the credential is minted for. GO_REGISTRY_URL stays
	// the one explicit override, for a job pinned at a fixture registry.
	registryURL := toolchain.DeclaredGoOrigin(os.Environ(), ctx.WorkspaceRoot)
	if registryURL == "" {
		const missing = "no Go module origin declared: set registries.go.origin in putnami.workspace.json, or GO_REGISTRY_URL"
		emit.Diagnostic("error", missing, "", 0)
		return "FAILED", nil, fmt.Errorf("%s", missing)
	}
	registryURL, err = validateGoRegistryURL(registryURL)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, err
	}
	route, err := resolveGoPublishRoute(registryURL, planned != nil)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, err
	}
	wsRoot := ctx.WorkspaceRoot
	projectPath := ctx.Project.Path

	if dryRun && planned != nil {
		// Managed publication uses the dry-run stream to authorize the exact
		// npm/Go member tuple before it grants write credentials. The package
		// command intentionally creates no Go artifacts during a dry run, so use
		// the already-validated release-set member instead of requiring
		// module.json that cannot exist. This event is planning evidence only: it
		// is never a kind=published proof and carries no verified digest.
		modulePath := planned.Member.Coordinate
		version := planned.Member.Version
		emit.Summary(fmt.Sprintf("Dry run: would publish %s@%s to %s", modulePath, version, registryURL))
		emit.ArtifactWithData("go", modulePath, "package", pkgmeta.PackageOutputDir(wsRoot, projectPath, "go"), map[string]any{
			"registry": "go", "version": version, "dryRun": true,
			"digestVerified": false,
		})
		return "OK", map[string]any{"dryRun": true, "modulePath": modulePath, "version": version}, nil
	}

	channels, err := pkgmeta.ReadChannelIndex(wsRoot, projectPath)
	if err != nil {
		emit.Diagnostic("error", fmt.Sprintf("No package metadata found. Run 'package' first: %v", err), "", 0)
		return "FAILED", nil, fmt.Errorf("read package metadata: %w", err)
	}
	if !channels.HasChannel("go") {
		emit.Summary("Skipped: go channel not in package metadata")
		return "SKIP", nil, nil
	}

	moduleMeta, err := pkgmeta.ReadGoModuleMetadata(wsRoot, projectPath)
	if err != nil {
		emit.Diagnostic("error", "Go module metadata not found: "+err.Error(), "", 0)
		return "FAILED", nil, fmt.Errorf("read go module metadata: %w", err)
	}

	modulePath := moduleMeta.ModulePath
	version := moduleMeta.Version
	if planned != nil {
		if modulePath != planned.Member.Coordinate {
			err := fmt.Errorf("packaged Go module %q does not match planned coordinate %q", modulePath, planned.Member.Coordinate)
			emit.Diagnostic("error", err.Error(), "", 0)
			return "FAILED", nil, err
		}
		if version != planned.Member.Version {
			err := fmt.Errorf("packaged Go module version %q does not match planned version %q", version, planned.Member.Version)
			emit.Diagnostic("error", err.Error(), "", 0)
			return "FAILED", nil, err
		}
	}

	if route.broker {
		emit.Info(fmt.Sprintf("Publishing Go module %s@%s to %s via private publication broker %s", modulePath, version, registryURL, route.endpoint))
	} else {
		emit.Info(fmt.Sprintf("Publishing Go module %s@%s to %s", modulePath, version, registryURL))
	}

	if dryRun {
		emit.Summary(fmt.Sprintf("Dry run: would publish %s@%s to %s", modulePath, version, registryURL))
		// A simulation cannot be publication proof for the release-set
		// reconciler, so it deliberately uses kind=package.
		emit.ArtifactWithData("go", modulePath, "package", moduleMeta.ZipPath, map[string]any{
			"registry": "go", "version": version, "dryRun": true,
			"digestVerified": false,
		})
		return "OK", map[string]any{"dryRun": true, "modulePath": modulePath, "version": version}, nil
	}

	// Explicit overrides win; otherwise the cloud supplies the credential for the
	// registry HOST. The seam carries the host and nothing else — no package, no
	// action, no owning workspace — so the cloud owns what authority the
	// credential grants. Under a private broker the host is the broker's, so
	// the cloud hands back the run's capability. The complete module path
	// remains the publication coordinate.
	registryToken, credHint := resolveGoPublishToken(ctx.Params, route.credentialHost)

	// The gomod registry rejects an anonymous blob upload, so a missing credential
	// fails fast with a neutral message — the cloud's own hint when it produced
	// one, else a generic explicit-credentials pointer.
	if registryToken == "" {
		msg := credHint
		if msg == "" {
			msg = "no Go module registry credential resolved; pass --go-registry-token or set PUTNAMI_REGISTRY_TOKEN"
		}
		emit.Diagnostic("error", msg, "", 0)
		return "FAILED", nil, fmt.Errorf("resolve go registry token: %s", msg)
	}

	client, err := newGoRegistryHTTPClient()
	if err != nil {
		return "FAILED", nil, err
	}

	emit.PhaseStart("upload-module")
	zipDigest, err := uploadZipBlob(client, emit, route.endpoint, registryToken, modulePath, moduleMeta.ZipPath)
	if err != nil {
		emit.PhaseEnd("upload-module", "failed")
		return "FAILED", nil, fmt.Errorf("upload module zip: %w", err)
	}

	goModContent, err := os.ReadFile(moduleMeta.ModPath)
	if err != nil {
		emit.Diagnostic("error", "Failed to read go.mod: "+err.Error(), "", 0)
		emit.PhaseEnd("upload-module", "failed")
		return "FAILED", nil, fmt.Errorf("read go.mod: %w", err)
	}

	reused, err := publishVersion(client, emit, route.endpoint, registryToken, modulePath, version, string(goModContent), zipDigest, planned != nil)
	if err != nil {
		emit.PhaseEnd("upload-module", "failed")
		return "FAILED", nil, fmt.Errorf("publish version: %w", err)
	}
	if err := verifyPublishedZip(client, route.endpoint, registryToken, modulePath, version, zipDigest); err != nil {
		emit.Diagnostic("error", "Published module zip verification failed: "+err.Error(), "", 0)
		emit.PhaseEnd("upload-module", "failed")
		return "FAILED", nil, fmt.Errorf("verify published module zip: %w", err)
	}
	if err := verifyPublishedMod(client, route.endpoint, registryToken, modulePath, version, goModContent); err != nil {
		emit.Diagnostic("error", "Published go.mod verification failed: "+err.Error(), "", 0)
		emit.PhaseEnd("upload-module", "failed")
		return "FAILED", nil, fmt.Errorf("verify published go.mod: %w", err)
	}
	emit.PhaseEnd("upload-module", "success")

	// Record the published module now; a later smoke-test failure must not hide a
	// successful registry write from the session's Published summary.
	emitPublished(emit, "go", modulePath, version, zipDigest, reused)
	// Report the member to the release set being assembled, after both
	// immutable verifications: a reuse carries the same proof as a first
	// publication.
	emitPublishedMember(emit, modulePath, version, zipDigest)

	emit.PhaseStart("smoke-test")
	if route.broker {
		// The go toolchain never sends credentials to an http:// proxy
		// ("refusing to pass credentials to insecure URL", cmd/go/internal/web),
		// and GOAUTH/netrc apply to https only, so `go mod download` cannot
		// authenticate against the loopback broker. The authenticated .zip and
		// .mod reads above already served the published bytes back through the
		// broker; that is the consumer proof on this route.
		emit.Log("info", fmt.Sprintf("Smoke test skipped: the go toolchain cannot authenticate against the http loopback broker %s; the published .zip and .mod were verified through it", route.endpoint))
		emit.PhaseEnd("smoke-test", "skipped")
	} else {
		emit.Log("info", fmt.Sprintf("Smoke test: go mod download %s@%s via %s", modulePath, version, route.endpoint))
		// The smoke is an authenticated private consumer proof. GOPROXY is pinned
		// to this endpoint with `,off`, and the subprocess disables SumDB, so
		// neither the public proxy nor checksum database can observe or satisfy
		// this coordinate.
		if err := smokeTestGoModule(route.endpoint, registryToken, modulePath, version); err != nil {
			emit.Diagnostic("error", "Smoke test failed: "+err.Error(), "", 0)
			emit.PhaseEnd("smoke-test", "failed")
			return "FAILED", nil, fmt.Errorf("smoke test: %w", err)
		}
		emit.PhaseEnd("smoke-test", "success")
	}

	emit.Summary(fmt.Sprintf("Published %s@%s", modulePath, version))
	result := map[string]any{
		"modulePath": modulePath, "version": version,
		"artifactDigest": zipDigest, "digestVerified": true,
	}
	if reused {
		result["digestReused"] = true
	}
	return "OK", result, nil
}

// validateGoRegistryURL rejects credential-bearing URLs before any diagnostic,
// dry-run output, lease request, or network call can render their userinfo.
// Publisher credentials have a dedicated token seam and must never travel in a
// repository-owned URL.
func validateGoRegistryURL(raw string) (string, error) {
	return gomodpublish.ValidateRegistryURL(raw)
}

func newGoRegistryHTTPClient() (*http.Client, error) {
	return gomodpublish.NewHTTPClient()
}

// resolveGoPublishToken resolves the bearer in priority order: the
// --go-registry-token flag / kebab param, PUTNAMI_REGISTRY_TOKEN, a committed
// camelCase config default, and finally
// @putnami/cloud's credential for the host. Explicit sources stay ahead of the
// committed config default so a config value cannot shadow a flag/env.
func resolveGoPublishToken(params pctx.Params, host string) (token, hint string) {
	if t := cmp.Or(
		params.String("go-registry-token"),
		os.Getenv("PUTNAMI_REGISTRY_TOKEN"),
		params.String("goRegistryToken"),
	); t != "" {
		return t, ""
	}
	return registrycred.ResolveToken(host)
}

// uploadZipBlob uploads the module zip to the gomod-write blob endpoint and
// returns the blob digest.
func uploadZipBlob(client *http.Client, emit *jsonl.Emitter, registryURL, token, modulePath, zipPath string) (string, error) {
	if zipPath == "" {
		emit.Diagnostic("error", "Missing zip artifact path (run 'package' with latest Go extension)", "", 0)
		return "", fmt.Errorf("missing zip path")
	}
	zipData, err := os.ReadFile(zipPath)
	if err != nil {
		emit.Diagnostic("error", "Failed to read zip: "+err.Error(), "", 0)
		return "", err
	}
	return gomodpublish.UploadZip(context.Background(), client, emit, registryURL, token, modulePath, zipData)
}

// publishVersion publishes the module version via the gomod-write version PUT
// and reports whether the registry already held that exact version. No dist
// tag is sent: the release-set coordinator advances every channel it names,
// once, after every selected member has a verified digest.
func publishVersion(client *http.Client, emit *jsonl.Emitter, registryURL, token, modulePath, version, goMod, zipDigest string, allowExisting bool) (reused bool, err error) {
	return gomodpublish.PublishVersion(context.Background(), client, emit, registryURL, token, modulePath, version, []byte(goMod), zipDigest, allowExisting)
}

// verifyPublishedZip downloads the immutable standard Go proxy artifact and
// hashes the exact bytes served to consumers.
func verifyPublishedZip(client *http.Client, registryURL, token, modulePath, version, wantDigest string) error {
	return gomodpublish.VerifyZip(context.Background(), client, registryURL, token, modulePath, version, wantDigest)
}

// verifyPublishedMod compares the standard Go proxy .mod projection byte for
// byte with the file submitted in the version PUT.
func verifyPublishedMod(client *http.Client, registryURL, token, modulePath, version string, want []byte) error {
	return gomodpublish.VerifyMod(context.Background(), client, registryURL, token, modulePath, version, want)
}

// hostFromURL returns the URL host (including any explicit port), used to key the
// registry credential lookup. Returns "" for an unparseable URL.
func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// emitPublished records a published artifact as a session event so the CLI
// summary can list it. digestReused marks a version the registry already held
// at exactly these bytes, so a reader can tell a fresh upload from a verified
// re-run without reading the member event.
func emitPublished(emit *jsonl.Emitter, registry, name, version, artifactDigest string, digestReused bool) {
	data := map[string]any{
		"registry": registry, "version": version, "dryRun": false,
		"digestVerified": true,
	}
	if artifactDigest != "" {
		data["artifactDigest"] = artifactDigest
	}
	if digestReused {
		data["digestReused"] = true
	}
	emit.ArtifactWithData(registry, name, "published", "", data)
}

// emitPublishedMember reports the module to the release set being assembled:
// one member, at its module path, with the zip digest a consumer can pin.
//
// The coordinate is the COMPLETE module path, because that is the identity a
// `go get` resolves and the identity the release set carries; the origin that
// served it is where a copy lives, not what the member is. `published` above
// stays as it is: it is the human-and-renderer event, this one is the
// release-set fact.
func emitPublishedMember(emit *jsonl.Emitter, modulePath, version, artifactDigest string) {
	emit.ArtifactWithData("go", modulePath, extproto.PublishedMemberEventKind, "", map[string]any{
		"ecosystem":      string(releaseplan.GoEcosystem),
		"coordinate":     modulePath,
		"version":        version,
		"artifactDigest": artifactDigest,
	})
}
