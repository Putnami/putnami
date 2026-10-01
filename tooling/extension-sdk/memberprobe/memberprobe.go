// Package memberprobe is the dry-run half of a publication: a publisher asks
// its registry whether it already holds a member at the version the real
// publish would write, and reports the answer as one member-probe event.
//
// A registry refuses to overwrite a published version. A dry run that never
// asks therefore passes on a release the real publish fails part-way through.
// Every publisher asks with read-only requests (GET or HEAD), and the
// orchestrator fails the dry run when any answer is a conflict or is missing.
//
// The package owns what the four registry kinds share: the verdict rules, the
// wording of a reason, the credential-free form of an endpoint and the emit
// helper. It also owns the probe of an archive member on the put registry. The
// npm, Go module and OCI probes live with their publishers, because each one
// reuses its publisher's route, credential and HTTP policy.
package memberprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Timeout bounds one probe. A probe answers a question about a single version,
// so a registry that has not answered within it is reported as unverified
// rather than left to hold the dry run.
const Timeout = 2 * time.Minute

// reasonLimit bounds, in characters, the reason a probe carries.
const reasonLimit = 300

// Reasons a held version is a conflict.
const (
	reasonNoArtifact  = "the registry already holds this version and the dry run built no artifact to compare"
	reasonOtherDigest = "the registry already holds this version with another digest"
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
// registryDigest. It is identical when that digest equals the local artifact's,
// a conflict when it differs or when the dry run built no artifact, and
// unverified when the registry advertised no digest to compare with.
func (s Subject) Held(registryDigest string) extproto.MemberProbe {
	switch {
	case s.ArtifactDigest == "":
		return s.Conflict(registryDigest, reasonNoArtifact)
	case registryDigest == "":
		return s.Unverified("the registry holds this version but advertised no digest to compare")
	case registryDigest == s.ArtifactDigest:
		probe := s.probe(extproto.MemberProbeIdentical)
		probe.RegistryDigest = registryDigest
		return probe
	default:
		return s.Conflict(registryDigest, reasonOtherDigest)
	}
}

// Conflict is the verdict for a held version the real publish cannot reuse.
// registryDigest may be empty when the registry advertised none in the form a
// probe carries.
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
// it. A 401 or a 403 names the credential, because a request without one can
// neither confirm nor rule out a private member.
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
// spaces and cuts the text to a bounded length. A registry may echo the
// request's Authorization in an error body, and a probe is printed and stored
// with the job result.
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

// Emit reports one probe as a member-probe event. The event is the dry run's
// only statement about the registry: it is never publication evidence, and the
// job that emits it still succeeds on a conflict so the orchestrator can report
// every member in one pass.
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

// envelopeKeys are the runtime-event fields an artifact event carries beside
// its payload. They are the envelope, not the probe.
var envelopeKeys = []string{"v", "type", "time", "level", "message", "id", "name", "kind", "path"}

// Decode reads the probe one member-probe artifact event carries. It is the
// inverse of Emit and the only reader of the event: everything left after the
// envelope goes through the protocol's strict parser and its validator, so a
// field or a state this build does not know is an error rather than a verdict
// dropped in silence.
func Decode(event map[string]any) (*extproto.MemberProbe, error) {
	payload := maps.Clone(event)
	for _, key := range envelopeKeys {
		delete(payload, key)
	}
	encoded, err := json.Marshal(payload)
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

// NewHTTPClient returns the client a probe sends its requests with. Ambient
// HTTP(S)_PROXY is ignored and a redirect is returned instead of followed: a
// bearer bound to the registry origin must not reach a second recipient.
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
