package deliverycli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.putnami.dev/client"
	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	registry "go.putnami.dev/protocol/registry"
)

// Environment the hosted run's launcher exports for library publication
// (the runtime provisioner, CI_LIBRARY_PUBLISH_*): the
// registry origins of this workspace's publication. The publish credential
// names these hosts and no other, whatever the repository's registry
// configuration says, and release sets are resolved and released on the Put
// origin.
const (
	publishPutURLEnv = "CI_LIBRARY_PUBLISH_PUT_URL"
	publishNPMURLEnv = "CI_LIBRARY_PUBLISH_NPM_URL"
	publishGoURLEnv  = "CI_LIBRARY_PUBLISH_GO_URL"
	publishOCIURLEnv = "CI_LIBRARY_PUBLISH_OCI_URL"
)

// nativePublicationProtocolVersion is the version Delivery's native
// publication answers carry.
const nativePublicationProtocolVersion = 1

// publicationMaxBearerSeconds is the longest lifetime Delivery mints a native
// publication bearer for.
const publicationMaxBearerSeconds = 300

// putServerReaderProfile is put-server's forwarded-bearer credential profile.
const putServerReaderProfile = "reader"

// forwardOnlyHeaders declare forward-only advance on every native publication
// call: this provider moves a forward-only channel only when Delivery's
// advance answer says the run is still the head of its default branch, so
// Delivery never refuses the run's plan as superseded.
var forwardOnlyHeaders = map[string]string{channelAdvanceHeader: channelAdvanceForwardOnly}

// publicationEndpoints are the registry origins of the hosted run's
// publication. err says why publication-v1 is not served.
type publicationEndpoints struct {
	// putBase is the Put origin; put-server's generated routes carry their
	// own /put mount.
	putBase string
	// hosts are the hosts of every origin, sorted and unique, in the
	// credential host grammar.
	hosts []string
	// absent reports that the launcher exported no publication origin: it
	// exports them only to a run that publishes. err is set too.
	absent bool
	err    error
}

// publicationEndpointsFrom reads the publication origins from the launcher's
// environment. The Put origin is required; npm, Go and OCI are optional, and
// any value that is set must be a valid origin.
func publicationEndpointsFrom(env map[string]string) publicationEndpoints {
	absent := true
	for _, name := range []string{publishPutURLEnv, publishNPMURLEnv, publishGoURLEnv, publishOCIURLEnv} {
		if strings.TrimSpace(env[name]) != "" {
			absent = false
		}
	}
	putBase, putHost, err := publicationOrigin(env[publishPutURLEnv])
	if err != nil {
		return publicationEndpoints{absent: absent, err: fmt.Errorf("%s %w", publishPutURLEnv, err)}
	}
	hosts := []string{putHost}
	for _, name := range []string{publishNPMURLEnv, publishGoURLEnv, publishOCIURLEnv} {
		if strings.TrimSpace(env[name]) == "" {
			continue
		}
		_, host, err := publicationOrigin(env[name])
		if err != nil {
			return publicationEndpoints{err: fmt.Errorf("%s %w", name, err)}
		}
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	return publicationEndpoints{putBase: putBase, hosts: slices.Compact(hosts)}
}

// publicationOrigin checks that raw is an origin a credential may be sent to:
// https, or http to a loopback host, with no user information, path, query or
// fragment. It returns the origin and its host in the credential grammar: the
// lowercase host name and a port other than the scheme's default.
func publicationOrigin(raw string) (origin, host string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("is not set")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", "", errors.New("is not an origin URL without credentials, path, query or fragment")
	}
	scheme := strings.ToLower(parsed.Scheme)
	defaultPort := "443"
	switch scheme {
	case "https":
	case "http":
		if !loopbackIngestHost(parsed.Hostname()) {
			return "", "", errors.New("uses http to a host that is not loopback")
		}
		defaultPort = "80"
	default:
		return "", "", errors.New("is not https")
	}
	host = strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" && port != defaultPort {
		host += ":" + port
	}
	if !registry.ValidCredentialHost(host) {
		return "", "", errors.New("names a host outside the credential host grammar")
	}
	return scheme + "://" + host, host, nil
}

