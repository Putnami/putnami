package events

import (
	"context"
	"path"
	"strings"

	"go.putnami.dev/errors"
)

// codeEventsRoute identifies routing transport failures.
const codeEventsRoute errors.Code = "events.route"

// TransportTarget names a configured event transport route target.
type TransportTarget string

// TransportRoute maps a channel or topic pattern to a transport target.
type TransportRoute struct {
	Channel string
	Topic   string
	Target  TransportTarget
}

// RoutingTransportConfig configures a transport router.
type RoutingTransportConfig struct {
	Transports       map[TransportTarget]Transport
	Routes           []TransportRoute
	DefaultTransport TransportTarget
}

// RoutingTransport routes published events across configured transports.
type RoutingTransport struct {
	config RoutingTransportConfig
}

// NewRoutingTransport creates a transport router.
func NewRoutingTransport(config RoutingTransportConfig) *RoutingTransport {
	return &RoutingTransport{config: config}
}

// Publish routes an envelope to the selected transport.
func (t *RoutingTransport) Publish(ctx context.Context, env Envelope) error {
	transport, err := t.selectTransport(env)
	if err != nil {
		return err
	}
	return transport.Publish(ctx, env)
}

// Subscribe registers a handler with all configured transports.
func (t *RoutingTransport) Subscribe(def *HandlerDefinition) error {
	for _, transport := range t.config.Transports {
		if err := transport.Subscribe(def); err != nil {
			return err
		}
	}
	return nil
}

// Start starts all configured transports.
func (t *RoutingTransport) Start(ctx context.Context) error {
	for _, transport := range t.config.Transports {
		if err := transport.Start(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Stop stops all configured transports and returns the first error.
func (t *RoutingTransport) Stop(ctx context.Context) error {
	var first error
	for _, transport := range t.config.Transports {
		if err := transport.Stop(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (t *RoutingTransport) selectTransport(env Envelope) (Transport, error) {
	for _, route := range t.config.Routes {
		if route.Target == "" {
			continue
		}
		if route.Channel != "" && route.Channel == env.Channel {
			return t.transport(route.Target)
		}
		if route.Topic != "" && matchTopic(route.Topic, env.Topic) {
			return t.transport(route.Target)
		}
	}
	if t.config.DefaultTransport != "" {
		return t.transport(t.config.DefaultTransport)
	}
	if len(t.config.Transports) == 1 {
		for _, transport := range t.config.Transports {
			return transport, nil
		}
	}
	return nil, errors.New(codeEventsRoute, "no event transport route matched",
		errors.String("topic", env.Topic),
		errors.String("channel", env.Channel),
	)
}

func (t *RoutingTransport) transport(target TransportTarget) (Transport, error) {
	transport := t.config.Transports[target]
	if transport == nil {
		return nil, errors.New(codeEventsRoute, "event transport target not configured", errors.String("target", string(target)))
	}
	return transport, nil
}

func matchTopic(pattern, topic string) bool {
	if pattern == topic {
		return true
	}
	if strings.ContainsAny(pattern, "*?[") {
		ok, err := path.Match(pattern, topic)
		return err == nil && ok
	}
	return false
}
