package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/privatebroker"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/typescript/extension/internal/pkg"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

const defaultNPMRegistry = "https://registry.npmjs.org"

// privateNPMRegistryURLEnv is the loopback publication broker a native
// publication run exports for the npm registry (see privatebroker).
const privateNPMRegistryURLEnv = "PUTNAMI_REGISTRY_NPM_URL"

// resolveManagedNPMRoute decides where a managed publication uploads and which
// host the credential seam is asked about. Without a private broker both are
// the declared registry. Under a native publication run the runner exports a
// numeric-loopback broker: uploads and verification reads go to the broker,
// and the seam is asked about the broker host, which the cloud answers with
// the run's capability instead of looking for a user session. The declared
// registry stays the logical coordinate; the wire payload carries no registry
// URL, so nothing about the broker reaches the registry or the evidence.
func resolveManagedNPMRoute(registry string) (endpoint, credentialHost string, err error) {
	broker, err := privatebroker.FromEnv(privateNPMRegistryURLEnv, "/npm")
	if err != nil {
		return "", "", err
	}
	if broker == nil {
		return registry, hostFromURL(registry), nil
	}
	return broker.URL, broker.Host, nil
}

var (
	npmExecRun              = exec.Run
	npmResolveRegistryToken = registrycred.ResolveToken
	npmSendManagedPublish   = sendManagedNPMPublish
	npmProbeManagedArtifact = probeManagedNPMArtifact
	npmLookPath             = osexec.LookPath
	npmGOOS                 = runtime.GOOS
)

// npmCommand returns the npm to run with args on goos. Elsewhere than Windows
// it is "npm", which exec finds on PATH. On Windows it is the file the shell
// would run: in each PATH directory PATHEXT puts npm.exe and npm.com before
// npm.cmd, so a native npm next to the batch file wins. npm.cmd is a batch
// file that Windows starts through cmd.exe, so it is refused with
// toolchain.ErrShimArgument when cmd.exe would reinterpret its path or an
// argument. When no npm is found, it returns "npm" and exec reports that, as
// on every other OS.
func npmCommand(goos string, args []string) (string, error) {
	if goos != "windows" {
		return "npm", nil
	}
	path, err := npmLookPath("npm")
	if err != nil {
		return "npm", nil
	}
	if err := toolchain.CheckShimArgsOn(goos, path, args); err != nil {
		return "", fmt.Errorf("npm: %w", err)
	}
	return path, nil
}

