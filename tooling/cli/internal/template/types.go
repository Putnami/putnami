package template

import (
	proto "go.putnami.dev/protocol/template"
)

// --- Manifest type alias (re-exported from protocol) ---

type Manifest = proto.Manifest

// --- Resolved type (CLI-specific) ---

// TemplateDescription is the resolved, in-memory representation of a template.
type TemplateDescription struct {
	Name                     string
	Version                  string
	Description              string
	Extension                string
	Path                     string // absolute filesystem path
	RelPath                  string // workspace-relative path
	WorkspaceDevDependencies map[string]string
}
