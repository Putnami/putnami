package github

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
	"strconv"
	"strings"
	"sync/atomic"
	"time"
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
	// or GitHub answered 5xx. A write may have happened.
	Uncertain
	// Answered: GitHub answered a definite 3xx or 4xx status. A write did not
	// happen.
	Answered
	// NoCredential: no credential could be resolved, so nothing was sent.
	NoCredential
)

// Error is one failed request. Message never carries a credential.
type Error struct {
	Kind Kind
	// Status is the HTTP status GitHub answered, 0 when none arrived.
	Status int
	// Message explains the failure: GitHub's own message when it gave one.
	Message string
	// RateLimited marks a 403 or 429 answer that reports an exhausted rate
	// limit rather than a permission.
	RateLimited bool
}

func (e *Error) Error() string { return e.Message }

// maxResponseBytes bounds every response body the client reads.
const maxResponseBytes = 16 << 20

// maxMessageRunes bounds a message taken from a GitHub error body.
const maxMessageRunes = 400

// apiVersion is the REST API version the client speaks.
const apiVersion = "2022-11-28"

// Options tune a client. Zero values take the defaults.
type Options struct {
	// HTTP sends the requests; the default is a client with its own
	// transport.
	HTTP *http.Client
	// Timeout bounds one request, from sending it to reading its whole
	// answer. Default 30s.
	Timeout time.Duration
	// ReadAttempts is how many times a read is sent before its failure is
	// reported: a read that was not answered, or answered 5xx, is repeated;
	// a definite 4xx answer, a rate limit included, never is. Default 3.
	ReadAttempts int
	// Pause waits before repeat attempt n (1-based) of a read. The default
	// waits 1s, then 3s.
	Pause func(ctx context.Context, attempt int) error
	// UserAgent names the client to GitHub.
	UserAgent string
}

// Client is one authenticated GitHub API client.
type Client struct {
	endpoint   Endpoint
	credential *Credential
	options    Options
}

// New returns a client of one endpoint, authenticated by credential.
func New(endpoint Endpoint, credential *Credential, options Options) *Client {
	base := options.HTTP
	if base == nil {
		base = &http.Client{}
		if transport, ok := http.DefaultTransport.(*http.Transport); ok {
			base.Transport = transport.Clone()
		}
	}
	httpClient := *base
	httpClient.CheckRedirect = followReadsOnSameHost
	options.HTTP = &httpClient
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Second
	}
	if options.ReadAttempts <= 0 {
		options.ReadAttempts = 3
	}
	if options.Pause == nil {
		options.Pause = backoff
	}
	if options.UserAgent == "" {
		options.UserAgent = "putnami-github-collaboration"
	}
	return &Client{endpoint: endpoint, credential: credential, options: options}
}

// followReadsOnSameHost follows a redirect only for a read that stays on the
// host it started on. A write is never replayed at another location: its 3xx
// answer is reported as a refusal, and nothing was written.
func followReadsOnSameHost(request *http.Request, via []*http.Request) error {
	first := via[0]
	if len(via) >= 5 || (first.Method != http.MethodGet && first.Method != http.MethodHead) || request.URL.Host != first.URL.Host {
		return http.ErrUseLastResponse
	}
	return nil
}

