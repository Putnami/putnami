package configcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"go.putnami.dev/client"
	configapiclient "go.putnami.dev/cloud/clients/config-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
)

// configAPI reaches config-api's generated client through the control-plane
// host the workspace link names, with the caller's bearer forwarded per call.
// The config, secrets and publish-config commands share it.
type configAPI struct {
	controlPlane string
	authToken    clicore.Bearer
	httpClient   *http.Client
	// parent is the invocation context every call derives from, so a caller
	// such as `putnami cloud status` can cancel the reads; nil is
	// context.Background().
	parent context.Context
}

func (ctx *secretsCtx) configAPI() configAPI {
	return configAPI{controlPlane: ctx.controlPlane, authToken: ctx.authToken, httpClient: ctx.io.Client, parent: ctx.io.Context}
}

func (ctx *publishCtx) configAPI() configAPI {
	return configAPI{controlPlane: ctx.controlPlane, authToken: ctx.authToken, httpClient: ctx.io.Client}
}

// configAPIError is a refusal config-api answered: its status, the prose of
// its `message`, the `details` list of a refused schema manifest, and whether
// it is config.schema_not_registered, the refusal of a write for an app with
// no registered schema.
type configAPIError struct {
	status              int
	message             string
	details             []string
	schemaNotRegistered bool
}

func (e *configAPIError) Error() string { return e.message }

// client resolves config-api's generated client for one call. headers are the
// static headers the call sends; answer records what the call's last attempt
// met.
func (api configAPI) client(headers map[string]string, answer *answerRecord) (*configapiclient.ConfigClient, error) {
	httpClient := withAnswerRecorder(withLegacyConfigErrors(api.httpClient), answer)
	binding := clicore.ServiceBindingFor(api.controlPlane, httpClient)
	binding.Headers = headers
	return clicore.NewServiceClient[configapiclient.ConfigClient](configapiclient.RegisterConfigClient, binding)
}

// target is the URL a failed call names, the same one the hand-written
// requests named before the generated client. A /v1/ path is a workspace
// route and is not under /api.
func (api configAPI) target(path string) string {
	base := strings.TrimRight(api.controlPlane, "/")
	if strings.HasPrefix(path, "/v1/") {
		return base + path
	}
	return base + "/api" + path
}

// configCall sends one generated config-api call and answers the reply the
// generated client decoded, never nil. A refusal is a *configAPIError.
func configCall[In, Out any](api configAPI, path string, headers map[string]string, input In,
	call func(*configapiclient.ConfigClient, context.Context, In) (*Out, error)) (*Out, error) {
	answer := &answerRecord{}
	configClient, err := api.client(headers, answer)
	if err != nil {
		return nil, err
	}
	parent := api.parent
	if parent == nil {
		parent = context.Background()
	}
	callCtx := (&clicore.WorkspaceContext{AuthToken: api.authToken}).CallContext(parent)
	result, err := call(configClient, callCtx, input)
	if err != nil {
		return nil, configAPIFailure(api.target(path), answer, err)
	}
	if result == nil {
		result = new(Out)
	}
	return result, nil
}

// configCallAs sends one generated config-api call and converts its reply into
// T, the generic shape a command walks: the schema commands read a schema
// document as a map. A refusal is a *configAPIError.
func configCallAs[In, Out, T any](api configAPI, path string, headers map[string]string, input In,
	call func(*configapiclient.ConfigClient, context.Context, In) (*Out, error)) (*T, error) {
	result, err := configCall(api, path, headers, input, call)
	if err != nil {
		return nil, err
	}
	converted, err := convertJSON[T](result)
	if err != nil {
		return nil, invalidConfigAnswer(api.target(path))
	}
	return converted, nil
}

// invalidConfigAnswer is the error for an answer the CLI cannot read.
func invalidConfigAnswer(target string) error {
	return clicore.NewError("invalid JSON response from "+target, clicore.ExitAPI)
}

