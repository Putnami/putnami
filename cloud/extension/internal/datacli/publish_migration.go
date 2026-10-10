package datacli

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.putnami.dev/client"
	dataapiclient "go.putnami.dev/cloud/clients/data-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
)

const (
	migrationRegistryAuthorizationHeader = "X-Putnami-Migration-Registry-Authorization"
	// migrationRegistryCredentialProfile is data-api's named-header profile for
	// this header (data-api's generated contract): a bounded Put
	// registry bearer distinct from the workspace user bearer, required
	// alongside it (both profiles are allOf on this operation).
	migrationRegistryCredentialProfile = "migrationRegistry"
	migrationBundleManifest            = "bundle.json"
	maxMigrationBundleBytes            = 16 << 20
)

var migrationAddressPartPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)

// migration-bundle.v1 uses bare hex for its semantic digest. Put artifact and
// blob descriptors use algorithm-qualified digests instead.
var migrationBundleDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var migrationDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// PreparedMigration is the canonical local artifact identity checked before
// credentials or network access. Files is intentionally excluded from JSON so
// structured/dry-run output cannot dump migration contents.
type PreparedMigration struct {
	Application        string            `json:"application"`
	ProjectPath        string            `json:"project_path"`
	Version            string            `json:"version"`
	ExpectedCoordinate string            `json:"expected_coordinate"`
	BundleDigest       string            `json:"bundle_digest"`
	BundleDir          string            `json:"-"`
	Files              map[string][]byte `json:"-"`
}

// MigrationSelection is the exact source provenance accepted by release-set
// planning for this member.
type MigrationSelection struct {
	SourceRevision       string
	SelectionFingerprint string
}

// MigrationMember is Data's exact native member for release-set emission.
type MigrationMember struct {
	Ecosystem            string `json:"ecosystem"`
	Coordinate           string `json:"coordinate"`
	Version              string `json:"version"`
	ArtifactDigest       string `json:"artifactDigest"`
	SourceRevision       string `json:"sourceRevision"`
	SelectionFingerprint string `json:"selectionFingerprint"`
}

// PublishedMigration is the bounded response the surface validates and emits.
type PublishedMigration struct {
	MigrationRef   string          `json:"migration_ref"`
	BundleDigest   string          `json:"bundle_digest"`
	BlobDigest     string          `json:"blob_digest"`
	ArtifactDigest string          `json:"artifact_digest"`
	Member         MigrationMember `json:"member"`
}

// MigrationTarget is the workspace project a migration publication addresses.
// It is resolved before the generated bundle is read, so a caller can decide
// what an absent bundle means before Prepare reports a skip.
//
// A project has two names, and they differ for a nested project: its manifest
// name ("putnami.dev") and its workspace path ("sites/putnami.dev"). The
// release set, the deploy executor and the migrate worker all name a project by
// its path, so the publication does too. The manifest name only labels messages
// and is what the build writes into the bundle's appName.
type MigrationTarget struct {
	// Name is the project's manifest name.
	Name string
	// Application is the publication identity Data keys the migration on and
	// derives the Put package from. MigrationApplication chooses it: the
	// workspace path without its leading "/", or the manifest name for the
	// workspace-root project and for a path that maps to no native Put package.
	Application string
	AppDir      string
	// ProjectPath is the release-set plan's project ID: "/" + the workspace
	// path without grouping folders such as "(web)" (clicore.ProjectIDFromPath).
	ProjectPath string
	BundleDir   string
}

// ResolveMigrationTarget resolves the selected project, its publication
// identity, its workspace-relative project path, and its generated bundle
// directory.
func ResolveMigrationTarget(params map[string]any, args []string, workspaceRoot string) (*MigrationTarget, error) {
	clicore.AdoptPositionalApp(params, args)
	appName, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return nil, err
	}
	appDir, err := clicore.FindAppDir(workspaceRoot, appName)
	if err != nil {
		return nil, err
	}
	resolvedRoot := workspaceRoot
	if evaluated, resolveErr := filepath.EvalSymlinks(workspaceRoot); resolveErr == nil {
		resolvedRoot = evaluated
	}
	rel, err := filepath.Rel(resolvedRoot, appDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, clicore.NewError("migration project is outside the workspace", clicore.ExitUsage)
	}
	// projectPath is the plan's project ID, which drops grouping folders such
	// as "(web)"; relPath stays the physical path the Put package derives from.
	projectPath, relPath := clicore.ProjectIDFromPath(filepath.ToSlash(rel)), filepath.ToSlash(rel)
	if rel == "." {
		relPath = ""
	}
	application := MigrationApplication(relPath, appName)
	bundleDir := clicore.StringParam(params, "bundle-from", "bundleFrom")
	if bundleDir == "" {
		bundleDir = resolveBundleDir(workspaceRoot, appDir, rel)
	}
	return &MigrationTarget{Name: appName, Application: application, AppDir: appDir, ProjectPath: projectPath, BundleDir: bundleDir}, nil
}

