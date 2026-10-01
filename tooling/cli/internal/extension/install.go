package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/store"
)

// Installer handles downloading, extracting, and verifying extensions.
type Installer struct {
	// WorkspaceRoot is the workspace root directory.
	WorkspaceRoot string
	// ResolverURL is the base URL for the extension resolver/registry.
	ResolverURL string
	// HTTPClient is the HTTP client for downloads. If nil, a default is used.
	HTTPClient *http.Client
	// AnonymousRegistry skips provider credential discovery. Bounded automatic
	// reads set it because the credential seam may mint an ephemeral Cloud key;
	// a private cold miss must fall back locally instead of causing that mutation.
	AnonymousRegistry bool
	// RequireVerified refuses the contained per-worktree fallback used by an
	// explicit unsafe install. Automatic reads set it because only lock-verified
	// bytes may become a live analysis surface.
	RequireVerified bool
	// Target selects a platform and/or an artifact-store root other than this
	// machine's. Its zero value is the historical behavior: host platform,
	// machine-global store, worktree symlink repointed.
	Target ArtifactTarget
}

// ArtifactTarget parameterizes an install for a platform and/or an
// artifact-store root other than this machine's — the `--platform` / `--dest`
// surface of `putnami extensions install`.
//
// The zero value means "install for this machine", which is what every
// pre-existing caller gets. A non-zero value turns the install into a
// MATERIALIZATION: the goal stops being "make this workspace able to run the
// artifact" and becomes "produce a tree some other machine will run", which is
// a different set of guarantees (see Materializes).
type ArtifactTarget struct {
	// OS and Arch override the platform artifacts are fetched and verified for.
	// Empty means the host's (runtime.GOOS / runtime.GOARCH).
	OS, Arch string
	// StoreRoot overrides the machine-global content-addressed store root, so
	// the materialized tree lands somewhere a packaging step can pick it up
	// without reaching into $HOME. Empty resolves the machine-global root.
	StoreRoot string
}

// Materializes reports whether this target produces a PACKAGEABLE tree rather
// than an installation this machine will execute — a foreign platform, or an
// artifact store outside the machine-global one.
//
// Everything that makes a materialization different hangs off this one
// predicate, so the two modes cannot drift apart:
//
//   - the per-worktree stable symlink is NOT repointed. Pointing
//     .putnami/bin/extensions/<name> at a linux tree would make the developer's
//     next `putnami build` exec a foreign binary; pointing it into a throwaway
//     --dest directory would dangle the moment that directory is packaged away.
//   - the extracted tree is NORMALIZED (artifactstore.NormalizeTree), because
//     the caller hashes it into a content key.
//   - bytes that cannot be verified are a hard ERROR instead of the unverified
//     per-worktree fallback. A per-worktree install is contained; a
//     CAS-shaped tree baked into an image is exactly the machine-wide
//     poisoning the verify-before-admit invariant exists to prevent.
func (t ArtifactTarget) Materializes() bool {
	if t.StoreRoot != "" {
		return true
	}
	if t.OS != "" && t.OS != runtime.GOOS {
		return true
	}
	if t.Arch != "" && t.Arch != runtime.GOARCH {
		return true
	}
	return false
}

// apply binds the target onto a kind's base spec. A target that names no
// platform keeps the kind's own (templates are platform-independent and pin a
// fixed pair), so `--dest` alone never silently re-points a template download.
func (t ArtifactTarget) apply(spec ArtifactSpec) ArtifactSpec {
	if t.OS != "" {
		spec.OS = t.OS
	}
	if t.Arch != "" {
		spec.Arch = t.Arch
	}
	spec.Materialize = t.Materializes()
	return spec
}

// NewInstaller creates an Installer for the given workspace. The registry is the
// put projection the workspace declares, so a workspace served by its own
// registry needs no per-machine environment variable.
func NewInstaller(workspaceRoot string) *Installer {
	return &Installer{
		WorkspaceRoot: workspaceRoot,
		ResolverURL:   WorkspacePutRegistryURL(workspaceRoot),
		HTTPClient:    NewRegistryHTTPClient(),
	}
}

