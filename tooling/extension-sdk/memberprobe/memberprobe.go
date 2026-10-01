// Package memberprobe is the dry-run half of a publication: a publisher asks
// its registry whether it already holds a member at the version the real
// publish would write, and reports the answer as one member-probe event.
//
// A probe sends read-only requests (GET or HEAD). It carries the credential the
// publisher's own sources yield, which for a managed host is the host-only
// registry-token seam the real publish asks, and is anonymous when none
// resolves. The orchestrator fails the dry run on a conflict or an unverified
// answer, and warns on a retag.
//
// The package owns what the four registry kinds share: the verdict rules, the
// wording of a reason, the credential-free form of an endpoint and the emit
// helper. It also owns the probe of an archive member on the put registry. The
// npm, Go module and OCI probes live with their publishers and use their
// publisher's route, credential sources and HTTP policy.
package memberprobe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Timeout bounds one probe. A registry that has not answered within it is
// reported as unverified.
const Timeout = 2 * time.Minute

// reasonLimit bounds, in characters, the reason a probe carries.
const reasonLimit = 300

// Reasons a held version is a conflict.
const (
	// ReasonNoArtifact is the reason of a held version the dry run has no
	// local artifact to compare with. It names the remedy.
	ReasonNoArtifact = "the registry already holds this version and the dry run built no artifact to compare; " +
		"run package for this project without --dry-run, then the dry run again, to compare the bytes"
	// ReasonOtherDigest is the reason of a held version whose digest differs
	// from the local artifact's, for a registry that keeps a version immutable.
	ReasonOtherDigest = "the registry already holds this version with another digest; the real publish cannot overwrite it"
)

// Subject is one question to one registry: the member a dry run would publish,
// the endpoint asked, and what the publisher holds locally.
type Subject struct {
	// Ecosystem is the id of the member's ecosystem profile.
	Ecosystem string
	// Coordinate is the member's package name in that ecosystem.
	Coordinate string
	// Version is the version the real publish would write.
	Version string
	// Platform is the "os/arch" of the artifact asked about, for a member
	// published as one artifact per platform. Empty for a single artifact.
	Platform string
	// Registry is the endpoint asked. Every verdict carries its credential-free
	// form (see Endpoint).
	Registry string
	// ArtifactDigest is the digest of the local artifact the real publish would
	// upload, or empty when the dry run built none.
	ArtifactDigest string
	// Anonymous is true when the request carried no credential.
	Anonymous bool
}

func (s Subject) probe(state string) extproto.MemberProbe {
	return extproto.MemberProbe{
		Ecosystem:      s.Ecosystem,
		Coordinate:     s.Coordinate,
		Version:        s.Version,
		Platform:       s.Platform,
		Registry:       Endpoint(s.Registry),
		State:          state,
		ArtifactDigest: s.ArtifactDigest,
		Anonymous:      s.Anonymous,
	}
}

// Absent is the verdict for a registry that does not hold the version.
func (s Subject) Absent() extproto.MemberProbe {
	return s.probe(extproto.MemberProbeAbsent)
}

// Held is the verdict for a registry that holds the version with
// registryDigest, for a publisher whose real publish reuses a version with the
// local artifact's digest and is refused on any other. It is identical when
// the digests are equal, a conflict when they differ or when the dry run built
// no artifact, and unverified when the registry advertised no digest.
func (s Subject) Held(registryDigest string) extproto.MemberProbe {
	return s.HeldWith(registryDigest, ReasonOtherDigest)
}

// HeldWith is Held with the reason of a held version whose digest differs from
// the local artifact's. otherDigest says what the real publish does with it.
func (s Subject) HeldWith(registryDigest, otherDigest string) extproto.MemberProbe {
	switch {
	case s.ArtifactDigest == "":
		return s.Conflict(registryDigest, ReasonNoArtifact)
	case registryDigest == "":
		return s.Unverified("the registry holds this version but advertised no digest to compare")
	case registryDigest == s.ArtifactDigest:
		probe := s.probe(extproto.MemberProbeIdentical)
		probe.RegistryDigest = registryDigest
		return probe
	default:
		return s.Conflict(registryDigest, otherDigest)
	}
}

// HeldTag is the verdict for a registry that holds the version tag at
// registryDigest, for a publisher whose real publish moves the tag to the local
// artifact. It is identical when the digests are equal, retag when they differ
// or when the dry run built no artifact, and unverified when the registry
// advertised no digest.
func (s Subject) HeldTag(registryDigest string) extproto.MemberProbe {
	switch {
	case registryDigest == "":
		return s.Unverified("the registry holds this version tag but advertised no digest to compare")
	case registryDigest == s.ArtifactDigest:
		probe := s.probe(extproto.MemberProbeIdentical)
		probe.RegistryDigest = registryDigest
		return probe
	default:
		probe := s.probe(extproto.MemberProbeRetag)
		probe.RegistryDigest = registryDigest
		return probe
	}
}

