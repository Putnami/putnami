package clicore

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// roundTripFunc adapts a function into an http.RoundTripper so SendJSONInto can
// be exercised against canned responses without a live server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func responseWith(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     StatusLine(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

// getRequest builds the request the SendJSON tests send.
func getRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

type bindingWire struct {
	Provider    string `json:"provider"`
	Active      bool   `json:"active"`
	WorkspaceID string `json:"workspace_id"`
}

func TestSendJSONIntoDecodesTypedBodyAndStampsHeaders(t *testing.T) {
	var gotAccept, gotUA string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotAccept = req.Header.Get("Accept")
		gotUA = req.Header.Get("User-Agent")
		return responseWith(http.StatusOK, `{"provider":"github","active":true,"workspace_id":"ws-acme"}`), nil
	})}

	got, err := SendJSONInto[bindingWire](client, getRequest(t, http.MethodPost, "https://control.test/v1/source/github/bindings"), nil)
	if err != nil {
		t.Fatalf("SendJSONInto: %v", err)
	}
	if got.Provider != "github" || !got.Active || got.WorkspaceID != "ws-acme" {
		t.Fatalf("decoded = %+v, want github/active/ws-acme", *got)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", gotAccept)
	}
	if gotUA == "" {
		t.Fatalf("User-Agent header was not stamped")
	}
}

func TestSendJSONIntoMapsUnauthorizedToExitAuth(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return responseWith(http.StatusUnauthorized, `{"error":"token expired"}`), nil
	})}

	_, err := SendJSONInto[bindingWire](client, getRequest(t, http.MethodGet, "https://control.test/x"), nil)
	if err == nil {
		t.Fatal("expected an error on 401")
	}
	if code := ExitCode(err); code != ExitAuth {
		t.Fatalf("exit code = %d, want ExitAuth (%d)", code, ExitAuth)
	}
}

func TestSendJSONIntoSurfacesServerErrorMessage(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return responseWith(http.StatusBadGateway, `{"error":"mint github installation token"}`), nil
	})}

	_, err := SendJSONInto[bindingWire](client, getRequest(t, http.MethodGet, "https://control.test/x"), nil)
	if err == nil {
		t.Fatal("expected an error on 502")
	}
	if code := ExitCode(err); code != ExitAPI {
		t.Fatalf("exit code = %d, want ExitAPI (%d)", code, ExitAPI)
	}
	if msg := err.Error(); !bytes.Contains([]byte(msg), []byte("mint github installation token")) {
		t.Fatalf("error = %q, want the server error body message", msg)
	}
}

func TestSendJSONIntoPreservesTypedAPIErrorCode(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return responseWith(http.StatusConflict, `{"code":"source.github.workspace_already_bound","error":"already connected"}`), nil
	})}

	_, err := SendJSONInto[bindingWire](client, getRequest(t, http.MethodGet, "https://control.test/x"), nil)
	if err == nil {
		t.Fatal("expected an error on 409")
	}
	if got := APIErrorCode(err); got != "source.github.workspace_already_bound" {
		t.Fatalf("APIErrorCode = %q", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("APIError = %#v", err)
	}
}

func TestSendJSONIntoHonorsAllowStatuses(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return responseWith(http.StatusAccepted, `{"provider":"github","active":true,"workspace_id":"ws-acme"}`), nil
	})}

	got, err := SendJSONInto[bindingWire](client, getRequest(t, http.MethodGet, "https://control.test/x"), []int{http.StatusAccepted})
	if err != nil {
		t.Fatalf("SendJSONInto with 202 allowed: %v", err)
	}
	if got.Provider != "github" {
		t.Fatalf("decoded = %+v, want provider github", *got)
	}
}

func TestServerErrorMessageHonorsFieldOrder(t *testing.T) {
	body := map[string]any{"error": "coarse", "message": "specific"}

	if got := ServerErrorMessage(body, "500 Internal Server Error", "error", "message"); got != "coarse" {
		t.Fatalf("error-first message = %q, want coarse", got)
	}
	if got := ServerErrorMessage(body, "500 Internal Server Error", "message", "error"); got != "specific" {
		t.Fatalf("message-first message = %q, want specific", got)
	}
	if got := ServerErrorMessage(map[string]any{}, "500 Internal Server Error", "error", "message"); got != "500 Internal Server Error" {
		t.Fatalf("fallback = %q, want status line", got)
	}
}

