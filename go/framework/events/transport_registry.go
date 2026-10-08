package events

import (
	"sync"

	"go.putnami.dev/errors"
)

// TransportBinding is the provider-neutral resolved binding passed to a
// BindingTransportFactory. Only the block selected by events.transport is
// expected to be populated.
type TransportBinding struct {
	PubSub      PubSubBinding
	EventServer EventServerBinding
}

// BindingTransportFactory builds a Transport from the complete, neutral events
// binding. New transports should use this extension point. In particular, a
// provider integration may register an eventserver factory that supplies its
// own workload-identity CredentialSource. A factory registered for the
// eventserver or pubsub kind replaces the built-in transport of that kind.
type BindingTransportFactory func(TransportBinding) (Transport, error)

var (
	transportFactoriesMu sync.RWMutex
	bindingFactories     = map[string]BindingTransportFactory{}
)

// RegisterBindingTransportFactory registers a provider-neutral transport
// factory under the events.transport kind. A nil factory is ignored.
func RegisterBindingTransportFactory(kind string, factory BindingTransportFactory) {
	if factory == nil {
		return
	}
	transportFactoriesMu.Lock()
	defer transportFactoriesMu.Unlock()
	bindingFactories[kind] = factory
}

func lookupBindingTransportFactory(kind string) (BindingTransportFactory, bool) {
	transportFactoriesMu.RLock()
	defer transportFactoriesMu.RUnlock()
	factory, ok := bindingFactories[kind]
	return factory, ok
}

// buildRegisteredTransport resolves the factory for kind and builds the
// transport from binding. A registered factory wins. Without one, the built-in
// eventserver and pubsub transports serve their kinds, and any other kind fails
// closed — a workload configured for a transport whose provider module was not
// imported must not silently fall back to the in-process broker.
func buildRegisteredTransport(kind string, binding TransportBinding) (Transport, error) {
	if factory, ok := lookupBindingTransportFactory(kind); ok {
		transport, err := factory(binding)
		if err != nil {
			return nil, errors.Wrapf(err, codeEventsConfig, "build events transport", errors.String("transport", kind))
		}
		return transport, nil
	}
	if kind == TransportKindEventServer {
		transport, err := NewEventServerTransport(EventServerTransportConfigFromBinding(binding.EventServer))
		if err != nil {
			return nil, errors.Wrapf(err, codeEventsConfig, "build events transport", errors.String("transport", kind))
		}
		return transport, nil
	}
	if kind == TransportKindPubSub {
		transport, err := NewDirectPubSubTransport(binding.PubSub)
		if err != nil {
			return nil, errors.Wrapf(err, codeEventsConfig, "build events transport", errors.String("transport", kind))
		}
		return transport, nil
	}
	return nil, errors.New(codeEventsConfig, "no transport factory registered for events.transport (is the provider module imported?)", errors.String("transport", kind))
}
