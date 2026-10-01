package events

import (
	"cmp"
	"context"
	"os"
	"sort"
	"strings"
	"sync"

	"go.putnami.dev/app"
	pconfig "go.putnami.dev/config"
	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	protocaps "go.putnami.dev/protocol/capabilities"
	protoevents "go.putnami.dev/protocol/events"
	protofeatures "go.putnami.dev/protocol/features"
)

// activeTransport is the process-wide default transport. It is guarded by
// activeTransportMu because handler/request goroutines call GetTransport
// concurrently with Configure (start) and Stop, which write it.
var (
	activeTransportMu sync.RWMutex
	activeTransport   Transport
)

// SetTransport sets the process-wide default event transport.
func SetTransport(transport Transport) {
	activeTransportMu.Lock()
	activeTransport = transport
	activeTransportMu.Unlock()
}

// GetTransport returns the process-wide default event transport.
func GetTransport() Transport {
	activeTransportMu.RLock()
	defer activeTransportMu.RUnlock()
	return activeTransport
}

func errorsNoTransport(topic string) error {
	return errors.New(CodeEventsNoTransport, "no transport configured", errors.String("topic", topic))
}

// PubSubBinding is the provider binding a transport factory receives: the GCP
// project the topics live in and the topic-name template (e.g. "events-{topic}")
// that maps a logical topic name to the provider topic id. It is the value of
// the events.pubsub config block.
type PubSubBinding struct {
	ProjectID     string `json:"projectId"`
	TopicTemplate string `json:"topicTemplate"`
}

// PluginConfig configures the events app plugin.
type PluginConfig struct {
	Transport        Transport
	Transports       map[TransportTarget]Transport
	Routes           []TransportRoute
	DefaultTransport TransportTarget
	Endpoint         string
	Token            string
	Port             int
	Handlers         []*HandlerDefinition
	// Publishes lists topic names this project emits to. They feed the
	// build-time infra describer; declaring a published topic here does not
	// create a runtime publisher (use NewPublisher for that).
	Publishes []string
	// Delivery selects how handlers receive events: pull (the default) or
	// stream hold a long-lived process; push registers an HTTP receiver route
	// (RegisterOn) and the provider POSTs each event to it, so the workload can
	// scale to zero. In push mode the broker/transport pull loop is not started.
	Delivery DeliveryMode
	// Push configures OIDC verification for the push receiver; used only when
	// Delivery is push.
	Push PushConfig
	// TransportKind selects a registered transport factory (see
	// RegisterBindingTransportFactory) by key — for example "pubsub". It is the
	// config-driven alternative to setting Transport in code, used only when
	// neither Transport nor Transports is set.
	TransportKind string
	// PubSub is the binding handed to the selected transport factory.
	PubSub PubSubBinding
	// EventServer is the provider-neutral managed HTTP publisher binding. A
	// provider integration may attach CredentialSource in code while the
	// endpoint/audience/protocol fields come from resolved config.
	EventServer EventServerBinding
}

// Plugin wires event transports and handlers into an app module.
type Plugin struct {
	config      PluginConfig
	transport   Transport
	localServer *LocalServer
	handlers    []*HandlerDefinition
	publishes   []string

	// logs holds the pinned event logger names the push receiver's per-delivery
	// boundary emits under (see delivery_logging.go).
	logs eventLoggers

	// pushRoundRobin holds per-topic round-robin counters for competing
	// distribution in push delivery (topic → *atomic.Uint64).
	pushRoundRobin sync.Map
}

// Events creates an events app plugin.
func Events(config ...PluginConfig) *Plugin {
	cfg := PluginConfig{}
	if len(config) > 0 {
		cfg = config[0]
	}
	return &Plugin{
		config:    cfg,
		handlers:  append([]*HandlerDefinition(nil), cfg.Handlers...),
		publishes: append([]string(nil), cfg.Publishes...),
		logs:      newEventLoggers(),
	}
}