func TestServerErrorMessageFromBodyFallsBackToStatus(t *testing.T) {
	if got := ServerErrorMessageFromBody([]byte(`not json`), "502 Bad Gateway", "error", "message"); got != "502 Bad Gateway" {
		t.Fatalf("invalid body fallback = %q, want status line", got)
	}
}

func TestInvalidJSONResponseCarriesStatusAndBodySnippet(t *testing.T) {
	// The exact production shape of a past outage: the control plane's handler
	// outlived its own HTTP write timeout, so the client got a small non-JSON
	// Cloud Run 503. The old error named only the URL, which read as a
	// client-side parse bug and hid a server-side timeout for weeks.
	const page = `<html><head><title>503 Service Unavailable</title></head><body>upstream request timeout</body></html>`
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return responseWith(http.StatusServiceUnavailable, page), nil
	})}

	_, err := SendJSON(client, getRequest(t, http.MethodPut, "https://api.putnami.cloud/api/configs"), nil)
	if err == nil {
		t.Fatal("an undecodable body must error")
	}
	msg := err.Error()
	for _, want := range []string{"https://api.putnami.cloud/api/configs", "503", "upstream request timeout"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must carry %q", msg, want)
		}
	}
}

func TestSendJSONIntoInvalidJSONCarriesStatusAndBodySnippet(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return responseWith(http.StatusOK, "not json at all"), nil
	})}

	_, err := SendJSONInto[bindingWire](client, getRequest(t, http.MethodGet, "https://control.test/x"), nil)
	if err == nil {
		t.Fatal("an undecodable body must error")
	}
	if !strings.Contains(err.Error(), "200") || !strings.Contains(err.Error(), "not json at all") {
		t.Errorf("error %q must carry the status and body snippet", err.Error())
	}
}

func TestInvalidJSONResponseErrorTruncatesAndSanitizes(t *testing.T) {
	// Security requirement: the snippet is an UNTRUSTED upstream body. It must be
	// bounded (an unexpected multi-megabyte body cannot flood a CI log) and free
	// of control characters (no terminal escape sequences, NULs, or raw binary).
	t.Run("truncates", func(t *testing.T) {
		err := InvalidJSONResponseError("https://api.test/x", http.StatusBadGateway, []byte(strings.Repeat("a", 10_000)))
		if len(err.Error()) > invalidJSONSnippetMax+256 {
			t.Fatalf("error length %d is not bounded by the snippet cap", len(err.Error()))
		}
		if !strings.Contains(err.Error(), "truncated") {
			t.Errorf("a cut snippet must say so: %q", err.Error())
		}
	})
	t.Run("strips control characters", func(t *testing.T) {
		err := InvalidJSONResponseError("https://api.test/x", http.StatusBadGateway,
			[]byte("head\x1b[31mred\x00\x07\ntail"))
		msg := err.Error()
		for _, bad := range []string{"\x1b", "\x00", "\x07", "\n"} {
			if strings.Contains(msg, bad) {
				t.Errorf("error %q still carries a control character %q", msg, bad)
			}
		}
		if !strings.Contains(msg, "head") || !strings.Contains(msg, "tail") {
			t.Errorf("printable text must survive sanitization: %q", msg)
		}
	})
	t.Run("empty body adds nothing", func(t *testing.T) {
		err := InvalidJSONResponseError("https://api.test/x", http.StatusBadGateway, []byte("  \n\t "))
		if strings.HasSuffix(err.Error(), ": ") {
			t.Errorf("a blank body must not leave a dangling separator: %q", err.Error())
		}
	})
	t.Run("invalid utf8 is repaired", func(t *testing.T) {
		err := InvalidJSONResponseError("https://api.test/x", http.StatusBadGateway, []byte{0xff, 0xfe, 'o', 'k'})
		if !strings.Contains(err.Error(), "ok") || !utf8.ValidString(err.Error()) {
			t.Errorf("snippet must stay valid UTF-8 and keep printable bytes: %q", err.Error())
		}
	})
}
