package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// maxResponseBytes bounds a response body observe reads.
	maxResponseBytes = 8 << 20
	// maxMessageBytes bounds the host's message a diagnostic quotes.
	maxMessageBytes = 300
	userAgent       = "putnami-observe"
)

// perPage and maxPages bound the notes observe reads to find its own.
var (
	perPage  = 100
	maxPages = 50
)

// apiClient returns client, or a default one, that never follows a redirect:
// a redirect could carry the token to another host.
func apiClient(client *http.Client) *http.Client {
	c := &http.Client{Timeout: requestTimeout}
	if client != nil {
		copied := *client
		c = &copied
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// apiBase parses an API or page URL and accepts only https without
// credentials, query or fragment. Its error quotes neither the credentials
// nor the query, which may carry a secret.
func apiBase(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("the API URL is not a URL")
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("the API URL %s://%s%s is not an https URL without credentials, query or fragment", u.Scheme, u.Host, u.EscapedPath())
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// api sends JSON requests to one code host API.
type api struct {
	client *http.Client
	base   *url.URL
	auth   func(*http.Request)
}

// apiError is a response outside 2xx.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("the API answered %d", e.status)
	}
	return fmt.Sprintf("the API answered %d: %s", e.status, e.message)
}

// do sends method to the base URL followed by path, whose segments the
// caller escaped, with query and body as JSON, and decodes a 2xx answer
// into out when out is not nil.
func (a api) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	target := a.base.String() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	a.auth(req)
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, a.base.Host, unwrapURLError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("reading the answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{status: resp.StatusCode, message: hostMessage(data)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("the API answered %d with a body that is not the expected JSON: %w", resp.StatusCode, err)
	}
	return nil
}

// unwrapURLError drops the request URL that *url.Error repeats.
func unwrapURLError(err error) error {
	u := &url.Error{}
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

// hostMessage reads the message of an error answer: GitHub's and GitLab's
// "message", GitLab's "error", else nothing.
func hostMessage(data []byte) string {
	var answer struct {
		Message json.RawMessage `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &answer) != nil {
		return ""
	}
	raw := answer.Message
	if len(raw) == 0 {
		raw = answer.Error
	}
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var compact bytes.Buffer
		if json.Compact(&compact, raw) != nil {
			return ""
		}
		text = compact.String()
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxMessageBytes {
		text = strings.ToValidUTF8(text[:maxMessageBytes], "") + "..."
	}
	return text
}

// pages calls fetch for page 1, 2, ... until a page holds fewer than perPage
// items, fetch reports a match or maxPages pages were read.
func pages(fetch func(page int) (items int, found bool, err error)) error {
	for page := 1; page <= maxPages; page++ {
		items, found, err := fetch(page)
		if err != nil || found || items < perPage {
			return err
		}
	}
	return nil
}
