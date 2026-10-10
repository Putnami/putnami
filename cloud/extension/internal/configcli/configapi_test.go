package configcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// answerOverride answers the requests match selects with its own response and
// sends every other request to base, so a test can make one config-api route
// answer the first-party envelope, a null member or a body that is not JSON.
type answerOverride struct {
	base  http.RoundTripper
	match func(*http.Request) (*http.Response, error, bool)
}

func (t answerOverride) RoundTrip(req *http.Request) (*http.Response, error) {
	if resp, err, ok := t.match(req); ok {
		return resp, err
	}
	return t.base.RoundTrip(req)
}

func rawResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func overrideRoute(base http.RoundTripper, method, path string, answer func() (*http.Response, error)) *http.Client {
	return &http.Client{Transport: answerOverride{base: base, match: func(req *http.Request) (*http.Response, error, bool) {
		if req.Method != method || req.URL.Path != path {
			return nil, nil, false
		}
		resp, err := answer()
		return resp, err, true
	}}}
}

func TestConfigAPI_SchemaInvalidEnvelopeSurfacesMessageAndDetails(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\n",
	})

	fake := newPublishFake(t)
	fake.schemaPostStatus = http.StatusBadRequest
	// `error` is the status text here, as every first-party writer will send it
	// once the legacy prose is dropped: the CLI must print `message`.
	fake.schemaPostErrorBody = map[string]any{
		"code":    "config.schema_invalid",
		"error":   "Bad Request",
		"message": "invalid schema manifest",
		"details": []any{`server.port: unknown field type "PortConfig"`, "  ", "server.host: duplicate field"},
	}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected schema error")
	}
	assertContains(t, err.Error(), `register schema: invalid schema manifest: server.port: unknown field type "PortConfig"; server.host: duplicate field`)
	if strings.Contains(err.Error(), "Bad Request") {
		t.Fatalf("error printed the envelope's status text instead of its message: %q", err.Error())
	}
}

func TestConfigAPI_SchemaNotRegisteredCodeDrivesPublishHint(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\n",
	})
	fake := newPublishFake(t)
	fake.configPutStatus = http.StatusBadRequest
	// The hint keys on the declared code, not on the prose.
	fake.configPutErrorBody = map[string]any{
		"code": "config.schema_not_registered", "error": "Bad Request", "message": "the schema gate refused the write",
	}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, fragment := range []string{`app "my-app"`, "the schema gate refused the write", "schema POST"} {
		assertContains(t, err.Error(), fragment)
	}
}

func TestConfigAPI_OtherRefusalCarriesNoSchemaHint(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writePublishApp(t, workspaceRoot, "my-app", publishAppLayout{
		schemaBlocks: []map[string]any{
			{"path": "server", "fields": []map[string]any{{"name": "host", "type": "string"}}},
		},
		envYAML: "server:\n  host: localhost\n",
	})
	fake := newPublishFake(t)
	fake.configPutStatus = http.StatusBadRequest
	fake.configPutErrorBody = map[string]any{
		"code": "http.bad_request", "error": "Bad Request", "message": "server.host: value does not match the schema",
	}
	ioctx, _, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runPublishConfig(ioctx, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := err.Error(), `write config "server": server.host: value does not match the schema`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestConfigAPI_SchemaNotRegisteredCodeDrivesSecretsHint(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	client := overrideRoute(newSecretsFake(t), http.MethodPut, "/api/secrets", func() (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"code": "config.schema_not_registered", "error": "Bad Request", "message": "the schema gate refused the write",
		}), nil
	})
	ioctx, _, _ := commonIO(t, home, client)
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"set", "database.password", "--value", "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, fragment := range []string{`cannot set secrets for app "my-app"`, "the schema gate refused the write", "putnami publish"} {
		assertContains(t, err.Error(), fragment)
	}
}

func TestConfigAPI_StatusOnlyRefusalPrintsStatusLine(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	client := overrideRoute(newSecretsFake(t), http.MethodPut, "/api/secrets", func() (*http.Response, error) {
		return rawResponse(http.StatusConflict, "application/json", ""), nil
	})
	ioctx, _, _ := commonIO(t, home, client)
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"set", "database.password", "--value", "x"})
	if err == nil || err.Error() != "409 Conflict" {
		t.Fatalf("error = %v, want the status line", err)
	}
}

