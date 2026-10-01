package events

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	protoevents "go.putnami.dev/protocol/events"
	"go.putnami.dev/security"
)

// DeliveryMode selects how a subscriber receives events. It reuses the canonical
// go.putnami.dev/protocol/events vocabulary:
//
//   - pull / stream hold a long-lived process that pulls or streams events.
//   - push has the provider POST each event to the receiver route the plugin
//     mounts on the application's HTTP server, so a scale-to-zero workload can
//     receive events. An HTTP 2xx acknowledges delivery.
type DeliveryMode = protoevents.DeliveryProfile

// Delivery modes.
const (
	DeliveryPull   = protoevents.DeliveryProfilePull
	DeliveryStream = protoevents.DeliveryProfileStream
	DeliveryPush   = protoevents.DeliveryProfilePush
)

// pushReceiverPath is the route the push receiver registers, in the Go router's
// {param} syntax. It matches the canonical convention
// protoevents.DefaultReceiverPath ("/_putnami/events/:subscription"); the
// :subscription / {subscription} placeholder is router-specific, the matched
// URLs are identical across frameworks.
const pushReceiverPath = "/_putnami/events/{subscription}"

// codeEventsPush identifies push receiver failures.
const codeEventsPush errors.Code = "events.push"

// PushConfig configures OIDC verification for the push receiver. The provider
// attaches an audience-pinned OIDC token minted as the events-server service
// account; the receiver verifies it fail-closed before dispatching anything.
type PushConfig struct {
	// Enabled gates receiver admission during managed first-deploy bootstrap.
	// Nil preserves the legacy behavior (enabled whenever delivery is push).
	// Explicit false still mounts the route but returns retryable 503 responses
	// without invoking a handler.
	Enabled *bool `json:"enabled"`
	// Issuer is the OIDC issuer of the pusher token (e.g.
	// "https://accounts.google.com"). It drives JWKS discovery and is enforced
	// as the required "iss".
	Issuer string `json:"issuer"`
	// Audience is the audience the pusher token must carry (the receiver
	// endpoint URL). Omitting it accepts any token from the issuer, so it must
	// be set in production.
	Audience string `json:"audience"`
	// JWKSURL overrides JWKS discovery for issuers without an OIDC discovery
	// document.
	JWKSURL string `json:"jwksUrl"`
	// AllowedServiceAccounts is the allowlist of pusher service-account emails.
	// A verified token whose email is not on this list is rejected with 403.
	// An empty allowlist rejects every request (fail-closed).
	AllowedServiceAccounts []string `json:"allowedServiceAccounts"`
	// AllowInsecure permits plaintext-http JWKS fetches. Tests/local only.
	AllowInsecure bool `json:"allowInsecure"`
}

// RegisterOn chooses server as the HTTP server of the push receiver. Call it
// before the application configures. Without it, Configure mounts the receiver
// on the application's single server.
//
// When the delivery set in code is push, RegisterOn registers the receiver
// route at once, from the push configuration set in code. For pull and stream
// it registers nothing and records server: Configure mounts the receiver on it
// when the resolved events config selects push.
//
// The route is secured fail-closed: security.JWKSJWT verifies the pusher's OIDC
// token (signature via JWKS, issuer, audience) and a service-account-email
// allowlist guard authorizes it. The pusher identity is used only for admission
// and never becomes a carried end-user identity.
func (p *Plugin) RegisterOn(server *phttp.ServerPlugin) {
	p.pushServer = server
	if p.deliveryProfile() != DeliveryPush {
		return
	}
	p.registerPushReceiver(server)
}

// mountPushReceiver mounts the push receiver from Configure, once the events
// config is resolved. It registers nothing for pull and stream delivery, and
// nothing when the route is already registered. Otherwise it registers the
// route on the server RegisterOn named, or on the single HTTP server of the
// owner's module tree; a tree with no server, or with several, fails with the
// http SingleServer error. A nil owner has no module tree to search.
func (p *Plugin) mountPushReceiver(owner *app.Module) error {
	if p.deliveryProfile() != DeliveryPush || p.pushMounted {
		return nil
	}
	server := p.pushServer
	if server == nil {
		if owner == nil {
			return nil
		}
		found, err := phttp.SingleServer(owner, p.Name())
		if err != nil {
			return err
		}
		server = found
	}
	p.registerPushReceiver(server)
	return nil
}

// registerPushReceiver registers the receiver route on server: the secured
// handler, or the retryable 503 handler while push admission is disabled.
func (p *Plugin) registerPushReceiver(server *phttp.ServerPlugin) {
	p.pushMounted = true
	if !p.pushEnabled() {
		server.POST(pushReceiverPath, p.handleDisabledPush)
		return
	}
	server.POST(pushReceiverPath, p.securedPushHandler())
}

// handleDisabledPush keeps first-deploy bootstrap fail-closed without turning
// a temporary disabled state into a provider dead-letter. It intentionally runs
// no auth middleware and never decodes or dispatches the body.
func (p *Plugin) handleDisabledPush(*phttp.Context) *phttp.Response {
	return phttp.JSONStatus(503, protoevents.EventServerError{
		Protocol:  protoevents.Protocol,
		Code:      protoevents.ErrorUpstreamUnavailable,
		Message:   "event push receiver is disabled",
		Retryable: true,
	})
}