// runPublishNpm publishes a pre-built npm package (produced by `package --npm`)
// to a registry. Unmanaged publication keeps npm's native .npmrc behavior. A
// release-set member is a managed, private publication: it resolves a fresh
// bearer for the registry host, isolates every npm subprocess from repository
// and ambient npm config, uploads without a dist-tag, and fails closed when the
// cloud supplies no credential. There is no managed fallback to third-party
// credentials or public npm.
func runPublishNpm(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	if ctx.Project.Name == "" {
		emit.Summary("Skipped: no project context")
		return "SKIP", nil, nil
	}
	plannedVersion, managedReleaseSet, err := npmPublishReleaseSetMember(ctx)
	if err != nil {
		return "FAILED", nil, err
	}

	// A channel is no longer a publish input: the release set advances every
	// channel it names, once, after every member has a verified digest. The
	// version is the only identity a publication writes.
	dryRun := ctx.Params.Bool("dry-run", false, "dryRun")
	access := ctx.Params.String("access")
	otp := ctx.Params.String("otp")
	wsRoot := ctx.WorkspaceRoot
	projectPath := ctx.Project.Path
	// The publish endpoint is the workspace `registries.npm.publish` entry unless
	// --registry names another one. No registry address is hard-coded here and
	// none is read back out of the .npmrc the install generates.
	registry, err := resolvePublishNPMRegistry(ctx)
	if err != nil {
		return "FAILED", nil, err
	}
	if managedReleaseSet {
		// Release-set coordination supplies coordinates, never visibility. Ignore
		// the repository-owned access parameter and leave package visibility to the
		// registry's server-owned policy. A future public release needs a separate,
		// explicit server attestation; sparse/full state cannot make it public.
		access = ""
		registry, err = validateManagedNPMRegistry(orDefaultNPMRegistry(registry))
		if err != nil {
			return "FAILED", nil, err
		}
	}
	// Where the bytes go and whose credential is asked for. Only a managed
	// publication may take the private broker; the unmanaged path keeps npm's
	// native behavior and never sees the variable.
	publishEndpoint, credentialHost := registry, hostFromURL(registry)
	if managedReleaseSet {
		publishEndpoint, credentialHost, err = resolveManagedNPMRoute(registry)
		if err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			return "FAILED", nil, err
		}
	}

	npmAuthEnv := map[string]string{}

	// The channel index is optional; if records exist and the npm channel is not
	// among them, skip (the project did not build an npm package this session).
	meta, _ := pkgmeta.ReadChannelIndex(wsRoot, projectPath)
	if meta != nil && !meta.HasChannel("npm") {
		emit.Summary("Skipped: npm channel not in package metadata")
		return "SKIP", nil, nil
	}

	npmDir := pkgmeta.PackageOutputDir(wsRoot, projectPath, "npm")
	if _, err := os.Stat(filepath.Join(npmDir, "package.json")); os.IsNotExist(err) {
		emit.Diagnostic("error", "npm package not found. Run 'package --npm' first: "+npmDir, "", 0)
		return "FAILED", nil, fmt.Errorf("npm package not found: %s", npmDir)
	}

	pkgData, err := os.ReadFile(filepath.Join(npmDir, "package.json"))
	if err != nil {
		emit.Diagnostic("error", "Failed to read npm package.json: "+err.Error(), "", 0)
		return "FAILED", nil, fmt.Errorf("read npm package.json: %w", err)
	}
	var pkgJSON struct {
		Name          string                     `json:"name"`
		Version       string                     `json:"version"`
		PublishConfig map[string]json.RawMessage `json:"publishConfig"`
	}
	if err := json.Unmarshal(pkgData, &pkgJSON); err != nil {
		emit.Diagnostic("error", "Failed to parse npm package.json: "+err.Error(), "", 0)
		return "FAILED", nil, fmt.Errorf("parse npm package.json: %w", err)
	}

	packageName := pkgJSON.Name
	version := pkgJSON.Version
	if packageName == "" || version == "" {
		return "FAILED", nil, fmt.Errorf("staged npm package is missing name or version")
	}
	if managedReleaseSet {
		for _, key := range []string{"registry", "access", "tag"} {
			if _, present := pkgJSON.PublishConfig[key]; present {
				return "FAILED", nil, fmt.Errorf("managed npm package publishConfig must not set registry, access, or tag")
			}
		}
	}
	if registry != "" {
		// An explicit registry is authoritative even when the checked-out
		// workspace has a scope-specific .npmrc entry. npm gives that project
		// entry precedence over --registry, so mirror the explicit override at
		// environment-config precedence for the exact package scope.
		npmAuthEnv[npmRegistryKey(packageName)] = registry
	}
	if managedReleaseSet && (packageName != ctx.Project.Name || version != plannedVersion) {
		return "FAILED", nil, fmt.Errorf("staged npm package %s@%s does not match planned %s@%s", packageName, version, ctx.Project.Name, plannedVersion)
	}

	emit.Info(fmt.Sprintf("Publishing %s@%s", packageName, version))

	if dryRun {
		emit.Summary(fmt.Sprintf("Dry run: would publish %s@%s", packageName, version))
		if managedReleaseSet {
			// A sparse dry run did not upload or verify registry bytes, so it must
			// not emit kind=published (the reconciler treats that kind as proof).
			emit.ArtifactWithData("npm", packageName, "package", npmDir, map[string]any{
				"registry": "npm", "version": version, "dryRun": true,
			})
		} else {
			// Preserve the legacy/full event contract outside managed release sets.
			emitPublished(emit, "npm", packageName, version, true)
		}
		return "OK", map[string]any{"dryRun": true, "version": version}, nil
	}

	// Ask Cloud only after dry-run has returned without requesting credentials.
	// The seam carries the registry HOST and nothing else: the cloud owns what
	// authority the credential grants, so no package coordinate or action name
	// crosses it. A managed host with no credential fails closed; only the
	// unmanaged path may fall back to native npm auth.
	registryToken := ""
	if registry != "" {
		tok, credHint := npmResolveRegistryToken(credentialHost)
		if tok != "" {
			registryToken = tok
			if !managedReleaseSet {
				npmAuthEnv[npmAuthKey(registry)] = tok
			}
		} else if managedReleaseSet {
			message := credHint
			if message == "" {
				message = "managed npm registry authorization unavailable"
			}
			emit.Diagnostic("error", message, "", 0)
			return "FAILED", nil, fmt.Errorf("resolve managed npm registry token: %s", message)
		}
	}

	if managedReleaseSet {
		return publishManagedNPM(emit, wsRoot, projectPath, npmDir, packageName, version, publishEndpoint, registryToken)
	}

	// Resolve npm once for both runs below, so a refused npm fails before any
	// registry call.
	viewArgs := []string{"view", packageName + "@" + version, "version"}
	publishArgs := buildNPMPublishArgs(access, registry)
	npmBin, err := npmCommand(npmGOOS, slices.Concat(viewArgs, publishArgs))
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, err
	}

	// One publish per version: detect an already-published version so the run
	// reuses it instead of re-uploading.
	emit.PhaseStart("check-version")
	viewResult, _ := npmExecRun(npmBin, viewArgs, npmExecOpts(npmDir, npmAuthEnv)...)
	alreadyPublished := viewResult != nil && viewResult.Success && strings.TrimSpace(viewResult.Stdout) == version
	emit.PhaseEnd("check-version", "success")

	if alreadyPublished {
		emit.Info(fmt.Sprintf("Version %s already published, reusing it", version))
	} else {
		emit.PhaseStart("npm-publish")
		publishEnv := maps.Clone(npmAuthEnv)
		if otp != "" {
			// Pass the OTP via NPM_CONFIG_OTP rather than --otp so the secret never
			// appears in npm's argv (world-readable via /proc/<pid>/cmdline; CWE-214).
			publishEnv["NPM_CONFIG_OTP"] = otp
		}
		result, _ := npmExecRun(npmBin, publishArgs, npmExecOpts(npmDir, publishEnv)...)
		if result == nil || !result.Success {
			emit.Diagnostic("error", "npm publish failed:\n"+execStderr(result), "", 0)
			emit.PhaseEnd("npm-publish", "failed")
			return "FAILED", nil, fmt.Errorf("npm publish failed: %s", execStderr(result))
		}
		emit.PhaseEnd("npm-publish", "success")
	}

	emit.Summary(fmt.Sprintf("Published %s@%s", packageName, version))
	emitPublished(emit, "npm", packageName, version, false)
	return "OK", map[string]any{"version": version, "packageName": packageName}, nil
}

