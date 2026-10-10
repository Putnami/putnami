package distributioncli

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
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/client"
	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	diag "go.putnami.dev/protocol/diagnostic"
	protocol "go.putnami.dev/protocol/distribution"
	regproto "go.putnami.dev/protocol/registry"
)

const (
	releaseSetProviderPath   = "/put/_/release-sets/"
	releaseSetRequestTimeout = 60 * time.Second
)

type preparedReleaseSetRequest struct {
	operation string
	body      []byte
	consume   func([]byte) ([]byte, error)
}

// ReleaseSet implements the exact distribution/release-set/v2 provider seam:
//
//	putnami cloud release-set resolve --request-file <absolute-path>
//	putnami cloud release-set release --request-file <absolute-path>
//	putnami cloud release-set channel-set --request-file <absolute-path>
//	putnami cloud release-set channel-status --request-file <absolute-path>
//
// The request file is treated as hostile input. The command emits exactly one
// compact, strictly validated protocol response and emits nothing on failure.
// The exchange is the whole command: a CI run reports its own lifecycle and the
// release set emits its own channel fact, so nothing else rides this call.
func ReleaseSet(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	operation, requestPath, err := parseReleaseSetInvocation(args)
	if err != nil {
		return err
	}
	raw, err := readReleaseSetRequestFile(requestPath)
	if err != nil {
		return err
	}
	prepared, err := prepareReleaseSetRequest(operation, raw)
	if err != nil {
		return err
	}
	credential, err := resolveReleaseSetProviderCredential(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	response, err := callReleaseSetProvider(ioctx, credential.Endpoint, credential.Token, prepared.operation, prepared.body)
	if err != nil {
		return err
	}
	output, err := prepared.consume(response)
	if err != nil {
		return err
	}
	ioctx.Stdout(string(output))
	return nil
}

func parseReleaseSetInvocation(args []string) (string, string, error) {
	protocolArgs := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] != "--putnamiContext" {
			protocolArgs = append(protocolArgs, args[index])
			continue
		}
		remaining := args[index+1:]
		if len(remaining) == 0 || strings.HasPrefix(remaining[0], "--") {
			return "", "", clicore.NewError("cloud release-set received an invalid --putnamiContext argument", clicore.ExitUsage)
		}
		index++
	}
	if len(protocolArgs) != 3 || protocolArgs[1] != protocol.RequestFileFlag || protocolArgs[2] == "" || strings.HasPrefix(protocolArgs[2], "--") {
		return "", "", releaseSetUsageError()
	}
	switch protocolArgs[0] {
	case protocol.ResolveCommand, protocol.ReleaseCommand, protocol.ChannelSetCommand, protocol.ChannelStatusCommand:
		return protocolArgs[0], protocolArgs[2], nil
	default:
		return "", "", releaseSetUsageError()
	}
}

func releaseSetUsageError() error {
	return clicore.NewError(
		"cloud release-set requires exactly resolve|release|channel-set|channel-status --request-file <absolute-path>",
		clicore.ExitUsage,
	)
}

func readReleaseSetRequestFile(path string) (data []byte, returnedErr error) {
	if !filepath.IsAbs(path) {
		return nil, clicore.NewError("cloud release-set --request-file must be an absolute path", clicore.ExitUsage)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, clicore.NewError("inspect release-set request file: "+err.Error(), clicore.ExitUsage)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, clicore.NewError("cloud release-set --request-file must be a regular non-symlink file", clicore.ExitUsage)
	}
	if before.Mode().Perm() != 0o600 {
		return nil, clicore.NewError("cloud release-set --request-file must have mode 0600", clicore.ExitUsage)
	}
	if before.Size() > int64(protocol.MaxJSONBytes) {
		return nil, releaseSetRequestSizeError()
	}

	file, err := os.Open(path) //nolint:gosec // G304: absolute path is explicitly supplied by the provider caller and verified before/after open.
	if err != nil {
		return nil, clicore.NewError("open release-set request file: "+err.Error(), clicore.ExitUsage)
	}
	defer func() {
		if closeErr := file.Close(); returnedErr == nil && closeErr != nil {
			returnedErr = clicore.NewError("close release-set request file: "+closeErr.Error(), clicore.ExitAPI)
		}
	}()

	after, err := file.Stat()
	if err != nil {
		return nil, clicore.NewError("inspect opened release-set request file: "+err.Error(), clicore.ExitUsage)
	}
	if !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 || !os.SameFile(before, after) {
		return nil, clicore.NewError("cloud release-set --request-file changed during secure open", clicore.ExitUsage)
	}
	pathAfterOpen, err := os.Lstat(path)
	if err != nil || pathAfterOpen.Mode()&os.ModeSymlink != 0 || !pathAfterOpen.Mode().IsRegular() || !os.SameFile(pathAfterOpen, after) {
		return nil, clicore.NewError("cloud release-set --request-file changed during secure open", clicore.ExitUsage)
	}
	if after.Size() > int64(protocol.MaxJSONBytes) {
		return nil, releaseSetRequestSizeError()
	}
	data, err = io.ReadAll(io.LimitReader(file, int64(protocol.MaxJSONBytes)+1))
	if err != nil {
		return nil, clicore.NewError("read release-set request file: "+err.Error(), clicore.ExitAPI)
	}
	if len(data) > protocol.MaxJSONBytes {
		return nil, releaseSetRequestSizeError()
	}
	return data, nil
}