// MigrationApplication is the one rule that picks the application a project's
// migration is published under. The CLI publishes under it and the workspace
// probe names the release-set member's package from it, so the two cannot
// disagree. path is the workspace-relative project path without a leading "/"
// ("" for the workspace root); name is the project's manifest name.
//
// The path wins when it is canonical and its package ("sites/putnami.dev" ->
// "sites-putnami.dev") is a native Put package name. Otherwise the manifest
// name answers: the root has no path, and a path with, for example, an
// uppercase segment maps to no package. The name is what every project was
// published under before the path was, so Control finds it on both paths.
func MigrationApplication(path, name string) string {
	if path == "" || strings.TrimSpace(path) != path || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") ||
		strings.Contains(path, "..") || strings.Contains(path, "//") ||
		!migrationAddressPartPattern.MatchString(migrationPackageName(path)) {
		return name
	}
	return path
}

// migrationPackageName is the Put package an application publishes at.
func migrationPackageName(application string) string {
	return strings.ReplaceAll(application, "/", "-")
}

// migrationBundleDirName is the directory both bundle locations use.
const migrationBundleDirName = "migration-bundle"

// resolveBundleDir picks the directory the build left the migration bundle in.
//
// The build task that produces it (the framework's build~describe) stages it
// under its own declared command output, so a cache hit restores it. The copy
// the describe binary writes into <project>/.gen survives only when
// build~generate's snapshot happened to contain it — generate owns .gen and
// runs first — which is why a fully cached build can leave .gen without a
// bundle.
//
// The staged copy therefore wins when it carries a manifest, and .gen remains
// the fallback for a build produced before the framework staged it.
func resolveBundleDir(workspaceRoot, appDir, projectRel string) string {
	staged := filepath.Join(workspaceRoot, ".putnami", "out", projectRel, "build", migrationBundleDirName)
	if _, err := os.Stat(filepath.Join(staged, migrationBundleManifest)); err == nil {
		return staged
	}
	return filepath.Join(appDir, ".gen", migrationBundleDirName)
}

// MigrationBundleBuilt reports whether the last build of the project at
// projectPath (slash-separated, relative to workspaceRoot) produced a
// migration bundle, read from the same locations publish-migration reads.
// `putnami cloud env doctor` uses it to find a workload that ships
// migrations but declares no namespace to publish them under.
func MigrationBundleBuilt(workspaceRoot, projectPath string) bool {
	rel := filepath.FromSlash(projectPath)
	dir := resolveBundleDir(workspaceRoot, filepath.Join(workspaceRoot, rel), rel)
	info, err := os.Stat(filepath.Join(dir, migrationBundleManifest))
	return err == nil && info.Mode().IsRegular()
}

// BundleManifestPath is the generated manifest whose presence proves the
// build produced a migration bundle for this project.
func (t *MigrationTarget) BundleManifestPath() string {
	return filepath.Join(t.BundleDir, migrationBundleManifest)
}

// BundleMissing reports whether the build produced no bundle manifest.
func (t *MigrationTarget) BundleMissing() bool {
	_, err := os.Stat(t.BundleManifestPath())
	return os.IsNotExist(err)
}

// Coordinate returns the Put coordinate this project's migration publishes
// to: <namespace>/<Application with "/" replaced by "-">, so a nested project
// publishes at <namespace>/sites-putnami.dev. The namespace comes
// from --namespace or the project manifest. An empty result means the project
// declares no migration namespace, so it has no migration coordinate.
func (t *MigrationTarget) Coordinate(params map[string]any) (string, error) {
	namespace, err := t.namespace(params)
	if err != nil || namespace == "" {
		return "", err
	}
	return namespace + "/" + t.packageName(), nil
}

