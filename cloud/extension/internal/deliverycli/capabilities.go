package deliverycli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.putnami.dev/client"

	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Delivery's run capability routes under the ingest base
// (SessionReporterIngestURLEnv), mirrored from the delivery API's records contract (the
// extension does not import server packages). Both are authenticated by the
// run credential alone and take an empty JSON object.
const (
	installCapabilityPath = "/capabilities/install"
	cacheCapabilityPath   = "/capabilities/cache"
)

// Delivery's native publication routes under the same ingest base, mirrored
// from the delivery API's records contract. All are authenticated by the run
// credential. planning takes an empty JSON object and answers the run's
// publication authority with a read-only release-set bearer; publish takes the
// publication plan, freezes its digest on the run, and answers the plan's
// write bearer; advance takes an empty JSON object and answers whether the
// run may move its forward-only channels now.
const (
	nativePublicationPlanningPath = "/native-publication/planning"
	nativePublicationPublishPath  = "/native-publication/publish"
	nativePublicationAdvancePath  = "/native-publication/advance"

	// channelAdvanceHeader with channelAdvanceForwardOnly states that the
	// caller moves a forward-only channel only when Delivery's advance answer
	// allows it. Delivery then answers the forward-only channels in the
	// planning answer and leaves the move to the caller.
	channelAdvanceHeader      = "X-Putnami-Channel-Advance"
	channelAdvanceForwardOnly = "forward-only"
)

// capabilityProtocolVersion is the version a granted capability body carries.
const capabilityProtocolVersion = 1

// Bounds of one capability exchange. Delivery's route runs a serial chain
// (subscription, track, planning token, release set, Identity) under its own
// 11 s deadline, so one 12 s attempt still reads Delivery's typed answer. Two
// attempts with 0.5 s between them end within 24.5 s, so a provider answers
// inside the engine's 30 s op timeout even when Delivery never answers.
const (
	capabilityAttemptTimeout = 12 * time.Second
	capabilityResponseBytes  = 64 << 10
)

// capabilityRetryBackoff is the wait before each retry of an unavailable
// answer: one retry, after 0.5 s.
var capabilityRetryBackoff = []time.Duration{500 * time.Millisecond}

// capabilityOutcome classifies Delivery's answer to one capability request.
type capabilityOutcome int

const (
	// capabilityGranted is a 200 or 201 whose body carries the capability.
	capabilityGranted capabilityOutcome = iota + 1
	// capabilityAbsent is a 204: the run holds no such capability.
	capabilityAbsent
	// capabilityRefused is a final 4xx answer other than 408 and 429:
	// Delivery's policy refused it. Its code, whatever it is, is passed on;
	// the caller never matches a list of known codes.
	capabilityRefused
	// capabilityUnavailable is a 5xx, 408 or 429, a transport failure or a
	// timeout on every attempt, or an ingest base that cannot carry a
	// credential.
	capabilityUnavailable
)

// capabilityAnswer is one classified exchange. body holds the response bytes
// of a granted or refused answer; it never holds the run credential, which
// only travels in the request header.
type capabilityAnswer struct {
	outcome capabilityOutcome
	status  int
	body    []byte
}

// capabilityCaller posts to one Delivery capability route with the run
// credential, through delivery-api's generated client.
// It is safe for concurrent use.
type capabilityCaller struct {
	// client is the HTTP client the generated client sends through.
	client  *http.Client
	ingest  string
	backoff []time.Duration
	// sleep waits between attempts; tests replace it to run without delay.
	sleep func(ctx context.Context, d time.Duration) error

	mu sync.Mutex
	// bound holds one generated client per set of binding headers.
	bound map[string]*deliveryapiclient.DeliveryClient
}

// newCapabilityCaller binds the caller to the launcher-exported ingest base.
// An unusable base is not an error here: every call then answers
// capabilityUnavailable, so the provider still answers each op.
func newCapabilityCaller(ingestURL string) *capabilityCaller {
	// The generated client sends through a copy of the default client whose
	// own timeout is the attempt bound, as the declared operation's is.
	bounded := *http.DefaultClient
	bounded.Timeout = capabilityAttemptTimeout
	return &capabilityCaller{
		client:  &bounded,
		ingest:  strings.TrimSpace(ingestURL),
		backoff: capabilityRetryBackoff,
		sleep:   sleepContext,
	}
}

// configured reports whether the ingest base can carry a run credential to
// Delivery's capability routes.
func (c *capabilityCaller) configured() bool {
	_, err := capabilityServiceBase(c.ingest)
	return err == nil
}