func releaseSetRequestSizeError() error {
	return clicore.NewError(
		fmt.Sprintf("cloud release-set --request-file exceeds %d bytes", protocol.MaxJSONBytes),
		clicore.ExitUsage,
	)
}

func prepareReleaseSetRequest(operation string, raw []byte) (*preparedReleaseSetRequest, error) {
	switch operation {
	case protocol.ResolveCommand:
		request, diagnostics := protocol.ParseAndValidateResolveRequest(raw)
		if request == nil || diag.HasErrors(diagnostics) {
			return nil, releaseSetContractError("resolve request", diagnostics, clicore.ExitUsage)
		}
		body, err := marshalReleaseSetRequest(operation, request)
		if err != nil {
			return nil, err
		}
		return &preparedReleaseSetRequest{
			operation: operation,
			body:      body,
			consume: func(data []byte) ([]byte, error) {
				response, responseDiagnostics := protocol.ParseAndValidateResolveResponse(data)
				if response == nil || diag.HasErrors(responseDiagnostics) {
					return nil, releaseSetContractError("resolve response", responseDiagnostics, clicore.ExitAPI)
				}
				if exchangeDiagnostics := protocol.ValidateResolveExchange(request, response); diag.HasErrors(exchangeDiagnostics) {
					return nil, releaseSetContractError("resolve exchange", exchangeDiagnostics, clicore.ExitAPI)
				}
				return marshalReleaseSetResponse(operation, response)
			},
		}, nil
	case protocol.ReleaseCommand:
		request, diagnostics := protocol.ParseAndValidateReleaseRequest(raw)
		if request == nil || diag.HasErrors(diagnostics) {
			return nil, releaseSetContractError("release request", diagnostics, clicore.ExitUsage)
		}
		body, err := marshalReleaseSetRequest(operation, request)
		if err != nil {
			return nil, err
		}
		return &preparedReleaseSetRequest{
			operation: operation,
			body:      body,
			consume: func(data []byte) ([]byte, error) {
				response, responseDiagnostics := protocol.ParseAndValidateReleaseResponse(data)
				if response == nil || diag.HasErrors(responseDiagnostics) {
					return nil, releaseSetContractError("release response", responseDiagnostics, clicore.ExitAPI)
				}
				if exchangeDiagnostics := protocol.ValidateReleaseExchange(request, response); diag.HasErrors(exchangeDiagnostics) {
					return nil, releaseSetContractError("release exchange", exchangeDiagnostics, clicore.ExitAPI)
				}
				return marshalReleaseSetResponse(operation, response)
			},
		}, nil
	case protocol.ChannelSetCommand:
		request, diagnostics := protocol.ParseAndValidateChannelSetRequest(raw)
		if request == nil || diag.HasErrors(diagnostics) {
			return nil, releaseSetContractError("channel-set request", diagnostics, clicore.ExitUsage)
		}
		body, err := marshalReleaseSetRequest(operation, request)
		if err != nil {
			return nil, err
		}
		return &preparedReleaseSetRequest{
			operation: operation,
			body:      body,
			consume: func(data []byte) ([]byte, error) {
				response, responseDiagnostics := protocol.ParseAndValidateReleaseResponse(data)
				if response == nil || diag.HasErrors(responseDiagnostics) {
					return nil, releaseSetContractError("channel-set response", responseDiagnostics, clicore.ExitAPI)
				}
				if exchangeDiagnostics := protocol.ValidateChannelSetExchange(request, response); diag.HasErrors(exchangeDiagnostics) {
					return nil, releaseSetContractError("channel-set exchange", exchangeDiagnostics, clicore.ExitAPI)
				}
				return marshalReleaseSetResponse(operation, response)
			},
		}, nil
	case protocol.ChannelStatusCommand:
		request, diagnostics := protocol.ParseAndValidateChannelStatusRequest(raw)
		if request == nil || diag.HasErrors(diagnostics) {
			return nil, releaseSetContractError("channel-status request", diagnostics, clicore.ExitUsage)
		}
		body, err := marshalReleaseSetRequest(operation, request)
		if err != nil {
			return nil, err
		}
		return &preparedReleaseSetRequest{
			operation: operation,
			body:      body,
			consume: func(data []byte) ([]byte, error) {
				response, responseDiagnostics := protocol.ParseAndValidateChannelStatusResponse(data)
				if response == nil || diag.HasErrors(responseDiagnostics) {
					return nil, releaseSetContractError("channel-status response", responseDiagnostics, clicore.ExitAPI)
				}
				return marshalReleaseSetResponse(operation, response)
			},
		}, nil
	default:
		return nil, releaseSetUsageError()
	}
}

