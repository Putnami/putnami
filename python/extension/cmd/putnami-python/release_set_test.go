package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/releaseset"
)

// releaseMembers decodes the release-set members of a probed project.
func releaseMembers(t *testing.T, metadata json.RawMessage) []releaseset.MemberDeclaration {
	t.Helper()
	if len(metadata) == 0 {
		return nil
	}
	var decoded map[string]releaseset.ProjectMetadata
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		t.Fatalf("metadata %s: %v", metadata, err)
	}
	return decoded[releaseset.ProjectMetadataKey].Ecosystems
}

func TestTheProbeDeclaresTheImageAsAnOCIReleaseSetMember(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "publishes-an-application-image", "the-probe-declares-the-image-as-an-oci-release-set-member")
	root := probeWorkspace(t)
	writeProbeFile(t, filepath.Join(root, "putnami.workspace.json"), `{"registries": {"oci": {"publish": "ghcr.io/acme/apps"}}}`)
	writeProbeFile(t, filepath.Join(root, "app", "putnami.json"), `{"name": "@acme/Shop_API", "options": {"publish": {"docker": true}}}`)
	writeProbeFile(t, filepath.Join(root, "lib", "putnami.json"), `{"type": "library", "publish": ["docker"]}`)

	result := probeAll(t, root, "app", "lib")
	app, _ := projectAt(result, "app")
	members := releaseMembers(t, app.Metadata)
	want := releaseset.MemberDeclaration{Ecosystem: ociEcosystem, Coordinate: "acme/apps/acme-shop_api", PackageStep: "docker", PublishStep: "docker"}
	if len(members) != 1 || members[0].Ecosystem != want.Ecosystem || members[0].Coordinate != want.Coordinate ||
		members[0].PackageStep != want.PackageStep || members[0].PublishStep != want.PublishStep {
		t.Errorf("app members = %+v, want %+v", members, want)
	}
	for _, watched := range []string{"app/putnami.json", "putnami.workspace.json"} {
		if !slices.Contains(app.WatchedFiles, watched) {
			t.Errorf("app watchedFiles = %v, want %s", app.WatchedFiles, watched)
		}
	}
	lib, _ := projectAt(result, "lib")
	if members := releaseMembers(t, lib.Metadata); len(members) != 0 {
		t.Errorf("a library declares %+v", members)
	}

	// A project's own oci registry replaces the workspace's.
	writeProbeFile(t, filepath.Join(root, "app", "putnami.json"), `{"publish": ["docker"], "registries": {"oci": {"publish": "registry.test/team"}}}`)
	app, _ = projectAt(probeAll(t, root, "app"), "app")
	if members := releaseMembers(t, app.Metadata); len(members) != 1 || members[0].Coordinate != "team/py_example_application" {
		t.Errorf("members with a project registry = %+v", members)
	}
}

func TestAnApplicationThatPublishesNoImageIsNoReleaseSetMember(t *testing.T) {
	root := probeWorkspace(t)
	app, _ := projectAt(probeAll(t, root, "app"), "app")
	if app.Metadata != nil {
		t.Errorf("a project with no putnami.json has metadata %s", app.Metadata)
	}
	writeProbeFile(t, filepath.Join(root, "app", "putnami.json"), `{"publish": ["archives"]}`)
	app, _ = projectAt(probeAll(t, root, "app"), "app")
	if app.Metadata != nil {
		t.Errorf("an application that publishes no image has metadata %s", app.Metadata)
	}
}