func backoff(ctx context.Context, attempt int) error {
	delay := time.Second
	if attempt > 1 {
		delay = 3 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Secrets lists the credential values to redact from anything the provider
// writes.
func (c *Client) Secrets() []string { return c.credential.Secrets() }

// Get reads one REST resource at path (built with Path) into out, and returns
// the response headers. A read that failed without a definite answer is sent
// again, up to Options.ReadAttempts in all.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) (http.Header, *Error) {
	target, failure := c.target(path, query)
	if failure != nil {
		return nil, failure
	}
	return c.read(ctx, http.MethodGet, target, nil, out)
}

// read sends one request that changes nothing and decodes its answer into
// out. A request that failed without a definite answer is sent again, up to
// Options.ReadAttempts in all.
func (c *Client) read(ctx context.Context, method, target string, payload []byte, out any) (http.Header, *Error) {
	for attempt := 1; ; attempt++ {
		answer, failure := c.send(ctx, method, target, payload)
		if failure == nil {
			failure = answer.failure()
		}
		if failure == nil {
			if failure = c.decode(answer, out); failure == nil {
				return answer.header, nil
			}
		}
		failure.Message = c.redact(failure.Message)
		repeat := failure.Kind == NotSent || failure.Kind == Uncertain
		if !repeat || attempt >= c.options.ReadAttempts || ctx.Err() != nil {
			return nil, failure
		}
		if err := c.options.Pause(ctx, attempt); err != nil {
			return nil, failure
		}
	}
}

// Write sends one mutating REST request exactly once and decodes a successful
// answer into out. A failure's Kind says whether the write can have happened.
func (c *Client) Write(ctx context.Context, method, path string, body, out any) *Error {
	target, failure := c.target(path, nil)
	if failure != nil {
		return failure
	}
	return c.write(ctx, method, target, body, out)
}

// GraphQL sends one GraphQL mutation exactly once and decodes its data into
// out. An answer that reports errors is a definite refusal: GitHub applies a
// mutation completely or not at all.
func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any, out any) *Error {
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	failure := c.write(ctx, http.MethodPost, c.endpoint.GraphQL.String(), map[string]any{"query": query, "variables": variables}, &envelope)
	if failure != nil {
		return failure
	}
	if len(envelope.Errors) > 0 {
		return &Error{Kind: Answered, Status: graphQLStatus(envelope.Errors[0].Type), Message: c.redact(bounded("GitHub refused the mutation: " + envelope.Errors[0].Message))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return &Error{Kind: Uncertain, Status: http.StatusOK, Message: "GitHub acknowledged the mutation with data this client cannot read"}
	}
	return nil
}

// graphQLStatus is the REST status that means what a GraphQL error type
// means.
func graphQLStatus(errorType string) int {
	switch errorType {
	case "FORBIDDEN":
		return http.StatusForbidden
	case "NOT_FOUND":
		return http.StatusNotFound
	}
	return http.StatusUnprocessableEntity
}

// GraphQLError is one error of a GraphQL answer: a field GitHub could not
// resolve, at Path, of a kind named by Type (NOT_FOUND, FORBIDDEN, ...).
// Message never carries a credential.
type GraphQLError struct {
	Type    string `json:"type"`
	Path    []any  `json:"path"`
	Message string `json:"message"`
}

// At reports whether the error is about the field at path.
func (e GraphQLError) At(path ...string) bool {
	if len(e.Path) != len(path) {
		return false
	}
	for i, name := range path {
		if element, ok := e.Path[i].(string); !ok || element != name {
			return false
		}
	}
	return true
}

// Query sends one GraphQL query, a read, and decodes its data into out. Like
// Get, a query that failed without a definite answer is sent again, up to
// Options.ReadAttempts in all. GitHub answers the fields it resolved next to
// an error for each field it could not: Query returns those errors for the
// caller to read field by field. An answer without data, or that reports an
// exhausted rate limit, is a failure.
func (c *Client) Query(ctx context.Context, query string, variables map[string]any, out any) ([]GraphQLError, *Error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, &Error{Kind: NotSent, Message: "encode the query: " + err.Error()}
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []GraphQLError  `json:"errors"`
	}
	if _, failure := c.read(ctx, http.MethodPost, c.endpoint.GraphQL.String(), payload, &envelope); failure != nil {
		return nil, failure
	}
	for i := range envelope.Errors {
		envelope.Errors[i].Message = c.redact(bounded(envelope.Errors[i].Message))
		if envelope.Errors[i].Type == "RATE_LIMITED" {
			return nil, &Error{Kind: Answered, Status: http.StatusForbidden, RateLimited: true, Message: "GitHub refused the query: " + envelope.Errors[i].Message}
		}
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		failure := &Error{Kind: Answered, Status: http.StatusUnprocessableEntity, Message: "GitHub answered the query without data"}
		if len(envelope.Errors) > 0 {
			failure.Status = graphQLStatus(envelope.Errors[0].Type)
			failure.Message = "GitHub refused the query: " + envelope.Errors[0].Message
		}
		return nil, failure
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return nil, &Error{Kind: Uncertain, Status: http.StatusOK, Message: "GitHub answered the query with data this client cannot read"}
		}
	}
	return envelope.Errors, nil
}

func (c *Client) write(ctx context.Context, method, target string, body, out any) *Error {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return &Error{Kind: NotSent, Message: "encode the request: " + err.Error()}
		}
		payload = encoded
	}
	answer, failure := c.send(ctx, method, target, payload)
	if failure != nil {
		return failure
	}
	if failure := answer.failure(); failure != nil {
		failure.Message = c.redact(failure.Message)
		return failure
	}
	if failure := c.decode(answer, out); failure != nil {
		failure.Kind = Uncertain
		return failure
	}
	return nil
}

