package events

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.putnami.dev/errors"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// TransportKindPubSub selects DirectPubSubTransport from events.transport. The
// plugin builds it from the events.pubsub block unless a provider module
// registered its own factory for this kind.
const TransportKindPubSub = "pubsub"

const (
	// googlePubSubPublishTimeout bounds one publish: every attempt, with its
	// credential lookup, request and answer, and the waits between attempts.
	googlePubSubPublishTimeout = 10 * time.Second
	// googlePubSubAttemptTimeout bounds one attempt. It is half the publish
	// limit, so an attempt that hangs still leaves time for another one.
	googlePubSubAttemptTimeout = 5 * time.Second
	// googlePubSubMaxAttempts is the number of attempts of one publish: the
	// first one and at most three retries.
	googlePubSubMaxAttempts = 4
	// googlePubSubRetryBaseBackoff is the wait before the first retry. Each
	// further retry doubles it, up to googlePubSubRetryMaxBackoff. retryBackoff
	// adds 0 to 25% of jitter.
	googlePubSubRetryBaseBackoff = 100 * time.Millisecond
	googlePubSubRetryMaxBackoff  = 2 * time.Second
	// googlePubSubMaxResponseBytes bounds how much of a publish answer the
	// transport reads. An answer holds message ids or an error.
	googlePubSubMaxResponseBytes = 1 << 20
	// googlePubSubMaxErrorMessageBytes bounds the answer text an error keeps.
	googlePubSubMaxErrorMessageBytes = 1 << 10
)

// googlePubSubEndpoint is the Pub/Sub REST API root. Tests replace it.
var googlePubSubEndpoint = "https://pubsub.googleapis.com/"

// googlePubSubScopes are the OAuth scopes requested from Application Default
// Credentials: the Google Cloud scope and the Pub/Sub scope.
var googlePubSubScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/pubsub",
}

// httpDoer sends an HTTP request. *http.Client implements it.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// GooglePubSubPublishError reports a publish that Google Cloud Pub/Sub answered
// with a status outside 2xx. StatusCode is the code of the error body, or the
// HTTP status when the body carries none. Message holds at most 1 KiB of the
// answer.
type GooglePubSubPublishError struct {
	Topic      string
	StatusCode int
	Status     string
	Message    string
}

