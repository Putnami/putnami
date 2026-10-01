package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/proto"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	connectwire "go.putnami.dev/protocol/clientcontract/connect"
)

// ApiBridgeConfig configures the api → Connect bridge.
type ApiBridgeConfig struct {
	// PackageName is the proto package the Connect URLs are scoped to. Default "api.v1";
	// must match the proto plugin's PackageName so generated clients route correctly.
	PackageName string
	// ServiceName is the Connect service name. Default "ApiService"; must match the proto
	// plugin's emitted service name.
	ServiceName string
	// MaxMessageBytes bounds one decoded Connect message, before and after gzip
	// decompression. Zero uses defaultConnectMessageBytes.
	MaxMessageBytes int64
	// StreamWriteTimeout bounds each individual write of a Connect server
	// stream. The bridge clears the server-wide write deadline for the stream's
	// lifetime — a long-lived stream is not a slow response — and re-arms this
	// bound before every write instead. Zero uses defaultConnectWriteTimeout; a
	// negative value disables the per-write bound.
	StreamWriteTimeout time.Duration
}

// ApiBridge registers Connect-style RPC paths on an api.Server, bridging each api.Plugin
// endpoint to a `/<package>.<Service>/<RPCName>` URL. The bridge:
//
//   - Decodes a request body — a JSON `{params, query, body}` envelope, or the
//     same envelope in protobuf binary when the provider publishes a descriptor —
//     into the endpoint's path params, query params and request body.
//   - Re-invokes the endpoint's validation pipeline so input validation behaves
//     identically to a direct REST call.
//   - Writes the handler's reply back in the codec the caller asked for, and any
//     failure as the Connect error document.
//   - Serves a declared server stream as enveloped messages ended by exactly one
//     EndStreamResponse.
//
// See doc/adr/0001-bridge-reenters-the-endpoint-pipeline.md for why the bridge
// re-enters the pipeline and what the proto codec and the streamed shape are
// derived from. Client and
// bidirectional streams stay outside this contract: Connect carries at most one
// request message per call.
type ApiBridge struct {
	apiPlugin *api.Plugin
	server    api.Server
	config    ApiBridgeConfig
	// bound records the Connect URL registered for each route, keyed by
	// "<METHOD> <path>", so Start can prove the mounted URLs are exactly the
	// method identities the published descriptor declares.
	bound map[string]string
	// duplex records the client and bidirectional stream routes the bridge
	// deliberately leaves unmounted. The descriptor still declares their
	// methods, and Start must not read that as a missing URL.
	duplex map[string]bool
	// codec carries the published protobuf descriptor. It is nil when the
	// provider mounts no proto plugin, and that is exactly when the bridge
	// serves — and advertises — JSON only.
	codec *connectwire.ProtoCodec
}

// NewApiBridge builds a bridge against an api plugin and a transport server. Defaults
// align with the proto plugin (package "api.v1", service "ApiService").
func NewApiBridge(apiPlugin *api.Plugin, server api.Server, opts ...func(*ApiBridgeConfig)) *ApiBridge {
	cfg := ApiBridgeConfig{PackageName: "api.v1", ServiceName: "ApiService"}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &ApiBridge{apiPlugin: apiPlugin, server: server, config: cfg}
}

// WithPackage overrides the proto package name (e.g. "myapp.v1").
func WithPackage(name string) func(*ApiBridgeConfig) {
	return func(c *ApiBridgeConfig) {
		if name != "" {
			c.PackageName = name
		}
	}
}

// WithMaxMessageBytes bounds one decoded Connect message. A Connect payload is
// decompressed and decoded before any handler sees it, so the ceiling exists
// before the first byte is read.
func WithMaxMessageBytes(limit int64) func(*ApiBridgeConfig) {
	return func(c *ApiBridgeConfig) {
		if limit > 0 {
			c.MaxMessageBytes = limit
		}
	}
}

// WithStreamWriteTimeout bounds each individual write of a Connect server
// stream. It replaces the server-wide write deadline.
func WithStreamWriteTimeout(bound time.Duration) func(*ApiBridgeConfig) {
	return func(c *ApiBridgeConfig) {
		c.StreamWriteTimeout = bound
	}
}

// Name implements app.Plugin. The bridge participates in the lifecycle so its Configure
// fires after the api plugin's, ensuring all endpoints are dispatched before binding.
func (b *ApiBridge) Name() string { return "grpc-api-bridge" }