// resolvePublishNPMRegistry settles the registry a publication uploads to: the
// explicit --registry flag first, then the workspace `registries.npm.publish`
// entry.
//
// The workspace document is the single declared source of the endpoint. The
// previous chain read the generated .npmrc back, which made the target depend
// on whether the install had run in this checkout yet.
//
// Neither being declared yields "", which the unmanaged path reads as "let npm
// resolve the registry from its own configuration"; only the managed path,
// which must name an exact validated origin, applies a default.
func resolvePublishNPMRegistry(ctx *pctx.Context) (string, error) {
	if explicit := strings.TrimSpace(ctx.Params.String("registry")); explicit != "" {
		return explicit, nil
	}
	declared, err := npmRegistriesFrom(ctx.Params)
	if err != nil {
		return "", err
	}
	return declared.Publish, nil
}

// orDefaultNPMRegistry supplies npm's own registry when the workspace declares
// none. A managed publication needs an exact origin to validate, resolve a
// credential for, and read the published bytes back from.
func orDefaultNPMRegistry(registry string) string {
	if registry == "" {
		return defaultNPMRegistry
	}
	return registry
}

func validateManagedNPMRegistry(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("managed npm registry must be an absolute HTTP(S) URL without credentials")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("managed npm registry must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" {
		hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		ip := net.ParseIP(hostname)
		loopback := hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || (ip != nil && ip.IsLoopback())
		if scheme != "http" || !loopback {
			return "", fmt.Errorf("managed npm registry must use HTTPS (HTTP is allowed only for loopback)")
		}
	}
	u.Scheme = scheme
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func npmPublishReleaseSetMember(ctx *pctx.Context) (string, bool, error) {
	plan, err := releaseset.FromContext(ctx)
	if err != nil || plan == nil {
		return "", false, err
	}
	member, ok := plan.Member("npm", ctx.Project.Name)
	if !ok {
		return "", true, fmt.Errorf("npm release-set plan has no member for %q", ctx.Project.Name)
	}
	if !member.Selected {
		return "", true, fmt.Errorf("npm release-set member %q is not selected for publishing", ctx.Project.Name)
	}
	if ctx.Identity != nil && member.ProjectID != ctx.Identity.Project.ID {
		return "", true, fmt.Errorf("npm release-set member %q belongs to project %q, not %q", ctx.Project.Name, member.ProjectID, ctx.Identity.Project.ID)
	}
	return member.Version, true, nil
}

// publishManagedNPM uploads a pre-built immutable tarball with an empty
// dist-tags map, then downloads the registry copy to prove the bytes match. The
// repository cannot select visibility or a channel through npm argv, ambient
// config, or publishConfig; Cloud advances a channel only after release-set
// reconciliation.
func publishManagedNPM(emit *jsonl.Emitter, wsRoot, projectPath, npmDir, packageName, version, registry, registryToken string) (string, map[string]any, error) {
	npmConfig, configCleanup, err := newManagedNPMConfig(nil)
	if err != nil {
		return "FAILED", nil, err
	}
	defer configCleanup()

	localArtifact, localDigest, cleanup, err := packNPMArtifact(npmConfig, wsRoot, projectPath, npmDir)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return "FAILED", nil, err
	}

	artifactInfo, err := os.Stat(localArtifact)
	if err != nil {
		return "FAILED", nil, fmt.Errorf("stat managed npm artifact: %w", err)
	}

	emit.PhaseStart("check-version")
	alreadyPublished, probeErr := npmProbeManagedArtifact(registry, registryToken, packageName, version, localDigest, artifactInfo.Size())
	if probeErr != nil {
		emit.PhaseEnd("check-version", "failed")
		return "FAILED", nil, fmt.Errorf("check published npm artifact: %w", probeErr)
	}
	emit.PhaseEnd("check-version", "success")

	if !alreadyPublished {
		emit.PhaseStart("npm-publish")
		payload, buildErr := buildManagedNPMPublishPayload(npmDir, localArtifact, packageName, version)
		if buildErr != nil {
			emit.PhaseEnd("npm-publish", "failed")
			return "FAILED", nil, buildErr
		}
		if publishErr := npmSendManagedPublish(registry, registryToken, payload); publishErr != nil {
			emit.PhaseEnd("npm-publish", "failed")
			return "FAILED", nil, fmt.Errorf("managed npm publish failed: %w", publishErr)
		}
		emit.PhaseEnd("npm-publish", "success")
	} else {
		emit.Info(fmt.Sprintf("Version %s already published; verifying immutable registry bytes", version))
	}

	if !alreadyPublished {
		found, verifyErr := npmProbeManagedArtifact(registry, registryToken, packageName, version, localDigest, artifactInfo.Size())
		if verifyErr != nil {
			return "FAILED", nil, fmt.Errorf("verify published npm artifact: %w", verifyErr)
		}
		if !found {
			return "FAILED", nil, fmt.Errorf("verify published npm artifact: authenticated tarball returned 404 Not Found")
		}
	}

	emit.Summary(fmt.Sprintf("Published %s@%s [%s]", packageName, version, localDigest))
	emitPublishedVerifiedNPM(emit, packageName, version, localDigest)
	// One member per artifact, on the fresh and on the reused path alike: the
	// release set is assembled from what EXISTS in the registry under a verified
	// digest, not from what this session happened to upload.
	emitPublishedNPMMember(emit, packageName, version, localDigest)
	data := map[string]any{
		"version": version, "packageName": packageName,
		"artifactDigest": localDigest, "digestVerified": true,
	}
	if alreadyPublished {
		data["alreadyPublished"] = true
	}
	return "OK", data, nil
}

type managedNPMConfig struct {
	dir          string
	userConfig   string
	globalConfig string
	env          map[string]string
}

func newManagedNPMConfig(_ map[string]string) (*managedNPMConfig, func(), error) {
	dir, err := os.MkdirTemp("", "putnami-managed-npm-")
	if err != nil {
		return nil, nil, fmt.Errorf("create managed npm config directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	userConfig := filepath.Join(dir, "user.npmrc")
	globalConfig := filepath.Join(dir, "global.npmrc")
	for _, path := range []string{userConfig, globalConfig} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("create isolated npm config: %w", err)
		}
	}
	return &managedNPMConfig{
		dir: dir, userConfig: userConfig, globalConfig: globalConfig,
		env: nil,
	}, cleanup, nil
}

func (c *managedNPMConfig) opts() []exec.Option {
	opts := []exec.Option{
		exec.Dir(c.dir),
		exec.UnsetEnv(managedNPMConfigEnvNames(os.Environ())...),
	}
	if len(c.env) > 0 {
		opts = append(opts, exec.Env(c.env))
	}
	return opts
}

func managedNPMConfigEnvNames(environment []string) []string {
	var names []string
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "npm_config_") || lower == "npm_config" ||
			lower == "npm_token" || lower == "node_auth_token" ||
			(strings.HasPrefix(lower, "putnami_") && strings.HasSuffix(lower, "_token")) ||
			lower == "go_registry_token" || lower == "node_options" || lower == "node_path" ||
			lower == "bun_options" || lower == "http_proxy" || lower == "https_proxy" ||
			lower == "all_proxy" || lower == "no_proxy" || lower == "ld_preload" ||
			lower == "dyld_insert_libraries" || lower == "dyld_library_path" ||
			lower == "ssh_auth_sock" || lower == "gpg_agent_info" {
			names = append(names, name)
		}
	}
	return names
}

