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

	"go.putnami.dev/errors"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// TransportKindPubSub selects DirectPubSubTransport from events.transport. The
// plugin builds it from the events.pubsub block unless a provider module
// registered its own factory for this kind.
const TransportKindPubSub = "pubsub"

const (
	googlePubSubDefaultEndpoint = "https://pubsub.googleapis.com/"
	// googlePubSubMaxResponseBytes bounds how much of a publish response the
	// transport reads. A publish response holds message ids or an error.
	googlePubSubMaxResponseBytes = 1 << 20
)

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
// HTTP status when the body carries none.
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

// DirectPubSubTransport publishes envelopes to Google Cloud Pub/Sub through its
// REST API, authenticated with Application Default Credentials: on Google Cloud,
// the runtime service account from the metadata server.
//
// It is publish-only. Handlers receive through push delivery
// (events.delivery: push); a pull subscription reports an unsupported receive
// and delivers nothing. It implements DurablePublishTransport on the
// PublishRouteDirectPubSub route.
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
	client := &http.Client{Transport: &oauth2.Transport{
		Source: credentials.TokenSource,
		Base:   quotaProjectTransport(credentials.JSON),
	}}
	return newDirectPubSubTransport(binding, client, googlePubSubDefaultEndpoint), nil
}

func validatePubSubBinding(binding PubSubBinding) error {
	if binding.ProjectID == "" {
		return errors.New(codeEventsConfig, "events.pubsub.projectId is required for the pubsub transport",
			errors.String("transport", TransportKindPubSub))
	}
	return nil
}

func newDirectPubSubTransport(binding PubSubBinding, client httpDoer, endpoint string) *DirectPubSubTransport {
	return &DirectPubSubTransport{delegate: NewGooglePubSubTransport(GooglePubSubTransportConfig{
		Client: &googlePubSubRESTClient{
			client:    client,
			endpoint:  endpoint,
			projectID: binding.ProjectID,
		},
		TopicName: PubSubTopicNamer(binding.TopicTemplate),
	})}
}

// Publish sends the envelope as one Pub/Sub message: the JSON envelope as data,
// the envelope attributes as message attributes, and the envelope key as the
// ordering key.
func (t *DirectPubSubTransport) Publish(ctx context.Context, envelope Envelope) error {
	return t.delegate.Publish(ctx, envelope)
}

// Subscribe registers the handler. The transport is publish-only, so the
// handler receives nothing unless the plugin uses push delivery.
func (t *DirectPubSubTransport) Subscribe(definition *HandlerDefinition) error {
	return t.delegate.Subscribe(definition)
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

// ClassifyPublishError maps a publish error to a durable publish outcome:
//
//   - 408, 409, 429 and 5xx answers are retryable.
//   - Other 4xx answers are permanent.
//   - Every other error is ambiguous: a stale route, a network or credential
//     failure, an unreadable answer, or any other status. Without an answer
//     from Pub/Sub, the message may have been published.
func (t *DirectPubSubTransport) ClassifyPublishError(err error) string {
	return classifyDirectPubSubPublishError(err)
}

func classifyDirectPubSubPublishError(err error) string {
	if err == nil {
		return ""
	}
	if stderrors.Is(err, ErrStalePublishRoute) {
		return PublishOutcomeAmbiguous
	}
	var publishErr *GooglePubSubPublishError
	if !stderrors.As(err, &publishErr) {
		return PublishOutcomeAmbiguous
	}
	code := publishErr.StatusCode
	if code == http.StatusRequestTimeout || code == http.StatusConflict ||
		code == http.StatusTooManyRequests || code >= http.StatusInternalServerError {
		return PublishOutcomeRetryable
	}
	if code >= http.StatusBadRequest && code < http.StatusInternalServerError {
		return PublishOutcomePermanent
	}
	return PublishOutcomeAmbiguous
}

// PubSubTopicNamer returns the function that maps a logical topic name to a
// Pub/Sub topic id through template, where "{topic}" stands for the logical
// name (for example "events-{topic}"). The result is sanitized with
// PubSubResourceID, so a provisioner that applies the same sanitizer to the same
// template creates the topic the transport publishes to. An empty template
// keeps the logical name unchanged.
func PubSubTopicNamer(template string) func(string) string {
	if template == "" {
		return func(topic string) string { return topic }
	}
	return func(topic string) string {
		return PubSubResourceID(strings.ReplaceAll(template, "{topic}", topic))
	}
}

// PubSubResourceID turns s into a valid Pub/Sub resource id: lowercase, only
// [a-z0-9-_.], starting with a letter, at most 255 characters. Every other
// character becomes "-", leading and trailing separators are trimmed, and an id
// that does not start with a letter gets the prefix "e-".
func PubSubResourceID(s string) string {
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
	if id == "" || id[0] < 'a' || id[0] > 'z' {
		id = "e-" + id
	}
	if len(id) > 255 {
		id = strings.Trim(id[:255], "-_.")
	}
	return id
}

// googlePubSubRESTClient implements GooglePubSubClient over the Pub/Sub REST
// API (projects.topics.publish).
type googlePubSubRESTClient struct {
	client    httpDoer
	endpoint  string
	projectID string
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return errors.Wrap(err, codeEventsGooglePubSub, errors.String("phase", "request"), errors.String("topic", t.name))
	}
	req.URL = endpoint
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		return errors.Wrapf(err, codeEventsGooglePubSub, "publish to "+t.name, errors.String("topic", t.name))
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // the publish outcome is already decided; a close error is not actionable
	data, err := io.ReadAll(io.LimitReader(resp.Body, googlePubSubMaxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newGooglePubSubPublishError(t.name, resp.StatusCode, data)
	}
	if err != nil {
		return errors.Wrapf(err, codeEventsGooglePubSub, "read publish response from "+t.name, errors.String("topic", t.name))
	}
	var reply googlePubSubRESTPublishResponse
	if err := json.Unmarshal(data, &reply); err != nil {
		return errors.Wrapf(err, codeEventsGooglePubSub, "decode publish response from "+t.name, errors.String("topic", t.name))
	}
	return nil
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
		publishErr.Message = reply.Error.Message
		return publishErr
	}
	publishErr.Message = strings.TrimSpace(string(body))
	return publishErr
}

// googlePubSubPushOnlySubscription is the subscription a publish-only transport
// hands back: handlers receive through push delivery, never a pull loop.
type googlePubSubPushOnlySubscription struct{ name string }

func (s *googlePubSubPushOnlySubscription) Receive(context.Context, func(context.Context, GooglePubSubMessage)) error {
	return errors.New(codeEventsGooglePubSub, "pull receive is not supported by the pubsub transport; use push delivery",
		errors.String("subscription", s.name))
}

// quotaProjectTransport returns the base round tripper for Pub/Sub requests. It
// bills requests to the quota project of the credentials (quota_project_id, as
// gcloud writes it for user credentials) when there is one.
func quotaProjectTransport(credentialsJSON []byte) http.RoundTripper {
	var file struct {
		QuotaProjectID string `json:"quota_project_id"`
	}
	if len(credentialsJSON) == 0 || json.Unmarshal(credentialsJSON, &file) != nil || file.QuotaProjectID == "" {
		return http.DefaultTransport
	}
	return &headerTransport{base: http.DefaultTransport, key: "X-Goog-User-Project", value: file.QuotaProjectID}
}

type headerTransport struct {
	base       http.RoundTripper
	key, value string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set(t.key, t.value)
	return t.base.RoundTrip(clone)
}