// issuedBearer is one Delivery bearer and the window it is valid in.
type issuedBearer struct {
	bearer    string
	issuedAt  time.Time
	expiresAt time.Time
}

// halfSpent reports whether half of the bearer's lifetime has passed at now.
func (b issuedBearer) halfSpent(now time.Time) bool {
	return !now.Before(b.expiresAt.Add(-b.expiresAt.Sub(b.issuedAt) / 2))
}

// deliveryBearer is the bearer member set of Delivery's native publication
// answers.
type deliveryBearer struct {
	AccessToken string `json:"access_token"` //nolint:gosec // G117: the OAuth answer member Delivery sends
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
	JTI         string `json:"jti"`
}

// issued checks the bearer and dates it from issuedAt, the instant before the
// request was sent, cut to the second: the window never ends after Delivery's.
func (b deliveryBearer) issued(issuedAt time.Time) (issuedBearer, error) {
	switch {
	case !strings.EqualFold(b.TokenType, "Bearer"):
		return issuedBearer{}, errors.New("token_type is not Bearer")
	case !registry.ValidCredentialBearer(b.AccessToken):
		return issuedBearer{}, errors.New("access_token is not a bearer the protocol can carry")
	case b.ExpiresIn < 1 || b.ExpiresIn > publicationMaxBearerSeconds:
		return issuedBearer{}, fmt.Errorf("expires_in %d is outside 1..%d", b.ExpiresIn, publicationMaxBearerSeconds)
	case strings.TrimSpace(b.Scope) == "" || strings.TrimSpace(b.JTI) == "":
		return issuedBearer{}, errors.New("scope or jti is empty")
	}
	start := issuedAt.UTC().Truncate(time.Second)
	return issuedBearer{bearer: b.AccessToken, issuedAt: start, expiresAt: start.Add(time.Duration(b.ExpiresIn) * time.Second)}, nil
}

// publicationAuthority is what Delivery derives for the run, and the only
// plan this provider opens: the namespace, the commit, the channels with the
// immutable one, and the channels that move only forward.
type publicationAuthority struct {
	WorkspaceID      string
	RunID            string
	SourceRevision   string
	Namespace        string
	Channels         []string
	ImmutableChannel string
	ForwardOnly      []string
}

func (a publicationAuthority) forwardOnly(channel string) bool {
	return slices.Contains(a.ForwardOnly, channel)
}

func (a publicationAuthority) equal(b publicationAuthority) bool {
	return a.WorkspaceID == b.WorkspaceID && a.RunID == b.RunID && a.SourceRevision == b.SourceRevision &&
		a.Namespace == b.Namespace && slices.Equal(a.Channels, b.Channels) &&
		a.ImmutableChannel == b.ImmutableChannel && slices.Equal(a.ForwardOnly, b.ForwardOnly)
}