// call POSTs `{}` to route and classifies the answer. A transport failure, a
// timeout, a 5xx, a 408 and a 429 are retried at most len(backoff) more times;
// every other answer is final.
func (c *capabilityCaller) call(ctx context.Context, route, runCredential string) capabilityAnswer {
	return c.post(ctx, route, runCredential, []byte("{}"), nil)
}

// post POSTs body to route with the extra headers and classifies the answer,
// with the retries of call. body must be a request the route may receive more
// than once: every route this caller reaches is idempotent for the run.
func (c *capabilityCaller) post(ctx context.Context, route, runCredential string, body []byte, headers map[string]string) capabilityAnswer {
	if runCredential == "" {
		return capabilityAnswer{outcome: capabilityUnavailable}
	}
	delivery, err := c.delivery(headers)
	if err != nil {
		return capabilityAnswer{outcome: capabilityUnavailable}
	}
	var last capabilityAnswer
	for attempt := 0; ; attempt++ {
		last = c.attempt(ctx, delivery, route, runCredential, body)
		if last.outcome != capabilityUnavailable || attempt >= len(c.backoff) || ctx.Err() != nil {
			return last
		}
		if err := c.sleep(ctx, c.backoff[attempt]); err != nil {
			return last
		}
	}
}

// delivery returns the generated client bound to the ingest base's origin,
// carrying headers on every call. headers are static per caller: the
// publication provider's channel-advance declaration.
func (c *capabilityCaller) delivery(headers map[string]string) (*deliveryapiclient.DeliveryClient, error) {
	base, err := capabilityServiceBase(c.ingest)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(headers))
	for name, value := range headers {
		names = append(names, name+": "+value)
	}
	slices.Sort(names)
	key := strings.Join(names, "\n")
	c.mu.Lock()
	defer c.mu.Unlock()
	if bound, ok := c.bound[key]; ok {
		return bound, nil
	}
	binding := clicore.ForwardedServiceBinding(base, capabilityHTTPClient(c.client), deliveryRunCredentialProfile)
	if len(headers) > 0 {
		binding.Headers = maps.Clone(headers)
	}
	bound, err := clicore.NewServiceClient[deliveryapiclient.DeliveryClient](deliveryapiclient.RegisterDeliveryClient, binding)
	if err != nil {
		return nil, err
	}
	if c.bound == nil {
		c.bound = map[string]*deliveryapiclient.DeliveryClient{}
	}
	c.bound[key] = bound
	return bound, nil
}

// attempt makes one generated call and classifies the answer Delivery sent.
// The classification reads the status and the bytes the transport saw, not
// the generated verdict: Delivery's refusal code sits outside the first-party
// error envelope, and each caller proves a grant over its exact bytes.
func (c *capabilityCaller) attempt(ctx context.Context, delivery *deliveryapiclient.DeliveryClient, route, runCredential string, payload []byte) capabilityAnswer {
	attemptCtx, cancel := context.WithTimeout(ctx, capabilityAttemptTimeout)
	defer cancel()
	seen := &capabilityExchange{}
	callCtx := client.WithForwardedUserToken(context.WithValue(attemptCtx, capabilityExchangeKey{}, seen), runCredential)
	// A call no answer reached — a transport failure, a timeout, an open
	// circuit — fails before the transport records a status.
	if err := sendCapability(callCtx, delivery, route, payload); err != nil && seen.status == 0 {
		return capabilityAnswer{outcome: capabilityUnavailable}
	}
	if seen.status == 0 || seen.readErr {
		return capabilityAnswer{outcome: capabilityUnavailable, status: seen.status}
	}
	answer := capabilityAnswer{status: seen.status, body: seen.body}
	switch {
	case seen.status == http.StatusOK || seen.status == http.StatusCreated:
		answer.outcome = capabilityGranted
	case seen.status == http.StatusNoContent:
		answer.outcome = capabilityAbsent
		answer.body = nil
	case seen.status == http.StatusRequestTimeout || seen.status == http.StatusTooManyRequests:
		answer.outcome = capabilityUnavailable
	case seen.status >= 400 && seen.status < 500:
		answer.outcome = capabilityRefused
	default:
		answer.outcome = capabilityUnavailable
	}
	return answer
}

