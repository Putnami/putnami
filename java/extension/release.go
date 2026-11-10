package java

import (
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/stacks"
)

// imageMember answers the release-set member of an application that publishes
// an image, as the stack core derives it.
func imageMember(root string, project wsproto.ProbeProject) []releaseset.MemberDeclaration {
	if project.Type != TypeApplication {
		return nil
	}
	return stacks.ImageMember(root, project.Path, project.SourceName)
}
