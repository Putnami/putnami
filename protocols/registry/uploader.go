package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	extension "go.putnami.dev/protocol/extension"
)

// uploader-v1 is the capability through which the engine's credential
// provider session also uploads the members of the opened plan. The engine
// offers CapabilityUploaderV1 beside CapabilityPublicationV1 at initialize; a
// provider that echoes both answers upload on the same JSONL session, after
// open and before release:
//
//	→ {"protocolVersion":1,"id":6,"op":"upload","payload":{"planDigest":"sha256:…","registry":"https://npm.putnami.dev","outbox":"/tmp/putnami-publication-1/0","member":{…extension.OutboxMember…}}}
//	← {"protocolVersion":1,"id":6,"ok":true,"payload":{"digest":"sha256:…"}}
//
// The request names one packed member of the publication outbox and the
// registry the engine resolved for it. It carries no credential: the
// provider uploads with a credential it holds itself. The provider reads
// every file of the member from the outbox, refuses one whose size or digest
// is not the member's, and answers the digest of the artifact the registry
// stores.
const (
	// CapabilityUploaderV1 is offered by the engine and echoed by a provider
	// that serves the upload op. A capability list that names it also names
	// CapabilityPublicationV1.
	CapabilityUploaderV1 = "uploader-v1"

	// MaxUploadRegistryBytes bounds the registry URL of an upload.
	MaxUploadRegistryBytes = 2048
	// MaxUploadOutboxBytes bounds the outbox root of an upload.
	MaxUploadOutboxBytes = 4096
)

// Wire grammars of uploader-v1, shared with schemas/credential-provider-v1.json.
const (
	// UploadRegistryPattern is the grammar of an upload registry: an http or
	// https URL of visible ASCII whose authority carries no '/', '?', '#' or
	// '@', and whose path, when present, carries no '?' or '#' and does not
	// end with '/'.
	UploadRegistryPattern = `^https?://[!-"$-.0->A-~]+(/[!-"$->@-~]*[!-"$-.0->@-~])?$`
	// UploadOutboxPattern is the grammar of an upload outbox: an absolute
	// path, rooted at '/' or at a drive letter, without control characters.
	UploadOutboxPattern = `^(/|[A-Za-z]:[\\/])[^\x00-\x1f\x7f]*$`
	// UploadPlatformPattern is the grammar of a platforms key: a lowercase
	// os/arch pair.
	UploadPlatformPattern = `^[a-z0-9]+/[a-z0-9]+$`
)

// Refusal codes an uploader-v1 provider answers upload with. Each matches
// RefusalCodePattern. Beside these, upload reuses RefusalPlanNotOpen,
// RefusalPlanMismatch and RefusalArtifactDigestMismatch.
const (
	// RefusalRegistryNotServed: the provider does not upload to the request's
	// registry.
	RefusalRegistryNotServed = "registry_not_served"
	// RefusalArtifactConflict: the registry already holds another artifact at
	// the member's coordinate and version; nothing was uploaded.
	RefusalArtifactConflict = "artifact_conflict"
	// RefusalUploadFailed: the registry refused the upload for another reason,
	// or the upload did not complete.
	RefusalUploadFailed = "upload_failed"
)

// UploadRefusalCodes lists the refusal codes of upload in their declared
// order.
var UploadRefusalCodes = []string{
	RefusalPlanNotOpen, RefusalPlanMismatch, RefusalRegistryNotServed,
	RefusalArtifactDigestMismatch, RefusalArtifactConflict, RefusalUploadFailed,
}

var (
	uploadRegistryGrammar = regexp.MustCompile(UploadRegistryPattern)
	uploadOutboxGrammar   = regexp.MustCompile(UploadOutboxPattern)
	uploadPlatformGrammar = regexp.MustCompile(UploadPlatformPattern)
)