func (e *GooglePubSubPublishError) Error() string {
	var b strings.Builder
	b.WriteString(string(codeEventsGooglePubSub))
	b.WriteString(": publish to ")
	b.WriteString(e.Topic)
	b.WriteString(": status ")
	b.WriteString(strconv.Itoa(e.StatusCode))
	if e.Status != "" {
		b.WriteString(" ")
		b.WriteString(e.Status)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// GooglePubSubCredentialError reports a publish that stopped before any request
// was sent, because no valid access token was available. It never carries the
// credential provider's error text, which can hold credential material.
type GooglePubSubCredentialError struct {
	Topic string
	// Invalid is true when the credentials returned a malformed token, and false
	// when they returned none in time.
	Invalid bool
}

func (e *GooglePubSubCredentialError) Error() string {
	if e.Invalid {
		return string(codeEventsGooglePubSub) + ": publish to " + e.Topic + ": the credentials returned an invalid access token"
	}
	return string(codeEventsGooglePubSub) + ": publish to " + e.Topic + ": no access token from the credentials"
}

// DirectPubSubTransport publishes envelopes to Google Cloud Pub/Sub through its
// REST API, authenticated with Application Default Credentials: on Google Cloud,
// the runtime service account from the metadata server.
//
// It is publish-only: Subscribe fails, so handlers receive through push
// delivery (events.delivery: push). It implements DurablePublishTransport on
// the PublishRouteDirectPubSub route.
type DirectPubSubTransport struct {
	delegate *GooglePubSubTransport
}

var _ DurablePublishTransport = (*DirectPubSubTransport)(nil)

// NewDirectPubSubTransport builds a Google Cloud Pub/Sub transport for the
// binding's project and topic template, independent of the process-wide events
// transport. Use it for a channel that must publish to Pub/Sub whatever
// events.transport selects.
//
// It finds Application Default Credentials when it is called, and fails when
// there are none. It never falls back to another credential or transport.
func NewDirectPubSubTransport(binding PubSubBinding) (*DirectPubSubTransport, error) {
	if err := validatePubSubBinding(binding); err != nil {
		return nil, err
	}
	credentials, err := google.FindDefaultCredentials(context.Background(), googlePubSubScopes...)
	if err != nil {
		return nil, errors.Wrapf(err, codeEventsGooglePubSub, "find Application Default Credentials")
	}
	client := &googlePubSubRESTClient{
		client:       &http.Client{},
		endpoint:     googlePubSubEndpoint,
		projectID:    binding.ProjectID,
		tokens:       credentials.TokenSource,
		quotaProject: quotaProjectOf(credentials.JSON),
	}
	return newDirectPubSubTransport(binding, client), nil
}

func validatePubSubBinding(binding PubSubBinding) error {
	if binding.ProjectID == "" {
		return errors.New(codeEventsConfig, "events.pubsub.projectId is required for the pubsub transport",
			errors.String("transport", TransportKindPubSub))
	}
	return nil
}

func newDirectPubSubTransport(binding PubSubBinding, client *googlePubSubRESTClient) *DirectPubSubTransport {
	return &DirectPubSubTransport{delegate: NewGooglePubSubTransport(GooglePubSubTransportConfig{
		Client:    client,
		TopicName: PubSubTopicNamer(binding.TopicTemplate),
	})}
}

// Publish sends the envelope as one Pub/Sub message: the JSON envelope as data,
// the envelope attributes as message attributes, and the envelope key as the
// ordering key. One publish takes at most 10 seconds.
//
// Publish retries a transient failure: a 408, 429, 500, 502, 503 or 504 answer,
// a network failure (connection refused or reset, end of stream), an attempt
// that exceeds its 5 second limit, and a failed access token lookup, unless the
// token endpoint answered a status outside that list. It makes at most 4
// attempts. It waits 100 ms before the first retry, then doubles the wait up to
// 2 s, and adds 0 to 25% of jitter. On a 429 or 503 answer it waits for the
// Retry-After header when that is longer. It stops when the next wait does not
// fit in what is left of the 10 seconds, and when ctx ends. It then returns the
// error of the last attempt, which is the one ClassifyPublishError reads.
// When an earlier attempt ended without telling whether Pub/Sub published the
// message, the outcome stays ambiguous whatever the last attempt says.
//
// Publish never retries another 4xx answer, a malformed access token, or a 2xx
// answer it cannot read: Pub/Sub accepted that publish.
//
// A retry after an attempt whose outcome is unknown (a timeout, a connection
// reset, a 5xx answer) can publish the same message twice. Every attempt sends
// the same bytes, so both copies carry the same envelope id in their JSON data.
// The id is not a message attribute: the attributes are only those of the
// envelope. Pub/Sub delivers at least once whatever the publisher does, so a
// handler must already tolerate a duplicate, and it deduplicates on the
// envelope id.
//
// The attempts of one Publish call run one after the other, so a retry does not
// reorder that call's message against a later call with the same ordering key.
// A duplicate from an attempt of unknown outcome can still reach Pub/Sub after
// the message of a later call. Calls that run at the same time have no order.
func (t *DirectPubSubTransport) Publish(ctx context.Context, envelope Envelope) error {
	return t.delegate.Publish(ctx, envelope)
}

// Subscribe always fails: the transport is publish-only. Set
// events.delivery: push so handlers receive through the push receiver, which
// never subscribes them to the transport.
func (t *DirectPubSubTransport) Subscribe(definition *HandlerDefinition) error {
	topic := ""
	if definition != nil {
		topic = definition.Topic
	}
	return errors.New(codeEventsConfig, "pubsub transport is publish-only; set events.delivery: push",
		errors.String("transport", TransportKindPubSub), errors.String("topic", topic))
}

// Start implements Transport.
func (t *DirectPubSubTransport) Start(ctx context.Context) error { return t.delegate.Start(ctx) }

// Stop implements Transport.
func (t *DirectPubSubTransport) Stop(ctx context.Context) error { return t.delegate.Stop(ctx) }

// ActivePublishRoute returns PublishRouteDirectPubSub with no topology
// generation.
func (t *DirectPubSubTransport) ActivePublishRoute() (string, string) {
	return PublishRouteDirectPubSub, ""
}

// ContextForPublishRoute returns ctx when the persisted route is
// PublishRouteDirectPubSub with no generation, and ErrStalePublishRoute
// otherwise.
func (t *DirectPubSubTransport) ContextForPublishRoute(ctx context.Context, transport, generation string) (context.Context, error) {
	return CheckPublishRoute(ctx, PublishRouteDirectPubSub, "", transport, generation)
}

// ClassifyPublishError maps a publish error to a durable publish outcome.
// Publish returns the error of its last attempt, so the outcome describes that
// attempt, after the retries. A publish with an earlier attempt of unknown
// outcome is ambiguous even when its last attempt is retryable or permanent:
//
//   - Retryable: Pub/Sub answered 429 or 503, or no access token was available
//     in time, so nothing was sent.
//   - Permanent: Pub/Sub answered another 4xx, except 408, 409 and 499, or the
//     credentials returned a malformed token.
//   - Ambiguous: everything else, including 408, 409, 499, 500, 502 and 504
//     answers, a network failure, a timeout after the request left, an
//     unreadable answer, and a stale persisted route. Pub/Sub may have
//     published the message.
func (t *DirectPubSubTransport) ClassifyPublishError(err error) string {
	return classifyDirectPubSubPublishError(err)
}

func classifyDirectPubSubPublishError(err error) string {
	if err == nil {
		return ""
	}
	var unknownErr *googlePubSubUnknownOutcomeError
	if stderrors.As(err, &unknownErr) {
		return PublishOutcomeAmbiguous
	}
	var credentialErr *GooglePubSubCredentialError
	if stderrors.As(err, &credentialErr) {
		if credentialErr.Invalid {
			return PublishOutcomePermanent
		}
		return PublishOutcomeRetryable
	}
	var publishErr *GooglePubSubPublishError
	if !stderrors.As(err, &publishErr) {
		return PublishOutcomeAmbiguous
	}
	switch code := publishErr.StatusCode; {
	case code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable:
		return PublishOutcomeRetryable
	case code == http.StatusRequestTimeout || code == http.StatusConflict || code == 499:
		// 499 is Google's CANCELED: the request was received and may have run.
		return PublishOutcomeAmbiguous
	case code >= http.StatusBadRequest && code < http.StatusInternalServerError:
		return PublishOutcomePermanent
	default:
		return PublishOutcomeAmbiguous
	}
}

// PubSubTopicNamer returns the function that maps a logical topic name to a
// Pub/Sub topic id through template, where "{topic}" stands for the logical
// name (for example "events-{topic}"). An empty template stands for the logical
// name alone. The result is always sanitized with PubSubResourceID, so a
// provisioner that applies the same sanitizer to the same template creates the
// topic the transport publishes to.
func PubSubTopicNamer(template string) func(string) string {
	if template == "" {
		return PubSubResourceID
	}
	return func(topic string) string {
		return PubSubResourceID(strings.ReplaceAll(template, "{topic}", topic))
	}
}

// PubSubResourceID turns s into a valid Pub/Sub resource id: lowercase, only
// [a-z0-9-_.], 3 to 255 characters, starting with a letter and not with
// "goog". Every other character becomes "-" and leading and trailing
// separators are trimmed. An id that is too short, does not start with a
// letter or starts with "goog" gets the prefix "e-"; an empty one becomes
// "e-e". A valid id is returned unchanged.
func PubSubResourceID(s string) string {
	const maxLen = 255
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-_.")
	if len(id) > maxLen {
		id = strings.Trim(id[:maxLen], "-_.")
	}
	if len(id) < 3 || id[0] < 'a' || id[0] > 'z' || strings.HasPrefix(id, "goog") {
		id = "e-" + id
		if len(id) > maxLen {
			id = strings.TrimRight(id[:maxLen], "-_.")
		}
	}
	for len(id) < 3 {
		id += "e"
	}
	return id
}

// googlePubSubRESTClient implements GooglePubSubClient over the Pub/Sub REST
// API (projects.topics.publish).
type googlePubSubRESTClient struct {
	client    httpDoer
	endpoint  string
	projectID string
	// tokens supplies the access token of each request. Nil sends requests
	// without credentials (tests against an in-memory endpoint).
	tokens oauth2.TokenSource
	// quotaProject, when set, bills requests to that project.
	quotaProject string
	// retry bounds the attempts of one publish. The zero value is the default
	// policy.
	retry googlePubSubRetryPolicy
}

func (c *googlePubSubRESTClient) Topic(name string) GooglePubSubTopic {
	return &googlePubSubRESTTopic{client: c, name: "projects/" + c.projectID + "/topics/" + name}
}

func (c *googlePubSubRESTClient) Subscription(name string) GooglePubSubSubscription {
	return &googlePubSubPushOnlySubscription{name: name}
}

type googlePubSubRESTTopic struct {
	client *googlePubSubRESTClient
	name   string
}

type googlePubSubRESTMessage struct {
	Data        string            `json:"data,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	OrderingKey string            `json:"orderingKey,omitempty"`
}

type googlePubSubRESTPublishRequest struct {
	Messages []googlePubSubRESTMessage `json:"messages"`
}

type googlePubSubRESTPublishResponse struct {
	MessageIDs []string `json:"messageIds"`
}

type googlePubSubRESTErrorReply struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// googlePubSubRetryPolicy bounds the attempts of one publish. A zero field
// stands for its default; tests replace backoff, sleep and now to run without
// real waits.
type googlePubSubRetryPolicy struct {
	maxAttempts    int
	attemptTimeout time.Duration
	budget         time.Duration
	// backoff returns the wait before retry number retry, which starts at 1.
	backoff func(retry int) time.Duration
	// sleep waits for d, or returns the context error when ctx ends first.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

func (p googlePubSubRetryPolicy) withDefaults() googlePubSubRetryPolicy {
	if p.maxAttempts <= 0 {
		p.maxAttempts = googlePubSubMaxAttempts
	}
	if p.attemptTimeout <= 0 {
		p.attemptTimeout = googlePubSubAttemptTimeout
	}
	if p.budget <= 0 {
		p.budget = googlePubSubPublishTimeout
	}
	if p.backoff == nil {
		p.backoff = func(retry int) time.Duration {
			return retryBackoff(retry, googlePubSubRetryBaseBackoff, googlePubSubRetryMaxBackoff)
		}
	}
	if p.sleep == nil {
		p.sleep = sleepWithContext
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// googlePubSubAttempt is the result of one publish attempt.
type googlePubSubAttempt struct {
	err error
	// transient is true when the same request may succeed if sent again.
	transient bool
	// retryAfter is the wait a 429 or 503 answer asked for, or 0.
	retryAfter time.Duration
}

// Publish sends the message and retries a transient failure within the retry
// policy. It returns the error of the last attempt, wrapped in
// googlePubSubUnknownOutcomeError when an earlier attempt may have published.
func (t *googlePubSubRESTTopic) Publish(ctx context.Context, message GooglePubSubPublishMessage) error {
	// The REST API carries the payload base64-encoded.
	body, err := json.Marshal(googlePubSubRESTPublishRequest{Messages: []googlePubSubRESTMessage{{
		Data:        base64.StdEncoding.EncodeToString(message.Data),
		Attributes:  message.Attributes,
		OrderingKey: message.OrderingKey,
	}}})
	if err != nil {
		return errors.Wrap(err, codeEventsGooglePubSub, errors.String("phase", "marshal"), errors.String("topic", t.name))
	}
	endpoint, err := t.publishURL()
	if err != nil {
		return errors.Wrap(err, codeEventsGooglePubSub, errors.String("phase", "url"), errors.String("topic", t.name))
	}

	policy := t.client.retry.withDefaults()
	deadline := policy.now().Add(policy.budget)
	budgetCtx, cancel := context.WithTimeout(ctx, policy.budget)
	defer cancel()
	// unknown is true once an attempt ended without telling whether Pub/Sub
	// published the message.
	unknown := false
	fail := func(err error) error {
		if unknown && classifyDirectPubSubPublishError(err) != PublishOutcomeAmbiguous {
			return &googlePubSubUnknownOutcomeError{last: err}
		}
		return err
	}
	for attempt := 1; ; attempt++ {
		result := t.publishOnce(budgetCtx, policy.attemptTimeout, endpoint.String(), body)
		if result.err == nil {
			return nil
		}
		if !result.transient || attempt >= policy.maxAttempts || ctx.Err() != nil {
			return fail(result.err)
		}
		wait := max(policy.backoff(attempt), result.retryAfter)
		// A wait that does not fit in the budget ends the publish now, with the
		// error of this attempt and not with a deadline error.
		if wait >= deadline.Sub(policy.now()) {
			return fail(result.err)
		}
		if policy.sleep(ctx, wait) != nil {
			return fail(result.err)
		}
		unknown = unknown || classifyDirectPubSubPublishError(result.err) == PublishOutcomeAmbiguous
	}
}

// googlePubSubUnknownOutcomeError is the error of a publish whose last attempt
// sent nothing or was refused, after an earlier attempt of the same publish
// ended without an answer. Pub/Sub may have published the message, so the
// outcome is ambiguous whatever the last attempt says.
type googlePubSubUnknownOutcomeError struct{ last error }

func (e *googlePubSubUnknownOutcomeError) Error() string {
	return e.last.Error() + "; an earlier attempt may have published the message"
}

func (e *googlePubSubUnknownOutcomeError) Unwrap() error { return e.last }

// publishOnce makes one attempt: it fetches the access token, sends the request
// and reads the answer, within timeout.
func (t *googlePubSubRESTTopic) publishOnce(ctx context.Context, timeout time.Duration, endpoint string, body []byte) googlePubSubAttempt {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return googlePubSubAttempt{err: errors.Wrap(err, codeEventsGooglePubSub, errors.String("phase", "request"), errors.String("topic", t.name))}
	}
	req.Header.Set("Content-Type", "application/json")
	if t.client.quotaProject != "" {
		req.Header.Set("X-Goog-User-Project", t.client.quotaProject)
	}
	if transient, err := t.authorize(ctx, req); err != nil {
		return googlePubSubAttempt{err: err, transient: transient}
	}

	resp, err := t.client.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		// No answer: the request may not have left, or the answer was lost.
		return googlePubSubAttempt{
			err:       errors.Wrapf(err, codeEventsGooglePubSub, "publish to "+t.name, errors.String("topic", t.name)),
			transient: true,
		}
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // the publish outcome is already decided; a close error is not actionable
	data, err := io.ReadAll(io.LimitReader(resp.Body, googlePubSubMaxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		publishErr := newGooglePubSubPublishError(t.name, resp.StatusCode, data)
		result := googlePubSubAttempt{err: publishErr, transient: transientPubSubStatus(publishErr.StatusCode)}
		if publishErr.StatusCode == http.StatusTooManyRequests || publishErr.StatusCode == http.StatusServiceUnavailable {
			result.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
		return result
	}
	// Pub/Sub answered 2xx: it accepted the publish, so nothing below is sent
	// again.
	if err != nil {
		return googlePubSubAttempt{err: errors.Wrapf(err, codeEventsGooglePubSub, "read publish answer from "+t.name, errors.String("topic", t.name))}
	}
	var reply googlePubSubRESTPublishResponse
	if err := json.Unmarshal(data, &reply); err != nil {
		return googlePubSubAttempt{err: errors.Wrapf(err, codeEventsGooglePubSub, "decode publish answer from "+t.name, errors.String("topic", t.name))}
	}
	if len(reply.MessageIDs) != 1 || reply.MessageIDs[0] == "" {
		return googlePubSubAttempt{err: errors.New(codeEventsGooglePubSub, "publish answer from "+t.name+" does not hold exactly one message id",
			errors.String("topic", t.name), errors.Int("messageIds", len(reply.MessageIDs)))}
	}
	return googlePubSubAttempt{}
}

// transientPubSubStatus reports whether a request answered with code may
// succeed when sent again.
func transientPubSubStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// parseRetryAfter reads a Retry-After header: a number of seconds or an HTTP
// date. It returns 0 for an empty, unreadable or past value.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil {
		return max(time.Duration(seconds)*time.Second, 0)
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}

// authorize sets the Authorization header from the token source before the
// request is sent, so a credential failure is known to have sent nothing. The
// lookup stops with ctx even when the token source ignores it. On an error,
// transient reports whether a new lookup may succeed.
func (t *googlePubSubRESTTopic) authorize(ctx context.Context, req *http.Request) (transient bool, err error) {
	if t.client.tokens == nil {
		return false, nil
	}
	type result struct {
		token *oauth2.Token
		err   error
	}
	done := make(chan result, 1)
	go func() {
		token, err := t.client.tokens.Token()
		done <- result{token: token, err: err}
	}()
	var got result
	select {
	case got = <-done:
	case <-ctx.Done():
		return true, &GooglePubSubCredentialError{Topic: t.name}
	}
	if got.err != nil {
		// Never wrap the provider error: it can carry credential material.
		return transientTokenError(got.err), &GooglePubSubCredentialError{Topic: t.name}
	}
	if got.token == nil {
		return false, &GooglePubSubCredentialError{Topic: t.name}
	}
	access := got.token.AccessToken
	if access == "" || access != strings.TrimSpace(access) || strings.ContainsAny(access, "\r\n") {
		return false, &GooglePubSubCredentialError{Topic: t.name, Invalid: true}
	}
	got.token.SetAuthHeader(req)
	return false, nil
}

// transientTokenError reports whether a failed access token lookup may succeed
// when tried again: the token endpoint was not reached, or it answered a
// transient status. An answer such as 400 invalid_grant is not transient.
func transientTokenError(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	if stderrors.As(err, &retrieveErr) && retrieveErr.Response != nil {
		return transientPubSubStatus(retrieveErr.Response.StatusCode)
	}
	return true
}

// publishURL builds {endpoint}v1/{topic}:publish. The topic resource name keeps
// its "/" separators and percent-encodes what the URI reserved set does not
// allow, as the Google REST clients do.
func (t *googlePubSubRESTTopic) publishURL() (*url.URL, error) {
	base, err := url.Parse(t.client.endpoint)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(base.Path, "/") + "/v1/"
	base.Path = prefix + t.name + ":publish"
	base.RawPath = prefix + escapeReservedPath(t.name) + ":publish"
	base.RawQuery = "alt=json&prettyPrint=false"
	return base, nil
}

// escapeReservedPath percent-encodes every byte outside the RFC 6570 reserved
// expansion set: unreserved characters and gen-delims and sub-delims stay.
func escapeReservedPath(s string) string {
	const allowed = "-._~:/?#[]@!$&'()*+,;="
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte(allowed, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

func newGooglePubSubPublishError(topic string, httpStatus int, body []byte) *GooglePubSubPublishError {
	publishErr := &GooglePubSubPublishError{Topic: topic, StatusCode: httpStatus}
	var reply googlePubSubRESTErrorReply
	if json.Unmarshal(body, &reply) == nil && reply.Error != nil {
		if reply.Error.Code != 0 {
			publishErr.StatusCode = reply.Error.Code
		}
		publishErr.Status = reply.Error.Status
		publishErr.Message = truncateErrorText(reply.Error.Message)
		return publishErr
	}
	publishErr.Message = truncateErrorText(strings.TrimSpace(string(body)))
	return publishErr
}

// truncateErrorText keeps at most googlePubSubMaxErrorMessageBytes of s, cut
// on a rune boundary.
func truncateErrorText(s string) string {
	if len(s) <= googlePubSubMaxErrorMessageBytes {
		return s
	}
	cut := googlePubSubMaxErrorMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// googlePubSubPushOnlySubscription completes the GooglePubSubClient surface.
// DirectPubSubTransport never subscribes, so nothing calls Receive in practice.
type googlePubSubPushOnlySubscription struct{ name string }

func (s *googlePubSubPushOnlySubscription) Receive(context.Context, func(context.Context, GooglePubSubMessage)) error {
	return errors.New(codeEventsGooglePubSub, "pull receive is not supported by the pubsub transport; use push delivery",
		errors.String("subscription", s.name))
}

// quotaProjectOf returns the quota project of a credentials file
// (quota_project_id, as gcloud writes it for user credentials), or "".
func quotaProjectOf(credentialsJSON []byte) string {
	var file struct {
		QuotaProjectID string `json:"quota_project_id"`
	}
	if len(credentialsJSON) == 0 || json.Unmarshal(credentialsJSON, &file) != nil {
		return ""
	}
	return file.QuotaProjectID
}