// Configure registers the Connect-style HTTP routes for every api endpoint definition.
//
// When the proto plugin has already published its descriptor, the URL comes from
// that published route binding rather than from a second local computation of
// the RPC name. The local computation cannot disambiguate two routes that reduce
// to the same name (POST /users and POST /users/{id} both yield CreateUsers),
// so it mounted one URL twice while the descriptor declared two distinct
// methods; a generated Connect client then called a method the bridge never
// served.
func (b *ApiBridge) Configure(_ context.Context, _ *app.Module) error {
	if b.apiPlugin == nil || b.server == nil {
		return nil
	}
	published := b.apiPlugin.ClientProtobufMethods()
	b.codec = nil
	// descriptorPublished is whether a descriptor exists at all, not whether it
	// binds any route: a descriptor that binds none still decides that none of
	// them is served over Connect.
	descriptorPublished := false
	if contract := b.apiPlugin.ClientServiceContract(); contract != nil && contract.Protobuf != nil {
		descriptorPublished = true
		codec, err := connectwire.NewProtoCodec(contract.Protobuf)
		if err != nil {
			// A published descriptor this bridge cannot read is a provider
			// declaration error, not a reason to fall back to JSON: the
			// contract would then advertise a descriptor nothing serves.
			return err
		}
		b.codec = codec
	}
	b.bound = map[string]string{}
	b.duplex = map[string]bool{}
	for _, def := range b.apiPlugin.Definitions() {
		if !isHTTPMethod(def.Method()) {
			continue
		}
		// Document-only endpoints have no handler attached to the api plugin
		// — the handler is mounted directly on the http server, outside the
		// api builder. Bridging them would expose a Connect-style RPC URL
		// whose handler returns 500 ("invalid handler type") because
		// EndpointDefinition.BuildHandler has nothing to wrap.
		if def.IsDocumentOnly() {
			continue
		}
		// A route an external authority owns speaks the standard's wire
		// format. It has no protobuf method, so a Connect URL for it would
		// serve a method no descriptor declares — with or without a published
		// descriptor.
		if def.ClientOptions().IsExternal() {
			continue
		}
		// A client or bidirectional stream has no Connect shape this bridge can
		// serve: Connect carries at most one request message per call. Mounting
		// a URL for one would answer a declared duplex stream with a single
		// buffered reply.
		if def.IsStream() && def.StreamMode() != api.StreamModeServer {
			b.duplex[proto.RouteKey(def.Method(), b.apiPlugin.PrefixPath(def.Path()))] = true
			continue
		}
		path := b.apiPlugin.PrefixPath(def.Path())
		key := proto.RouteKey(def.Method(), path)
		rpcPath, declared := published[key]
		if !declared && descriptorPublished {
			// The published descriptor decides which routes Connect serves, and
			// it leaves this one out — today because its shape has no lossless
			// proto3 form (opaque JSON). The route keeps its REST transport. A
			// locally computed URL would serve a method the descriptor does not
			// declare, and could collide with one it does. Only a provider with
			// no published descriptor falls back to computed names below.
			continue
		}
		if !declared {
			rpcName := clientcontract.RPCName(def.Method(), path)
			rpcPath = fmt.Sprintf("/%s.%s/%s", b.config.PackageName, b.config.ServiceName, rpcName)
		}
		b.bound[key] = rpcPath
		route := connectRoute{definition: def, path: path, identity: rpcPath, serverSide: def.IsStream()}
		if route.serverSide {
			route.stream = def.BuildStreamHandler()
		} else {
			route.unary = b.bridgeHandler(def, path)
		}
		b.server.Handle("POST", rpcPath, b.connectHandler(route))
	}
	// A mounted bridge is what makes a Connect transport honorable: the contract
	// advertises Connect only for a provider that serves it, and only with the
	// encodings served here. proto is advertised exactly when a descriptor is
	// published, because that descriptor is the codec.
	encodings := []clientcontract.Encoding{clientcontract.EncodingJSON}
	if b.codec != nil {
		encodings = append(encodings, clientcontract.EncodingProto)
	}
	b.apiPlugin.PublishClientConnectTransport(encodings)
	return nil
}

// Start proves the mounted Connect URLs are the exact method identities the
// published descriptor declares.
func (b *ApiBridge) Start(_ context.Context, _ *app.Module) error {
	if b.apiPlugin == nil || b.server == nil {
		return nil
	}
	published := b.apiPlugin.ClientProtobufMethods()
	if len(published) == 0 {
		return nil
	}
	for _, key := range sortedRouteKeys(published) {
		if b.duplex[key] {
			continue
		}
		declared := published[key]
		mounted, ok := b.bound[key]
		if !ok {
			return fmt.Errorf(
				"grpc: the protobuf descriptor declares %s for route %s but the bridge mounted no Connect URL for it; register the api bridge after the api plugin",
				declared, key)
		}
		if mounted != declared {
			return fmt.Errorf(
				"grpc: route %s is mounted at %s but the protobuf descriptor declares %s; register the api bridge after the proto plugin so both read one naming",
				key, mounted, declared)
		}
	}
	return nil
}

