package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/releaseset"
)

// imageEcosystem is the release-set ecosystem of a container image, and the
// registries entry that names where it publishes.
const imageEcosystem = "oci"

// imageChannel is the channel, and the package and publish step, that ships an
// image.
const imageChannel = "docker"

// ImageName derives an image name from a project name: one lowercase path
// segment. The leading "@" of a scoped name goes and every "/" becomes "-". An
// OCI repository name has no uppercase letter, and the shared Docker publisher
// derives the project's managed repository by the same rule, so a project named
// userService packages, and publishes, the image userservice.
func ImageName(project string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(project, "@"), "/", "-"))
}

// imageDocument is the part of a putnami.json that says whether a project
// publishes an image, and under which name and registry.
type imageDocument struct {
	Name       string                     `json:"name"`
	Publish    []string                   `json:"publish"`
	Registries map[string]json.RawMessage `json:"registries"`
	Options    struct {
		Publish struct {
			Docker bool `json:"docker"`
		} `json:"publish"`
	} `json:"options"`
}

// ImageMember answers the release-set member of the component at dir when its
// putnami.json publishes an image: options.publish.docker, or docker among its
// publish channels. Its coordinate is the repository path the shared publisher
// reports: the namespace of registries.oci.publish, then the ImageName of the
// name the workspace gives the project. sourceName is the component's source
// name, as the probe reports it. The caller decides which components may
// publish an image at all, such as applications at their own stack root.
func ImageMember(root, dir, sourceName string) []releaseset.MemberDeclaration {
	var document imageDocument
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "putnami.json")) //nolint:gosec // the project's own config file
	if err != nil || json.Unmarshal(data, &document) != nil {
		return nil
	}
	if !document.Options.Publish.Docker && !slices.Contains(document.Publish, imageChannel) {
		return nil
	}
	repository := ImageName(projectName(root, dir, sourceName, document.Name))
	if namespace := imageNamespace(root, document); namespace != "" {
		repository = namespace + "/" + repository
	}
	return []releaseset.MemberDeclaration{{
		Ecosystem:   imageEcosystem,
		Coordinate:  repository,
		PackageStep: imageChannel,
		PublishStep: imageChannel,
	}}
}

// projectName is the name the workspace gives the project at dir: its explicit
// name, else its source name, else its scope's name pattern, else its
// directory's name.
func projectName(root, dir, sourceName, explicit string) string {
	base := path.Base(dir)
	scopeName := ""
	if scope, _ := wsproto.LoadScopeChainWithSources(root, filepath.Join(root, filepath.FromSlash(dir))); scope != nil {
		scopeName = scope.ResolveNamePattern(base)
	}
	return wsproto.ResolveProjectName(explicit, sourceName, scopeName, base)
}

// imageNamespace is the path of registries.oci.publish after its host. A
// project's own oci entry replaces the workspace's.
func imageNamespace(root string, document imageDocument) string {
	registries := document.Registries
	if _, overridden := registries[imageEcosystem]; !overridden {
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
	if json.Unmarshal(registries[imageEcosystem], &registry) != nil {
		return ""
	}
	_, namespace, _ := strings.Cut(strings.Trim(strings.TrimSpace(registry.Publish), "/"), "/")
	return namespace
}

// loadTimeout bounds each docker call of LoadImage.
const loadTimeout = 5 * time.Minute

// LoadImage loads the OCI layout into the local Docker daemon as tags[0], then
// tags it with each other tag. Each docker call runs at most loadTimeout, and
// its output goes to the job's log through emit, or to stderr when emit is nil.
func LoadImage(layout string, tags []string, emit *jsonl.Emitter) error {
	if len(tags) == 0 {
		return errors.New("no tag to load the image as")
	}
	archive, err := os.CreateTemp("", "putnami-image-*.tar")
	if err != nil {
		return err
	}
	_ = archive.Close()
	defer func() { _ = os.Remove(archive.Name()) }()
	if err := oci.WriteDockerTarball(layout, tags[0], archive.Name()); err != nil {
		return fmt.Errorf("write the docker tarball: %w", err)
	}
	docker := func(args ...string) error {
		timed, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		cmd := exec.CommandContext(timed, "docker", args...) //nolint:gosec // fixed verbs with the task's own tags
		if err := runAttached(cmd, emit); err != nil {
			return fmt.Errorf("docker %s %s: %w", args[0], tags[0], err)
		}
		return nil
	}
	if err := docker("load", "-i", archive.Name()); err != nil {
		return err
	}
	for _, alias := range tags[1:] {
		if err := docker("tag", tags[0], alias); err != nil {
			return err
		}
	}
	return nil
}