// UploadParams is the payload of upload: one packed member of the opened
// plan, where its files are, and the registry it goes to.
type UploadParams struct {
	// PlanDigest names the opened plan the member belongs to.
	PlanDigest string `json:"planDigest"`
	// Registry is the registry endpoint the engine resolved for the member: an
	// https URL, or an http URL to a loopback host, without userinfo, query,
	// fragment or trailing slash. For an npm member it is the registry the
	// member names, normalized (extension.ManagedNPMRegistry).
	Registry string `json:"registry"`
	// Kind is the release-set member kind a Put registry member is uploaded
	// as: present for a put or archive member, absent for any other. An
	// archive member has kind archive, and a put member any other kind.
	Kind distribution.MemberKind `json:"kind,omitempty"`
	// Outbox is the absolute path of the outbox directory every path of
	// Member is relative to.
	Outbox string `json:"outbox"`
	// Member is the packed member, valid under the publication outbox
	// contract (extension.ValidatePublicationOutbox).
	Member extension.OutboxMember `json:"member"`
}

// UploadResult answers upload with what the registry stores for the member.
type UploadResult struct {
	// Digest is the artifact digest of the uploaded member: the npm tarball's,
	// the Go module zip's, the OCI manifest's, or the stored Put manifest
	// payload's.
	Digest string `json:"digest"`
	// Platforms maps each "os/arch" of a member published as a multi-platform
	// set to the digest of that platform's artifact. Absent for a single
	// artifact.
	Platforms map[string]string `json:"platforms,omitempty"`
}

// ValidateUploadParams checks an upload payload: a plan digest, a member the
// publication outbox contract accepts, a kind present exactly for a put or
// archive member, a registry URL that is the npm member's own registry for
// an npm member, and an outbox root. No error repeats the registry, which
// may carry a credential.
func ValidateUploadParams(params UploadParams) error {
	if !sha256Digest.MatchString(params.PlanDigest) {
		return fmt.Errorf("registry: upload planDigest must match %s", PlanDigestPattern)
	}
	outbox := extension.PublicationOutbox{ProtocolVersion: extension.PublicationOutboxVersion, Members: []extension.OutboxMember{params.Member}}
	if diagnostics := extension.ValidatePublicationOutbox(&outbox); diag.HasErrors(diagnostics) {
		for index := range diagnostics {
			if rest, found := strings.CutPrefix(diagnostics[index].Field, "members[0]"); found {
				diagnostics[index].Field = "member" + rest
			}
		}
		return diagnosticError("upload member", diagnostics)
	}
	member := params.Member
	if err := validUploadKind(member.Ecosystem, params.Kind); err != nil {
		return err
	}
	if err := validUploadRegistry(params.Registry); err != nil {
		return err
	}
	if member.Ecosystem == extension.OutboxEcosystemNPM {
		named, err := extension.ManagedNPMRegistry(member.NPM.Registry)
		if err != nil || named != params.Registry {
			return errors.New("registry: upload registry is not the registry the npm member names")
		}
	}
	return validUploadOutbox(params.Outbox)
}

// validUploadKind applies the kind rule: a put or archive member names the
// kind it is uploaded as, archive exactly for an archive member; any other
// member names none.
func validUploadKind(ecosystem string, kind distribution.MemberKind) error {
	switch ecosystem {
	case extension.OutboxEcosystemPut, extension.OutboxEcosystemArchive:
		if !kind.Valid() {
			return fmt.Errorf("registry: upload of a %s member needs a kind among %v", ecosystem, distribution.MemberKinds)
		}
		if (ecosystem == extension.OutboxEcosystemArchive) != (kind == distribution.KindArchive) {
			return fmt.Errorf("registry: upload kind %s does not fit a %s member", kind, ecosystem)
		}
	default:
		if kind != "" {
			return fmt.Errorf("registry: upload of a %s member names no kind", ecosystem)
		}
	}
	return nil
}