// sendCapability makes the generated call of route. The capability routes but
// the plan take exactly {}; the plan's own bytes become the generated plan, so
// Delivery decodes the same values and recomputes the same digest.
func sendCapability(ctx context.Context, delivery *deliveryapiclient.DeliveryClient, route string, payload []byte) error {
	var err error
	switch route {
	case installCapabilityPath:
		_, err = delivery.CreateV1DeliveryRecordsCapabilitiesInstall(ctx, deliveryapiclient.CreateV1DeliveryRecordsCapabilitiesInstallInput{})
	case cacheCapabilityPath:
		_, err = delivery.CreateV1DeliveryRecordsCapabilitiesCache(ctx, deliveryapiclient.CreateV1DeliveryRecordsCapabilitiesCacheInput{})
	case nativePublicationPlanningPath:
		_, err = delivery.CreateV1DeliveryRecordsNativePublicationPlanning(ctx, deliveryapiclient.CreateV1DeliveryRecordsNativePublicationPlanningInput{})
	case nativePublicationAdvancePath:
		_, err = delivery.CreateV1DeliveryRecordsNativePublicationAdvance(ctx, deliveryapiclient.CreateV1DeliveryRecordsNativePublicationAdvanceInput{})
	case nativePublicationPublishPath:
		var plan deliveryapiclient.NativePublicationPlan
		if err := json.Unmarshal(payload, &plan); err != nil {
			return err
		}
		_, err = delivery.CreateV1DeliveryRecordsNativePublicationPublish(ctx, deliveryapiclient.CreateV1DeliveryRecordsNativePublicationPublishInput{Body: plan})
	default:
		err = errors.New("unknown capability route " + route)
	}
	return err
}

// deliveryRunCredentialProfile is delivery-api's forwarded bearer profile;
// the run credential rides it.
const deliveryRunCredentialProfile = "user"

// capabilityServiceBase is the origin the generated client binds to: the
// credential-safe ingest base without Delivery's ingest path, which every
// generated capability route carries. The launcher always exports the base
// with that path (delivery-api's run contract appends it).
func capabilityServiceBase(raw string) (string, error) {
	base, err := credentialIngestBase(raw)
	if err != nil {
		return "", err
	}
	origin, ok := strings.CutSuffix(base, recordsIngestPath)
	if !ok {
		return "", errors.New("the ingest base does not end with " + recordsIngestPath)
	}
	return origin, nil
}

// capabilityExchange is what the transport saw of one capability answer: its
// status and up to capabilityResponseBytes of its body.
type capabilityExchange struct {
	status  int
	body    []byte
	readErr bool
}

type capabilityExchangeKey struct{}

// capabilityHTTPClient sends through base's transport and records each
// answer's status and bytes in the call's capabilityExchange. The generated
// client still reads the answer: the transport hands it the same bytes.
func capabilityHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	wrapped := *base
	wrapped.Transport = capabilityTransport{base: base.Transport}
	return &wrapped
}

type capabilityTransport struct{ base http.RoundTripper }

func (t capabilityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	seen, ok := request.Context().Value(capabilityExchangeKey{}).(*capabilityExchange)
	if !ok {
		return response, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, capabilityResponseBytes))
	seen.status, seen.body, seen.readErr = response.StatusCode, body, readErr != nil
	response.Body = capabilityBody{Reader: io.MultiReader(bytes.NewReader(body), response.Body), Closer: response.Body}
	return response, nil
}

type capabilityBody struct {
	io.Reader
	io.Closer
}

// credentialIngestBase returns the ingest base without its trailing slash when
// it may carry a credential: https, or http to a loopback host, with no user
// information, query or fragment.
func credentialIngestBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("the ingest base is not set")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("the ingest base is not an absolute URL without credentials, query or fragment")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !loopbackIngestHost(parsed.Hostname()) {
			return "", errors.New("the ingest base uses http to a host that is not loopback")
		}
	default:
		return "", errors.New("the ingest base is not https")
	}
	return strings.TrimRight(raw, "/"), nil
}

func loopbackIngestHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// capabilityRefusal reads Delivery's refusal body `{error, message, code}`
// leniently: a body that is not that object yields empty values.
func capabilityRefusal(body []byte) (code, message string) {
	var refusal struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(body, &refusal) != nil {
		return "", ""
	}
	message = refusal.Message
	if message == "" {
		if text, ok := refusal.Error.(string); ok {
			message = text
		}
	}
	return strings.TrimSpace(refusal.Code), message
}

// boundedDiagnosticText makes text safe to show: every secret is replaced, a
// control character, U+FFFD and invalid UTF-8 become a space, and the result
// is cut at a rune boundary to at most limit bytes.
func boundedDiagnosticText(text string, limit int, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "<redacted>")
		}
	}
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(text, " ") {
		if r < ' ' || r == 0x7f || r == utf8.RuneError {
			r = ' '
		}
		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