func marshalReleaseSetRequest(operation string, request any) ([]byte, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, clicore.NewError("encode release-set "+operation+" request: "+err.Error(), clicore.ExitAPI)
	}
	if len(body) > protocol.MaxJSONBytes {
		return nil, clicore.NewError("encoded release-set "+operation+" request exceeds protocol bounds", clicore.ExitAPI)
	}
	return body, nil
}

func marshalReleaseSetResponse(operation string, response any) ([]byte, error) {
	output, err := json.Marshal(response)
	if err != nil {
		return nil, clicore.NewError("encode release-set "+operation+" response: "+err.Error(), clicore.ExitAPI)
	}
	if len(output) > protocol.MaxJSONBytes {
		return nil, clicore.NewError("encoded release-set "+operation+" response exceeds protocol bounds", clicore.ExitAPI)
	}
	return output, nil
}

func releaseSetContractError(subject string, diagnostics []diag.Diagnostic, exitCode int) error {
	message := "invalid release-set " + subject
	if len(diagnostics) > 0 {
		message += ":\n" + formatDiagnostics(diagnostics)
	}
	return clicore.NewError(message, exitCode)
}

// resolveReleaseSetProviderCredential selects the native Put credential for
// the configured Put host. A sealed managed-publish broker advertises its
// exclusive credential by storing PutAuth for that exact host; once present,
// any exchange failure is terminal and can never fall through to a human
// session. A normal user path mints the same aud=distribution token used by
// `cloud token --for put`, whose bare marker delegates namespace authority to
// the registry's IAM projection.
//
// The one stored credential that does not take the broker path is a lease
// bearer that is no longer fresh (stalePutLease): `cloud token --for put
// --materialize` leaves such a lease behind after its one invocation, and
// put-server can only answer it with 401. That entry is skipped, not deleted,
// and the user path mints a fresh token. A fresh stored lease, a lease
// without an exp claim and an opaque key keep the broker path.
func resolveReleaseSetProviderCredential(
	params map[string]any,
	workspaceRoot string,
	env map[string]string,
	ioctx clicore.IO,
) (resolvedRegistryToken, error) {
	endpoint, err := selectedReleaseSetPutEndpoint(params, env)
	if err != nil {
		return resolvedRegistryToken{}, err
	}
	state, err := readRegistriesState(env)
	if err != nil {
		return resolvedRegistryToken{}, err
	}
	stored := ""
	if state != nil {
		stored = strings.TrimSpace(state.PutAuth[endpoint.Host])
	}
	if expiresAt, stale := stalePutLease(stored, nowOrDefault(ioctx.Now)()); stale {
		if ioctx.Stderr != nil {
			ioctx.Stderr(fmt.Sprintf("Skipping the stored Put lease for %s: it expires at %s. Minting a fresh credential.",
				endpoint.Host, expiresAt.UTC().Format(time.RFC3339)))
		}
		stored = ""
	}
	if stored != "" {
		baseURL, token, machineErr := resolvePutCredentials(params, env, ioctx)
		if machineErr != nil {
			return resolvedRegistryToken{}, machineErr
		}
		if strings.TrimRight(baseURL, "/") != strings.TrimRight(endpoint.URL, "/") {
			return resolvedRegistryToken{}, clicore.NewError("release-set credential resolved a different Put endpoint", clicore.ExitAuth)
		}
		if !regproto.ValidBearer(token) {
			return resolvedRegistryToken{}, clicore.NewError("release-set credential resolved an empty or malformed bearer", clicore.ExitAuth)
		}
		return resolvedRegistryToken{Token: token, Endpoint: endpoint}, nil
	}
	token, err := mintUserRegistryToken(params, workspaceRoot, env, ioctx, endpoint)
	if err != nil {
		return resolvedRegistryToken{}, err
	}
	if !regproto.ValidBearer(token) {
		return resolvedRegistryToken{}, clicore.NewError("release-set credential resolved an empty or malformed bearer", clicore.ExitAuth)
	}
	return resolvedRegistryToken{Token: token, Endpoint: endpoint}, nil
}