// validUploadRegistry accepts a registry URL under UploadRegistryPattern, at
// most MaxUploadRegistryBytes, with a host, a port from 1 to 65535 when one
// is named, and the http scheme only for a loopback host: localhost, a name
// under .localhost, or a loopback IP.
func validUploadRegistry(registry string) error {
	if len(registry) > MaxUploadRegistryBytes || !uploadRegistryGrammar.MatchString(registry) {
		return fmt.Errorf("registry: upload registry must be an http or https URL of at most %d visible ASCII bytes, without userinfo, query, fragment or trailing slash", MaxUploadRegistryBytes)
	}
	target, err := url.Parse(registry)
	if err != nil || target.Opaque != "" || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return errors.New("registry: upload registry is not a URL with a host")
	}
	if raw := target.Port(); raw != "" {
		if port, err := strconv.Atoi(raw); err != nil || port < 1 || port > 65535 {
			return errors.New("registry: upload registry port must be between 1 and 65535")
		}
	}
	if target.Scheme == "http" && !uploadLoopbackHost(target.Hostname()) {
		return errors.New("registry: upload registry uses http to a host that is not loopback")
	}
	return nil
}

// uploadLoopbackHost is the loopback rule of the engine's registry URLs
// (extension.ManagedNPMRegistry): localhost, a name under .localhost, or a
// loopback IP.
func uploadLoopbackHost(hostname string) bool {
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
	ip := net.ParseIP(hostname)
	return hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || (ip != nil && ip.IsLoopback())
}

// validUploadOutbox accepts an outbox root under UploadOutboxPattern, of at
// most MaxUploadOutboxBytes of UTF-8, with no "." or ".." segment between
// '/' or '\' separators.
func validUploadOutbox(outbox string) error {
	if len(outbox) > MaxUploadOutboxBytes || !utf8.ValidString(outbox) || !uploadOutboxGrammar.MatchString(outbox) {
		return fmt.Errorf("registry: upload outbox must be an absolute path of at most %d bytes without control characters", MaxUploadOutboxBytes)
	}
	for _, segment := range strings.FieldsFunc(outbox, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == "." || segment == ".." {
			return errors.New("registry: upload outbox holds a . or .. segment")
		}
	}
	return nil
}

// ValidateUploadResult checks an upload answer: a sha256 digest, and, when
// platforms are present, 1 to distribution.MaxPlatformsPerMember lowercase
// os/arch keys, each mapped to a sha256 digest.
func ValidateUploadResult(result UploadResult) error {
	if !sha256Digest.MatchString(result.Digest) {
		return fmt.Errorf("registry: upload digest must match %s", PlanDigestPattern)
	}
	if result.Platforms == nil {
		return nil
	}
	if len(result.Platforms) == 0 || len(result.Platforms) > distribution.MaxPlatformsPerMember {
		return fmt.Errorf("registry: upload platforms must name 1 to %d platforms", distribution.MaxPlatformsPerMember)
	}
	for platform, digest := range result.Platforms {
		if !uploadPlatformGrammar.MatchString(platform) {
			return fmt.Errorf("registry: upload platform %q must match %s", platform, UploadPlatformPattern)
		}
		if !sha256Digest.MatchString(digest) {
			return fmt.Errorf("registry: upload platform %q digest must match %s", platform, PlanDigestPattern)
		}
	}
	return nil
}

// ParseUploadParams strictly decodes an upload payload and validates it
// (ValidateUploadParams). Every member without omitempty is present and every
// omitempty member is absent rather than empty, at every depth, so the
// payload is the canonical encoding of the value it decodes to.
func ParseUploadParams(payload json.RawMessage) (*UploadParams, error) {
	var params UploadParams
	if err := strictPublicationPayload(payload, &params, ""); err != nil {
		return nil, err
	}
	if err := ValidateUploadParams(params); err != nil {
		return nil, err
	}
	return &params, nil
}

// ParseUploadResult strictly decodes an upload answer and validates it
// (ValidateUploadResult).
func ParseUploadResult(payload json.RawMessage) (*UploadResult, error) {
	var result UploadResult
	if err := strictPublicationPayload(payload, &result, ""); err != nil {
		return nil, err
	}
	if err := ValidateUploadResult(result); err != nil {
		return nil, err
	}
	return &result, nil
}