func (t *MigrationTarget) namespace(params map[string]any) (string, error) {
	if namespace := clicore.StringParam(params, "namespace"); namespace != "" {
		return namespace, nil
	}
	namespace, err := declaredMigrationNamespace(t.AppDir)
	if err != nil {
		return "", clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	return namespace, nil
}

func (t *MigrationTarget) packageName() string {
	return migrationPackageName(t.Application)
}

// PrepareMigration discovers and bounds one generated migration bundle. An
// absent bundle returns nil only for the generic publish task's --if-present
// mode; an existing bundle always requires an explicit authored namespace.
func PrepareMigration(params map[string]any, args []string, workspaceRoot string, ioctx clicore.IO) (*PreparedMigration, error) {
	target, err := ResolveMigrationTarget(params, args, workspaceRoot)
	if err != nil {
		return nil, err
	}
	return target.Prepare(params, ioctx)
}

// Prepare reads and bounds the target's generated bundle. See PrepareMigration.
func (t *MigrationTarget) Prepare(params map[string]any, ioctx clicore.IO) (*PreparedMigration, error) {
	appName, bundleDir := t.Name, t.BundleDir
	if _, err := os.Stat(t.BundleManifestPath()); err != nil {
		if os.IsNotExist(err) && clicore.Truthy(clicore.Param(params, "if-present", "ifPresent")) {
			clicore.WriteResult(map[string]any{"status": "skipped", "app": appName, "reason": "no migration bundle"}, params, ioctx,
				fmt.Sprintf("No migration bundle for %s; skipping migration publication.", appName))
			return nil, nil
		}
		return nil, clicore.NewError(fmt.Sprintf("no migration bundle at %s; run `putnami build %s` first", bundleDir, appName), clicore.ExitUsage)
	}
	namespace, err := t.namespace(params)
	if err != nil {
		return nil, err
	}
	if !migrationAddressPartPattern.MatchString(namespace) {
		return nil, clicore.NewError("migration publication requires an explicit canonical --namespace from the project option", clicore.ExitUsage)
	}
	packageName := t.packageName()
	if !migrationAddressPartPattern.MatchString(packageName) {
		return nil, clicore.NewError("migration application does not map to a canonical Put package", clicore.ExitUsage)
	}
	files, err := readMigrationFiles(bundleDir)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Protocol string `json:"protocol"`
		AppName  string `json:"appName"`
		Version  string `json:"version"`
		Digest   string `json:"digest"`
	}
	if err := json.Unmarshal(files[migrationBundleManifest], &manifest); err != nil {
		return nil, clicore.NewError("parse migration bundle manifest: "+err.Error(), clicore.ExitUsage)
	}
	// Name the field that disagrees. The bundle has several writers across
	// extensions and a CI run has no tree left to inspect afterwards, so a bare
	// "does not match" cannot be traced back to the writer that produced it.
	//
	// appName is compared with the manifest name, not the publication identity:
	// the build writes the manifest name, and the bundle digest covers it, so
	// the CLI cannot rewrite it. Data treats appName as a label and checks
	// neither name (see migrations.Publisher), so this local check is what
	// catches a writer that put another project's name into the bundle.
	switch {
	case manifest.Protocol != "migration-bundle.v1":
		return nil, clicore.NewError(fmt.Sprintf("migration bundle manifest identity does not match the selected application: protocol %q, want migration-bundle.v1 (%s)", manifest.Protocol, t.BundleManifestPath()), clicore.ExitUsage)
	case manifest.AppName != appName:
		return nil, clicore.NewError(fmt.Sprintf("migration bundle manifest identity does not match the selected application: appName %q, want %q (%s)", manifest.AppName, appName, t.BundleManifestPath()), clicore.ExitUsage)
	case !migrationBundleDigestPattern.MatchString(manifest.Digest):
		return nil, clicore.NewError(fmt.Sprintf("migration bundle manifest identity does not match the selected application: digest %q is not a bare sha256 hex (%s)", manifest.Digest, t.BundleManifestPath()), clicore.ExitUsage)
	}
	version := clicore.FirstString(clicore.StringParam(params, "bundle-version", "bundleVersion", "version"), manifest.Version, generatedVersion(t.AppDir))
	if version == "" || len(version) > 255 || strings.TrimSpace(version) != version || strings.ContainsRune(version, '\x00') {
		return nil, clicore.NewError("migration bundle has no canonical version; build the selected release or pass --bundle-version", clicore.ExitUsage)
	}
	return &PreparedMigration{
		Application: t.Application, ProjectPath: t.ProjectPath, Version: version,
		ExpectedCoordinate: namespace + "/" + packageName, BundleDigest: manifest.Digest,
		BundleDir: bundleDir, Files: files,
	}, nil
}