// deliverySourceRevision is a full lowercase SHA-1 or SHA-256 commit.
var deliverySourceRevision = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// parsePlanningGrant reads Delivery's planning answer leniently, like the
// install grant, and checks every member this provider relies on.
func parsePlanningGrant(body []byte, issuedAt time.Time) (publicationAuthority, issuedBearer, error) {
	var granted struct {
		ProtocolVersion     int      `json:"protocolVersion"`
		WorkspaceID         string   `json:"workspaceId"`
		RunID               string   `json:"runId"`
		SourceRevision      string   `json:"sourceRevision"`
		Namespace           string   `json:"namespace"`
		Channels            []string `json:"channels"`
		ImmutableChannel    string   `json:"immutableChannel"`
		ForwardOnlyChannels []string `json:"forwardOnlyChannels"`
		ExpiresUnix         int64    `json:"expiresUnix"`
		deliveryBearer
	}
	if err := json.Unmarshal(body, &granted); err != nil {
		return publicationAuthority{}, issuedBearer{}, errors.New("the body is not a JSON object")
	}
	switch {
	case granted.ProtocolVersion != nativePublicationProtocolVersion:
		return publicationAuthority{}, issuedBearer{}, fmt.Errorf("protocolVersion %d is not %d", granted.ProtocolVersion, nativePublicationProtocolVersion)
	case strings.TrimSpace(granted.WorkspaceID) == "" || strings.TrimSpace(granted.RunID) == "":
		return publicationAuthority{}, issuedBearer{}, errors.New("workspaceId or runId is empty")
	case !deliverySourceRevision.MatchString(granted.SourceRevision):
		return publicationAuthority{}, issuedBearer{}, errors.New("sourceRevision is not a full lowercase commit")
	case granted.ExpiresUnix <= issuedAt.Unix():
		return publicationAuthority{}, issuedBearer{}, errors.New("the run deadline has passed")
	}
	selector := distribution.ResolveRequest{ProtocolVersion: distribution.ProtocolVersion, Namespace: granted.Namespace, Channels: granted.Channels}
	if diagnostics := distribution.ValidateResolveRequest(&selector); diag.HasErrors(diagnostics) {
		return publicationAuthority{}, issuedBearer{}, fmt.Errorf("the namespace or the channels are not a valid selector: %s", diag.ErrorText(diagnostics))
	}
	if granted.ImmutableChannel != "" && !slices.Contains(granted.Channels, granted.ImmutableChannel) {
		return publicationAuthority{}, issuedBearer{}, errors.New("immutableChannel is not one of the channels")
	}
	var forwardOnly []string
	for _, channel := range granted.ForwardOnlyChannels {
		if !slices.Contains(granted.Channels, channel) || channel == granted.ImmutableChannel || slices.Contains(forwardOnly, channel) {
			return publicationAuthority{}, issuedBearer{}, errors.New("forwardOnlyChannels is not a set of the mutable channels")
		}
		forwardOnly = append(forwardOnly, channel)
	}
	bearer, err := granted.issued(issuedAt)
	if err != nil {
		return publicationAuthority{}, issuedBearer{}, err
	}
	return publicationAuthority{
		WorkspaceID:      granted.WorkspaceID,
		RunID:            granted.RunID,
		SourceRevision:   granted.SourceRevision,
		Namespace:        granted.Namespace,
		Channels:         slices.Clone(granted.Channels),
		ImmutableChannel: granted.ImmutableChannel,
		ForwardOnly:      forwardOnly,
	}, bearer, nil
}

// parsePublishGrant reads Delivery's answer to a plan: the plan's write
// bearer, for the plan named planDigest.
func parsePublishGrant(body []byte, planDigest string, issuedAt time.Time) (issuedBearer, error) {
	var granted struct {
		ProtocolVersion int    `json:"protocolVersion"`
		PlanDigest      string `json:"planDigest"`
		deliveryBearer
	}
	if err := json.Unmarshal(body, &granted); err != nil {
		return issuedBearer{}, errors.New("the body is not a JSON object")
	}
	switch {
	case granted.ProtocolVersion != nativePublicationProtocolVersion:
		return issuedBearer{}, fmt.Errorf("protocolVersion %d is not %d", granted.ProtocolVersion, nativePublicationProtocolVersion)
	case granted.PlanDigest != planDigest:
		return issuedBearer{}, errors.New("planDigest is not the plan's")
	}
	return granted.issued(issuedAt)
}