// Conflict is the verdict for a held version the real publish cannot reuse.
// registryDigest may be empty when the registry advertised none in the form a
// probe carries, and may equal the local artifact's digest when the publisher
// may refuse the held version whatever its bytes.
func (s Subject) Conflict(registryDigest, reason string) extproto.MemberProbe {
	probe := s.probe(extproto.MemberProbeConflict)
	probe.RegistryDigest = registryDigest
	probe.Reason = Redact(reason)
	return probe
}

// Unverified is the verdict for a registry that could not answer.
func (s Subject) Unverified(reason string) extproto.MemberProbe {
	probe := s.probe(extproto.MemberProbeUnverified)
	probe.Reason = Redact(reason)
	return probe
}

// Unreachable is the unverified verdict for a request that got no response: a
// refused connection, a failed name lookup, a timeout.
func (s Subject) Unreachable(err error) extproto.MemberProbe {
	return s.Unverified(UnreachableReason(err))
}

// Refused is the unverified verdict for an HTTP status that states neither a
// held version nor an absent one. excerpt is the part of the response body
// worth showing, already free of the request's credential, or empty.
func (s Subject) Refused(status int, excerpt string) extproto.MemberProbe {
	return s.Unverified(RefusedReason(status, s.Anonymous, excerpt))
}

// UnreachableReason says that a registry could not be reached and why. It keeps
// the cause and drops the request URL an HTTP client wraps it in: the probe
// already names the registry.
func UnreachableReason(err error) string {
	if err == nil {
		return "the registry could not be reached"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return "the registry did not answer in time"
		}
		err = urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the registry did not answer in time"
	}
	return "the registry could not be reached: " + err.Error()
}

// RefusedReason says which HTTP status a registry answered and what to do about
// it. For a 401 or a 403 it names the credential: an anonymous request is told
// to sign in or supply the registry token.
func RefusedReason(status int, anonymous bool, excerpt string) string {
	answer := fmt.Sprintf("%d %s", status, http.StatusText(status))
	if excerpt = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(excerpt), ":")); excerpt != "" {
		answer += ": " + excerpt
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		if anonymous {
			return "the registry answered " + answer + " to a request without a credential; sign in or supply the registry token, then run the dry run again"
		}
		return "the registry refused the credential with " + answer
	default:
		return "the registry answered " + answer
	}
}

var bearerCredential = regexp.MustCompile(`(?i)bearer\s+[^\s"',;]+`)

// Redact makes upstream-controlled text safe to carry in a probe: it replaces
// every given secret and every bearer value, turns control characters into
// spaces and cuts the text to a bounded length.
func Redact(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[redacted]")
		if escaped := url.QueryEscape(secret); escaped != secret {
			text = strings.ReplaceAll(text, escaped, "[redacted]")
		}
	}
	text = bearerCredential.ReplaceAllString(text, "Bearer [redacted]")
	text = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text))
	if runes := []rune(text); len(runes) > reasonLimit {
		text = string(runes[:reasonLimit]) + "..."
	}
	return text
}

// Endpoint returns the credential-free form of a registry endpoint: the URL
// without user information, query or fragment, or the bare host form unchanged.
// A value that cannot be reduced safely is replaced, never passed through.
func Endpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		if at := strings.LastIndexByte(raw, '@'); at >= 0 {
			raw = raw[at+1:]
		}
		raw, _, _ = strings.Cut(raw, "?")
		return strings.TrimRight(raw, "/")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "an unparseable registry URL"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String()
}

// Emit reports one probe as a member-probe artifact event. The event is not
// publication evidence, and emitting a conflict does not fail the job.
func Emit(emit *jsonl.Emitter, probe extproto.MemberProbe) {
	data := map[string]any{
		"ecosystem":  probe.Ecosystem,
		"coordinate": probe.Coordinate,
		"version":    probe.Version,
		"registry":   Endpoint(probe.Registry),
		"state":      probe.State,
	}
	for key, value := range map[string]string{
		"platform":       probe.Platform,
		"artifactDigest": probe.ArtifactDigest,
		"registryDigest": probe.RegistryDigest,
		"reason":         probe.Reason,
	} {
		if value != "" {
			data[key] = value
		}
	}
	if probe.Anonymous {
		data["anonymous"] = true
	}
	emit.ArtifactWithData(probe.Ecosystem, probe.Coordinate, extproto.MemberProbeEventKind, "", data)
}

// Decode reads the probe one flattened member-probe artifact event carries. It
// is the inverse of Emit: the payload goes through the protocol's strict
// parser and its validator, so an unknown field or state is an error.
func Decode(event map[string]any) (*extproto.MemberProbe, error) {
	encoded, err := extproto.ArtifactEventPayload(event)
	if err != nil {
		return nil, err
	}
	probe, diagnostics := extproto.ParseMemberProbe(encoded)
	if probe != nil && len(diagnostics) == 0 {
		diagnostics = extproto.ValidateMemberProbe(probe)
	}
	if len(diagnostics) > 0 {
		return nil, errors.New(diagnostics[0].Message)
	}
	return probe, nil
}

// NewHTTPClient returns the client a probe sends its requests with. It ignores
// ambient HTTP(S)_PROXY and returns a redirect instead of following it, so a
// request and its bearer reach the registry origin only.
func NewHTTPClient() (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("member probe transport is unavailable")
	}
	transport := base.Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:   Timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