func (c *Client) target(path string, query url.Values) (string, *Error) {
	parsed, err := url.Parse(c.endpoint.REST.String() + path)
	if err != nil {
		return "", &Error{Kind: NotSent, Message: "build the request URL: " + err.Error()}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// answer is one HTTP answer, read whole.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// send performs one request. It reports NotSent when the failure came before
// the request headers were written, and Uncertain after.
func (c *Client) send(ctx context.Context, method, target string, payload []byte) (*answer, *Error) {
	token, failure := c.credential.Token(ctx)
	if failure != nil {
		return nil, failure
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.options.Timeout)
	defer cancel()
	var written atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { written.Store(true) }}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(requestCtx, trace), method, target, body)
	if err != nil {
		return nil, &Error{Kind: NotSent, Message: "build the request: " + err.Error()}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	request.Header.Set("User-Agent", c.options.UserAgent)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.options.HTTP.Do(request) //nolint:gosec // G704: the destination is the API root of the binding's host or a loopback stand-in, both validated by ResolveEndpoint; a write never follows a redirect
	if err != nil {
		kind := NotSent
		if written.Load() {
			kind = Uncertain
		}
		return nil, &Error{Kind: kind, Message: c.redact(describe(method, err, requestCtx, c.options.Timeout))}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, &Error{Kind: Uncertain, Status: response.StatusCode,
			Message: fmt.Sprintf("%s answered %d and the answer could not be read whole", method, response.StatusCode)}
	}
	return &answer{status: response.StatusCode, header: response.Header, body: data}, nil
}

// describe explains a transport failure without the request URL.
func describe(method string, err error, requestCtx context.Context, timeout time.Duration) string {
	if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("%s: GitHub did not answer within %s", method, timeout)
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		err = urlError.Err
	}
	return fmt.Sprintf("%s: %v", method, err)
}

// failure classifies a non-2xx answer.
func (a *answer) failure() *Error {
	if a.status >= 200 && a.status < 300 {
		return nil
	}
	message := githubMessage(a.status, a.body)
	if a.status >= 500 {
		return &Error{Kind: Uncertain, Status: a.status, Message: message}
	}
	failure := &Error{Kind: Answered, Status: a.status, Message: message}
	if a.status == http.StatusTooManyRequests ||
		(a.status == http.StatusForbidden && (a.header.Get("X-RateLimit-Remaining") == "0" || a.header.Get("Retry-After") != "" ||
			strings.Contains(strings.ToLower(message), "rate limit"))) {
		failure.RateLimited = true
		if reset, err := strconv.ParseInt(a.header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			failure.Message += "; the limit resets at " + time.Unix(reset, 0).UTC().Format(time.RFC3339)
		}
	}
	return failure
}

func (c *Client) decode(a *answer, out any) *Error {
	if out == nil || a.status == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(a.body, out); err != nil {
		return &Error{Kind: Uncertain, Status: a.status, Message: fmt.Sprintf("GitHub answered %d with a body this client cannot read", a.status)}
	}
	return nil
}

func (c *Client) redact(message string) string {
	return Redact(message, c.credential.Secrets())
}

// githubMessage renders GitHub's error body: its message and its errors —
// plain messages, or the messages or codes of field errors — bounded. A body that is not GitHub's JSON error
// document is not reproduced.
func githubMessage(status int, body []byte) string {
	var document struct {
		Message string            `json:"message"`
		Errors  []json.RawMessage `json:"errors"`
	}
	text := fmt.Sprintf("GitHub answered %d", status)
	if json.Unmarshal(body, &document) != nil || document.Message == "" {
		return text
	}
	text += ": " + document.Message
	var details []string
	for _, raw := range document.Errors {
		var plain string
		var item struct {
			Field   string `json:"field"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		switch {
		case json.Unmarshal(raw, &plain) == nil && plain != "":
			details = append(details, plain)
		case json.Unmarshal(raw, &item) != nil:
		case item.Message != "":
			details = append(details, item.Message)
		case item.Field != "" || item.Code != "":
			details = append(details, strings.TrimSpace(item.Field+" "+item.Code))
		}
	}
	if len(details) > 0 {
		text += " (" + strings.Join(details, "; ") + ")"
	}
	return bounded(text)
}

func bounded(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > maxMessageRunes {
		return string(runes[:maxMessageRunes]) + "…"
	}
	return text
}

// HasNext reports whether a list answer's Link header names a next page.
func HasNext(header http.Header) bool {
	for _, link := range header.Values("Link") {
		for _, part := range strings.Split(link, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}