// jsonTree decodes a free-form tree that config-api answers (a resolved config
// or secrets tree, which the generated client keeps as raw JSON members) into
// the plain values the commands walk and print. An absent or null tree is nil.
// target names the call in the error.
func jsonTree(target string, tree client.Optional[map[string]json.RawMessage]) (map[string]any, error) {
	var out map[string]any
	if members, ok := tree.Value(); ok && members != nil {
		out = make(map[string]any, len(members))
		for key, raw := range members {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, invalidConfigAnswer(target)
			}
			out[key] = value
		}
	}
	return out, nil
}

// requestBody converts a request the CLI builds as a map into the generated
// input type. The generated types hold every member config-api declares, so
// the request sent is the one the hand-written call sent.
func requestBody[T any](body any) (T, error) {
	converted, err := convertJSON[T](body)
	if err != nil {
		var zero T
		return zero, clicore.NewError("marshal request body: "+err.Error(), clicore.ExitAPI)
	}
	return *converted, nil
}

func convertJSON[T any](value any) (*T, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	converted := new(T)
	if err := json.Unmarshal(data, converted); err != nil {
		return nil, err
	}
	return converted, nil
}

// configAPIFailure renders a failed generated call. A provider refusal is a
// *configAPIError carrying the provider's message. When the refusal sent none,
// a body that is not JSON (a load balancer or Cloud Run page) is shown as a
// sanitized snippet with its status, and anything else as the status
// line. An answer outside the contract and a transport failure keep the text
// the hand-written requests printed: the generated client reports a transport
// failure without its cause, so the cause comes from answer.
func configAPIFailure(target string, answer *answerRecord, err error) error {
	var remote *client.RemoteError
	if errors.As(err, &remote) {
		message := strings.TrimSpace(remote.Message)
		if message == "" && answer.recorded() {
			message = clicore.InvalidJSONResponseError(target, answer.status, answer.body).Error()
		}
		failure := &configAPIError{
			status:              remote.StatusCode,
			message:             clicore.FirstString(message, clicore.StatusLine(remote.StatusCode)),
			schemaNotRegistered: isSchemaNotRegistered(err),
		}
		var invalid *configapiclient.CreateApiSchemasConfigSchemaInvalidError
		if errors.As(err, &invalid) && invalid.Payload != nil {
			failure.details = *invalid.Payload
		}
		return failure
	}
	if perrors.Is(err, client.CodeClientResponse) {
		if answer.recorded() {
			return clicore.InvalidJSONResponseError(target, answer.status, answer.body)
		}
		return clicore.NewError("invalid JSON response from "+target, clicore.ExitAPI)
	}
	detail := err.Error()
	if answer.transportErr != nil && perrors.Is(err, client.CodeClientRequest) {
		detail = answer.transportErr.Error()
	}
	return clicore.NewError(fmt.Sprintf("request failed for %s: %s", target, detail), clicore.ExitAPI)
}

// answerRecord is what the last attempt of one call met: the status and body
// of an answer whose body was not JSON, or the transport failure that left it
// without an answer. A failure shows it so the operator sees what happened.
type answerRecord struct {
	status       int
	body         []byte
	transportErr error
}

func (answer *answerRecord) recorded() bool { return answer.body != nil }

// answerBodyLimit bounds the body a transport below buffers to inspect it; a
// longer body passes through unread.
const answerBodyLimit = 1 << 20

// withAnswerRecorder returns a copy of base whose transport records into answer
// what each attempt met; see answerRecord.
func withAnswerRecorder(base *http.Client, answer *answerRecord) *http.Client {
	wrapped := *base
	wrapped.Transport = answerRecorder{base: base.Transport, answer: answer}
	return &wrapped
}

type answerRecorder struct {
	base   http.RoundTripper
	answer *answerRecord
}

func (t answerRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, data, err := roundTripBuffered(t.base, req, func(*http.Response) bool { return true })
	*t.answer = answerRecord{transportErr: err}
	if err == nil && len(bytes.TrimSpace(data)) > 0 && !json.Valid(data) {
		t.answer.status, t.answer.body = resp.StatusCode, data
	}
	return resp, err
}

type replayBody struct {
	io.Reader
	io.Closer
}