// packNPMArtifact turns the staged package directory into the exact tarball
// the managed PUT carries. It packs with bun — the workspace's package manager,
// present on every laptop and baked into the CI runner image, which ships node
// and bun but no npm — from inside the staged directory, with lifecycle scripts
// off and the same ambient-config isolation as every other managed call. The
// digest is taken after pkg.NormalizeNPMArchive, which on Windows gives the
// entries the modes a Unix host packs.
func packNPMArtifact(config *managedNPMConfig, wsRoot, projectPath, npmDir string) (string, string, func(), error) {
	bunBin, err := resolveBunBin()
	if err != nil {
		return "", "", nil, fmt.Errorf("bun pm pack: %w", err)
	}
	tempDir, err := os.MkdirTemp("", "putnami-npm-artifact-")
	if err != nil {
		return "", "", nil, fmt.Errorf("create npm artifact directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	args := []string{"pm", "pack", "--ignore-scripts", "--quiet", "--destination", tempDir}
	opts := append(config.opts(), exec.Dir(npmDir))
	result, runErr := npmExecRun(bunBin, args, opts...)
	if runErr != nil || result == nil || !result.Success {
		cleanup()
		if runErr != nil {
			return "", "", nil, fmt.Errorf("bun pm pack failed: %w", runErr)
		}
		return "", "", nil, fmt.Errorf("bun pm pack failed: %s", execStderr(result))
	}
	matches, err := filepath.Glob(filepath.Join(tempDir, "*.tgz"))
	if err != nil || len(matches) != 1 {
		cleanup()
		return "", "", nil, fmt.Errorf("bun pm pack produced %d tarballs, want exactly one", len(matches))
	}
	if err := pkg.NormalizeNPMArchive(npmGOOS, wsRoot, projectPath, npmDir, matches[0]); err != nil {
		cleanup()
		return "", "", nil, err
	}
	digest, err := sha256File(matches[0])
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	return matches[0], digest, cleanup, nil
}

// The managed npm upload lives in go.putnami.dev/sdk/extension/npmpublish;
// these names bind this job and its tests to it.
type (
	managedNPMAttachment     = npmpublish.Attachment
	managedNPMPublishPayload = npmpublish.Payload
)

// managedNPMRegistryExcerptLimit bounds, in characters, how much of a registry
// error body a managed npm error carries.
const managedNPMRegistryExcerptLimit = npmpublish.ErrorExcerptLimit

func buildManagedNPMPublishPayload(npmDir, artifact, packageName, version string) (managedNPMPublishPayload, error) {
	manifest, err := os.ReadFile(filepath.Join(npmDir, "package.json"))
	if err != nil {
		return managedNPMPublishPayload{}, fmt.Errorf("read managed npm manifest: %w", err)
	}
	artifactBytes, err := os.ReadFile(artifact)
	if err != nil {
		return managedNPMPublishPayload{}, fmt.Errorf("read managed npm artifact: %w", err)
	}
	return npmpublish.BuildPayload(npmpublish.Artifact{Name: packageName, Version: version, Manifest: manifest, Tarball: artifactBytes})
}

func sendManagedNPMPublish(registry, token string, payload managedNPMPublishPayload) error {
	client, err := newManagedNPMHTTPClient()
	if err != nil {
		return err
	}
	return npmpublish.Put(context.Background(), client, registry, token, payload)
}

func newManagedNPMHTTPClient() (*http.Client, error) {
	return npmpublish.NewHTTPClient()
}

func probeManagedNPMArtifact(registry, token, packageName, version, wantDigest string, wantSize int64) (bool, error) {
	client, err := newManagedNPMHTTPClient()
	if err != nil {
		return false, err
	}
	return npmpublish.Probe(context.Background(), client, registry, token, packageName, version, wantDigest, wantSize)
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open npm artifact: %w", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash npm artifact: %w", err)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

// runPublishDocker publishes a pre-built Docker image via the shared,
// ecosystem-agnostic dockerpublish.Publish — the same orchestration the Go
// extension uses, since docker packaging output is language-independent.
func runPublishDocker(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return dockerpublish.Publish(ctx, emit, args)
}

// buildNPMPublishArgs constructs the argument list for `npm publish`.
//
// No --tag is passed: a dist-tag IS a channel, and which channels a publication
// advances is the release set's decision, taken once for every member after all
// of them have a verified digest. A publisher that named one here would move a
// channel member by member, so a consumer reading it mid-publish would see a set
// that was never released.
//
// The OTP is deliberately not included either: it is passed via the
// NPM_CONFIG_OTP environment variable so the secret never appears in the
// process argv.
func buildNPMPublishArgs(access, registry string) []string {
	args := []string{"publish"}
	if access != "" {
		args = append(args, "--access", access)
	}
	if registry != "" {
		args = append(args, "--registry", registry)
	}
	return args
}

// npmExecOpts builds the exec options for an npm child: the working directory
// plus, when non-empty, the registry-scoped authToken env. An empty env adds no
// exec.Env, so npm's native .npmrc auth is used unchanged.
func npmExecOpts(npmDir string, env map[string]string) []exec.Option {
	opts := []exec.Option{exec.Dir(npmDir)}
	if len(env) > 0 {
		opts = append(opts, exec.Env(env))
	}
	return opts
}

// npmAuthKey returns the environment variable that carries the registry-scoped
// auth token for registry, e.g. "https://npm.putnami.dev" →
// "npm_config_//npm.putnami.dev/:_authToken". npm reads any "npm_config_<key>"
// env var as config, so this sets the same per-registry bearer that .npmrc's
// "//host/:_authToken=" line would — but resolved fresh per run. The scheme is
// stripped and the host/path preserved so per-path registries key correctly.
func npmAuthKey(registry string) string {
	r := registry
	if i := strings.Index(r, "://"); i >= 0 {
		r = r[i+len("://"):]
	}
	return "npm_config_//" + strings.TrimRight(r, "/") + "/:_authToken"
}

// npmRegistryKey returns the npm environment-config key which makes an
// explicit registry override authoritative over a project .npmrc. Scoped
// packages need their exact @scope:registry key; unscoped packages use the
// ordinary registry key.
func npmRegistryKey(packageName string) string {
	if strings.HasPrefix(packageName, "@") {
		if slash := strings.IndexByte(packageName, '/'); slash > 1 {
			return "npm_config_" + packageName[:slash] + ":registry"
		}
	}
	return "npm_config_registry"
}

// hostFromURL returns the URL host (including any explicit port), used to key a
// registry credential lookup by host. Returns "" for an unparseable URL. Shared
// by the npm and docker publishers.
func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// execStderr extracts stderr from an exec result, returning "" if result is nil.
// Shared by the npm and docker publishers.
func execStderr(result *exec.Result) string {
	if result != nil {
		return result.Stderr
	}
	return ""
}

// emitPublished records a published artifact as a session event so the CLI
// summary can list it. registry is the channel ("npm", "docker"), name is the
// registry coordinate without a version, and version is the published version.
// dryRun marks artifacts that were prepared but not actually uploaded.
//
// There are no tags: a publication writes no channel, so there is none to
// report. The release advances every channel it names, once, after every member
// has a verified digest.
func emitPublished(emit *jsonl.Emitter, registry, name, version string, dryRun bool) {
	data := map[string]any{"registry": registry, "version": version}
	if dryRun {
		data["dryRun"] = true
	}
	emit.ArtifactWithData(registry, name, "published", "", data)
}

// emitPublishedNPMMember reports the package to the release set being assembled:
// one member, at its npm coordinate, with the digest verified against the bytes
// the registry serves. `published` above stays as it is: it is the
// human-and-renderer event, this one is the release-set fact.
func emitPublishedNPMMember(emit *jsonl.Emitter, packageName, version, artifactDigest string) {
	emit.ArtifactWithData(npmEcosystem, packageName, extproto.PublishedMemberEventKind, "", map[string]any{
		"ecosystem":      npmEcosystem,
		"coordinate":     packageName,
		"version":        version,
		"artifactDigest": artifactDigest,
	})
}

func emitPublishedVerifiedNPM(emit *jsonl.Emitter, name, version, artifactDigest string) {
	emit.ArtifactWithData("npm", name, "published", "", map[string]any{
		"registry":       "npm",
		"version":        version,
		"artifactDigest": artifactDigest,
		"digestVerified": true,
		"dryRun":         false,
	})
}