// PublishPreparedMigration performs the command's single call to data-api's
// generated client: data-api's contract declares a migrationRegistry
// named-header profile for this header, required alongside the
// workspace user profile on the same operation. The Put bearer still travels
// only in the dedicated header and never enters JSON, output, markers, or
// error text — it is bound once as a static credential on a client built for
// this single call, never logged or forwarded through ctx.
func PublishPreparedMigration(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, prepared *PreparedMigration, selection MigrationSelection, registryBearer string) (*PublishedMigration, error) {
	if prepared == nil {
		return nil, clicore.NewError("migration publication was not prepared", clicore.ExitUsage)
	}
	if selection.SourceRevision == "" || selection.SelectionFingerprint == "" || registryBearer == "" || strings.ContainsAny(registryBearer, " \t\r\n\x00") {
		return nil, clicore.NewError("migration publication requires exact release provenance and a bounded Put credential", clicore.ExitAuth)
	}
	if ioctx.Client == nil {
		ioctx.Client = http.DefaultClient
	}
	workspace, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, err
	}
	reqCtx := ioctx.Context
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(reqCtx, 5*time.Minute)
	defer cancel()

	binding := workspace.ServiceBinding()
	binding.Credentials[migrationRegistryCredentialProfile] = client.CredentialBinding{
		Source: client.CredentialSourceStatic,
		Value:  "Bearer " + registryBearer,
	}
	dataClient, err := clicore.NewServiceClient[dataapiclient.DataClient](dataapiclient.RegisterDataClient, binding)
	if err != nil {
		return nil, err
	}
	application, expectedCoordinate, version := prepared.Application, prepared.ExpectedCoordinate, prepared.Version
	sourceRevision, selectionFingerprint := selection.SourceRevision, selection.SelectionFingerprint
	remote, err := clicore.CallWithSession(reqCtx, workspace, func(callCtx context.Context) (*dataapiclient.AcceptedMigration, error) {
		return dataClient.CreateV1WorkspacesDataMigrationsPublish(callCtx, dataapiclient.CreateV1WorkspacesDataMigrationsPublishInput{
			Path: dataapiclient.CreateV1WorkspacesDataMigrationsPublishPath{Workspace: workspace.WorkspaceID},
			Body: dataapiclient.PublishRequest{
				Application:          &application,
				ExpectedCoordinate:   &expectedCoordinate,
				Files:                &prepared.Files,
				SelectionFingerprint: &selectionFingerprint,
				SourceRevision:       &sourceRevision,
				Version:              &version,
			},
		})
	})
	if err != nil {
		return nil, mapPublishMigrationError(err)
	}
	accepted := publishedMigrationFrom(remote)
	if err := validateAcceptedMigration(&accepted, prepared, selection); err != nil {
		return nil, err
	}
	clicore.WriteResult(accepted, params, ioctx,
		fmt.Sprintf("Data accepted migration %s@%s (artifact %s).", accepted.Member.Coordinate, accepted.Member.Version, clicore.ShortDigest(accepted.ArtifactDigest)))
	return &accepted, nil
}

// publishedMigrationFrom projects the generated client's pointer-shaped
// AcceptedMigration onto the stable PublishedMigration/MigrationMember value
// shape --output and validateAcceptedMigration already share.
func publishedMigrationFrom(remote *dataapiclient.AcceptedMigration) PublishedMigration {
	member := clicore.Deref(remote.Member)
	return PublishedMigration{
		MigrationRef:   clicore.Deref(remote.MigrationRef),
		BundleDigest:   clicore.Deref(remote.BundleDigest),
		BlobDigest:     clicore.Deref(remote.BlobDigest),
		ArtifactDigest: clicore.Deref(remote.ArtifactDigest),
		Member: MigrationMember{
			Ecosystem:            clicore.Deref(member.Ecosystem),
			Coordinate:           clicore.Deref(member.Coordinate),
			Version:              clicore.Deref(member.Version),
			ArtifactDigest:       clicore.Deref(member.ArtifactDigest),
			SourceRevision:       clicore.Deref(member.SourceRevision),
			SelectionFingerprint: clicore.Deref(member.SelectionFingerprint),
		},
	}
}

