package ruby

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
)

// Component types the probe reports.
const (
	TypeApplication = "application"
	TypeLibrary     = "library"
)

// rackConfig is the file that makes a directory a Rack application.
const rackConfig = "config.ru"

// ociEcosystem is the release-set ecosystem of a container image.
const ociEcosystem = "oci"

// componentType is application for a component whose directory holds
// config.ru, and library otherwise.
func componentType(root, dir string) string {
	if isFile(filepath.Join(root, filepath.FromSlash(dir), rackConfig)) {
		return TypeApplication
	}
	return TypeLibrary
}

// projectDocument is the part of a putnami.json that says whether a project
// publishes an image, and under which name and registry.
type projectDocument struct {
	Name       string                     `json:"name"`
	Publish    []string                   `json:"publish"`
	Registries map[string]json.RawMessage `json:"registries"`
	Options    struct {
		Publish struct {
			Docker bool `json:"docker"`
		} `json:"publish"`
	} `json:"options"`
}

// imageMember answers the release-set member of an application at its own
// stack root that publishes an image. Its coordinate is the repository path
// the shared publisher reports: the namespace of registries.oci.publish, then
// the image name.
func imageMember(root, dir, sourceName, stackRoot, kind string) []releaseset.MemberDeclaration {
	if kind != TypeApplication || stackRoot != dir {
		return nil
	}
	var document projectDocument
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "putnami.json")) //nolint:gosec // the project's own config file
	if err != nil || json.Unmarshal(data, &document) != nil {
		return nil
	}
	if !document.Options.Publish.Docker && !slices.Contains(document.Publish, dockerChannel) {
		return nil
	}
	repository := imageName(resolvedProjectName(root, dir, sourceName, document.Name))
	if namespace := registryNamespace(root, document); namespace != "" {
		repository = namespace + "/" + repository
	}
	return []releaseset.MemberDeclaration{{
		Ecosystem:   ociEcosystem,
		Coordinate:  repository,
		PackageStep: dockerChannel,
		PublishStep: dockerChannel,
	}}
}

// resolvedProjectName is the name the workspace gives the project.
func resolvedProjectName(root, dir, sourceName, explicit string) string {
	base := path.Base(dir)
	scopeName := ""
	if scope, _ := wsproto.LoadScopeChainWithSources(root, filepath.Join(root, filepath.FromSlash(dir))); scope != nil {
		scopeName = scope.ResolveNamePattern(base)
	}
	return wsproto.ResolveProjectName(explicit, sourceName, scopeName, base)
}

// imageName derives an image name from a project name: an image name is one
// path segment.
func imageName(project string) string {
	return strings.ReplaceAll(strings.TrimPrefix(project, "@"), "/", "-")
}

// registryNamespace is the path of registries.oci.publish after its host. A
// project's own oci entry replaces the workspace's.
func registryNamespace(root string, document projectDocument) string {
	registries := document.Registries
	if _, overridden := registries[ociEcosystem]; !overridden {
		var workspace struct {
			Registries map[string]json.RawMessage `json:"registries"`
		}
		data, err := os.ReadFile(filepath.Join(root, "putnami.workspace.json")) //nolint:gosec // the workspace's own config file
		if err != nil || json.Unmarshal(data, &workspace) != nil {
			return ""
		}
		registries = workspace.Registries
	}
	var registry struct {
		Publish string `json:"publish"`
	}
	if json.Unmarshal(registries[ociEcosystem], &registry) != nil {
		return ""
	}
	_, namespace, _ := strings.Cut(strings.Trim(strings.TrimSpace(registry.Publish), "/"), "/")
	return namespace
}