// Name returns the app plugin name.
func (p *Plugin) Name() string { return "events" }

// DesignInfraRequirements projects every natively registered publish/subscribe
// topic through app's bounded infrastructure seam.
func (p *Plugin) DesignInfraRequirements() []app.DesignInfraRequirement {
	names := append([]string(nil), p.publishes...)
	names = append(names, p.subscribeTopics()...)
	sort.Strings(names)
	requirements := make([]app.DesignInfraRequirement, 0, len(names))
	for _, name := range names {
		if len(requirements) > 0 && requirements[len(requirements)-1].Name == name {
			continue
		}
		requirements = append(requirements, app.DesignInfraRequirement{Name: name, Kind: protocaps.InfraKindEvents})
	}
	return requirements
}

// Register adds a handler definition before the plugin is configured.
func (p *Plugin) Register(handler *HandlerDefinition) *Plugin {
	p.handlers = append(p.handlers, handler)
	return p
}

// RegisterPublisher records that this project publishes to topic so the
// build-time infra describer can declare it as a publish requirement. It
// returns p for chaining and has no effect on runtime publishing.
func RegisterPublisher[T any](p *Plugin, topic *Topic[T]) *Plugin {
	p.publishes = append(p.publishes, topic.Name)
	return p
}

// ContributeDesign derives event publications and subscriptions from the same
// typed registrations used by the runtime and infrastructure describer.
func (p *Plugin) ContributeDesign(builder *app.DesignBuilder) error {
	for _, topic := range p.publishes {
		topicID := "event.topic:" + topic
		if err := builder.AddNode(protofeatures.DesignNode{ID: topicID, Kind: protofeatures.DesignNodeEventTopic, Name: topic}); err != nil {
			return err
		}
		if err := builder.RelateFromModule(topicID, protofeatures.DesignEdgePublishes, protofeatures.DesignAuthorityExact); err != nil {
			return err
		}
	}
	for _, handler := range p.handlers {
		topicID := "event.topic:" + handler.Topic
		if err := builder.AddNode(protofeatures.DesignNode{ID: topicID, Kind: protofeatures.DesignNodeEventTopic, Name: handler.Topic}); err != nil {
			return err
		}
		symbol := handler.designSymbol
		if symbol == "" {
			symbol = handlerSymbol(handler.Handler)
		}
		if symbol == "" {
			symbol = "handler"
		}
		handlerID := "event.handler:" + strings.TrimPrefix(builder.ModuleID(), "module:") + ":" + handler.Topic + ":" + symbol
		if err := builder.AddNode(protofeatures.DesignNode{
			ID: handlerID, Kind: protofeatures.DesignNodeEventHandler, Name: symbol,
			Properties: map[string]string{"topic": handler.Topic},
		}); err != nil {
			return err
		}
		if err := builder.RelateFromModule(handlerID, protofeatures.DesignEdgeContains, protofeatures.DesignAuthorityExact); err != nil {
			return err
		}
		if err := builder.AddEdge(protofeatures.DesignEdge{From: handlerID, To: topicID, Kind: protofeatures.DesignEdgeSubscribes, Authority: protofeatures.DesignAuthorityExact}); err != nil {
			return err
		}
	}
	return nil
}

// Provides returns dependency injection registrations for the active transport.
func (p *Plugin) Provides() []inject.Registration {
	return []inject.Registration{
		inject.Provide(inject.TokenOf[Transport](), func(_ inject.Resolver) (any, error) {
			if p.transport == nil {
				return nil, errorsNoTransport("*")
			}
			return p.transport, nil
		}, inject.WithLazy()),
	}
}

// codeEventsConfig identifies events config-resolution failures.
const codeEventsConfig errors.Code = "events.config"

