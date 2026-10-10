package cloudcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	configcli "go.putnami.dev/cloud/extension/internal/configcli"
	datacli "go.putnami.dev/cloud/extension/internal/datacli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	distributionproto "go.putnami.dev/protocol/distribution"
	bundlepkg "go.putnami.dev/protocol/migration/bundle"
	workspaceproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
)

const (
	cloudProjectMarker       = "putnami.json"
	cloudExtensionManifest   = "putnami.extension.json"
	cloudGoModule            = "go.mod"
	cloudArchiveEcosystem    = distributionproto.Ecosystem("archive")
	cloudArchivePackageStep  = "archives"
	cloudArchivePublishStep  = "cloud-publish-archives"
	cloudArchivePackageOwner = "@putnami/go"
	// @putnami/scaffold's package-content task emits both archive content
	// forms, each under its own package step id: a project template, and a
	// content-only extension whose manifest declares agentContent. Its
	// extension manifest declares both ids. An extension with a Go module
	// stays with @putnami/go, whose archives step stages its agent content.
	cloudContentPackageOwner     = "@putnami/scaffold"
	cloudTemplatePackageStep     = "template"
	cloudAgentContentPackageStep = "agent-content"
	cloudMigrationEcosystem      = distributionproto.Ecosystem("put")
	cloudMigrationPublishStep    = "cloud-publish-migration"
	cloudConfigEcosystem         = distributionproto.Ecosystem("put")
	cloudConfigPackageStep       = "cloud-config-member"
	cloudConfigPublishStep       = "cloud-publish-config"
	cloudConfigPackageOwner      = cloudRuntimeIdentity
	// Site-content bundles are Put members of the documentation site project
	// that declares publish: ["site-content"]; one member per section.
	cloudSiteContentEcosystem    = distributionproto.Ecosystem("put")
	cloudSiteContentPackageStep  = "cloud-site-content"
	cloudSiteContentPublishStep  = "cloud-publish-site-content"
	cloudSiteContentPackageOwner = cloudRuntimeIdentity
)

var scopedExtensionName = regexp.MustCompile(`^@[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`)
var nativeMigrationPart = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)

// HandleWorkspaceProbe serves the framework's reserved workspace control
// exchange. The process entrypoint calls it before RunMain, so the probe never
// loads command context or writes runtime job events.
func HandleWorkspaceProbe(args []string, stdin io.Reader, stdout io.Writer) (bool, error) {
	if !workspaceproto.IsProbeInvocation(args) {
		return false, nil
	}
	root, err := os.Getwd()
	if err != nil {
		return true, fmt.Errorf("resolve workspace root: %w", err)
	}
	return handleWorkspaceProbeAt(root, args, stdin, stdout)
}

func handleWorkspaceProbeAt(root string, args []string, stdin io.Reader, stdout io.Writer) (bool, error) {
	return workspaceproto.ServeProbe(args, stdin, stdout, func(request workspaceproto.ProbeRequest) (workspaceproto.ProbeResult, error) {
		return probeCloudWorkspace(root, request)
	})
}