// roundTripBuffered sends req through base and, when inspect selects the
// response, reads its body into memory and puts an identical body back. data
// is nil when the response was not selected, carried no body, or its body
// could not be read whole within answerBodyLimit; the body then reaches the
// client unchanged.
func roundTripBuffered(base http.RoundTripper, req *http.Request, inspect func(*http.Response) bool) (*http.Response, []byte, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || !inspect(resp) {
		return resp, nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, answerBodyLimit+1))
	if readErr != nil || len(data) > answerBodyLimit {
		// The client reads the bytes already taken, then the rest of the body,
		// and meets the read error itself.
		resp.Body = replayBody{Reader: io.MultiReader(bytes.NewReader(data), resp.Body), Closer: resp.Body}
		return resp, nil, nil
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, data, nil
}

func isSchemaNotRegistered(err error) bool {
	var configs *configapiclient.UpdateApiConfigsConfigSchemaNotRegisteredError
	var secrets *configapiclient.UpdateApiSecretsConfigSchemaNotRegisteredError
	return errors.As(err, &configs) || errors.As(err, &secrets)
}

// configStatus is the HTTP status of a config-api refusal, or 0.
func configStatus(err error) int {
	var apiErr *configAPIError
	if errors.As(err, &apiErr) {
		return apiErr.status
	}
	return 0
}

// putConfig writes one config block: PUT /api/configs. roll and rollShared are
// the roll-control query values, sent only when set.
func (api configAPI) putConfig(body map[string]any, roll, rollShared string) (*configapiclient.PutConfigResponse, error) {
	entry, err := requestBody[configapiclient.Entry](body)
	if err != nil {
		return nil, err
	}
	input := configapiclient.UpdateApiConfigsInput{Body: entry}
	if roll != "" {
		input.Query.Roll = &roll
	}
	if rollShared != "" {
		input.Query.RollShared = &rollShared
	}
	return configCall(api, "/configs", nil, input, (*configapiclient.ConfigClient).UpdateApiConfigs)
}

// resolveConfigs resolves an app's config: POST /api/configs/resolve. A
// non-empty workspaceID travels as the workspace header a Google
// service-account caller needs.
func (api configAPI) resolveConfigs(body map[string]any, workspaceID string) (*resolvedConfig, error) {
	request, err := requestBody[configapiclient.ResolveConfigsRequest](body)
	if err != nil {
		return nil, err
	}
	var headers map[string]string
	if workspaceID != "" {
		headers = map[string]string{configWorkspaceHeader: workspaceID}
	}
	resp, err := configCall(api, "/configs/resolve", headers, configapiclient.CreateApiConfigsResolveInput{Body: request},
		(*configapiclient.ConfigClient).CreateApiConfigsResolve)
	if err != nil {
		return nil, err
	}
	return resolvedConfigFrom(api.target("/configs/resolve"), resp)
}

// registerSchema registers an app's schema manifest: POST /api/schemas.
func (api configAPI) registerSchema(doc map[string]any) error {
	manifest, err := requestBody[configapiclient.SchemaManifestDoc](doc)
	if err != nil {
		return err
	}
	_, err = configCall(api, "/schemas", nil, configapiclient.CreateApiSchemasInput{Body: manifest},
		(*configapiclient.ConfigClient).CreateApiSchemas)
	return err
}

// fetchSchema reads an app's latest registered schema: GET /api/schemas?appName=,
// then GET /api/schemas/{app} when that route answers 404.
func (api configAPI) fetchSchema(app string) (map[string]any, error) {
	schema, err := configCallAs[configapiclient.ListApiSchemasInput, configapiclient.SchemaManifestDoc, map[string]any](
		api, "/schemas", nil, configapiclient.ListApiSchemasInput{Query: configapiclient.ListApiSchemasQuery{AppName: app}},
		(*configapiclient.ConfigClient).ListApiSchemas)
	if configStatus(err) == http.StatusNotFound {
		schema, err = configCallAs[configapiclient.GetApiSchemasInput, configapiclient.SchemaManifestDoc, map[string]any](
			api, "/schemas/"+clicore.URLPathEscape(app), nil, configapiclient.GetApiSchemasInput{Path: configapiclient.GetApiSchemasPath{App: app}},
			(*configapiclient.ConfigClient).GetApiSchemas)
	}
	if err != nil {
		return nil, err
	}
	return *schema, nil
}