// securedPushHandler builds the receiver handler wrapped in its OIDC + service-
// account-allowlist middleware chain. JWKSJWT verifies the pusher token and sets
// the identity (fail-open by itself); the allowlist Guard supplies the enforcing
// 401 (no/invalid token) and 403 (non-allowlisted) responses.
func (p *Plugin) securedPushHandler() phttp.Handler {
	return phttp.Chain(
		security.JWKSJWT(security.JWKSJWTConfig{
			Issuer:        p.config.Push.Issuer,
			JWKSURL:       p.config.Push.JWKSURL,
			Audience:      p.config.Push.Audience,
			AllowInsecure: p.config.Push.AllowInsecure,
		}),
		security.Guard(p.allowPusher).Middleware(),
	)(p.handlePush)
}

// deliveryProfile returns the configured delivery mode, defaulting to pull.
func (p *Plugin) deliveryProfile() DeliveryMode {
	if p.config.Delivery == "" {
		return DeliveryPull
	}
	return p.config.Delivery
}

func (p *Plugin) pushEnabled() bool {
	return p.config.Push.Enabled == nil || *p.config.Push.Enabled
}

// allowPusher is the service-account allowlist guard layered on top of OIDC
// verification: the verified token's email must be on the configured
// allowlist, and email_verified (when present) must be true. It answers 401
// (no/invalid token) and 403 (non-allowlisted), fail-closed.
func (p *Plugin) allowPusher(user *phttp.Claims, _ *phttp.Context) bool {
	email, ok := user.Extra["email"].(string)
	if !ok || email == "" {
		return false
	}
	if verified, ok := user.Extra["email_verified"].(bool); ok && !verified {
		return false
	}
	for _, allowed := range p.config.Push.AllowedServiceAccounts {
		if allowed == email {
			return true
		}
	}
	return false
}

// handlePush decodes a provider push wrapper, dispatches the carried envelope to
// the matching handlers, and maps the outcome to an HTTP status that encodes the
// acknowledgement:
//
//	2xx  ack   (delivered; or no matching handler, so the message is dropped)
//	4xx  DLQ   (permanent: malformed push body or envelope)
//	5xx  retry (transient: a handler failed)
//
// Push delivery is at-least-once, so handlers must be idempotent. Authentication
// runs in the middleware chain (securedPushHandler) before this handler.
func (p *Plugin) handlePush(ctx *phttp.Context) *phttp.Response {
	var wrapper protoevents.PushEnvelope
	if err := ctx.Body(&wrapper); err != nil {
		return phttp.JSONStatus(400, map[string]string{"error": "invalid push body"})
	}
	env, err := decodePushEnvelope(wrapper)
	if err != nil {
		return phttp.JSONStatus(400, map[string]string{"error": "invalid envelope"})
	}
	for _, def := range p.selectPushTargets(env) {
		// Each target is its own delivery boundary with its own terminal record;
		// the 5xx below is what turns the failure into a provider retry.
		if _, err := invokeTransportHandler(ctx.Request.Context(), p.logs, def, env); err != nil {
			return phttp.InternalError("handler failed")
		}
	}
	// 2xx acknowledges the message; a push with no matching handler is dropped.
	return phttp.NoContent()
}

// selectPushTargets applies the same distribution semantics as the broker
// (memory_broker.go selectTargets): every matching broadcast handler receives
// the event, and competing handlers share deliveries round-robin (one per
// message). Without this, push delivery would invoke every competing handler
// and duplicate side effects.
func (p *Plugin) selectPushTargets(env Envelope) []*HandlerDefinition {
	var broadcast, competing []*HandlerDefinition
	for _, def := range p.handlers {
		if def.Topic != env.Topic || !matchesFilter(def.Filter, env.Attributes) {
			continue
		}
		if def.Options.Distribution == Broadcast {
			broadcast = append(broadcast, def)
		} else {
			competing = append(competing, def)
		}
	}
	targets := broadcast
	if len(competing) > 0 {
		val, _ := p.pushRoundRobin.LoadOrStore(env.Topic, &atomic.Uint64{})
		counter := val.(*atomic.Uint64) //nolint:errcheck // type guaranteed by LoadOrStore
		n := counter.Add(1) - 1
		idx := int(n % uint64(len(competing))) //nolint:gosec // len(competing) > 0 here
		targets = append(targets, competing[idx])
	}
	return targets
}

// decodePushEnvelope unwraps a provider push message into the framework
// envelope, applying the same field defaults as the Google Pub/Sub transport
// and stripping caller-supplied auth.* attributes (anti-spoofing: an external
// pusher must not be able to forge a carried end-user identity).
func decodePushEnvelope(wrapper protoevents.PushEnvelope) (Envelope, error) {
	var env Envelope
	if wrapper.Subscription == "" {
		return env, errors.New(codeEventsPush, "push wrapper missing subscription")
	}
	raw, err := base64.StdEncoding.DecodeString(wrapper.Message.Data)
	if err != nil {
		return env, errors.Wrap(err, codeEventsPush, errors.String("phase", "base64"))
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, errors.Wrap(err, codeEventsPush, errors.String("phase", "decode"))
	}
	if env.Topic == "" {
		return env, errors.New(codeEventsPush, "push envelope missing topic")
	}
	if env.ID == "" {
		env.ID = wrapper.Message.MessageID
	}
	if env.Attributes == nil {
		env.Attributes = wrapper.Message.Attributes
	}
	if env.Attributes == nil {
		env.Attributes = map[string]string{}
	}
	stripAuthAttributes(env.Attributes)
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now()
	}
	if env.Attempt < 1 {
		env.Attempt = 1
	}
	return env, nil
}

// stripAuthAttributes removes caller-supplied auth.* attributes from a wire
// envelope. The pusher service-account identity is for admission only; any
// carried end-user identity must come from a trusted source, never from
// attributes an external pusher could set.
func stripAuthAttributes(attributes map[string]string) {
	for k := range attributes {
		if strings.HasPrefix(k, "auth.") {
			delete(attributes, k)
		}
	}
}
