package template

import (
	proto "go.putnami.dev/protocol/template"
)

// ManifestFilename is the name of the template manifest file.
const ManifestFilename = proto.ManifestFilename

// LoadManifest reads and parses a putnami.template.json file.
func LoadManifest(path string) (*Manifest, error) {
	return proto.LoadManifest(path)
}

// Resolve converts a raw manifest into a TemplateDescription.
func Resolve(manifest *Manifest, absPath string) *TemplateDescription {
	return &TemplateDescription{
		Name:                     manifest.Name,
		Version:                  manifest.Version,
		Description:              manifest.Description,
		Extension:                manifest.Extension,
		Path:                     absPath,
		WorkspaceDevDependencies: manifest.WorkspaceDevDependencies,
	}
}
