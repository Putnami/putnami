package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"go.putnami.dev/sdk/extension/collabhelper"
)

// Kind classifies a failed request by what it proves about the remote state.
type Kind int

// Failure kinds.
const (
	// NotSent: no byte of the request left this process — the connection,
	// name resolution or TLS failed first. Nothing can have changed.
	NotSent Kind = iota + 1
	// Uncertain: the request was written and no definite answer came back —
	// the connection broke, the deadline passed, the answer was unreadable,
	// or Linear answered 5xx. A mutation may have happened.
	Uncertain
	// Answered: Linear answered a definite refusal, an HTTP 4xx status or a
	// GraphQL error other than INTERNAL_ERROR and LOCK_TIMEOUT. A mutation did
	// not happen.
	Answered
	// NoCredential: no API key could be resolved, so nothing was sent.
	NoCredential
)

// Error is one failed request. Message never carries a credential.
type Error struct {
	Kind Kind
	// Status is the HTTP status Linear answered, 0 when none arrived.
	Status int
	// Code is the code of Linear's first GraphQL error, such as
	// AUTHENTICATION_ERROR, FORBIDDEN, RATELIMITED or INPUT_ERROR; empty when
	// Linear named none.
	Code string
	// Message explains the failure: Linear's own message when it gave one.
	Message string
	// RateLimited marks an answer that reports an exhausted rate limit.
	RateLimited bool
	// NotFound marks an answer that says an entity the request names does
	// not exist.
	NotFound bool
}

func (e *Error) Error() string { return e.Message }

// maxResponseBytes bounds every response body the client reads.
const maxResponseBytes = 16 << 20

// maxMessageRunes bounds a message taken from a Linear error body.
const maxMessageRunes = 400

// Options tune a client. Zero values take the defaults.
type Options struct {
	// HTTP sends the requests; the default is a client with its own
	// transport.
	HTTP *http.Client
	// Timeout bounds one request, from sending it to reading its whole
	// answer. Default 30s.
	Timeout time.Duration
	// ReadAttempts is how many times a query is sent before its failure is
	// reported: a query that was not answered, or answered 5xx, is repeated;
	// a definite refusal, a rate limit included, never is. Default 3.
	ReadAttempts int
	// Pause waits before repeat attempt n (1-based) of a query. The default
	// waits 1s, then 3s.
	Pause func(ctx context.Context, attempt int) error
	// UserAgent names the client to Linear.
	UserAgent string
}

// Client is one authenticated Linear GraphQL client.
type Client struct {
	endpoint   *url.URL
	credential *Credential
	options    Options
}

// New returns a client of one endpoint, authenticated by credential.
func New(endpoint *url.URL, credential *Credential, options Options) *Client {
	base := options.HTTP
	if base == nil {
		base = &http.Client{}
		if transport, ok := http.DefaultTransport.(*http.Transport); ok {
			base.Transport = transport.Clone()
		}
	}
	httpClient := *base
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	options.HTTP = &httpClient
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Second
	}
	if options.ReadAttempts <= 0 {
		options.ReadAttempts = 3
	}
	if options.Pause == nil {
		options.Pause = collabhelper.Backoff
	}
	if options.UserAgent == "" {
		options.UserAgent = "putnami-linear-collaboration"
	}
	return &Client{endpoint: endpoint, credential: credential, options: options}
}

// Secrets lists the credential values to redact from anything the provider
// writes.
func (c *Client) Secrets() []string { return c.credential.Secrets() }

// Query sends one GraphQL query, a read, and decodes its data into out. A
// query that failed without a definite answer is sent again, up to
// Options.ReadAttempts in all. An answer that reports an error is a failure.
func (c *Client) Query(ctx context.Context, name, query string, variables map[string]any, out any) *Error {
	payload, failure := encode(name, query, variables)
	if failure != nil {
		return failure
	}
	for attempt := 1; ; attempt++ {
		failure := c.exchange(ctx, payload, out)
		if failure == nil {
			return nil
		}
		repeat := failure.Kind == NotSent || failure.Kind == Uncertain
		if !repeat || attempt >= c.options.ReadAttempts || ctx.Err() != nil {
			return failure
		}
		if err := c.options.Pause(ctx, attempt); err != nil {
			return failure
		}
	}
}

// Mutate sends one GraphQL mutation exactly once and decodes its data into
// out. A failure's Kind says whether the mutation can have happened: an error
// Linear reports is a definite refusal, since Linear applies a mutation
// completely or not at all, except a 5xx answer or a transient code, which
// is Uncertain.
func (c *Client) Mutate(ctx context.Context, name, mutation string, variables map[string]any, out any) *Error {
	payload, failure := encode(name, mutation, variables)
	if failure != nil {
		return failure
	}
	return c.exchange(ctx, payload, out)
}