func sortedRouteKeys(routes map[string]string) []string {
	keys := make([]string, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Stop is a no-op.
func (b *ApiBridge) Stop(_ context.Context, _ *app.Module) error { return nil }

// bridgeHandler captures the api endpoint's handler and wraps it with input remapping.
// The wrapper:
//
//  1. Decodes a Connect-style JSON envelope of the shape
//     `{"params": {...}, "query": {...}, "body": {...}}` — keeping the three sections
//     namespaced so colliding field names (e.g. PUT /users/{id} with a body field
//     "id") no longer fight for the same key.
//  2. Maps each section onto the wrapped Context: path params populate ctx.Params,
//     query fields rebuild the URL's RawQuery, body is re-encoded as JSON for the
//     endpoint's validateBody pipeline.
//  3. Calls the wrapped endpoint handler and returns its response unchanged.
func (b *ApiBridge) bridgeHandler(def api.EndpointDefinition, path string) phttp.Handler {
	wrapped := def.BuildHandler()

	return func(ctx *phttp.Context) *phttp.Response {
		var data []byte
		if ctx.Request.Body != nil {
			read, err := io.ReadAll(ctx.Request.Body)
			if err != nil {
				return phttp.JSONStatus(400, map[string]string{
					"error":   "Bad Request",
					"message": "Failed to read Connect RPC body",
				})
			}
			data = read
		}
		if rejection := applyBridgeEnvelope(ctx, def, path, data); rejection != nil {
			return rejection
		}
		return wrapped(ctx)
	}
}

// applyBridgeEnvelope maps the `{params, query, body}` envelope onto the
// Context so the wrapped endpoint sees the exact shape a direct REST call to the
// original path would have produced. Both bridged shapes — the unary handler and
// the server-stream handler — go through it, so a path param means the same
// thing whichever codec carried it.
func applyBridgeEnvelope(ctx *phttp.Context, def api.EndpointDefinition, path string, data []byte) *phttp.Response {
	envelope := bridgeEnvelope{}
	if len(data) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&envelope); err != nil {
			return phttp.JSONStatus(400, map[string]string{
				"error":   "Bad Request",
				"message": "Invalid JSON body for Connect RPC",
			})
		}
	}

	params := map[string]string{}
	for k, v := range envelope.Params {
		params[k] = bridgeValueToString(v)
	}
	query := url.Values{}
	for k, v := range envelope.Query {
		query.Set(k, bridgeValueToString(v))
	}

	ctx.Method = def.Method()
	ctx.Path = path
	ctx.Route = path
	ctx.Params = params

	// Replace the request URL's RawQuery so QueryParams() returns the bridged values.
	if ctx.Request.URL != nil {
		ctx.Request.URL.RawQuery = query.Encode()
	}
	ctx.Request.Method = def.Method()

	// Replace the body with the (possibly empty) section so validateBody decodes
	// against a clean payload. GET/DELETE methods short-circuit body validation in
	// the api builder, so an empty body is harmless.
	if len(envelope.Body) == 0 {
		ctx.Request.Body = io.NopCloser(strings.NewReader("{}"))
		return nil
	}
	encoded, err := json.Marshal(envelope.Body)
	if err != nil {
		return phttp.JSONStatus(500, map[string]string{
			"error":   "Internal Server Error",
			"message": "Failed to re-encode bridged request body",
		})
	}
	ctx.Request.Body = io.NopCloser(bytes.NewReader(encoded))
	return nil
}

// bridgeValueToString renders a JSON-decoded envelope value as the string the
// downstream endpoint expects for a path param or query value. The bridge
// decodes envelopes with Decoder.UseNumber so integer identifiers larger than
// float64's precise range keep their original decimal text. The float64 branch
// remains for callers that construct bridgeEnvelope values directly in tests or
// internal code.
func bridgeValueToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// bridgeEnvelope is the JSON payload shape Connect callers POST to the bridged RPC URL.
// Each section is optional; missing sections imply empty maps.
type bridgeEnvelope struct {
	Params map[string]any `json:"params,omitempty"`
	Query  map[string]any `json:"query,omitempty"`
	Body   map[string]any `json:"body,omitempty"`
}

func isHTTPMethod(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}