// probeCloudWorkspace contributes only the release-set member Cloud can
// publish itself. Project identity and language dependency edges remain owned
// by core and the language provider respectively.
func probeCloudWorkspace(root string, request workspaceproto.ProbeRequest) (workspaceproto.ProbeResult, error) {
	result := workspaceproto.ProbeResult{
		Version:   workspaceproto.ProbeProtocolVersion,
		Extension: cloudRuntimeIdentity,
	}
	paths := append([]string(nil), request.Paths...)
	sort.Strings(paths)
	for _, rawPath := range paths {
		projectPath, ok := workspaceproto.NormalizeProbePath(rawPath)
		if !ok {
			return workspaceproto.ProbeResult{}, fmt.Errorf("invalid project path %q", rawPath)
		}
		projectFile := workspaceFile(projectPath, cloudProjectMarker)
		project, found, err := readCloudProject(filepath.Join(root, filepath.FromSlash(projectFile)))
		if err != nil {
			return workspaceproto.ProbeResult{}, fmt.Errorf("read %s: %w", projectFile, err)
		}
		if !found {
			continue
		}
		declarations := make([]releaseset.MemberDeclaration, 0, 3)
		watchedFiles := []string{projectFile}
		publishesArchives, err := project.publishesArchives()
		if err != nil {
			return workspaceproto.ProbeResult{}, fmt.Errorf("read %s archive intent: %w", projectFile, err)
		}
		if publishesArchives && slices.Contains(project.Publish, "template-archives") {
			templateFile := workspaceFile(projectPath, "putnami.template.json")
			identity, found, err := readArchiveExtensionIdentity(filepath.Join(root, filepath.FromSlash(templateFile)))
			if err != nil || !found || !nativeMigrationPart.MatchString(identity) {
				return workspaceproto.ProbeResult{}, fmt.Errorf("read %s template identity: expected one canonical native package name", templateFile)
			}
			declarations = append(declarations, releaseset.MemberDeclaration{
				Ecosystem: cloudArchiveEcosystem, Coordinate: distributioncli.ArchivePackageCoordinate(identity),
				PackagePublisher: cloudContentPackageOwner, PackageStep: cloudTemplatePackageStep, PublishStep: cloudArchivePublishStep,
			})
			watchedFiles = append(watchedFiles, templateFile)
		} else if publishesArchives {
			extensionFile := workspaceFile(projectPath, cloudExtensionManifest)
			manifest, hasExtension, err := readArchiveExtensionManifest(filepath.Join(root, filepath.FromSlash(extensionFile)))
			if err != nil {
				return workspaceproto.ProbeResult{}, fmt.Errorf("read %s for archives publication: %w", extensionFile, err)
			}
			artifact, declared, err := project.archiveArtifact(manifest.Name, hasExtension)
			if err != nil {
				return workspaceproto.ProbeResult{}, fmt.Errorf("read %s archive coordinate: %w", projectFile, err)
			}
			if declared {
				// A content-only extension carries no Go module: @putnami/scaffold
				// packages it under the step its agentContent key activates. A Go
				// extension that also declares agentContent stays with the Go
				// packager, whose archives step builds the binary and stages the
				// content; scaffold never plans a step for a Go project.
				packagePublisher, packageStep := cloudArchivePackageOwner, cloudArchivePackageStep
				if manifest.declaresAgentContent() {
					goModuleFile := workspaceFile(projectPath, cloudGoModule)
					hasGoModule, err := fileExists(filepath.Join(root, filepath.FromSlash(goModuleFile)))
					if err != nil {
						return workspaceproto.ProbeResult{}, fmt.Errorf("read %s: %w", goModuleFile, err)
					}
					if hasGoModule {
						watchedFiles = append(watchedFiles, goModuleFile)
					} else {
						packagePublisher, packageStep = cloudContentPackageOwner, cloudAgentContentPackageStep
					}
				}
				declarations = append(declarations, releaseset.MemberDeclaration{
					Ecosystem: cloudArchiveEcosystem, Coordinate: distributioncli.ArchivePackageCoordinate(artifact),
					PackagePublisher: packagePublisher, PackageStep: packageStep, PublishStep: cloudArchivePublishStep,
				})
				if hasExtension {
					watchedFiles = append(watchedFiles, extensionFile)
				}
			}
		}
		namespace, migrationDeclared, err := project.migrationNamespace()
		if err != nil {
			return workspaceproto.ProbeResult{}, fmt.Errorf("read %s migration namespace: %w", projectFile, err)
		}
		if migrationDeclared {
			// The member names the package the CLI publishes the migration at,
			// through the CLI's own rule: the workspace path's package
			// ("sites/putnami.dev" publishes at "sites-putnami.dev"), or the
			// manifest name for the root project and for a path that maps to
			// no native Put package.
			path := projectPath
			if projectPath == workspaceproto.ProbeRootPath {
				path = ""
			}
			application := datacli.MigrationApplication(path, project.Name)
			if application == "" || strings.TrimSpace(application) != application || strings.Contains(application, "..") || strings.Contains(application, "//") {
				return workspaceproto.ProbeResult{}, fmt.Errorf("read %s migration coordinate: project name must be canonical", projectFile)
			}
			packageName := bundlepkg.PackageName(application)
			if !nativeMigrationPart.MatchString(packageName) {
				return workspaceproto.ProbeResult{}, fmt.Errorf("read %s migration coordinate: project name does not map to a native Put package", projectFile)
			}
			packagePublisher, packageStep, err := project.migrationPackageProducer()
			if err != nil {
				return workspaceproto.ProbeResult{}, fmt.Errorf("read %s migration package owner: %w", projectFile, err)
			}
			declarations = append(declarations, releaseset.MemberDeclaration{
				Ecosystem: cloudMigrationEcosystem, Coordinate: namespace + "/" + packageName,
				PackagePublisher: packagePublisher, PackageStep: packageStep, PublishStep: cloudMigrationPublishStep,
			})
		}
		if configFiles := existingConfigMemberFiles(root, projectPath); len(configFiles) != 0 {
			namespace, declared, err := project.configNamespace()
			if err != nil {
				return workspaceproto.ProbeResult{}, fmt.Errorf("read %s Config namespace: %w", projectFile, err)
			}
			if declared {
				coordinate, err := configcli.AuthoredConfigCoordinate(namespace, project.Name)
				if err != nil {
					return workspaceproto.ProbeResult{}, fmt.Errorf("read %s Config coordinate: %w", projectFile, err)
				}
				declarations = append(declarations, releaseset.MemberDeclaration{
					Ecosystem: cloudConfigEcosystem, Coordinate: coordinate,
					PackagePublisher: cloudConfigPackageOwner, PackageStep: cloudConfigPackageStep, PublishStep: cloudConfigPublishStep,
				})
				watchedFiles = append(watchedFiles, configFiles...)
				// The deployment member shares the Config namespace and reads
				// only putnami.json, which every announced project watches.
				deployment, deploys, err := project.deploymentDeclaration(namespace)
				if err != nil {
					return workspaceproto.ProbeResult{}, fmt.Errorf("read %s deployment declaration: %w", projectFile, err)
				}
				if deploys {
					declarations = append(declarations, deployment)
				}
			}
		}
		if slices.Contains(project.Publish, distributioncli.SiteContentPublishEntry) {
			sections, err := distributioncli.SiteContentSections(filepath.Join(root, filepath.FromSlash(projectPath)))
			if err != nil {
				return workspaceproto.ProbeResult{}, fmt.Errorf("read %s site-content sections: %w", projectFile, err)
			}
			for _, section := range sections {
				declarations = append(declarations, releaseset.MemberDeclaration{
					Ecosystem: cloudSiteContentEcosystem, Coordinate: distributioncli.SiteContentCoordinate(section),
					PackagePublisher: cloudSiteContentPackageOwner, PackageStep: cloudSiteContentPackageStep, PublishStep: cloudSiteContentPublishStep,
				})
			}
		}
		if len(declarations) == 0 {
			continue
		}
		metadata, err := json.Marshal(map[string]any{
			releaseset.ProjectMetadataKey: releaseset.ProjectMetadata{Ecosystems: declarations},
		})
		if err != nil {
			return workspaceproto.ProbeResult{}, fmt.Errorf("encode release-set metadata for %s: %w", projectPath, err)
		}
		sort.Strings(watchedFiles)
		result.Projects = append(result.Projects, workspaceproto.ProbeProject{
			Path:         projectPath,
			WatchedFiles: watchedFiles,
			Metadata:     metadata,
		})
	}
	return result, nil
}