func TestConfigAPI_DeleteNotFoundEnvelopeIsIdempotent(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	var query string
	client := &http.Client{Transport: answerOverride{base: newSecretsFake(t), match: func(req *http.Request) (*http.Response, error, bool) {
		if req.Method != http.MethodDelete {
			return nil, nil, false
		}
		query = req.URL.RawQuery
		return jsonResponse(http.StatusNotFound, map[string]any{
			"code": "not_found", "error": "Not Found", "message": "The requested resource was not found",
		}), nil, true
	}}}
	ioctx, stdout, _ := commonIO(t, home, client)
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runSecrets(ioctx, []string{"delete", "database.password"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertContains(t, strings.Join(*stdout, "\n"), "already absent")
	if want := "appName=my-app&environment=prod&field=password&path=database"; query != want {
		t.Fatalf("delete query = %q, want %q", query, want)
	}
}

func TestConfigAPI_ResolveAcceptsNullMembers(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	// config-api writes JSON null for a resolve that matched no layer.
	client := overrideRoute(&configFakeServer{t: t}, http.MethodPost, "/api/configs/resolve", func() (*http.Response, error) {
		return rawResponse(http.StatusOK, "application/json",
			`{"config":null,"resolved":false,"schemaMatch":false,"layers":null}`), nil
	})
	ioctx, stdout, _ := commonIO(t, home, client)
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runConfig(ioctx, []string{"resolve", "apps/auth-server", "--env", "prod", "--json"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var out map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &out)
	if out["resolved"] != false || out["config"] != nil {
		t.Fatalf("resolve output = %v, want an unresolved, empty config", out)
	}
}

func TestConfigAPI_ListAcceptsNullEntries(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	var query string
	client := &http.Client{Transport: answerOverride{base: newSecretsFake(t), match: func(req *http.Request) (*http.Response, error, bool) {
		if req.Method != http.MethodGet || req.URL.Path != "/api/secrets" {
			return nil, nil, false
		}
		query = req.URL.RawQuery
		return rawResponse(http.StatusOK, "application/json", `{"entries":null}`), nil, true
	}}}
	ioctx, stdout, _ := commonIO(t, home, client)
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	if err := runSecrets(ioctx, []string{"list", "my-app"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	assertContains(t, strings.Join(*stdout, "\n"), "No secrets set for my-app/prod.")
	if want := "appName=my-app&environment=prod"; query != want {
		t.Fatalf("list query = %q, want %q", query, want)
	}
}

func TestConfigAPI_SuccessBodyNotJSONReportsStatusAndBody(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	client := overrideRoute(newSecretsFake(t), http.MethodGet, "/api/secrets", func() (*http.Response, error) {
		return rawResponse(http.StatusOK, "text/html", "<html>sign in\x1b[0m</html>"), nil
	})
	ioctx, _, _ := commonIO(t, home, client)
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"list", "my-app"})
	if err == nil {
		t.Fatal("a body that is not JSON must fail the list")
	}
	for _, fragment := range []string{"invalid JSON response from https://control.test/api/secrets", "HTTP 200 OK", "sign in"} {
		assertContains(t, err.Error(), fragment)
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Fatalf("body snippet must be sanitized, got %q", err.Error())
	}
}

func TestConfigAPI_TransportFailureNamesTarget(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeProjectFile(t, workspaceRoot, "my-app")

	client := overrideRoute(newSecretsFake(t), http.MethodPut, "/api/secrets", func() (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	ioctx, _, _ := commonIO(t, home, client)
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot

	err := runSecrets(ioctx, []string{"set", "database.password", "--value", "x"})
	if err == nil {
		t.Fatal("expected a transport failure")
	}
	for _, fragment := range []string{"request failed for https://control.test/api/secrets", "connection refused"} {
		assertContains(t, err.Error(), fragment)
	}
}

func TestLegacyErrorEnvelope(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   map[string]any
	}{
		{name: "not JSON", status: 503, body: "upstream timeout"},
		{name: "not an object", status: 400, body: `["no schema registered"]`},
		{name: "already coded", status: 400, body: `{"code":"http.bad_request","error":"Bad Request","message":"bad"}`},
		{name: "no prose", status: 400, body: `{"message":"bad"}`},
		{name: "blank prose", status: 400, body: `{"error":"  "}`},
		{
			name: "schema invalid", status: 400,
			body: `{"error":"invalid schema manifest","details":["a: b"]}`,
			want: map[string]any{"code": "config.schema_invalid", "error": "invalid schema manifest", "message": "invalid schema manifest", "details": []any{"a: b"}},
		},
		{
			name: "schema not registered", status: 400,
			body: `{"error":"no schema registered for app; publish schema before writing secrets"}`,
			want: map[string]any{
				"code":    "config.schema_not_registered",
				"error":   "no schema registered for app; publish schema before writing secrets",
				"message": "no schema registered for app; publish schema before writing secrets",
			},
		},
		{
			name: "other refusal keeps its own message", status: 409,
			body: `{"error":"Conflict","message":"credential custody state conflict"}`,
			want: map[string]any{"error": "Conflict", "message": "credential custody state conflict"},
		},
		{
			name: "prose outside a 400 gets no code", status: 403,
			body: `{"error":"no schema registered here"}`,
			want: map[string]any{"error": "no schema registered here", "message": "no schema registered here"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope, ok := legacyErrorEnvelope(tc.status, []byte(tc.body))
			if tc.want == nil {
				if ok {
					t.Fatalf("rewrote %s into %s", tc.body, envelope)
				}
				return
			}
			if !ok {
				t.Fatalf("did not rewrite %s", tc.body)
			}
			var got map[string]any
			if err := json.Unmarshal(envelope, &got); err != nil {
				t.Fatalf("envelope is not JSON: %v", err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("envelope = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

type staticTransport struct {
	resp *http.Response
	err  error
}

func (t staticTransport) RoundTrip(*http.Request) (*http.Response, error) { return t.resp, t.err }

func TestLegacyConfigErrors_RewritesOnlyLegacyRefusals(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://control.test/api/secrets", nil) //nolint:noctx // test request
	legacy := `{"error":"invalid schema manifest","details":["a: b"]}`
	resp, err := legacyConfigErrors{base: staticTransport{resp: rawResponse(http.StatusBadRequest, "application/json", legacy)}}.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), `"code":"config.schema_invalid"`) {
		t.Fatalf("legacy refusal not rewritten: %s", data)
	}
	if resp.ContentLength != int64(len(data)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("content length = %d / %q, want %d", resp.ContentLength, resp.Header.Get("Content-Length"), len(data))
	}

	success := `{"error":"a member of a success body"}`
	resp, err = legacyConfigErrors{base: staticTransport{resp: rawResponse(http.StatusOK, "application/json", success)}}.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if data, _ := io.ReadAll(resp.Body); string(data) != success {
		t.Fatalf("success body changed: %s", data)
	}

	refused := errors.New("connection refused")
	if _, err := (legacyConfigErrors{base: staticTransport{err: refused}}).RoundTrip(req); !errors.Is(err, refused) {
		t.Fatalf("transport error = %v, want %v", err, refused)
	}
}

type failingBody struct{ data *strings.Reader }

func (b failingBody) Read(p []byte) (int, error) {
	if b.data.Len() == 0 {
		return 0, errors.New("connection reset")
	}
	return b.data.Read(p)
}

func (failingBody) Close() error { return nil }

func TestRoundTripBuffered_PassesUnreadableBodiesThrough(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://control.test/api/secrets", nil) //nolint:noctx // test request
	inspectAll := func(*http.Response) bool { return true }

	large := strings.Repeat("x", answerBodyLimit+10)
	resp, data, err := roundTripBuffered(staticTransport{resp: rawResponse(http.StatusOK, "text/plain", large)}, req, inspectAll)
	if err != nil || data != nil {
		t.Fatalf("oversized body: data=%d bytes err=%v, want it passed through", len(data), err)
	}
	if got, _ := io.ReadAll(resp.Body); len(got) != len(large) {
		t.Fatalf("replayed %d bytes, want %d", len(got), len(large))
	}

	broken := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: failingBody{data: strings.NewReader("partial")}}
	resp, data, err = roundTripBuffered(staticTransport{resp: broken}, req, inspectAll)
	if err != nil || data != nil {
		t.Fatalf("broken body: data=%q err=%v, want it passed through", data, err)
	}
	got, readErr := io.ReadAll(resp.Body)
	if string(got) != "partial" || readErr == nil {
		t.Fatalf("replayed %q, %v; want the partial body and its read error", got, readErr)
	}
	_ = resp.Body.Close()
}