// mapPublishMigrationError renders a failed publish call exactly as the
// hand-written POST it replaces did: a provider refusal reads "Data migration
// publication was refused (<status>)" with the same status-to-exit-code split
// (401/403 -> ExitAuth, 400/409 -> ExitUsage, else ExitAPI) exitForPublicationStatus
// always used; a response outside the declared contract reads "...returned an
// invalid response"; anything else (transport failure, cancellation) reads
// "...request failed", all ExitAPI, matching the plain http.Client path this
// replaces.
func mapPublishMigrationError(err error) error {
	if status := clicore.ServiceStatus(err); status != 0 {
		return clicore.NewError(fmt.Sprintf("Data migration publication was refused (%d)", status), exitForPublicationStatus(status))
	}
	if perrors.Is(err, client.CodeClientResponse) {
		return clicore.NewError("Data migration publication returned an invalid response", clicore.ExitAPI)
	}
	return clicore.NewError("Data migration publication request failed", clicore.ExitAPI)
}

// ReportMigrationDryRun emits the validated authored identity without reading
// credentials or making a network request.
func ReportMigrationDryRun(params map[string]any, ioctx clicore.IO, prepared *PreparedMigration, selection MigrationSelection) {
	clicore.WriteResult(map[string]any{
		"status": "dry-run", "application": prepared.Application, "coordinate": prepared.ExpectedCoordinate,
		"version": prepared.Version, "bundle_digest": prepared.BundleDigest,
		"source_revision": selection.SourceRevision, "selection_fingerprint": selection.SelectionFingerprint,
	}, params, ioctx, fmt.Sprintf("Dry run: Data migration %s@%s is ready for publication.", prepared.ExpectedCoordinate, prepared.Version))
}

func readMigrationFiles(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	total := 0
	err := fs.WalkDir(os.DirFS(dir), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if name == "." || !entry.Type().IsRegular() || !fs.ValidPath(name) {
			return fmt.Errorf("migration bundle contains unsupported entry %q", name)
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		total += len(data)
		if total > maxMigrationBundleBytes {
			return fmt.Errorf("migration bundle exceeds %d bytes", maxMigrationBundleBytes)
		}
		files[name] = data
		return nil
	})
	if err != nil {
		return nil, clicore.NewError("read migration bundle: "+err.Error(), clicore.ExitUsage)
	}
	return files, nil
}

// generatedVersion reads the canonical version the build generated for this
// PROJECT, the fallback when the bundle manifest carries none — a plain build
// writes `"version": null` and only a selected release fills it in.
//
// It is anchored on the project, not on the directory the bundle was read
// from. Those were the same place while the only bundle lived in
// <project>/.gen; they stopped being the same when the publisher started
// preferring the build's declared output (see resolveBundleDir), whose sibling
// VERSION file carries the WORKSPACE version — "0.0.0" on a plain build, not
// the build's canonical one. Reading .gen/version.json keeps one answer
// whichever copy of the bundle won.
func generatedVersion(appDir string) string {
	data, err := os.ReadFile(filepath.Join(appDir, ".gen", "version.json"))
	if err != nil {
		return ""
	}
	var document struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &document) != nil {
		return ""
	}
	return document.Version
}

func validateAcceptedMigration(accepted *PublishedMigration, prepared *PreparedMigration, selection MigrationSelection) error {
	if accepted == nil || accepted.MigrationRef == "" || accepted.Member.Ecosystem != "put" ||
		accepted.Member.Coordinate != prepared.ExpectedCoordinate || accepted.Member.Version != prepared.Version ||
		accepted.Member.SourceRevision != selection.SourceRevision || accepted.Member.SelectionFingerprint != selection.SelectionFingerprint ||
		accepted.Member.ArtifactDigest != accepted.ArtifactDigest || accepted.BundleDigest != prepared.BundleDigest ||
		!migrationBundleDigestPattern.MatchString(accepted.BundleDigest) || !migrationDigestPattern.MatchString(accepted.BlobDigest) ||
		!migrationDigestPattern.MatchString(accepted.ArtifactDigest) || "sha256:"+accepted.BundleDigest == accepted.BlobDigest ||
		"sha256:"+accepted.BundleDigest == accepted.ArtifactDigest || accepted.BlobDigest == accepted.ArtifactDigest {
		return clicore.NewError("Data migration acceptance differs from the selected immutable member", clicore.ExitAPI)
	}
	return nil
}

func exitForPublicationStatus(status int) int {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return clicore.ExitAuth
	}
	if status == http.StatusBadRequest || status == http.StatusConflict || status == http.StatusRequestEntityTooLarge {
		return clicore.ExitUsage
	}
	return clicore.ExitAPI
}