type cloudProjectProbe struct {
	Name       string                    `json:"name"`
	Type       string                    `json:"type"`
	Publish    []string                  `json:"publish"`
	Extensions []string                  `json:"extensions"`
	Options    map[string]map[string]any `json:"options"`
}

// Archive publication may be activated by the publish list or the existing
// command options. Both declarations must contribute members to a full set.
func (p cloudProjectProbe) publishesArchives() (bool, error) {
	enabled := slices.Contains(p.Publish, "archives") || slices.Contains(p.Publish, "template-archives")
	for _, layer := range []string{"publish", cloudRuntimeIdentity, cloudRuntimeIdentity + ":publish"} {
		value, found := p.Options[layer]["archives"]
		if !found {
			continue
		}
		candidate, ok := value.(bool)
		if !ok {
			return false, fmt.Errorf("options.%s.archives must be a boolean", layer)
		}
		enabled = candidate
	}
	return enabled, nil
}

// migrationNamespace reads only an explicitly authored existing project
// option. Its precedence mirrors the framework's command option layering and
// has no legacy default; absence means Cloud declares no migration member.
func (p cloudProjectProbe) migrationNamespace() (string, bool, error) {
	return datacli.MigrationNamespaceFromOptions(p.Options)
}

// configNamespace shares its declaration rule with the package/publish steps,
// so a project the probe announces is exactly a project those steps build.
func (p cloudProjectProbe) configNamespace() (string, bool, error) {
	return configcli.ConfigNamespaceFromOptions(p.Options)
}

func existingConfigMemberFiles(root, projectPath string) []string {
	candidates := []string{
		workspaceFile(projectPath, "schema/config.json"),
		workspaceFile(projectPath, ".gen/config-schema.json"),
	}
	files := make([]string, 0, 8)
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(candidate))); err == nil {
			files = append(files, candidate)
			break
		}
	}
	if len(files) == 0 {
		return nil
	}
	for _, relative := range []string{
		"schema/config-authored-fields.json",
		"conf/env.yaml", "conf/.env.yaml", "conf/env.prod.yaml", "conf/.env.prod.yaml",
		".gen/conf/env.prod.yaml", ".gen/conf/.env.prod.yaml", ".gen/version.json",
	} {
		candidate := workspaceFile(projectPath, relative)
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(candidate))); err == nil {
			files = append(files, candidate)
		}
	}
	return files
}

