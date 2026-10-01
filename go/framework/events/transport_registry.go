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
// cloud integration may register an eventserver factory that supplies its own
// workload-identity CredentialSource without importing provider SDKs here.
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
// transport from binding. It fails closed when no factory is registered — a
// workload configured for a transport whose provider module was not imported
// must not silently fall back to the in-process broker.
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
	return nil, errors.New(codeEventsConfig, "no transport factory registered for events.transport (is the provider module imported?)", errors.String("transport", kind))
}