// configDoc is the events config block resolved from the application config
// document (the "events" section). It overlays the code-constructed
// PluginConfig so a deployer can supply the delivery mode and the push receiver
// binding (issuer, audience, allowed pusher service accounts) as managed config
// — never a per-project requirement — while existing programmatic usage keeps
// working. Code values are the base; document values win per-field when set.
type configDoc struct {
	// Delivery is the raw delivery-mode string ("pull" | "stream" | "push"). It
	// is a plain string rather than DeliveryMode so the config schema describes
	// it as a string field instead of reflecting the named type as an object.
	Delivery    string             `json:"delivery"`
	Push        PushConfig         `json:"push"`
	Transport   string             `json:"transport"`
	PubSub      PubSubBinding      `json:"pubsub"`
	EventServer EventServerBinding `json:"eventServer"`
}

// configSection binds the "events" path of the resolved config document.
var configSection = pconfig.Config[configDoc]("events")

// ConfigDefinitions implements app.ConfigContributor: it publishes the events
// config schema (events.delivery, events.push.*) at describe time so the block
// is known and validated wherever the workload's config is resolved.
func (p *Plugin) ConfigDefinitions() []pconfig.Descriptor {
	return []pconfig.Descriptor{configSection.Descriptor()}
}

// applyResolvedConfig overlays the resolved "events" config block onto the
// code-constructed PluginConfig. The document is the deploy-time source for the
// delivery mode and the push receiver binding; unset fields keep the
// programmatic value, so code-only usage — and pull/stream workloads with no
// events config — is unchanged.
func (p *Plugin) applyResolvedConfig(ctx context.Context) error {
	doc, err := pconfig.LoadContext(ctx, configSection)
	if err != nil {
		return errors.Wrapf(err, codeEventsConfig, "resolve events config")
	}
	return p.overlayConfigDoc(doc)
}

// overlayConfigDoc merges a resolved events config block onto p.config,
// field-by-field, so a value set in the config document wins while an unset
// field keeps whatever the code-constructed PluginConfig provided. An
// unrecognized delivery mode is rejected rather than silently treated as pull:
// since delivery now arrives as deploy-time config, a typo (e.g. "Push") would
// otherwise leave a push workload's receiver route unmounted with no error.
func (p *Plugin) overlayConfigDoc(doc configDoc) error {
	if doc.Delivery != "" {
		profile := DeliveryMode(doc.Delivery)
		if !protoevents.ValidateDeliveryProfile(profile) {
			return errors.New(codeEventsConfig, "unknown events delivery mode", errors.String("delivery", doc.Delivery))
		}
		p.config.Delivery = profile
	}
	if doc.Push.Issuer != "" {
		p.config.Push.Issuer = doc.Push.Issuer
	}
	if doc.Push.Enabled != nil {
		enabled := *doc.Push.Enabled
		p.config.Push.Enabled = &enabled
	}
	if doc.Push.Audience != "" {
		p.config.Push.Audience = doc.Push.Audience
	}
	if doc.Push.JWKSURL != "" {
		p.config.Push.JWKSURL = doc.Push.JWKSURL
	}
	if len(doc.Push.AllowedServiceAccounts) > 0 {
		p.config.Push.AllowedServiceAccounts = doc.Push.AllowedServiceAccounts
	}
	if doc.Push.AllowInsecure {
		p.config.Push.AllowInsecure = doc.Push.AllowInsecure
	}
	if doc.Transport != "" {
		p.config.TransportKind = doc.Transport
	}
	if doc.PubSub.ProjectID != "" {
		p.config.PubSub.ProjectID = doc.PubSub.ProjectID
	}
	if doc.PubSub.TopicTemplate != "" {
		p.config.PubSub.TopicTemplate = doc.PubSub.TopicTemplate
	}
	if doc.EventServer.ContractVersion != 0 {
		p.config.EventServer.ContractVersion = doc.EventServer.ContractVersion
	}
	if doc.EventServer.Endpoint != "" {
		p.config.EventServer.Endpoint = doc.EventServer.Endpoint
	}
	if doc.EventServer.Audience != "" {
		p.config.EventServer.Audience = doc.EventServer.Audience
	}
	if doc.EventServer.Protocol != "" {
		p.config.EventServer.Protocol = doc.EventServer.Protocol
	}
	if doc.EventServer.WorkspaceID != "" {
		p.config.EventServer.WorkspaceID = doc.EventServer.WorkspaceID
	}
	if doc.EventServer.Environment != "" {
		p.config.EventServer.Environment = doc.EventServer.Environment
	}
	if doc.EventServer.Workload != "" {
		p.config.EventServer.Workload = doc.EventServer.Workload
	}
	if doc.EventServer.TopologyGenerationID != "" {
		p.config.EventServer.TopologyGenerationID = doc.EventServer.TopologyGenerationID
	}
	return nil
}

