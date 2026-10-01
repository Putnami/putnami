package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
)

func TestProbeDeclaresOptedInImageWithoutGoModule(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-ecosystem-profile", "probe-declares-image-member-without-go-module")
	for _, tc := range []struct {
		name       string
		workspace  string
		project    string
		scope      string
		coordinate string
	}{
		{"native runner", `{"name":"team","registries":{"oci":{"publish":"oci.putnami.dev/team"}}}`, `{"name":"delivery/images/ci-runner","type":"image","publish":["docker"]}`, "", "team/delivery-images-ci-runner"},
		{"managed default", `{"name":"@Team/Images"}`, `{"name":"@Team/Runner","type":"image","publish":["docker"]}`, "", "team-images/team-runner"},
		{"managed bare host", `{"name":"team","registries":{"oci":{"publish":"oci.putnami.dev"}}}`, `{"name":"runner","type":"image","publish":["docker"]}`, "", "team/runner"},
		{"publish option", `{"name":"team"}`, `{"name":"runner","type":"image","options":{"publish":{"docker":true}}}`, "", "team/runner"},
		{"basename", `{"name":"team"}`, `{"type":"image","publish":["docker"]}`, "", "team/runner"},
		{"scope name", `{"name":"team"}`, `{"type":"image","publish":["docker"]}`, `{"publishConfig":{"docker":{"namePattern":"@Team/{name}"}}}`, "team/team-runner"},
		{"authored name precedes scope", `{"name":"team"}`, `{"name":"authored","type":"image","publish":["docker"]}`, `{"publishConfig":{"docker":{"namePattern":"@Team/{name}"}}}`, "team/authored"},
		{"external workspace destination", `{"name":"team","registries":{"oci":{"publish":"ghcr.io/acme/images"}}}`, `{"name":"@Team/Runner","type":"image","publish":["docker"]}`, "", "acme/images/team-runner"},
		{"project external override", `{"name":"team","registries":{"oci":{"publish":"oci.putnami.dev/team"}}}`, `{"name":"@Team/Runner","type":"image","publish":["docker"],"registries":{"oci":{"publish":"ghcr.io/acme/images"}}}`, "", "acme/images/team-runner"},
		{"managed declared namespace", `{"name":"acme-platform","registries":{"oci":{"publish":"oci.putnami.dev/putnami"}}}`, `{"name":"delivery/images/ci-runner","type":"image","publish":["docker"]}`, "", "putnami/delivery-images-ci-runner"},
		{"managed project namespace override", `{"name":"team","registries":{"oci":{"publish":"oci.putnami.dev/team"}}}`, `{"name":"runner","type":"image","publish":["docker"],"registries":{"oci":{"publish":"oci.putnami.dev/images"}}}`, "", "images/runner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			const projectPath = "images/runner"
			writeProbeFile(t, filepath.Join(root, goWorkspaceConfigFile), tc.workspace)
			writeProbeFile(t, filepath.Join(root, projectPath, goProjectConfigFile), tc.project)
			if tc.scope != "" {
				writeProbeFile(t, filepath.Join(root, "images", goProjectConfigFile), tc.scope)
			}
			result, err := probeGoWorkspace(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: goExtensionName, Paths: []string{projectPath}})
			if err != nil || len(result.Projects) != 1 || len(result.Diagnostics) != 0 {
				t.Fatalf("image-only probe = %+v, %v", result, err)
			}
			project := result.Projects[0]
			if project.Type != goImageProjectType || project.SourceName != "" || project.SourceFile != projectPath+"/putnami.json" || len(project.Dependencies) != 0 {
				t.Fatalf("image was given a fabricated Go identity or dependency: %+v", project)
			}
			metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{goExtensionName: project.Metadata})
			if err != nil || !found || len(metadata.Ecosystems) != 1 {
				t.Fatalf("image metadata = %+v, found=%v, err=%v", metadata, found, err)
			}
			member := metadata.Ecosystems[0]
			if member.Ecosystem != "oci" || member.Coordinate != tc.coordinate || member.PackageStep != "image" || member.PublishStep != "docker" {
				t.Fatalf("member = %+v, want OCI %s via image/docker", member, tc.coordinate)
			}
			for _, input := range []string{goWorkspaceConfigFile, projectPath + "/putnami.json", projectPath + "/go.mod"} {
				if !slices.Contains(project.WatchedFiles, input) {
					t.Fatalf("probe omitted identity input %s", input)
				}
			}
			if tc.scope != "" && !slices.Contains(project.WatchedFiles, "images/putnami.json") {
				t.Fatal("scope-derived image name lacks invalidation input")
			}
		})
	}
}

func TestProbeDoesNotInventImagePublicationWithoutOptIn(t *testing.T) {
	for _, document := range []string{
		`{"name":"runner","type":"image"}`,
		`{"name":"runner","type":"image","publish":["go"]}`,
		`{"name":"runner","type":"image","options":{"package":{"docker":true}}}`,
		`{"name":"runner","type":"application","publish":["docker"]}`,
	} {
		t.Run(document, func(t *testing.T) {
			root := t.TempDir()
			writeProbeFile(t, filepath.Join(root, "runner", goProjectConfigFile), document)
			result, err := probeGoWorkspace(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: goExtensionName, Paths: []string{"runner"}})
			if err != nil || len(result.Projects) != 0 {
				t.Fatalf("unclaimed candidate = %+v, %v", result, err)
			}
		})
	}
}
