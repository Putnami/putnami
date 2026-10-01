package client

import (
	"context"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/config"
	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
)

// CredentialSourceKind selects a framework-owned credential acquisition path.
type CredentialSourceKind string

const (
	// CredentialSourceOAuthClientCredentials acquires OAuth 2 access tokens with
	// the client_credentials grant.
	CredentialSourceOAuthClientCredentials CredentialSourceKind = "oauth-client-credentials"
	// CredentialSourceOAuthExtensionGrant acquires an OAuth access token with a
	// configured extension grant such as an API-key exchange. The framework owns
	// the token HTTP request; consumers provide only typed runtime config.
	CredentialSourceOAuthExtensionGrant CredentialSourceKind = "oauth-extension-grant"
	// CredentialSourceOAuthClientAssertion acquires OAuth 2 access tokens using
	// an RFC 7523 client assertion. GCP Workload Identity can mint the assertion.
	CredentialSourceOAuthClientAssertion CredentialSourceKind = "oauth-client-assertion"
	// CredentialSourceGCPIDToken acquires an audience-bound ID token from the GCP
	// metadata server.
	CredentialSourceGCPIDToken CredentialSourceKind = "gcp-id-token"
	// CredentialSourceStatic supplies an API key or named header value. It is not
	// accepted for a provider-declared service-token profile.
	CredentialSourceStatic CredentialSourceKind = "static"
	// CredentialSourceForwardedUser explicitly enables propagation of a
	// per-call user token for a provider-declared forwarded-user-token profile.
	CredentialSourceForwardedUser CredentialSourceKind = "forwarded-user"
)

// Credential is a short-lived value returned by a TokenSource. Expiry is used
// to refresh service credentials before they become invalid.
type Credential struct {
	Value  string
	Expiry time.Time
}

// CredentialRequest is the non-secret identity of a credential acquisition.
type CredentialRequest struct {
	ServiceID string
	ClientID  string
	Profile   string
	Audience  string
	Scopes    []string
}

// TokenSource is the low-level extension seam for external identity systems.
// First-party applications normally select a built-in Source in config.
type TokenSource interface {
	Credential(context.Context, CredentialRequest) (Credential, error)
}

// TokenSourceFunc adapts a function for low-level integrations and tests.
type TokenSourceFunc func(context.Context, CredentialRequest) (Credential, error)

// Credential implements TokenSource.
func (f TokenSourceFunc) Credential(ctx context.Context, request CredentialRequest) (Credential, error) {
	return f(ctx, request)
}

// CredentialBinding supplies one provider-declared credential profile. Secret
// values are runtime config only and are never part of a generated client.
type CredentialBinding struct {
	Source CredentialSourceKind `json:"source"`

	// Audience is the audience the service token is requested for. It is a
	// deployment value, so it wins over the provider's profile and contract
	// audience for every source that takes one. For gcp-id-token an empty
	// Audience means the binding URL: Cloud Run verifies a Google ID token
	// against the URL it is presented to, and that URL differs per environment.
	Audience string `json:"audience,omitempty"`

	TokenURL     string `json:"tokenUrl,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty" sensitive:"true"`
	GrantType    string `json:"grantType,omitempty"`
	// TokenRequestFormat is "form" (default) or "json".
	TokenRequestFormat string            `json:"tokenRequestFormat,omitempty"`
	Parameters         map[string]string `json:"parameters,omitempty" sensitive:"true"`

	// AssertionSource selects how an RFC 7523 assertion is minted. The supported
	// normal path is gcp-id-token; Assertion is retained for controlled external
	// identity integrations that rotate the value through config.
	AssertionSource   CredentialSourceKind `json:"assertionSource,omitempty"`
	AssertionAudience string               `json:"assertionAudience,omitempty"`
	Assertion         string               `json:"assertion,omitempty" sensitive:"true"`

	Value string `json:"value,omitempty" sensitive:"true"`

	// MetadataURL is a complete low-level test/emulator endpoint override.
	// Production config should leave it empty so the well-known metadata endpoint
	// is used.
	MetadataURL string `json:"metadataUrl,omitempty"`

	AllowInsecure bool `json:"allowInsecure,omitempty"`

	// Provider is an explicit low-level integration seam. It cannot be loaded
	// from config and is intended for tests and third-party identity systems.
	Provider TokenSource `json:"-"`

	// Refresh re-mints a CredentialSourceForwardedUser token once after a
	// declared or undeclared 401, mirroring a hand-written HTTP client's own
	// single-remint contract (for example a CLI re-authenticating a stale or
	// expiring workspace session and replaying the request). It is consulted
	// only for the forwarded-user source; every other source already owns its
	// own acquisition and retry through TokenSource/Provider. Nil disables the
	// remint retry, leaving every existing binding's behavior unchanged. It
	// cannot be loaded from config and exists for programmatic binding only.
	Refresh func(ctx context.Context) (string, error) `json:"-"`
}

