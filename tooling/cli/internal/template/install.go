package template

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// Installer handles downloading, extracting, and verifying templates.
type Installer struct {
	// WorkspaceRoot is the workspace root directory.
	WorkspaceRoot string
	// ResolverURL is the base URL for the template resolver/registry.
	ResolverURL string
	// HTTPClient is the HTTP client for downloads. If nil, a default is used.
	HTTPClient *http.Client
}

// NewInstaller creates an Installer for the given workspace.
//
// The endpoint follows the same chain as `upgrade --cli` and every other
// archive download: the workspace's `registries.put.registry`, then
// PUTNAMI_REGISTRY_URL, then the default. A workspace that declares its own put
// registry must not have template downloads silently go somewhere else.
func NewInstaller(workspaceRoot string) *Installer {
	return &Installer{
		WorkspaceRoot: workspaceRoot,
		ResolverURL:   strings.TrimRight(extension.WorkspacePutRegistryURL(workspaceRoot), "/"),
		HTTPClient:    extension.NewRegistryHTTPClient(),
	}
}

// InstallResult is the outcome of installing a single template.
type InstallResult struct {
	Name         string
	Version      string
	Integrity    string
	Integrities  map[string]string
	ManifestHash string
	InstallDir   string
	Source       string
	FromCache    bool
	Changed      bool
}

// templateSpec describes templates to the shared artifact installer.
func templateSpec() extension.ArtifactSpec {
	return extension.ArtifactSpec{
		Kind:             layout.Templates,
		Label:            "template",
		ManifestFilename: ManifestFilename,
		// Raw read, not LoadManifest — same rule as extensionSpec: the probe
		// answers an IDENTITY question and must keep
		// answering for a manifest a future load-time gate refuses, or the
		// shared EnsureArtifactLocked machinery reinstalls forever. The
		// template loader has no gate today; the probe must not grow one by
		// accident.
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
		// Templates are platform-independent but the registry expects os/arch params.
		OS:             "linux",
		Arch:           "x64",
		ArchivePattern: "putnami-tpl-*.tar.gz",
		// Templates are much smaller than extensions.
		MaxArchiveSize: 100 * 1024 * 1024,
	}
}

// Install downloads and installs a single template to the version-qualified
// directory: .putnami/bin/artifacts/templates/{name}@{version}/
// and creates a stable symlink at .putnami/bin/templates/{name}.
// It runs the shared artifact install flow (registry download, trust ladder,
// extraction) parameterized for templates.
func (inst *Installer) Install(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry) (*InstallResult, error) {
	shared := &extension.Installer{
		WorkspaceRoot: inst.WorkspaceRoot,
		ResolverURL:   inst.ResolverURL,
		HTTPClient:    inst.HTTPClient,
	}
	res, err := shared.InstallArtifact(ctx, name, constraint, lockEntry, templateSpec())
	if err != nil {
		return nil, err
	}
	return &InstallResult{
		Name:         res.Name,
		Version:      res.Version,
		Integrity:    res.Integrity,
		Integrities:  res.Integrities,
		ManifestHash: res.ManifestHash,
		InstallDir:   res.InstallDir,
		Source:       res.Source,
		FromCache:    res.FromCache,
		Changed:      res.Changed,
	}, nil
}

func (inst *Installer) Resolve(ctx context.Context, name, constraint string) (*InstallResult, error) {
	shared := &extension.Installer{
		WorkspaceRoot: inst.WorkspaceRoot,
		ResolverURL:   inst.ResolverURL,
		HTTPClient:    inst.HTTPClient,
	}
	res, err := shared.ResolveArtifact(ctx, name, constraint, templateSpec())
	if err != nil {
		return nil, err
	}
	return &InstallResult{
		Name:      res.Name,
		Version:   res.Version,
		Integrity: res.Integrity,
		Source:    res.Source,
	}, nil
}

// Platform returns the "os/arch" lock key template downloads are bound to.
// Templates are platform-independent, so every host records the same key and a
// version bump has no foreign platforms to carry forward — the method exists so
// the extension and template installers stay interchangeable behind artifactOps.
func (inst *Installer) Platform() string {
	spec := templateSpec()
	return lockfile.PlatformKey(spec.OS, spec.Arch)
}

// ResolveIntegrityForPlatform returns the template archive digest the registry
// advertises for name@version on the given platform. See
// extension.Installer.ResolveArtifactIntegrity for the trust properties.
func (inst *Installer) ResolveIntegrityForPlatform(ctx context.Context, name, version, goos, goarch string) (string, error) {
	shared := &extension.Installer{
		WorkspaceRoot: inst.WorkspaceRoot,
		ResolverURL:   inst.ResolverURL,
		HTTPClient:    inst.HTTPClient,
	}
	spec := templateSpec()
	spec.OS, spec.Arch = goos, goarch
	return shared.ResolveArtifactIntegrity(ctx, name, version, spec)
}

// Ensure materializes a template from the lock if its stable symlink does not
// already resolve, reusing the shared artifact-ensure path (lock-read only;
// downloads only when the machine-global store lacks the pinned digest).
func (inst *Installer) Ensure(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry) error {
	shared := &extension.Installer{
		WorkspaceRoot: inst.WorkspaceRoot,
		ResolverURL:   inst.ResolverURL,
		HTTPClient:    inst.HTTPClient,
	}
	return shared.EnsureArtifact(ctx, name, constraint, lockEntry, templateSpec())
}

// Remove removes an installed template by deleting only its per-worktree stable
// symlink. The version argument is ignored: the machine-global store is shared,
// so reclaiming bytes is the artifact GC's job, not remove's.
func (inst *Installer) Remove(name, _ string) error {
	return layout.UnlinkArtifact(inst.WorkspaceRoot, layout.Templates, name)
}

// InstalledVersions returns the list of installed versions for a template.
func (inst *Installer) InstalledVersions(name string) ([]string, error) {
	return layout.ListVersions(inst.WorkspaceRoot, layout.Templates, name)
}