// putSecret writes one secret block: PUT /api/secrets.
func (api configAPI) putSecret(body map[string]any) (*putSecretResult, error) {
	request, err := requestBody[configapiclient.PutSecretRequest](body)
	if err != nil {
		return nil, err
	}
	resp, err := configCall(api, "/secrets", nil, configapiclient.UpdateApiSecretsInput{Body: request},
		(*configapiclient.ConfigClient).UpdateApiSecrets)
	if err != nil {
		return nil, err
	}
	return &putSecretResult{
		AppName:     clicore.Deref(resp.AppName),
		Environment: clicore.Deref(resp.Environment),
		Path:        clicore.Deref(resp.Path),
		Status:      clicore.Deref(resp.Status),
	}, nil
}

// listSecrets lists secret entries: GET /api/secrets. A scoped list names the
// app and the environment, even when empty, as the hand-written query did; an
// unscoped one lists the whole workspace.
func (api configAPI) listSecrets(app, environment string, scoped bool) ([]secretEntry, error) {
	input := configapiclient.ListApiSecretsInput{}
	if scoped {
		input.Query.AppName = &app
		input.Query.Environment = &environment
	}
	resp, err := configCall(api, "/secrets", nil, input, (*configapiclient.ConfigClient).ListApiSecrets)
	if err != nil {
		return nil, err
	}
	return secretEntriesFrom(resp.Entries), nil
}

// resolveSecrets resolves an app's secrets: POST /api/secrets/resolve.
func (api configAPI) resolveSecrets(body map[string]any) (*resolvedSecrets, error) {
	request, err := requestBody[configapiclient.ResolveSecretsRequest](body)
	if err != nil {
		return nil, err
	}
	resp, err := configCall(api, "/secrets/resolve", nil, configapiclient.CreateApiSecretsResolveInput{Body: request},
		(*configapiclient.ConfigClient).CreateApiSecretsResolve)
	if err != nil {
		return nil, err
	}
	secrets, err := jsonTree(api.target("/secrets/resolve"), resp.Secrets)
	if err != nil {
		return nil, err
	}
	return &resolvedSecrets{Secrets: secrets, Layers: optionalLayers(resp.Layers)}, nil
}

// deleteSecret deletes one secret field, or a whole block when field is empty:
// DELETE /api/secrets.
func (api configAPI) deleteSecret(app, environment, path, field string) error {
	input := configapiclient.DeleteApiSecretsInput{Query: configapiclient.DeleteApiSecretsQuery{
		AppName: app, Environment: environment, Path: path,
	}}
	if field != "" {
		input.Query.Field = &field
	}
	_, err := configCall(api, "/secrets", nil, input, (*configapiclient.ConfigClient).DeleteApiSecrets)
	return err
}

// secretStatus lists the schema-declared secret keys and their state:
// GET /api/secrets/status.
func (api configAPI) secretStatus(app, environment string) (*configapiclient.SecretStatusResponse, error) {
	input := configapiclient.ListApiSecretsStatusInput{Query: configapiclient.ListApiSecretsStatusQuery{
		AppName: app, Environment: environment,
	}}
	return configCall(api, "/secrets/status", nil, input, (*configapiclient.ConfigClient).ListApiSecretsStatus)
}

// listConfigApps lists the workspace's config apps and the environments each
// has values or secrets in: GET /v1/workspaces/{workspace}/config/apps. It
// returns names only, never a value.
func (api configAPI) listConfigApps(workspaceID string) (*configapiclient.ConfigAppList, error) {
	input := configapiclient.GetV1WorkspacesConfigAppsInput{Path: configapiclient.GetV1WorkspacesConfigAppsPath{Workspace: workspaceID}}
	return configCall(api, "/v1/workspaces/"+clicore.URLPathEscape(workspaceID)+"/config/apps", nil, input,
		(*configapiclient.ConfigClient).GetV1WorkspacesConfigApps)
}