// artifactStore returns the content-addressed artifact store this installer
// writes to: Target.StoreRoot when a materialization named one, otherwise the
// machine-global store for this installer's workspace. It is built from
// WorkspaceRoot (not held as a struct field) so it works whether the Installer
// came from NewInstaller or was constructed directly — as the template
// installer does.
func (inst *Installer) artifactStore() *artifactstore.Store {
	if inst.Target.StoreRoot != "" {
		return artifactstore.New(inst.Target.StoreRoot)
	}
	return artifactstore.New(store.ResolveArtifactStoreRoot(inst.WorkspaceRoot))
}

// FinalizeMaterialization strips the host-local bookkeeping from a --dest store
// root and normalizes what remains, so the produced tree is a drop-in
// artifact-store root that hashes identically across machines and runs. It is a
// no-op unless Target names a store root: the machine-global store keeps its
// locks and recency sidecars, which it needs to stay a live store.
func (inst *Installer) FinalizeMaterialization() error {
	if inst.Target.StoreRoot == "" {
		return nil
	}
	return inst.artifactStore().Normalize()
}

// InstallResult is the outcome of installing a single extension.
type InstallResult struct {
	Name      string
	Version   string
	Integrity string
	// Integrities carries the archive digest for the platform this install ran
	// on, keyed by "os/arch", so the caller can merge it into the lock's
	// per-platform map.
	Integrities  map[string]string
	ManifestHash string
	InstallDir   string
	Source       string
	FromCache    bool
	// Changed reports whether this install changed the workspace's stable link
	// or materialized new bytes. A warm store hit whose link already points at
	// the pinned digest is therefore an observable no-op.
	Changed bool
}

// ArtifactSpec parameterizes the shared install flow over the artifact kind
// being installed (extensions and templates share the registry protocol,
// trust ladder, and on-disk layout conventions).
type ArtifactSpec struct {
	// Kind selects the layout directory family (layout.Extensions or
	// layout.Templates).
	Kind layout.Kind
	// Label names the artifact kind in logs ("extension" or "template").
	Label string
	// ManifestFilename is the manifest the extracted archive must contain.
	ManifestFilename string
	// ManifestVersion reads the version from an extracted manifest, consulted
	// when the resolver does not advertise a resolved version. Returns ""
	// when the version cannot be determined.
	//
	// INVARIANT: implementations must read the RAW
	// document, never a contract-gated loader. This answers an identity
	// question — what is materialized on disk — and it feeds
	// lockedArtifactMatches, so a probe that refuses what a loader refuses
	// turns every gated-but-pinned artifact into a reinstall-forever wedge.
	ManifestVersion func(manifestPath string) string
	// OS and Arch are the platform parameters sent to the registry.
	OS, Arch string
	// Materialize marks this install as a packaging materialization rather than
	// an installation this machine will execute. It is derived from
	// ArtifactTarget.Materializes, whose doc comment owns the list of behaviors
	// it switches; nothing else may set it.
	Materialize bool
	// ArchivePattern is the temp-file pattern for the downloaded archive.
	ArchivePattern string
	// MaxArchiveSize caps the download size in bytes to prevent disk
	// exhaustion from malicious archives.
	MaxArchiveSize int64
}

func extensionSpec() ArtifactSpec {
	return ArtifactSpec{
		Kind:             layout.Extensions,
		Label:            "extension",
		ManifestFilename: "putnami.extension.json",
		// The version probe reads the raw document, NOT LoadManifest: the
		// contract ladder decides whether an extension may RUN, while this
		// answers what version is materialized on disk. Probing through the
		// gated loader made a contract-2 pinned artifact report "" — so
		// lockedArtifactMatches mismatched, EnsureArtifactLocked unlinked the
		// stable dir and reinstalled forever, and discovery lost the skip
		// record that carries the re-package remedy. Identity questions must stay answerable for artifacts the
		// loader refuses.
		ManifestVersion: func(manifestPath string) string {
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				return ""
			}
			var m struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal(data, &m); err != nil {
				return ""
			}
			return m.Version
		},
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		ArchivePattern: "putnami-ext-*.tar.gz",
		MaxArchiveSize: 500 * 1024 * 1024,
	}
}

// targetSpec is the extension spec this installer actually installs with: the
// kind's base spec with Target applied. Every install/resolve entry point goes
// through it so a target can never be honored on one path and ignored on
// another.
func (inst *Installer) targetSpec() ArtifactSpec {
	return inst.Target.apply(extensionSpec())
}

