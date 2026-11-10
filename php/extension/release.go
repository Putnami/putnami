package php

import (
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/observe/detect/composer"
	"go.putnami.dev/tooling/stacks"
)

// imageMember answers the release-set member of an application at its own
// stack root that publishes an image, as the stack core derives it.
func imageMember(root string, project wsproto.ProbeProject, stackRoot string) []releaseset.MemberDeclaration {
	if project.Type != composer.TypeApplication || stackRoot != project.Path {
		return nil
	}
	return stacks.ImageMember(root, project.Path, project.SourceName)
}
