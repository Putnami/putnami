package dockerpublish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/memberprobe"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/releaseset"
)

// probeManifestHead is the manifest HEAD a dry run asks the registry with. It
// is the only request a dry-run image publication sends.
var probeManifestHead = crane.Head

// imageProbe is one question a dry-run image publication asks its registry:
// does ref already exist, and at which digest.
type imageProbe struct {
	// host is the registry host, and repository the full repository path that
	// starts with it.
	host, repository string
	// version is the member version the real publish would report.
	version string
	// ref is the reference asked: the version tag of a workload image, the
	// digest of an image project.
	ref string
	// digest is the packaged manifest digest, or empty when package assembled
	// no image.
	digest string
	// movesTag is true when ref is a version tag the real publish moves to the
	// packaged image: a held tag at another digest is then a retag.
	movesTag bool
	keychain authn.Keychain
	// transport is the per-call transport of a private publication broker, or
	// nil for the direct registry path.
	transport http.RoundTripper
}

// emitImageProbe asks the registry for one manifest with a HEAD and emits the
// answer as a member-probe event. A registry that refuses or cannot be reached
// is unverified. It emits nothing for a repository without a host.
func emitImageProbe(emit *jsonl.Emitter, probe imageProbe) {
	coordinate := strings.TrimPrefix(probe.repository, probe.host+"/")
	if probe.host == "" || coordinate == "" || coordinate == probe.repository {
		return
	}
	subject := memberprobe.Subject{
		Ecosystem:  "oci",
		Coordinate: coordinate,
		Version:    probe.version,
		Registry:   probe.host,
		Anonymous:  resolvesAnonymous(probe.keychain, probe.host),
	}
	if isImmutableDigest(probe.digest) {
		subject.ArtifactDigest = probe.digest
	}

	ctx, cancel := context.WithTimeout(context.Background(), memberprobe.Timeout)
	defer cancel()
	options := []crane.Option{crane.WithAuthFromKeychain(probe.keychain), crane.WithContext(ctx)}
	if probe.transport != nil {
		options = append(options, crane.WithTransport(probe.transport))
	}
	descriptor, err := probeManifestHead(probe.ref, options...)
	if err == nil {
		if probe.movesTag {
			memberprobe.Emit(emit, subject.HeldTag(descriptor.Digest.String()))
			return
		}
		memberprobe.Emit(emit, subject.Held(descriptor.Digest.String()))
		return
	}
	var registryErr *transport.Error
	switch {
	case errors.As(err, &registryErr) && registryErr.StatusCode == http.StatusNotFound:
		memberprobe.Emit(emit, subject.Absent())
	case errors.As(err, &registryErr):
		memberprobe.Emit(emit, subject.Refused(registryErr.StatusCode, registryErrorCodes(registryErr)))
	case oci.IsAuthError(err):
		memberprobe.Emit(emit, subject.Refused(http.StatusUnauthorized, ""))
	default:
		memberprobe.Emit(emit, subject.Unreachable(err))
	}
}

// registryErrorCodes lists the distribution error codes of a registry answer,
// such as MANIFEST_UNKNOWN, without the free-text message beside them.
func registryErrorCodes(err *transport.Error) string {
	codes := make([]string, 0, len(err.Errors))
	for _, diagnostic := range err.Errors {
		if diagnostic.Code != "" {
			codes = append(codes, string(diagnostic.Code))
		}
	}
	return strings.Join(codes, ", ")
}

// resolvesAnonymous reports whether the keychain holds no credential for the
// registry host, so the probe is sent without one.
func resolvesAnonymous(keychain authn.Keychain, host string) bool {
	registry, err := name.NewRegistry(host)
	if err != nil {
		return true
	}
	authenticator, err := keychain.Resolve(registry)
	return err != nil || authenticator == authn.Anonymous
}

// probePlannedImageMembers probes the OCI members the release-set plan selects
// for this project when package assembled no image. The plan supplies the
// coordinate and the version, and the probe asks for the version tag with no
// local digest. When the real publish pushes that tag, as for a workload or for
// an image project outside a private publication broker, a held tag is a retag
// to the image the publish builds; otherwise a held version is a conflict whose
// reason names the remedy. The transport and the credential are resolved once
// for the host.
func probePlannedImageMembers(ctx *pctx.Context, emit *jsonl.Emitter, configuredRegistry string) {
	plan, err := releaseset.FromContext(ctx)
	if err != nil {
		emit.Warn("Dry run: the release-set plan could not be read, so the registry was not asked: " + err.Error())
		return
	}
	if plan == nil || ctx.Identity == nil || ctx.Identity.Project.ID == "" {
		return
	}
	host := oci.RegistryHost(configuredRegistry)
	if host == "" && ctx.Project.Type == "image" {
		host = managedOCIRegistry
	}
	if host == "" {
		return
	}
	var members []releaseset.PlannedMember
	for _, member := range plan.SelectedMembers() {
		if member.Ecosystem == "oci" && member.ProjectID == ctx.Identity.Project.ID {
			members = append(members, member)
		}
	}
	if len(members) == 0 {
		return
	}
	privateTransport, credentialHost, routeErr := privateOCITransportFor(imagePublishTarget{Host: host, Managed: host == managedOCIRegistry})
	var keychain authn.Keychain
	if routeErr == nil {
		token, _ := resolveManagedDockerCredential(credentialHost)
		keychain = oci.NewRegistryKeychain(host, token)
	}
	for _, member := range members {
		if routeErr != nil {
			subject := memberprobe.Subject{Ecosystem: "oci", Coordinate: member.Coordinate, Version: member.Version, Registry: host}
			memberprobe.Emit(emit, subject.Unverified(routeErr.Error()))
			continue
		}
		repository := host + "/" + member.Coordinate
		emitImageProbe(emit, imageProbe{
			host: host, repository: repository, version: member.Version,
			ref:      fmt.Sprintf("%s:%s", repository, member.Version),
			movesTag: ctx.Project.Type != "image" || privateTransport == nil,
			keychain: keychain, transport: privateTransport,
		})
	}
}