// Install downloads and installs a single extension to the version-qualified
// directory: .putnami/bin/artifacts/extensions/{name}@{version}/
// and creates a stable symlink at .putnami/bin/extensions/{name}.
func (inst *Installer) Install(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry) (*InstallResult, error) {
	return inst.InstallArtifact(ctx, name, constraint, lockEntry, inst.targetSpec())
}

func (inst *Installer) Resolve(ctx context.Context, name, constraint string) (*InstallResult, error) {
	return inst.ResolveArtifact(ctx, name, constraint, inst.targetSpec())
}

// Platform returns the "os/arch" lock key this installer's downloads are bound
// to — the only platform whose digest may come from locally hashed bytes. With
// a Target it is the TARGET platform, because that is the platform whose
// digests these downloads can be checked against.
func (inst *Installer) Platform() string {
	spec := inst.targetSpec()
	return lockfile.PlatformKey(spec.OS, spec.Arch)
}

// ResolveIntegrityForPlatform returns the extension archive digest the registry
// advertises for name@version on the given platform, which may differ from the
// host's. See ResolveArtifactIntegrity for the trust properties of the result.
func (inst *Installer) ResolveIntegrityForPlatform(ctx context.Context, name, version, goos, goarch string) (string, error) {
	spec := extensionSpec()
	spec.OS, spec.Arch = goos, goarch
	return inst.ResolveArtifactIntegrity(ctx, name, version, spec)
}

// InstallArtifact resolves a single artifact of the kind described by spec and
// points a per-worktree stable symlink at it.
//
// Verified artifacts live in the machine-global, content-addressed store keyed
// by the archive's SHA-256 — the digest the lock already records — so when the
// lock pins this platform's digest and a sibling worktree or repo has already
// warmed the store, the install is a symlink swap with no download. Freshly
// downloaded archives are verified BEFORE they are admitted to the shared store.
// An install whose binary bytes cannot be verified (an unsafe install, or a
// cross-platform lock with no digest for this os/arch) is kept per-worktree so
// it can never poison the namespace every repo on the machine trusts.
func (inst *Installer) InstallArtifact(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry, spec ArtifactSpec) (*InstallResult, error) {
	// Fast path: the lock pins this platform's archive digest and the global
	// store already holds it — relink, no download.
	if lockEntry != nil {
		if res := inst.linkFromGlobalStore(ctx, name, lockEntry, spec); res != nil {
			return res, nil
		}
	}

	// Lock-first: when the lock pins a concrete version, install that exact
	// version from a version-pinned channel instead of re-resolving the
	// workspace constraint (e.g. "latest"), which would otherwise drift the
	// moment the channel moves ahead of the lock — breaking installs even on
	// the platform that generated it. Only `update`/`--latest`, which
	// pass a nil lock entry, advance the pin.
	downloadChannel := constraint
	if lockEntry != nil && isPinnableVersion(lockEntry.Version) {
		downloadChannel = lockEntry.Version
	}

	// Not wrapped with the artifact name: every error `download` returns already
	// carries it, and this frame used to produce "download X: download X at ..."
	// once the fetch failure started naming its own pin.
	archivePath, resolvedVersion, advertisedIntegrity, err := inst.download(ctx, name, downloadChannel, spec)
	if err != nil {
		return nil, err
	}
	defer os.Remove(archivePath)

	integrity, err := HashFileContext(ctx, archivePath)
	if err != nil {
		return nil, fmt.Errorf("hash archive: %w", err)
	}

	// Verify the downloaded archive bytes before extracting them. `verified`
	// reports whether the bytes were checked against a trusted digest (a lock or
	// resolver-advertised hash) — only verified bytes may enter the shared store.
	verified, err := verifyArchiveIntegrity(lockEntry, spec, name, resolvedVersion, integrity, advertisedIntegrity)
	if err != nil {
		return nil, err
	}

	if verified {
		return inst.installShared(ctx, name, resolvedVersion, integrity, constraint, lockEntry, spec, archivePath)
	}
	if inst.RequireVerified {
		return nil, fmt.Errorf("cannot prepare %s@%s: archive bytes are not verified for %s/%s", name, resolvedVersion, spec.OS, spec.Arch)
	}
	// A materialization has no per-worktree fallback: its output is a
	// CAS-shaped tree someone else will run, so unverified bytes would be
	// laundered into a shape every consumer trusts. Fail loudly instead.
	if spec.Materialize {
		return nil, unverifiableMaterialization(spec, name, resolvedVersion)
	}
	return inst.installPerWorktree(name, resolvedVersion, constraint, lockEntry, spec, archivePath)
}