// ResolvePutPublicationBearer returns the existing short-lived Put credential
// selected by Distribution's CLI authority. Callers may forward it only to a
// server-side adapter whose Put origin is fixed independently of caller input.
// The endpoint remains internal so a consumer cannot turn this into a generic
// registry broker.
func ResolvePutPublicationBearer(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (string, error) {
	credential, err := resolveReleaseSetProviderCredential(params, workspaceRoot, env, ioctx)
	if err != nil {
		return "", err
	}
	if credential.Endpoint.Registry != RegistryPut || strings.TrimSpace(credential.Endpoint.URL) == "" || !regproto.ValidBearer(credential.Token) {
		return "", clicore.NewError("migration publication credential is not an exact Put bearer", clicore.ExitAuth)
	}
	return credential.Token, nil
}

func selectedReleaseSetPutEndpoint(params map[string]any, env map[string]string) (RegistryEndpoint, error) {
	for _, endpoint := range resolveRegistryEndpoints(params, env) {
		if endpoint.Registry == RegistryPut && strings.TrimSpace(endpoint.URL) != "" && endpoint.Host != "" {
			return endpoint, nil
		}
	}
	return RegistryEndpoint{}, clicore.NewError("release-set provider has no configured Put endpoint", clicore.ExitUsage)
}

// callReleaseSetProvider sends one release-set command to put-server through
// its generated client, forwarding the owner's Put bearer, and returns the
// answer encoded as JSON for the protocol validator the caller runs (the
// generated struct's tags are the protocol's wire names). The endpoint must be
// HTTPS (HTTP only on loopback); the generated client refuses every redirect,
// so the bearer can never follow one to another authority.
func callReleaseSetProvider(
	ioctx clicore.IO,
	endpoint RegistryEndpoint,
	token, operation string,
	body []byte,
) ([]byte, error) {
	if _, err := releaseSetOperationURL(endpoint.URL, operation); err != nil {
		return nil, err
	}
	ctx := ioctx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, releaseSetRequestTimeout)
	defer cancel()
	ctx = client.WithForwardedUserToken(ctx, token)
	ctx = context.WithValue(ctx, verbatimReleaseSetBodyKey{}, body)
	verbatim := ioctx
	verbatim.Client = verbatimReleaseSetClient(ioctx.Client)
	put, err := putOwnerClient(verbatim, strings.TrimRight(strings.TrimSpace(endpoint.URL), "/"))
	if err != nil {
		return nil, err
	}
	var answer any
	switch operation {
	case protocol.ResolveCommand:
		var in putserverclient.CreatePutReleaseSetsResolveInput
		if err = decodeReleaseSetRequest(operation, body, &in.Body); err == nil {
			answer, err = put.CreatePutReleaseSetsResolve(ctx, in)
		}
	case protocol.ReleaseCommand:
		var in putserverclient.CreatePutReleaseSetsReleaseInput
		if err = decodeReleaseSetRequest(operation, body, &in.Body); err == nil {
			answer, err = put.CreatePutReleaseSetsRelease(ctx, in)
		}
	case protocol.ChannelSetCommand:
		var in putserverclient.CreatePutReleaseSetsChannelSetInput
		if err = decodeReleaseSetRequest(operation, body, &in.Body); err == nil {
			answer, err = put.CreatePutReleaseSetsChannelSet(ctx, in)
		}
	case protocol.ChannelStatusCommand:
		var in putserverclient.CreatePutReleaseSetsChannelStatusInput
		if err = decodeReleaseSetRequest(operation, body, &in.Body); err == nil {
			answer, err = put.CreatePutReleaseSetsChannelStatus(ctx, in)
		}
	default:
		return nil, releaseSetUsageError()
	}
	if err != nil {
		var exit *clicore.ExitError
		if errors.As(err, &exit) {
			return nil, err
		}
		if status := clicore.ServiceStatus(err); status != 0 {
			exitCode := clicore.ExitAPI
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				exitCode = clicore.ExitAuth
			}
			return nil, clicore.NewError(releaseSetRefusal(operation, status, err), exitCode)
		}
		return nil, clicore.NewError("release-set "+operation+" request failed: "+err.Error(), clicore.ExitAPI)
	}
	data, err := json.Marshal(answer)
	if err != nil {
		return nil, clicore.NewError("encode release-set "+operation+" response: "+err.Error(), clicore.ExitAPI)
	}
	if len(data) > protocol.MaxJSONBytes {
		return nil, clicore.NewError("release-set provider response exceeds protocol bounds", clicore.ExitAPI)
	}
	return data, nil
}