// listSecretGrants lists the workspace's secret grants, the reads one
// project's service account may make of another app's secret paths: GET
// /v1/workspaces/{workspace}/config/secret-grants. It returns no value and
// needs platform.workspace.manage.
func (api configAPI) listSecretGrants(workspaceID string) (*configapiclient.GrantList, error) {
	input := configapiclient.GetV1WorkspacesConfigSecretGrantsInput{Path: configapiclient.GetV1WorkspacesConfigSecretGrantsPath{Workspace: workspaceID}}
	return configCall(api, "/v1/workspaces/"+clicore.URLPathEscape(workspaceID)+"/config/secret-grants", nil, input,
		(*configapiclient.ConfigClient).GetV1WorkspacesConfigSecretGrants)
}

// Compatibility: an older config-api answers a refusal with a hand-rolled
// body, `{"error": <prose>}` plus `details` for a refused schema manifest, with no `code` and usually no
// `message`. A generated client reads only `code`, `message` and `details`, so
// legacyConfigErrors rewrites such a body into the first-party envelope before
// the client reads it: the prose becomes `message`, and the two refusals the
// commands explain get the code the current config-api declares for them.
// Remove withLegacyConfigErrors and everything below it once every control
// plane serves a config-api whose refusals carry `code`.
const (
	legacySchemaNotRegistered = "config.schema_not_registered"
	legacySchemaInvalid       = "config.schema_invalid"
)

// withLegacyConfigErrors returns a copy of base whose transport rewrites a
// legacy config-api refusal; see legacyConfigErrors.
func withLegacyConfigErrors(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	wrapped := *base
	wrapped.Transport = legacyConfigErrors{base: base.Transport}
	return &wrapped
}

type legacyConfigErrors struct{ base http.RoundTripper }

func (t legacyConfigErrors) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, data, err := roundTripBuffered(t.base, req, func(resp *http.Response) bool {
		return resp.StatusCode >= http.StatusBadRequest
	})
	if err != nil || data == nil {
		return resp, err
	}
	if envelope, ok := legacyErrorEnvelope(resp.StatusCode, data); ok {
		resp.Body = io.NopCloser(bytes.NewReader(envelope))
		resp.ContentLength = int64(len(envelope))
		if resp.Header == nil {
			resp.Header = http.Header{}
		}
		resp.Header.Set("Content-Length", strconv.Itoa(len(envelope)))
	}
	return resp, nil
}

// legacyErrorEnvelope answers the first-party envelope of a legacy refusal
// body, or false when body is not one: not a JSON object, already carrying a
// `code`, or with no `error` prose.
func legacyErrorEnvelope(status int, body []byte) ([]byte, bool) {
	var members map[string]json.RawMessage
	if json.Unmarshal(body, &members) != nil || members == nil {
		return nil, false
	}
	if _, ok := members["code"]; ok {
		return nil, false
	}
	var prose string
	if json.Unmarshal(members["error"], &prose) != nil || strings.TrimSpace(prose) == "" {
		return nil, false
	}
	var message string
	if json.Unmarshal(members["message"], &message) != nil || strings.TrimSpace(message) == "" {
		members["message"], _ = json.Marshal(prose)
	}
	if code := legacyErrorCode(status, prose); code != "" {
		members["code"], _ = json.Marshal(code)
	}
	envelope, err := json.Marshal(members)
	if err != nil {
		return nil, false
	}
	return envelope, true
}

// legacyErrorCode is the code the current config-api writes for the legacy
// prose, or "" for a refusal no command explains.
func legacyErrorCode(status int, prose string) string {
	if status != http.StatusBadRequest {
		return ""
	}
	switch {
	case prose == "invalid schema manifest":
		return legacySchemaInvalid
	case strings.Contains(strings.ToLower(prose), "no schema registered"):
		return legacySchemaNotRegistered
	default:
		return ""
	}
}