// unverifiableMaterialization is the fail-closed error for a materialization
// whose archive bytes could not be checked against a trusted digest: the lock
// records no integrity for the requested platform and the registry advertised
// none. PUTNAMI_UNSAFE_INSTALL is deliberately not honored here — it exists to
// unblock ONE machine's own install, not to sign off on bytes that get baked
// into an image every consumer of that image then trusts.
func unverifiableMaterialization(spec ArtifactSpec, name, version string) error {
	return fmt.Errorf(
		"cannot materialize %s@%s for %s/%s: the archive bytes could not be verified — "+
			"%s records no integrity for %s/%s and the registry advertised none; "+
			"run `putnami %ss update` (or an install on %s/%s) to record that platform's digest. "+
			"Refusing to package unverified bytes; %s does not apply to a materialization",
		name, version, spec.OS, spec.Arch,
		lockfile.LockFilename, spec.OS, spec.Arch,
		spec.Label, spec.OS, spec.Arch,
		UnsafeInstallEnv,
	)
}

// linkFromGlobalStore is the zero-download fast path: when the lock pins this
// platform's archive digest and the global store already holds it, re-point the
// stable symlink and return. It returns nil to fall through to a download when
// the digest is absent for this platform or the cached copy fails its manifest
// re-check (defense in depth across the shared, cross-repo store).
func (inst *Installer) linkFromGlobalStore(ctx context.Context, name string, lockEntry *lockfile.LockEntry, spec ArtifactSpec) *InstallResult {
	digest := lockEntry.IntegrityFor(spec.OS, spec.Arch)
	if digest == "" {
		return nil
	}
	as := inst.artifactStore()
	var result *InstallResult
	// Hold the store's shared lock across Has → manifest re-check → link → Touch
	// so a concurrent (cross-worktree) GC, which evicts under the exclusive lock,
	// cannot reap dir in the window between Has and the symlink swap and leave a
	// dangling link. Sibling admits/links coexist (also shared).
	withArtifactStoreShared(ctx, as, func() {
		if !as.Has(digest) {
			return
		}
		dir := as.Path(digest)
		// The digest path guarantees byte-identity only under the verify-before-admit
		// invariant; re-bind to this repo's lock via the cheap manifest hash before
		// trusting a copy a neighbor repo admitted.
		if lockEntry.ManifestHash != "" {
			got, err := HashFile(filepath.Join(dir, spec.ManifestFilename))
			if err != nil || got != lockEntry.ManifestHash {
				return // cached copy not trustworthy for this lock; re-download
			}
		}
		// A materialization never repoints the worktree's stable symlink: the
		// tree it resolves is for another platform, or lives in a --dest
		// directory that is about to be packaged away.
		changed := false
		if !spec.Materialize {
			var err error
			changed, err = linkArtifactGlobalIfChanged(inst.WorkspaceRoot, spec.Kind, name, dir)
			if err != nil {
				slog.Warn("failed to link cached "+spec.Label, spec.Label, name, "error", err)
				return
			}
		}
		as.Touch(digest)
		result = &InstallResult{
			Name:         name,
			Version:      lockEntry.Version,
			Integrity:    lockEntry.Integrity,
			Integrities:  lockEntry.Integrities,
			ManifestHash: lockEntry.ManifestHash,
			InstallDir:   dir,
			FromCache:    true,
			Changed:      changed,
		}
	})
	return result
}

// withArtifactStoreShared preserves the store's historical best-effort lock
// behavior for ordinary install calls while letting explicitly bounded read
// preparation fail closed at its deadline. A canceled context is also bounded:
// it must not mutate the stable link after its caller has stopped.
func withArtifactStoreShared(ctx context.Context, as *artifactstore.Store, fn func()) {
	if ctx != nil {
		if _, bounded := ctx.Deadline(); bounded || ctx.Err() != nil {
			_ = as.WithSharedContext(ctx, fn)
			return
		}
	}
	as.WithShared(fn)
}

