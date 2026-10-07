package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	intelligenceclient "go.putnami.dev/intelligence/agent-readiness/clients/go"
	protocolcli "go.putnami.dev/protocol/cli"
)

const reportsPath = "/v1/intelligence/agent-readiness/reports"

// Submit calls the generated first-party operation. Its typed body proves the
// provider shape; the transport swaps only equivalent JSON for the printed bytes.
func Submit(ctx context.Context, baseURL string, httpClient *http.Client, body []byte) (*intelligenceclient.Submission, error) {
	if err := validatePayload(body); err != nil {
		return nil, fmt.Errorf("the payload does not match schema v1, nothing was sent: %w", err)
	}
	var typed intelligenceclient.Payload
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&typed); err != nil {
		return nil, fmt.Errorf("the payload does not match the intelligence-api contract, nothing was sent: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("the payload has trailing data, nothing was sent")
	}
	generated, err := client.EncodeJSON(typed)
	if err != nil || !sameJSON(generated, body) {
		return nil, errors.New("the payload does not survive the intelligence-api client's encoding, nothing was sent")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, protocolcli.Usagef("invalid agent-readiness API base URL")
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "localhost" && !net.ParseIP(parsed.Hostname()).IsLoopback() {
		return nil, protocolcli.Usagef("agent-readiness API URL must use HTTPS outside localhost")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	sending := *httpClient
	sending.Jar = nil
	sending.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	swap := &exactBody{next: transportOf(&sending), body: body}
	sending.Transport = swap
	bound, err := intelligenceclient.NewIntelligenceClientBinding(client.ServiceBinding{
		URL: baseURL, ClientID: "putnami-cli", Credentials: nil,
		AllowInsecure: parsed.Scheme == "http", CarryRemoteMessage: true, HTTPClient: &sending,
	})
	if err != nil {
		return nil, protocolcli.Usagef("configure the intelligence-api client: %v", err)
	}
	defer func() { _ = intelligenceclient.CloseIntelligenceClient(bound) }()
	submission, err := bound.CreateV1IntelligenceAgentReadinessReports(ctx,
		intelligenceclient.CreateV1IntelligenceAgentReadinessReportsInput{Body: typed})
	if err != nil && swap.refused {
		return nil, errors.New(errBodyMismatch.Error() + ", nothing was sent")
	}
	if err != nil {
		return nil, err
	}
	responseDecoder := json.NewDecoder(bytes.NewReader(swap.response.Bytes()))
	responseDecoder.DisallowUnknownFields()
	var strict intelligenceclient.Submission
	if err := responseDecoder.Decode(&strict); err != nil {
		return nil, perrors.New(client.CodeClientResponse, "service response is outside contract")
	}
	if responseDecoder.Decode(new(any)) != io.EOF {
		return nil, perrors.New(client.CodeClientResponse, "service response is outside contract")
	}
	reportURL, err := url.Parse(submission.Url)
	if err != nil || reportURL.Scheme != "https" || reportURL.Host == "" || reportURL.User != nil {
		return nil, perrors.New(client.CodeClientResponse, "report URL is not HTTPS")
	}
	return submission, nil
}

func transportOf(httpClient *http.Client) http.RoundTripper {
	if httpClient.Transport != nil {
		return httpClient.Transport
	}
	return http.DefaultTransport
}

var errBodyMismatch = errors.New("the generated request body is not the printed payload")

type exactBody struct {
	next     http.RoundTripper
	body     []byte
	refused  bool
	response bytes.Buffer
}

func (t *exactBody) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, reportsPath) || req.Body == nil ||
		req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
		t.refused = true
		return nil, errBodyMismatch
	}
	generated, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read generated request body: %w", err)
	}
	if !sameJSON(generated, t.body) {
		t.refused = true
		return nil, errBodyMismatch
	}
	swapped := req.Clone(req.Context())
	swapped.Body = io.NopCloser(bytes.NewReader(t.body))
	swapped.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(t.body)), nil }
	swapped.ContentLength = int64(len(t.body))
	swapped.Header.Set("Content-Length", strconv.Itoa(len(t.body)))
	response, err := t.next.RoundTrip(swapped)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusCreated && response.Body != nil {
		response.Body = &capturingBody{ReadCloser: response.Body, captured: &t.response}
	}
	return response, nil
}

// The framework's HTTP transport limits how many response bytes it reads.
// Capture those same bytes so this command can reject undeclared fields after
// the generated client validates status, media type, and the declared schema.
type capturingBody struct {
	io.ReadCloser
	captured *bytes.Buffer
}

func (body *capturingBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	_, _ = body.captured.Write(p[:n])
	return n, err
}

func sameJSON(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func sendFailure(ctx context.Context, err error, baseURL string) error {
	if errors.Is(err, protocolcli.ErrUsage) {
		return err
	}
	host := baseURL
	if parsed, parseErr := url.Parse(baseURL); parseErr == nil && parsed.Host != "" {
		host = parsed.Host
	}
	var remote *client.RemoteError
	var sentence string
	switch {
	case perrors.Is(err, client.CodeClientResponse):
		sentence = host + " answered outside its contract, so the report link could not be read"
	case errors.As(err, &remote) && remote.StatusCode == http.StatusTooManyRequests:
		sentence = host + " created no report: too many reports were sent from this network; try again in a minute"
	case remote != nil && remote.StatusCode == http.StatusRequestEntityTooLarge:
		sentence = host + " created no report: the payload is larger than it accepts"
	case remote != nil && remote.StatusCode >= 500:
		sentence = fmt.Sprintf("%s created no report: it answered %d", host, remote.StatusCode)
	case remote != nil:
		sentence = fmt.Sprintf("%s refused the payload (%d)", host, remote.StatusCode)
		if remote.Message != "" {
			sentence += ": " + remote.Message
		}
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		sentence = "No report came back from " + host + ": the request timed out; raise --timeout"
	case perrors.Is(err, client.CodeClientDeadline):
		sentence = "No report came back from " + host + ": it did not answer in time"
	default:
		var netErr interface{ Timeout() bool }
		if errors.As(err, &netErr) && netErr.Timeout() {
			sentence = "No report came back from " + host + ": the request timed out; raise --timeout"
		} else {
			sentence = "Nothing was sent to " + host + ": the service could not be reached"
		}
	}
	return protocolcli.APIf("%s. Inspect the payload with: %s", sentence, InspectCommand)
}