func encode(name, document string, variables map[string]any) ([]byte, *Error) {
	payload, err := json.Marshal(map[string]any{"operationName": name, "query": document, "variables": variables})
	if err != nil {
		return nil, &Error{Kind: NotSent, Message: "encode the request: " + err.Error()}
	}
	return payload, nil
}

// graphQLError is one error of a GraphQL answer.
type graphQLError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code                   string `json:"code"`
		Type                   string `json:"type"`
		UserPresentableMessage string `json:"userPresentableMessage"`
	} `json:"extensions"`
}

// exchange sends one request and decodes its answer.
func (c *Client) exchange(ctx context.Context, payload []byte, out any) *Error {
	answer, failure := c.send(ctx, payload)
	if failure != nil {
		return failure
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	decoded := json.Unmarshal(answer.body, &envelope) == nil
	if len(envelope.Errors) > 0 {
		return c.refusal(answer, envelope.Errors[0])
	}
	if answer.status < 200 || answer.status >= 300 {
		return c.refusal(answer, graphQLError{})
	}
	if !decoded || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return &Error{Kind: Uncertain, Status: answer.status, Message: "Linear answered without data"}
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return &Error{Kind: Uncertain, Status: answer.status, Message: "Linear answered with data this client cannot read"}
		}
	}
	return nil
}

// transientCodes are the GraphQL error codes with which Linear reports a
// failure on its side: the request may or may not have taken effect.
var transientCodes = map[string]bool{"INTERNAL_ERROR": true, "LOCK_TIMEOUT": true}

// refusal classifies an answer that reports an error or a non-2xx status. A
// 5xx status or a transient code is Uncertain whatever the body says.
func (c *Client) refusal(a *answer, reported graphQLError) *Error {
	code := strings.ToUpper(reported.Extensions.Code)
	message := reported.Extensions.UserPresentableMessage
	if message == "" {
		message = reported.Message
	}
	text := fmt.Sprintf("Linear answered %d", a.status)
	if message != "" {
		text = "Linear refused the request: " + message
	}
	failure := &Error{Kind: Answered, Status: a.status, Code: code, Message: c.redact(collabhelper.Bounded(text, maxMessageRunes))}
	if a.status >= 500 || transientCodes[code] {
		failure.Kind = Uncertain
		return failure
	}
	lowered := strings.ToLower(reported.Message + " " + reported.Extensions.Type)
	switch {
	case a.status == http.StatusTooManyRequests || code == "RATELIMITED" || strings.Contains(lowered, "rate limit"):
		failure.RateLimited = true
		if reset := a.header.Get("X-RateLimit-Requests-Reset"); reset != "" {
			failure.Message += "; the limit resets at " + reset
		}
	case strings.Contains(lowered, "entity not found") || code == "NOT_FOUND" || code == "ENTITY_NOT_FOUND":
		failure.NotFound = true
	}
	return failure
}

// answer is one HTTP answer, read whole.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// send performs one request. It reports NotSent when the failure came before
// the request headers were written, and Uncertain after.
func (c *Client) send(ctx context.Context, payload []byte) (*answer, *Error) {
	key, failure := c.credential.Key()
	if failure != nil {
		return nil, failure
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.options.Timeout)
	defer cancel()
	var written atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { written.Store(true) }}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(requestCtx, trace), http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, &Error{Kind: NotSent, Message: "build the request: " + err.Error()}
	}
	request.Header.Set("Authorization", authorization(key))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.options.UserAgent)
	response, err := c.options.HTTP.Do(request)
	if err != nil {
		kind := NotSent
		if written.Load() {
			kind = Uncertain
		}
		return nil, &Error{Kind: kind, Message: c.redact(describe(err, ctx, requestCtx, c.options.Timeout))}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, &Error{Kind: Uncertain, Status: response.StatusCode,
			Message: fmt.Sprintf("Linear answered %d and the answer could not be read whole", response.StatusCode)}
	}
	return &answer{status: response.StatusCode, header: response.Header, body: data}, nil
}

// describe explains a transport failure without the request URL.
func describe(err error, ctx, requestCtx context.Context, timeout time.Duration) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "the operation's time budget ran out before Linear answered"
	}
	if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("Linear did not answer within %s", timeout)
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		err = urlError.Err
	}
	return fmt.Sprintf("the request to Linear failed: %v", err)
}

func (c *Client) redact(message string) string {
	return Redact(message, c.credential.Secrets())
}