// installShared admits a VERIFIED archive into the machine-global store keyed by
// its digest and links the worktree at it. Admit stages, the manifest binding is
// re-checked, and only then is the tree published, so the canonical digest path
// only ever holds verified bytes.
func (inst *Installer) installShared(ctx context.Context, name, resolvedVersion, integrity, constraint string, lockEntry *lockfile.LockEntry, spec ArtifactSpec, archivePath string) (*InstallResult, error) {
	as := inst.artifactStore()
	dir, err := as.AdmitContext(ctx, integrity, func(stageDir string) error {
		if err := ExtractTarGzContext(ctx, archivePath, stageDir); err != nil {
			return err
		}
		// Normalize INSIDE the stage function so the tree Admit publishes is
		// already reproducible — the rename is atomic, so no consumer can ever
		// observe a half-normalized entry. Contents are untouched, so the
		// manifest hash validated next (and re-validated on every cached link)
		// is unaffected, and the digest keyed here is the ARCHIVE's, not the
		// tree's, so normalization cannot invalidate a recorded lock entry.
		if spec.Materialize {
			if err := artifactstore.NormalizeTree(stageDir); err != nil {
				return err
			}
		}
		_, _, err := validateArtifactManifestContext(ctx, name, resolvedVersion, lockEntry, spec, stageDir)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("admit %s: %w", name, err)
	}

	// Re-check the published tree too: Admit may have short-circuited to a copy
	// a sibling already published for the same digest.
	resolvedVersion, manifestHash, err := validateArtifactManifestContext(ctx, name, resolvedVersion, lockEntry, spec, dir)
	if err != nil {
		return nil, err
	}

	// See linkFromGlobalStore: a materialization leaves the worktree's stable
	// symlink exactly where it was.
	if !spec.Materialize {
		if err := layout.LinkArtifactGlobal(inst.WorkspaceRoot, spec.Kind, name, dir); err != nil {
			slog.Warn("failed to create stable symlink", spec.Label, name, "error", err)
		}
	}
	as.Touch(integrity)

	return &InstallResult{
		Name:         name,
		Version:      resolvedVersion,
		Integrity:    integrity,
		Integrities:  map[string]string{lockfile.PlatformKey(spec.OS, spec.Arch): integrity},
		ManifestHash: manifestHash,
		InstallDir:   dir,
		Source:       inst.sourceURL(name, resolvedVersion, constraint),
		Changed:      true,
	}, nil
}

// linkArtifactGlobalIfChanged avoids replacing an already-correct link, which
// lets install reporting distinguish a cache check from an actual restoration.
// LinkArtifactGlobal retains the atomic temp-link + rename for the changed path.
func linkArtifactGlobalIfChanged(wsRoot string, kind layout.Kind, name, target string) (bool, error) {
	current, err := os.Readlink(layout.StableDir(wsRoot, kind, name))
	if err == nil && filepath.Clean(current) == filepath.Clean(target) {
		return false, nil
	}
	if err := layout.LinkArtifactGlobal(wsRoot, kind, name, target); err != nil {
		return false, err
	}
	return true, nil
}

// installPerWorktree handles the UNVERIFIED archive: an unsafe install, or a
// cross-platform lock with no digest for this os/arch and no resolver-advertised
// integrity. The binary bytes could not be checked, so the artifact stays in the
// per-worktree layout and is NEVER admitted to the shared store, and the result
// carries no archive digest — so an unverified install can never be promoted to
// a trusted, shared entry on a later run.
func (inst *Installer) installPerWorktree(name, resolvedVersion, constraint string, lockEntry *lockfile.LockEntry, spec ArtifactSpec, archivePath string) (*InstallResult, error) {
	installDir := layout.ArtifactDir(inst.WorkspaceRoot, spec.Kind, name, resolvedVersion)
	if err := os.RemoveAll(installDir); err != nil {
		return nil, fmt.Errorf("clean install dir: %w", err)
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return nil, fmt.Errorf("create install dir: %w", err)
	}
	if err := ExtractTarGz(archivePath, installDir); err != nil {
		os.RemoveAll(installDir)
		return nil, fmt.Errorf("extract %s: %w", name, err)
	}

	manifestPath := filepath.Join(installDir, spec.ManifestFilename)
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		os.RemoveAll(installDir)
		return nil, fmt.Errorf("extracted archive for %s has no %s", name, spec.ManifestFilename)
	}
	// Per-worktree dirs ARE version-named, so relocate when the resolver could
	// not name the version.
	if resolvedVersion == "0.0.0" {
		if v := spec.ManifestVersion(manifestPath); v != "" {
			correctDir := layout.ArtifactDir(inst.WorkspaceRoot, spec.Kind, name, v)
			if correctDir != installDir {
				os.RemoveAll(correctDir)
				if err := os.Rename(installDir, correctDir); err == nil {
					installDir = correctDir
					resolvedVersion = v
					manifestPath = filepath.Join(installDir, spec.ManifestFilename)
				}
			}
		}
	}

	manifestHash, err := HashFile(manifestPath)
	if err != nil {
		os.RemoveAll(installDir)
		return nil, fmt.Errorf("hash manifest for %s: %w", name, err)
	}
	if lockEntry != nil && lockEntry.ManifestHash != "" && manifestHash != lockEntry.ManifestHash {
		os.RemoveAll(installDir)
		return nil, fmt.Errorf(
			"manifest integrity mismatch for %s@%s: expected %s, got %s; run `putnami %ss update` to refresh the lock",
			name, resolvedVersion, lockEntry.ManifestHash, manifestHash, spec.Label,
		)
	}

	if err := layout.LinkArtifact(inst.WorkspaceRoot, spec.Kind, name, resolvedVersion); err != nil {
		slog.Warn("failed to create stable symlink", spec.Label, name, "error", err)
	}

	// Integrity and Integrities are intentionally empty: the bytes were not
	// verified, so no archive digest is pinned. This keeps the artifact
	// per-worktree on every future install too, rather than laundering
	// unverified bytes into the shared store on a later run.
	return &InstallResult{
		Name:         name,
		Version:      resolvedVersion,
		ManifestHash: manifestHash,
		InstallDir:   installDir,
		Source:       inst.sourceURL(name, resolvedVersion, constraint),
		Changed:      true,
	}, nil
}

