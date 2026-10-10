package distributioncli

import (
	"context"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
)

// DeploymentMemberMediaType is the Put manifest media type of a workload's
// deployment declaration: the put-write profile the framework pairs with the
// release-set member kind "deployment". It must equal the framework's
// put.DeploymentManifestMediaType; the Cloud producer's tests pin the two.
const DeploymentMemberMediaType = "application/vnd.putnami.infra.deployment.v2+json"

const deploymentMemberSubject = "deployment member publication"

// DeploymentMemberPublication is the closed input Distribution accepts from the
// Cloud deployment member producer. Payload is the canonical declaration the
// producer has already strict-parsed; Distribution stores it byte for byte.
// Distribution selects the Put origin and bearer; callers cannot use this
// contract as an arbitrary HTTP or credential broker.
type DeploymentMemberPublication struct {
	Namespace string
	Package   string
	Version   string
	Payload   []byte
}

// DeploymentMemberPublicationResult is the exact immutable Put identity
// confirmed by the atomic publish response, by authenticated collision
// readback, or by the publication outbox. ArtifactDigest is the sha256 of the
// stored payload bytes.
type DeploymentMemberPublicationResult struct {
	Coordinate     string
	Version        string
	ArtifactDigest string
}

// checkDeploymentMemberPublication is the immutable Put identity rule a
// deployment member must meet before it is published or packed into the
// publication outbox.
func checkDeploymentMemberPublication(publication DeploymentMemberPublication) error {
	if !validImmutablePutMember(publication.Namespace, publication.Package, publication.Version, publication.Payload) {
		return clicore.NewError(deploymentMemberSubject+" has an invalid immutable Put identity", clicore.ExitUsage)
	}
	return nil
}

// PublishDeploymentMember publishes one deployment declaration to the existing
// selected Put endpoint under DeploymentMemberMediaType. It never advances a
// channel: the release set, not the package, carries the channel move.
func PublishDeploymentMember(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, publication DeploymentMemberPublication) (*DeploymentMemberPublicationResult, error) {
	if err := checkDeploymentMemberPublication(publication); err != nil {
		return nil, err
	}
	credential, err := resolveReleaseSetProviderCredential(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, err
	}
	ctx := ioctx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	publisher, err := newPutPublisher(ioctx.Client, credential.Endpoint.URL, credential.Token)
	if err != nil {
		return nil, err
	}
	digest, err := publishImmutablePutManifest(
		ctx, publisher, publication.Namespace, publication.Package, publication.Version, DeploymentMemberMediaType,
		publication.Payload, deploymentMemberSubject,
	)
	if err != nil {
		return nil, err
	}
	return &DeploymentMemberPublicationResult{
		Coordinate: publication.Namespace + "/" + publication.Package,
		Version:    publication.Version, ArtifactDigest: digest,
	}, nil
}

// PackDeploymentMember packs one deployment declaration into the publication
// outbox: the manifest payload PublishDeploymentMember publishes, with the same
// media type and no blob. It uploads nothing and moves no channel. projectID
// is the plan's project id of the member.
func PackDeploymentMember(outbox, projectID string, publication DeploymentMemberPublication) (*DeploymentMemberPublicationResult, error) {
	if err := checkDeploymentMemberPublication(publication); err != nil {
		return nil, err
	}
	packer, err := newPutOutboxPacker(outbox)
	if err != nil {
		return nil, err
	}
	coordinate := publication.Namespace + "/" + publication.Package
	member, err := packer.commit(distributionproto.KindDeployment, extensionproto.OutboxEcosystemPut, coordinate, publication.Version, projectID,
		DeploymentMemberMediaType, publication.Payload)
	if err != nil {
		return nil, err
	}
	return &DeploymentMemberPublicationResult{
		Coordinate: coordinate, Version: publication.Version, ArtifactDigest: member.Put.Manifest.Digest,
	}, nil
}