func (p cloudProjectProbe) migrationPackageProducer() (string, string, error) {
	// Each language emits its bundle as part of an existing package step.
	// Neither language declares a separate "migrations" package step.
	owner, step := "", ""
	for _, producer := range []struct{ owner, step string }{
		{"@putnami/go", "describe"},
		{"@putnami/typescript", "generate"},
	} {
		if slices.Contains(p.Extensions, producer.owner) {
			if owner != "" {
				return "", "", fmt.Errorf("explicit migration namespace requires exactly one migration-bundle language publisher")
			}
			owner, step = producer.owner, producer.step
		}
	}
	if owner == "" {
		return "", "", fmt.Errorf("explicit migration namespace requires exactly one migration-bundle language publisher")
	}
	return owner, step, nil
}

// archiveArtifact follows the same authored parameter precedence as the
// framework's publish command: command options, extension options, then
// extension-command options. A configured binary-name is the publisher's
// artifact override (including putnami -> putnami/cli); otherwise an extension
// archive uses its manifest identity. A plain project without either source is
// not announced because its eventual package metadata does not exist at probe
// time and cannot safely define a release member.
func (p cloudProjectProbe) archiveArtifact(extensionName string, hasExtension bool) (string, bool, error) {
	artifact := ""
	for _, layer := range []string{"publish", cloudRuntimeIdentity, cloudRuntimeIdentity + ":publish"} {
		options, found := p.Options[layer]
		if !found {
			continue
		}
		value, present := options["binary-name"]
		if !present {
			value, present = options["binaryName"]
		}
		if !present {
			continue
		}
		name, ok := value.(string)
		if !ok || name != strings.TrimSpace(name) || name == "" {
			return "", false, fmt.Errorf("options.%s.binary-name must be a non-empty canonical string", layer)
		}
		artifact = name
	}
	if artifact != "" {
		return artifact, true, nil
	}
	if !hasExtension {
		return "", false, nil
	}
	// Source extension manifests may omit their name. The Go packager stamps
	// the authored project identity into those archives before publication.
	if extensionName == "" {
		extensionName = p.Name
	}
	if err := validateArchiveIdentity(extensionName); err != nil {
		return "", false, err
	}
	return extensionName, true, nil
}

func readCloudProject(filename string) (cloudProjectProbe, bool, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return cloudProjectProbe{}, false, nil
		}
		return cloudProjectProbe{}, false, err
	}
	var project cloudProjectProbe
	if err := json.Unmarshal(data, &project); err != nil {
		return cloudProjectProbe{}, false, err
	}
	return project, true, nil
}

// archiveExtensionManifest is the part of putnami.extension.json that routes an
// archive member: its identity, and whether it declares agent content.
type archiveExtensionManifest struct {
	Name         string          `json:"name"`
	AgentContent json.RawMessage `json:"agentContent"`
}

// declaresAgentContent mirrors @putnami/scaffold's activation, which selects
// its agent-content step on the agentContent key alone.
func (m archiveExtensionManifest) declaresAgentContent() bool {
	return len(m.AgentContent) != 0 && string(m.AgentContent) != "null"
}

func readArchiveExtensionManifest(filename string) (archiveExtensionManifest, bool, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return archiveExtensionManifest{}, false, nil
		}
		return archiveExtensionManifest{}, false, err
	}
	var manifest archiveExtensionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return archiveExtensionManifest{}, false, err
	}
	return manifest, true, nil
}

func readArchiveExtensionIdentity(filename string) (string, bool, error) {
	manifest, found, err := readArchiveExtensionManifest(filename)
	return manifest.Name, found, err
}

// validateArchiveIdentity refuses an identity that cannot own an archive
// coordinate. Every archive route resolves its coordinate through the
// publisher's own resolver, so this check is the gate, not a second resolver.
func validateArchiveIdentity(identity string) error {
	if identity != strings.TrimSpace(identity) || !scopedExtensionName.MatchString(identity) {
		return fmt.Errorf("extension name %q must be a canonical scoped package before it can own an archive coordinate", identity)
	}
	return nil
}

func workspaceFile(projectPath, name string) string {
	if projectPath == workspaceproto.ProbeRootPath {
		return name
	}
	return path.Join(projectPath, name)
}

// fileExists reports whether filename names an existing file.
func fileExists(filename string) (bool, error) {
	info, err := os.Stat(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir(), nil
}