// ServiceBinding supplies deployment-specific values for one generated
// service contract. URL and credentials stay outside generated source.
type ServiceBinding struct {
	URL         string                       `json:"url"`
	ClientID    string                       `json:"clientId,omitempty"`
	Credentials map[string]CredentialBinding `json:"credentials,omitempty"`
	// OperationPaths binds shared unary REST operations to owner-specific paths.
	// This programmatic routing choice never changes the operation's contract.
	OperationPaths map[string]string `json:"-"`

	// Headers supplies static, non-secret request defaults. Operation headers
	// take precedence. Credential, identity and transport headers are reserved.
	// Constructors snapshot this map; later caller mutations have no effect.
	Headers map[string]string `json:"headers,omitempty"`

	AllowInsecure bool `json:"allowInsecure,omitempty"`

	// CarryRemoteMessage opts this binding in to carrying the provider's
	// free-text error `message` onto RemoteError.Message, redacted of this
	// call's own credential material. It is false by default: the envelope's
	// prose is written for the human who made the request, and a consumer that
	// only logs or forwards a failure must not widen its own exposure by
	// default. Set it on the deployment that displays a provider error to the
	// request's author; that consumer then owns what it logs. The envelope's
	// `error` member is never carried. See ADR 0006 of the client contract.
	CarryRemoteMessage bool `json:"carryRemoteMessage,omitempty"`

	// HTTPClient is a low-level transport override for tests and external
	// integrations. The safe path clones it and disables redirects.
	HTTPClient *http.Client `json:"-"`
}

// ServicesOptions is the typed configuration block for generated clients.
//
// Services is not sensitive as a whole: a binding's URL, credential source and
// audience are deployment values that publish as ordinary config. Only the
// secret material on CredentialBinding (clientSecret, parameters, assertion,
// value) carries its own sensitive tag.
type ServicesOptions struct {
	ClientID string                    `json:"clientId" env:"PUTNAMI_CLIENT_ID"`
	Services map[string]ServiceBinding `json:"services"`
}

var servicesConfig = config.Config[ServicesOptions]("clients")

// ServicesConfigDefinition exposes the adjacent typed config definition.
func ServicesConfigDefinition() config.Definition[ServicesOptions] { return servicesConfig }

// ServiceBindings is the application-scoped runtime registry resolved by
// generated client registrations. Its deployment values are immutable; its
// credential cache and the stream sessions it tracks live exactly as long as
// the application that published it. See service_registry.go for the lifetime
// rules and Close.
type ServiceBindings struct {
	clientID    string
	services    map[string]ServiceBinding
	credentials *credentialManager
	// responses is the response cache of every generated operation that
	// declares one. Like the credential cache it lives exactly as long as the
	// registry.
	responses *responseCaches

	mu            sync.Mutex
	closed        bool
	streams       map[*StreamSession]struct{}
	binaryStreams map[*ownedResponseStream]struct{}
}

// For resolves and defensively copies a service binding. A registry the
// application already stopped refuses to bind a new client.
func (bindings *ServiceBindings) For(serviceID string) (ServiceBinding, error) {
	if bindings == nil {
		return ServiceBinding{}, errors.New(CodeClientConfig, "service bindings are not configured")
	}
	if bindings.isClosed() {
		return ServiceBinding{}, errClosedRegistry()
	}
	binding, ok := bindings.services[serviceID]
	if !ok {
		return ServiceBinding{}, errors.Newf(CodeClientConfig, "service %q has no binding", serviceID)
	}
	if binding.ClientID == "" {
		binding.ClientID = bindings.clientID
	}
	binding.Credentials = cloneCredentialBindings(binding.Credentials)
	binding.OperationPaths = cloneOperationPaths(binding.OperationPaths)
	binding.Headers = maps.Clone(binding.Headers)
	return binding, nil
}

