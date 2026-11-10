package ruby

import (
	"path/filepath"

	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/stacks"
)

// Component types the probe reports.
const (
	TypeApplication = "application"
	TypeLibrary     = "library"
)

// rackConfig is the file that makes a directory a Rack application.
const rackConfig = "config.ru"

// componentType is application for a component whose directory holds
// config.ru, and library otherwise.
func componentType(root, dir string) string {
	if isFile(filepath.Join(root, filepath.FromSlash(dir), rackConfig)) {
		return TypeApplication
	}
	return TypeLibrary
}

// imageMember answers the release-set member of an application at its own
// stack root that publishes an image, as the stack core derives it.
func imageMember(root, dir, sourceName, stackRoot, kind string) []releaseset.MemberDeclaration {
	if kind != TypeApplication || stackRoot != dir {
		return nil
	}
	return stacks.ImageMember(root, dir, sourceName)
}
