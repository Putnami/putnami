package gitlab

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
	"sort"
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
	// or GitLab answered 5xx. A write may have happened.
	Uncertain
	// Answered: GitLab answered a definite 3xx or 4xx status. A write did not
	// happen.
	Answered
	// NoCredential: no credential could be resolved, so nothing was sent.
	NoCredential
)

// Error is one failed request. Message never carries a credential.
type Error struct {
	Kind Kind
	// Status is the HTTP status GitLab answered, 0 when none arrived.
	Status int
	// Message explains the failure: GitLab's own message when it gave one.
	Message string
	// RateLimited marks a 429 answer.
	RateLimited bool
}

func (e *Error) Error() string { return e.Message }

// maxResponseBytes bounds every response body the client reads.
const maxResponseBytes = 16 << 20

// maxMessageRunes bounds a message taken from a GitLab error body.
const maxMessageRunes = 400

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
	// UserAgent names the client to GitLab.
	UserAgent string
}

// Client is one authenticated GitLab API client.
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
	httpClient.CheckRedirect = collabhelper.FollowReadsOnSameHost
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
		options.UserAgent = "putnami-gitlab-collaboration"
	}
	return &Client{endpoint: endpoint, credential: credential, options: options}
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
	for attempt := 1; ; attempt++ {
		answer, failure := c.send(ctx, http.MethodGet, target, nil)
		if failure == nil {
			failure = answer.failure()
		}
		if failure == nil {
			if failure = decode(answer, out); failure == nil {
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
	if failure := decode(answer, out); failure != nil {
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
	if token.Scheme == JobToken {
		request.Header.Set("JOB-TOKEN", token.Value)
	} else {
		request.Header.Set("Authorization", "Bearer "+token.Value)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.options.UserAgent)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.options.HTTP.Do(request)
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
		return fmt.Sprintf("%s: GitLab did not answer within %s", method, timeout)
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
	message := gitlabMessage(a.status, a.body)
	if a.status >= 500 {
		return &Error{Kind: Uncertain, Status: a.status, Message: message}
	}
	failure := &Error{Kind: Answered, Status: a.status, Message: message}
	if a.status == http.StatusTooManyRequests {
		failure.RateLimited = true
		if reset := a.header.Get("RateLimit-ResetTime"); reset != "" {
			failure.Message += "; the limit resets at " + collabhelper.Bounded(reset, 64)
		} else if after := a.header.Get("Retry-After"); after != "" {
			failure.Message += "; retry after " + collabhelper.Bounded(after, 16) + "s"
		}
	}
	return failure
}

func decode(a *answer, out any) *Error {
	if out == nil || a.status == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(a.body, out); err != nil {
		return &Error{Kind: Uncertain, Status: a.status, Message: fmt.Sprintf("GitLab answered %d with a body this client cannot read", a.status)}
	}
	return nil
}

func (c *Client) redact(message string) string {
	return Redact(message, c.credential.Secrets())
}

// gitlabMessage renders GitLab's error body, bounded: its message, a string
// or an object of field errors, or its OAuth error and description. A body
// that is not GitLab's JSON error document is not reproduced.
func gitlabMessage(status int, body []byte) string {
	text := fmt.Sprintf("GitLab answered %d", status)
	var document struct {
		Message          json.RawMessage `json:"message"`
		Error            string          `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if json.Unmarshal(body, &document) != nil {
		return text
	}
	var detail string
	var plain string
	var fields map[string]json.RawMessage
	switch {
	case len(document.Message) > 0 && json.Unmarshal(document.Message, &plain) == nil && plain != "":
		detail = plain
	case len(document.Message) > 0 && json.Unmarshal(document.Message, &fields) == nil && len(fields) > 0:
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			var messages []string
			if json.Unmarshal(fields[name], &messages) == nil && len(messages) > 0 {
				parts = append(parts, name+" "+strings.Join(messages, ", "))
			} else {
				parts = append(parts, name)
			}
		}
		detail = strings.Join(parts, "; ")
	case document.Error != "":
		detail = strings.TrimSpace(document.Error + " " + document.ErrorDescription)
	}
	if detail != "" {
		text += ": " + detail
	}
	return collabhelper.Bounded(text, maxMessageRunes)
}

// HasNext reports whether a list answer names a next page, in X-Next-Page or
// in a Link header.
func HasNext(header http.Header) bool {
	if strings.TrimSpace(header.Get("X-Next-Page")) != "" {
		return true
	}
	for _, link := range header.Values("Link") {
		for _, part := range strings.Split(link, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}