func cloneCredentialBindings(source map[string]CredentialBinding) map[string]CredentialBinding {
	if source == nil {
		return nil
	}
	out := make(map[string]CredentialBinding, len(source))
	for name, binding := range source {
		if binding.Parameters != nil {
			parameters := make(map[string]string, len(binding.Parameters))
			for key, value := range binding.Parameters {
				parameters[key] = value
			}
			binding.Parameters = parameters
		}
		out[name] = binding
	}
	return out
}

func cloneOperationPaths(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for id, path := range source {
		result[id] = path
	}
	return result
}

// ServicesPlugin owns typed generated-client config and publishes the binding
// registry in dependency injection. One plugin instance belongs to one
// application: it owns the registries it publishes and closes them when the
// application stops.
type ServicesPlugin struct {
	override *ServicesOptions

	mu        sync.Mutex
	published []*ServiceBindings
}

// Services registers framework-owned service bindings. With no argument the
// `clients` config block is loaded through the standard Putnami config sources.
// Passing one value is the explicit programmatic/test override.
func Services(override ...ServicesOptions) *ServicesPlugin {
	p := &ServicesPlugin{}
	if len(override) > 0 {
		copy := cloneServicesOptions(override[0])
		p.override = &copy
	}
	return p
}

// Name implements app.Plugin.
func (*ServicesPlugin) Name() string { return "generated-service-clients" }

// ConfigDefinitions publishes the `clients` config schema.
func (*ServicesPlugin) ConfigDefinitions() []config.Descriptor {
	return []config.Descriptor{servicesConfig.Descriptor()}
}

// Provides loads config and publishes the immutable binding registry.
func (p *ServicesPlugin) Provides() []inject.Registration {
	configRegistration := config.Provide(servicesConfig)
	if p.override != nil {
		configRegistration = inject.ProvideValue(config.Token(servicesConfig), cloneServicesOptions(*p.override))
	}
	return []inject.Registration{
		configRegistration,
		inject.Provide(
			inject.TokenOf[*ServiceBindings](),
			func(resolver inject.Resolver) (any, error) {
				options, err := inject.ResolveAs[ServicesOptions](resolver, config.Token(servicesConfig))
				if err != nil {
					return nil, errors.Wrapf(err, CodeClientConfig, "resolve generated service client config")
				}
				bindings, err := newServiceBindings(options)
				if err != nil {
					return nil, err
				}
				p.publish(bindings)
				return bindings, nil
			},
			inject.WithDeps(config.Token(servicesConfig)),
		),
	}
}

func newServiceBindings(options ServicesOptions) (*ServiceBindings, error) {
	options = cloneServicesOptions(options)
	for serviceID, binding := range options.Services {
		if strings.TrimSpace(serviceID) == "" {
			return nil, errors.New(CodeClientConfig, "service binding id is empty")
		}
		if _, err := parseBoundURL(binding.URL, binding.AllowInsecure); err != nil {
			return nil, errors.Wrapf(err, CodeClientConfig, "invalid service binding", errors.String("service", serviceID))
		}
		headers, err := snapshotBindingHeaders(binding.Headers)
		if err != nil {
			return nil, err
		}
		binding.Headers = headers
		options.Services[serviceID] = binding
	}
	return &ServiceBindings{
		clientID:    options.ClientID,
		services:    options.Services,
		credentials: newCredentialManager(),
		responses:   newResponseCaches(),
		streams:     make(map[*StreamSession]struct{}),
	}, nil
}

func cloneServicesOptions(options ServicesOptions) ServicesOptions {
	copy := ServicesOptions{ClientID: options.ClientID, Services: make(map[string]ServiceBinding, len(options.Services))}
	for serviceID, binding := range options.Services {
		binding.Credentials = cloneCredentialBindings(binding.Credentials)
		binding.OperationPaths = cloneOperationPaths(binding.OperationPaths)
		binding.Headers = maps.Clone(binding.Headers)
		copy.Services[serviceID] = binding
	}
	return copy
}

func parseBoundURL(raw string, allowInsecure bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return nil, errors.New(CodeClientConfig, "service URL must be an absolute hierarchical URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New(CodeClientConfig, "service URL must not contain user info, query, or fragment")
	}
	if parsed.Scheme != "https" {
		host := strings.ToLower(parsed.Hostname())
		ip := net.ParseIP(host)
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		if parsed.Scheme != "http" || (!allowInsecure && !loopback) {
			return nil, errors.New(CodeClientConfig, "service URL must use https")
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

var _ app.Provider = (*ServicesPlugin)(nil)
var _ app.ConfigContributor = (*ServicesPlugin)(nil)
var _ app.Stopper = (*ServicesPlugin)(nil)