func validateArtifactManifestContext(ctx context.Context, name, resolvedVersion string, lockEntry *lockfile.LockEntry, spec ArtifactSpec, dir string) (string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	manifestPath := filepath.Join(dir, spec.ManifestFilename)
	if _, err := os.Stat(manifestPath); err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("extracted archive for %s has no %s", name, spec.ManifestFilename)
		}
		return "", "", fmt.Errorf("stat manifest for %s: %w", name, err)
	}
	if resolvedVersion == "0.0.0" {
		if v := spec.ManifestVersion(manifestPath); v != "" {
			resolvedVersion = v
		}
	}
	manifestHash, err := HashFileContext(ctx, manifestPath)
	if err != nil {
		return "", "", fmt.Errorf("hash manifest for %s: %w", name, err)
	}
	if lockEntry != nil && lockEntry.ManifestHash != "" && manifestHash != lockEntry.ManifestHash {
		return "", "", fmt.Errorf(
			"manifest integrity mismatch for %s@%s: expected %s, got %s; run `putnami %ss update` to refresh the lock",
			name, resolvedVersion, lockEntry.ManifestHash, manifestHash, spec.Label,
		)
	}
	return resolvedVersion, manifestHash, nil
}

// sourceURL builds the version-pinned download URL recorded in the lock so a
// committed lock reinstalls the exact resolved version rather than whatever the
// original constraint resolves to.
func (inst *Installer) sourceURL(name, resolvedVersion, constraint string) string {
	ns, pkg := RegistryRef(name)
	channel := resolvedVersion
	if !isPinnableVersion(channel) {
		channel = constraint
	}
	return fmt.Sprintf("%s/%s/%s/download?channel=%s", inst.ResolverURL, ns, pkg, url.QueryEscape(channel))
}

// isPinnableVersion reports whether v is a concrete version usable as a
// version-pinned download channel — not empty and not the "0.0.0" sentinel the
// resolver returns when it cannot determine a version.
func isPinnableVersion(v string) bool {
	return v != "" && v != "0.0.0"
}

