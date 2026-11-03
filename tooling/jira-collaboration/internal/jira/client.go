package jira

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
	// or Jira answered 5xx. A write may have happened.
	Uncertain
	// Answered: Jira answered a definite 3xx or 4xx status. A write did not
	// happen.
	Answered
	// NoCredential: no credential could be resolved, so nothing was sent.
	NoCredential
)

// Error is one failed request. Message never carries a credential.
type Error struct {
	Kind Kind
	// Status is the HTTP status Jira answered, 0 when none arrived.
	Status int
	// Message explains the failure: Jira's own messages when it gave some.
	Message string
	// RateLimited marks a 429 answer.
	RateLimited bool
}

func (e *Error) Error() string { return e.Message }

// maxResponseBytes bounds every response body the client reads.
const maxResponseBytes = 16 << 20

// maxMessageRunes bounds a message taken from a Jira error body.
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
	// UserAgent names the client to Jira.
	UserAgent string
}

// Client is one authenticated Jira Cloud REST API v2 client.
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
		options.UserAgent = "putnami-jira-collaboration"
	}
	return &Client{endpoint: endpoint, credential: credential, options: options}
}

// followReadsOnSameHost follows a redirect only for a GET that stays on the
// host it started on. A write is never replayed at another location: its 3xx
// answer is reported as a refusal, and nothing was written.
func followReadsOnSameHost(request *http.Request, via []*http.Request) error {
	first := via[0]
	if len(via) >= 5 || first.Method != http.MethodGet || request.URL.Host != first.URL.Host {
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

// Site is the Jira Cloud host this client serves.
func (c *Client) Site() string { return c.endpoint.Site }

// Get reads one REST resource at path (built with Path) into out. A read that
// failed without a definite answer is sent again, up to Options.ReadAttempts
// in all.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) *Error {
	return c.read(ctx, http.MethodGet, path, query, nil, out)
}

// Search sends one POST that changes nothing, such as a JQL search, and
// decodes its answer into out. Like Get, it is sent again after a failure
// without a definite answer.
func (c *Client) Search(ctx context.Context, path string, body, out any) *Error {
	payload, err := json.Marshal(body)
	if err != nil {
		return &Error{Kind: NotSent, Message: "encode the request: " + err.Error()}
	}
	return c.read(ctx, http.MethodPost, path, nil, payload, out)
}

func (c *Client) read(ctx context.Context, method, path string, query url.Values, payload []byte, out any) *Error {
	target, failure := c.target(path, query)
	if failure != nil {
		return failure
	}
	for attempt := 1; ; attempt++ {
		answer, failure := c.send(ctx, method, target, payload)
		if failure == nil {
			failure = answer.failure()
		}
		if failure == nil {
			if failure = decode(answer, out); failure == nil {
				return nil
			}
		}
		failure.Message = c.redact(failure.Message)
		repeat := failure.Kind == NotSent || failure.Kind == Uncertain
		if !repeat || attempt >= c.options.ReadAttempts || ctx.Err() != nil {
			return failure
		}
		if err := c.options.Pause(ctx, attempt); err != nil {
			return failure
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
	authorization, failure := c.credential.Authorization()
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
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.options.UserAgent)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.options.HTTP.Do(request) //nolint:gosec // G704: the destination is the bound site's REST root or a loopback stand-in, both validated by ResolveEndpoint; a write never follows a redirect
	if err != nil {
		kind := NotSent
		if written.Load() {
			kind = Uncertain
		}
		return nil, &Error{Kind: kind, Message: c.redact(describe(method, err, ctx, requestCtx, c.options.Timeout))}
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
func describe(method string, err error, ctx, requestCtx context.Context, timeout time.Duration) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return method + ": the operation's time budget ran out before Jira answered"
	}
	if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("%s: Jira did not answer within %s", method, timeout)
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
	message := jiraMessage(a.status, a.body)
	if a.status >= 500 {
		return &Error{Kind: Uncertain, Status: a.status, Message: message}
	}
	failure := &Error{Kind: Answered, Status: a.status, Message: message}
	if a.status == http.StatusTooManyRequests {
		failure.RateLimited = true
		if after := a.header.Get("Retry-After"); after != "" {
			failure.Message += "; retry after " + bounded(after) + "s"
		}
	}
	return failure
}

func decode(a *answer, out any) *Error {
	if out == nil || a.status == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(a.body, out); err != nil {
		return &Error{Kind: Uncertain, Status: a.status, Message: fmt.Sprintf("Jira answered %d with a body this client cannot read", a.status)}
	}
	return nil
}

func (c *Client) redact(message string) string {
	return Redact(message, c.credential.Secrets())
}

// jiraMessage renders Jira's error body: its errorMessages and its field
// errors, bounded. A body that is not Jira's JSON error document is not
// reproduced.
func jiraMessage(status int, body []byte) string {
	var document struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	text := fmt.Sprintf("Jira answered %d", status)
	if json.Unmarshal(body, &document) != nil {
		return text
	}
	details := append([]string{}, document.ErrorMessages...)
	fields := make([]string, 0, len(document.Errors))
	for field := range document.Errors {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		details = append(details, field+": "+document.Errors[field])
	}
	if len(details) > 0 {
		text += ": " + strings.Join(details, "; ")
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