// Configure resolves the events config block (overlaying the document onto the
// code-constructed PluginConfig) and then selects and registers the event
// transport.
func (p *Plugin) Configure(ctx context.Context, _ *app.Module) error {
	if err := p.applyResolvedConfig(ctx); err != nil {
		return err
	}
	if p.config.Transport != nil {
		p.transport = p.config.Transport
	} else if len(p.config.Transports) > 0 {
		p.transport = NewRoutingTransport(RoutingTransportConfig{
			Transports:       p.config.Transports,
			Routes:           p.config.Routes,
			DefaultTransport: p.config.DefaultTransport,
		})
	} else if p.config.TransportKind != "" {
		transport, err := buildRegisteredTransport(p.config.TransportKind, TransportBinding{
			PubSub:      p.config.PubSub,
			EventServer: p.config.EventServer,
		})
		if err != nil {
			return err
		}
		p.transport = transport
	} else if endpoint := cmp.Or(p.config.Endpoint, os.Getenv("EVENTS_ENDPOINT")); endpoint != "" {
		token := cmp.Or(p.config.Token, os.Getenv("EVENTS_TOKEN"))
		p.transport = NewLocalServerTransport(endpoint, token)
	} else {
		port := p.config.Port
		if port == 0 {
			port = DefaultLocalServerPort
		}
		p.localServer = NewLocalServer(LocalServerConfig{Port: port, Token: p.config.Token})
		p.transport = p.localServer.Broker()
	}
	SetTransport(p.transport)
	if p.deliveryProfile() == DeliveryPush {
		// Push delivery: the receiver route (RegisterOn) owns delivery. Do not
		// subscribe handlers to the transport, so it never double-delivers; the
		// transport stays available for publishing.
		return nil
	}
	for _, handler := range p.handlers {
		if err := p.transport.Subscribe(handler); err != nil {
			return err
		}
	}
	return nil
}

// Start starts the selected transport or local server, except in push mode
// where the provider POSTs to the receiver route instead of a pull/stream loop.
func (p *Plugin) Start(ctx context.Context, _ *app.Module) error {
	if p.deliveryProfile() == DeliveryPush {
		return nil
	}
	if p.localServer != nil {
		return p.localServer.Start(ctx)
	}
	if p.transport != nil {
		return p.transport.Start(ctx)
	}
	return nil
}

// Stop stops the selected transport or local server.
func (p *Plugin) Stop(ctx context.Context, _ *app.Module) error {
	SetTransport(nil)
	if p.localServer != nil {
		return p.localServer.Stop(ctx)
	}
	if p.transport != nil {
		return p.transport.Stop(ctx)
	}
	return nil
}

var _ app.Plugin = (*Plugin)(nil)
var _ app.Configurer = (*Plugin)(nil)
var _ app.Starter = (*Plugin)(nil)
var _ app.Stopper = (*Plugin)(nil)
var _ app.Provider = (*Plugin)(nil)
var _ app.Describer = (*Plugin)(nil)
var _ app.ConfigContributor = (*Plugin)(nil)
var _ app.DesignContributor = (*Plugin)(nil)