// parseAdvanceAnswer reads Delivery's advance answer leniently, like the
// grants: whether the run may move its forward-only channels, and the newer
// head of its default branch when Delivery names one.
func parseAdvanceAnswer(body []byte) (advance bool, head string, err error) {
	var answer struct {
		ProtocolVersion int    `json:"protocolVersion"`
		Advance         *bool  `json:"advance"`
		Head            string `json:"head"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return false, "", errors.New("the body is not a JSON object")
	}
	switch {
	case answer.ProtocolVersion != nativePublicationProtocolVersion:
		return false, "", fmt.Errorf("protocolVersion %d is not %d", answer.ProtocolVersion, nativePublicationProtocolVersion)
	case answer.Advance == nil:
		return false, "", errors.New("advance is missing")
	case answer.Head != "" && !deliverySourceRevision.MatchString(answer.Head):
		return false, "", errors.New("head is not a full lowercase commit")
	case *answer.Advance && answer.Head != "":
		return false, "", errors.New("a run that may advance has no newer head")
	}
	return *answer.Advance, answer.Head, nil
}

// withChannels returns the release request bytes body with channels in place
// of its channel list. Every other member is passed on as the engine sent it.
func withChannels(body []byte, channels []distribution.ChannelRequest) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(channels)
	if err != nil {
		return nil, err
	}
	request["channels"] = encoded
	return json.Marshal(request)
}

// publicationContractError is a put-server answer outside the release-set
// protocol: put-server answered, but its answer cannot be trusted.
type publicationContractError struct{ detail string }

func (e *publicationContractError) Error() string { return e.detail }

// newPutServerClient binds put-server's generated client to the Put origin.
// Every call forwards the bearer its context carries and sends the verbatim
// body its context carries (publicationBodyKey).
func newPutServerClient(putBase string, base *http.Client) (*putserverclient.PutClient, error) {
	return clicore.NewServiceClient[putserverclient.PutClient](putserverclient.RegisterPutClient,
		clicore.ForwardedServiceBinding(putBase, verbatimBodyClient(base), putServerReaderProfile))
}

// publicationBodyKey carries the bytes one put-server call sends.
type publicationBodyKey struct{}

// verbatimBodyClient sends the bytes a call's context carries in place of the
// generated client's re-encoding of the same document, so put-server reads
// the request the engine sent and this provider checked, member order
// included. Each call is bounded by its op's context, not by a client
// timeout.
func verbatimBodyClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	wrapped := *base
	wrapped.Timeout = 0
	wrapped.Transport = verbatimBodyTransport{base: base.Transport}
	return &wrapped
}

type verbatimBodyTransport struct{ base http.RoundTripper }

func (t verbatimBodyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	body, ok := request.Context().Value(publicationBodyKey{}).([]byte)
	if !ok || request.Body == nil {
		return base.RoundTrip(request)
	}
	_ = request.Body.Close()
	request = request.Clone(request.Context())
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	request.ContentLength = int64(len(body))
	return base.RoundTrip(request)
}

// putResolve resolves the channels of the request whose bytes body carries
// and returns put-server's answer once it is bound to the request.
func (p *publisher) putResolve(ctx context.Context, bearer string, body []byte, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
	var in putserverclient.CreatePutReleaseSetsResolveInput
	if err := json.Unmarshal(body, &in.Body); err != nil {
		return nil, &publicationContractError{detail: "the resolve request does not fit put-server's contract"}
	}
	answer, err := p.putExchange(ctx, bearer, body, func(ctx context.Context) (any, error) {
		return p.put.CreatePutReleaseSetsResolve(ctx, in)
	})
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(answer)
	if err != nil {
		return nil, &publicationContractError{detail: "put-server's resolve answer cannot be encoded"}
	}
	response, diagnostics := distribution.ParseAndValidateResolveResponse(data)
	if response == nil || diag.HasErrors(diagnostics) {
		return nil, &publicationContractError{detail: "put-server's resolve answer does not follow the protocol: " + diag.ErrorText(diagnostics)}
	}
	if response.Heads == nil {
		return nil, &publicationContractError{detail: "put-server answered a channel resolve with a release"}
	}
	if diagnostics := distribution.ValidateResolveExchange(request, response); diag.HasErrors(diagnostics) {
		return nil, &publicationContractError{detail: "put-server's resolve answer does not answer the request: " + diag.ErrorText(diagnostics)}
	}
	return response, nil
}

// putRelease releases the request whose bytes body carries and returns
// put-server's answer once it is bound to the request: a conflict outcome
// included, which is an answer, not a failure.
func (p *publisher) putRelease(ctx context.Context, bearer string, body []byte, request *distribution.ReleaseRequest) (*distribution.ReleaseResponse, error) {
	var in putserverclient.CreatePutReleaseSetsReleaseInput
	if err := json.Unmarshal(body, &in.Body); err != nil {
		return nil, &publicationContractError{detail: "the release request does not fit put-server's contract"}
	}
	answer, err := p.putExchange(ctx, bearer, body, func(ctx context.Context) (any, error) {
		return p.put.CreatePutReleaseSetsRelease(ctx, in)
	})
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(answer)
	if err != nil {
		return nil, &publicationContractError{detail: "put-server's release answer cannot be encoded"}
	}
	response, diagnostics := distribution.ParseAndValidateReleaseResponse(data)
	if response == nil || diag.HasErrors(diagnostics) {
		return nil, &publicationContractError{detail: "put-server's release answer does not follow the protocol: " + diag.ErrorText(diagnostics)}
	}
	if diagnostics := distribution.ValidateReleaseExchange(request, response); diag.HasErrors(diagnostics) {
		return nil, &publicationContractError{detail: "put-server's release answer does not answer the request: " + diag.ErrorText(diagnostics)}
	}
	return response, nil
}

// putExchange sends one call with bearer and the verbatim body, and once more
// after a failure put-server did not answer (a transport failure, a timeout,
// a 408, a 429 or a 5xx) while ctx leaves room for a second attempt. Both
// release-set calls may be repeated: resolve reads, and a release whose first
// attempt applied answers already-current.
func (p *publisher) putExchange(ctx context.Context, bearer string, body []byte, call func(context.Context) (any, error)) (any, error) {
	ctx = client.WithForwardedUserToken(ctx, bearer)
	ctx = context.WithValue(ctx, publicationBodyKey{}, body)
	for attempt := 0; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, p.putAttempt)
		answer, err := call(attemptCtx)
		cancel()
		if err == nil {
			return answer, nil
		}
		if attempt > 0 || !putUnanswered(err) || !p.roomForRetry(ctx) {
			return nil, err
		}
		if p.sleep(ctx, p.retryWait) != nil {
			return nil, err
		}
	}
}

// putUnanswered reports whether a failed call may not have reached a
// decision: no HTTP answer, or one that states put-server could not decide.
func putUnanswered(err error) bool {
	status := clicore.ServiceStatus(err)
	return status == 0 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// roomForRetry reports whether ctx leaves time for a wait and a useful second
// attempt.
func (p *publisher) roomForRetry(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > p.retryWait+publicationMinAttempt
}

// putRefusal maps a failed put-server call to the refusal its op answers, and
// reports whether the failure is definitive: put-server decided it, so a
// release it refused moved no channel. Any other failure leaves a release's
// outcome unknown.
func (p *publisher) putRefusal(op string, err error) (*registry.CredentialRefusal, bool) {
	var contract *publicationContractError
	if errors.As(err, &contract) {
		p.diagnose("%s: %s", op, contract.detail)
		return &registry.CredentialRefusal{Code: refusalPublicationUnavailable, Message: "put-server answered " + op + " outside the release-set protocol"}, false
	}
	status := clicore.ServiceStatus(err)
	remote := p.safeText(clicore.ServiceMessage(err))
	var missing *putserverclient.CreatePutReleaseSetsReleaseMemberMissingError
	var immutable *putserverclient.CreatePutReleaseSetsReleaseRegistryReleaseSetChannelImmutableError
	var conflict *putserverclient.CreatePutReleaseSetsReleaseConflictError
	var code, message string
	definitive := true
	switch {
	case errors.As(err, &missing):
		code, message = registry.RefusalArtifactMissing, "a member of the released set has no artifact in its registry"
	case errors.As(err, &immutable):
		code, message = registry.RefusalChannelImmutable, "an immutable channel of the release already has a head"
	case errors.As(err, &conflict):
		code, message = registry.RefusalConflict, "put-server refused the release as a conflict"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		code, message = registry.RefusalNamespaceForbidden, fmt.Sprintf("put-server refused the %s bearer (HTTP %d)", op, status)
	case putUnanswered(err):
		code, definitive = refusalPublicationUnavailable, false
		message = "put-server did not answer " + op
		if status != 0 {
			message = fmt.Sprintf("put-server did not answer %s (last answer HTTP %d)", op, status)
		}
	default:
		code, message = refusalPublicationRefused, fmt.Sprintf("put-server refused %s (HTTP %d)", op, status)
	}
	if status != 0 {
		p.diagnose("%s: put-server answered HTTP %d (%s)", op, status, code)
	} else {
		p.diagnose("%s: put-server did not answer: %v", op, err)
	}
	if remote != "" && definitive {
		message += ": " + remote
	}
	return &registry.CredentialRefusal{Code: code, Message: p.safeText(message)}, definitive
}