// releaseSetRefusal renders a provider refusal as the status line. A
// member_missing refusal (the release introduced a put member whose artifact
// the Put registry does not store) also names its code and the provider's
// message, so the publisher reads which member to publish first. Every other
// refusal keeps the status line alone.
func releaseSetRefusal(operation string, status int, err error) string {
	refusal := fmt.Sprintf("release-set %s request failed: HTTP %s", operation, clicore.StatusLine(status))
	var missing *putserverclient.CreatePutReleaseSetsReleaseMemberMissingError
	if !errors.As(err, &missing) {
		return refusal
	}
	refusal += ": " + missing.Remote.Code()
	if message := clicore.ServiceMessage(err); message != "" {
		refusal += ": " + message
	}
	return refusal
}

// verbatimReleaseSetBodyKey carries the validated protocol bytes of one
// release-set call to the transport.
type verbatimReleaseSetBodyKey struct{}

// verbatimReleaseSetClient sends the validated protocol bytes in place of the
// generated client's re-encoding. The generated body orders its keys
// alphabetically, the protocol does not, and the CI publication broker
// compares each inherited member byte for byte with the one put-server
// resolved. A re-encoded member never matched, so the broker refused every
// release that inherited one.
func verbatimReleaseSetClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	wrapped := *base
	wrapped.Transport = verbatimReleaseSetTransport{base: base.Transport}
	return &wrapped
}

type verbatimReleaseSetTransport struct{ base http.RoundTripper }

func (t verbatimReleaseSetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	body, ok := request.Context().Value(verbatimReleaseSetBodyKey{}).([]byte)
	if !ok || request.Body == nil {
		return base.RoundTrip(request)
	}
	_ = request.Body.Close()
	request = request.Clone(request.Context())
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	request.ContentLength = int64(len(body))
	return base.RoundTrip(request)
}

// decodeReleaseSetRequest projects the validated protocol request onto the
// generated request body; both carry the protocol's wire names.
func decodeReleaseSetRequest(operation string, body []byte, into any) error {
	if err := json.Unmarshal(body, into); err != nil {
		return clicore.NewError("encode release-set "+operation+" request: "+err.Error(), clicore.ExitAPI)
	}
	return nil
}

func releaseSetOperationURL(rawBaseURL, operation string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", clicore.NewError("release-set provider has an invalid Put endpoint", clicore.ExitUsage)
	}
	if !secureReleaseSetEndpoint(base) {
		return "", clicore.NewError("release-set provider endpoint must use HTTPS (HTTP is allowed only on loopback)", clicore.ExitUsage)
	}
	base.Path = strings.TrimRight(base.Path, "/") + releaseSetProviderPath + operation
	base.RawPath = ""
	return base.String(), nil
}

func secureReleaseSetEndpoint(endpoint *url.URL) bool {
	switch strings.ToLower(endpoint.Scheme) {
	case "https":
		return true
	case "http":
		host := endpoint.Hostname()
		return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
	default:
		return false
	}
}