// verifyArchiveIntegrity checks the downloaded archive bytes before extraction
// and reports whether they were VERIFIED against a trusted digest. Only verified
// bytes may be admitted to the machine-global shared store; an unverified
// (but not erroring) result keeps the artifact per-worktree so it cannot poison
// the namespace every repo trusts.
//
// Archives are per-os/arch, so a lock generated on one platform records only
// that platform's digest; the platform-independent manifest hash (enforced after
// extraction) is what lets one committed lock verify everywhere. Sources
// are tried in order of decreasing trust:
//  1. a per-platform digest recorded in the lock for THIS os/arch (verified),
//  2. a legacy single-digest lock that matches this platform's archive (verified),
//  3. the resolver-advertised integrity header (verified; trusts the CLI↔registry
//     channel but catches a MITM-substituted archive),
//  4. a cross-platform lock with no advertised integrity — the bytes can't be
//     checked here (UNVERIFIED): the manifest-hash binding still secures the
//     install, but it stays per-worktree,
//  5. PUTNAMI_UNSAFE_INSTALL=1 — explicit bypass (UNVERIFIED), per-worktree,
//  6. nothing — fail-closed.
//
// The UNVERIFIED outcomes (4 and 5) are only survivable because the artifact
// stays per-worktree. A materialization has no such containment, so
// InstallArtifact turns any unverified result into a hard error there rather
// than duplicating the ladder — see unverifiableMaterialization.
func verifyArchiveIntegrity(lockEntry *lockfile.LockEntry, spec ArtifactSpec, name, version, integrity, advertisedIntegrity string) (verified bool, err error) {
	platformDigest := ""
	if lockEntry != nil {
		platformDigest = lockEntry.IntegrityFor(spec.OS, spec.Arch)
	}
	switch {
	case platformDigest != "":
		if integrity != platformDigest {
			return false, integrityMismatch(spec, name, version, "expected", platformDigest, integrity)
		}
		return true, nil
	case lockEntry != nil && lockEntry.Integrity != "" && integrity == lockEntry.Integrity:
		// Legacy single-platform digest matches this platform's archive.
		return true, nil
	case lockEntry != nil && lockEntry.Integrity != "" && lockEntry.ManifestHash == "":
		// Legacy lock whose digest does not match and which carries no
		// platform-independent anchor: preserve the historical strict behavior.
		return false, integrityMismatch(spec, name, version, "expected", lockEntry.Integrity, integrity)
	case advertisedIntegrity != "":
		expected, nerr := NormalizeIntegrity(advertisedIntegrity)
		if nerr != nil {
			return false, fmt.Errorf("invalid advertised integrity for %s@%s: %w", name, version, nerr)
		}
		if !strings.EqualFold(integrity, expected) {
			return false, integrityMismatch(spec, name, version, "resolver advertised", expected, integrity)
		}
		return true, nil
	case lockEntry != nil && lockEntry.ManifestHash != "":
		// Cross-platform lock: no digest for this os/arch and the resolver did
		// not advertise one. The archive bytes cannot be verified here; the
		// manifest-hash binding secures the install, but because the binary bytes
		// are unverified they must NOT enter the shared store.
		return false, nil
	case os.Getenv(UnsafeInstallEnv) == "1":
		slog.Warn("installing "+spec.Label+" without integrity verification ("+UnsafeInstallEnv+"=1)",
			spec.Label, name, "version", version, "integrity", integrity)
		return false, nil
	default:
		return false, fmt.Errorf(
			"no integrity available for %s@%s (no lockfile entry, resolver did not advertise one); "+
				"refusing to install unverified bytes. Add a lockfile entry, or set %s=1 to override",
			name, version, UnsafeInstallEnv,
		)
	}
}

// integrityMismatch builds an integrity-mismatch error carrying a remediation
// hint that points at the update command which refreshes the lock.
// source is the human phrasing for where the expected digest came from
// ("expected" for the lock, "resolver advertised" for the header).
func integrityMismatch(spec ArtifactSpec, name, version, source, expected, got string) error {
	return fmt.Errorf(
		"integrity mismatch for %s@%s: %s %s, got %s; run `putnami %ss update` to refresh the lock (or set %s=1 to bypass)",
		name, version, source, expected, got, spec.Label, UnsafeInstallEnv,
	)
}

// Remove removes an installed extension by deleting only its per-worktree stable
// symlink. The version argument is ignored: the machine-global store is
// content-addressed and shared across worktrees and repos, so reclaiming bytes
// is the artifact GC's job, not remove's — deleting them here could break a
// sibling worktree mid-flight.
func (inst *Installer) Remove(name, _ string) error {
	return layout.UnlinkArtifact(inst.WorkspaceRoot, layout.Extensions, name)
}
