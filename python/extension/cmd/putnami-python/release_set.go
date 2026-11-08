package main

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/python/extension/internal/jobs"
	"go.putnami.dev/sdk/extension/releaseset"
)

const (
	// ociEcosystem is the release-set ecosystem of a container image.
	ociEcosystem = "oci"
	// pythonProjectConfigFile is the project document the release-set member
	// reads: whether the project is a library and whether it publishes an image.
	pythonProjectConfigFile = "putnami.json"
	// pythonWorkspaceConfigFile holds the registries an image member's
	// coordinate is read from.
	pythonWorkspaceConfigFile = "putnami.workspace.json"
)

// projectDocument is the part of a putnami.json that says whether a project
// publishes an image, and under which name and registry. Nothing else is
// decoded, so an unrelated key cannot change the answer.
type projectDocument struct {
	Name       string                     `json:"name"`
	Type       string                     `json:"type"`
	Publish    []string                   `json:"publish"`
	Registries map[string]json.RawMessage `json:"registries"`
	Options    struct {
		Publish struct {
			Docker bool `json:"docker"`
		} `json:"publish"`
	} `json:"options"`
}

// projectConfigPathFor is the repo-relative putnami.json of a candidate.
func projectConfigPathFor(candidate string) string {
	if candidate == "" || candidate == wsproto.ProbeRootPath {
		return pythonProjectConfigFile
	}
	return path.Join(candidate, pythonProjectConfigFile)
}

// imageMember answers the release-set metadata of an application that
// publishes an image, and the scope files its coordinate was read from. Its
// coordinate is the repository path the shared publisher reports: the
// namespace of registries.oci.publish, then the image name the package job
// derives. A library, and a project that publishes no image, answer nothing.
func imageMember(root string, project wsproto.ProbeProject) (json.RawMessage, []string) {
	var document projectDocument
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(projectConfigPathFor(project.Path)))) //nolint:gosec // the project's own config file
	if err != nil || json.Unmarshal(data, &document) != nil {
		return nil, nil
	}
	if document.Type == "library" || (!document.Options.Publish.Docker && !slices.Contains(document.Publish, jobs.DockerChannel)) {
		return nil, nil
	}
	name, scopeFiles := resolvedProjectName(root, project, document.Name)
	repository := jobs.ImageName(name)
	if namespace := registryNamespace(root, document); namespace != "" {
		repository = namespace + "/" + repository
	}
	encoded, err := json.Marshal(map[string]any{
		releaseset.ProjectMetadataKey: releaseset.ProjectMetadata{Ecosystems: []releaseset.MemberDeclaration{{
			Ecosystem:   ociEcosystem,
			Coordinate:  repository,
			PackageStep: jobs.DockerChannel,
			PublishStep: jobs.DockerChannel,
		}}},
	})
	if err != nil {
		return nil, nil
	}
	return encoded, scopeFiles
}

// resolvedProjectName is the name the workspace gives the project, and the
// scope files that name was read from.
func resolvedProjectName(root string, project wsproto.ProbeProject, explicit string) (string, []string) {
	base := path.Base(project.Path)
	scopeName := ""
	scope, scopeFiles := wsproto.LoadScopeChainWithSources(root, filepath.Join(root, filepath.FromSlash(project.Path)))
	if scope != nil {
		scopeName = scope.ResolveNamePattern(base)
	}
	return wsproto.ResolveProjectName(explicit, project.SourceName, scopeName, base), scopeFiles
}

// registryNamespace is the path of registries.oci.publish after its host. A
// project's own oci entry replaces the workspace's.
func registryNamespace(root string, document projectDocument) string {
	registries := document.Registries
	if _, overridden := registries[ociEcosystem]; !overridden {
		var workspace struct {
			Registries map[string]json.RawMessage `json:"registries"`
		}
		data, err := os.ReadFile(filepath.Join(root, pythonWorkspaceConfigFile)) //nolint:gosec // the workspace's own config file
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
