package cloudcli

import (
	"encoding/json"
	"fmt"
	"strings"

	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	extensionproto "go.putnami.dev/protocol/extension"
)

// publishSiteContent publishes the planned site-content members of one docs
// project and reports each one to the release-set coordinator.
func publishSiteContent(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	published, err := distributioncli.PublishSiteContentMembers(params, args, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	for _, member := range published {
		if err := emitSiteContentPublishedMember(ioctx, member); err != nil {
			return err
		}
	}
	return nil
}

func emitSiteContentPublishedMember(ioctx IO, published distributioncli.SiteContentMemberResult) error {
	if ioctx.Artifact == nil {
		return nil
	}
	member := &extensionproto.PublishedMember{
		Ecosystem: string(cloudSiteContentEcosystem), Coordinate: published.Coordinate,
		Version: published.Version, ArtifactDigest: published.ArtifactDigest,
	}
	if diagnostics := extensionproto.ValidatePublishedMember(member); len(diagnostics) != 0 {
		return fmt.Errorf("refuse invalid site-content publication result: %s", diagnostics[0].String())
	}
	encoded, err := json.Marshal(member)
	if err != nil {
		return fmt.Errorf("encode site-content publication result: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		return fmt.Errorf("encode site-content publication event data: %w", err)
	}
	return ioctx.Artifact(
		"site-content-"+strings.NewReplacer("/", "-", "@", "").Replace(member.Coordinate),
		member.Coordinate, extensionproto.PublishedMemberEventKind, "", data,
	)
}
