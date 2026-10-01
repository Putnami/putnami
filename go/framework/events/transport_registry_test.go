package events

import (
	"context"
	"testing"

	pconfig "go.putnami.dev/config"
)

// resetTransportFactories snapshots the process-global factory registry and
// restores it after the test, so registrations don't leak across tests
// (mirroring resetSourceDiscoverers in the config package).
func resetTransportFactories(t *testing.T) {
	t.Helper()
	transportFactoriesMu.Lock()
	originalBinding := make(map[string]BindingTransportFactory, len(bindingFactories))
	for kind, factory := range bindingFactories {
		originalBinding[kind] = factory
	}
	bindingFactories = map[string]BindingTransportFactory{}
	transportFactoriesMu.Unlock()
	t.Cleanup(func() {
		transportFactoriesMu.Lock()
		bindingFactories = originalBinding
		transportFactoriesMu.Unlock()
	})
}

func TestBuildRegisteredTransport(t *testing.T) {
	resetTransportFactories(t)
	want := &recordingTransport{}
	var gotBinding PubSubBinding
	RegisterBindingTransportFactory("unit-build", func(b TransportBinding) (Transport, error) {
		gotBinding = b.PubSub
		return want, nil
	})

	got, err := buildRegisteredTransport("unit-build", TransportBinding{PubSub: PubSubBinding{ProjectID: "proj", TopicTemplate: "events-{topic}"}})
	if err != nil {
		t.Fatalf("buildRegisteredTransport: %v", err)
	}
	if got != want {
		t.Errorf("transport = %p, want the registered factory's transport %p", got, want)
	}
	if gotBinding.ProjectID != "proj" || gotBinding.TopicTemplate != "events-{topic}" {
		t.Errorf("factory received binding %+v", gotBinding)
	}

	// An unregistered kind fails closed — no silent fallback to the in-process broker.
	if _, err := buildRegisteredTransport("unit-unregistered", TransportBinding{}); err == nil {
		t.Fatal("buildRegisteredTransport accepted an unregistered kind, want error")
	}
}

func TestConfigureSelectsRegisteredTransport(t *testing.T) {
	resetTransportFactories(t)
	want := &recordingTransport{}
	RegisterBindingTransportFactory("unit-configure", func(TransportBinding) (Transport, error) { return want, nil })

	// TransportKind (as the config overlay sets it via events.transport) selects
	// the registered factory over the EVENTS_ENDPOINT / in-process fallback.
	p := Events(PluginConfig{
		TransportKind: "unit-configure",
		PubSub:        PubSubBinding{ProjectID: "proj", TopicTemplate: "events-{topic}"},
	})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.transport != want {
		t.Errorf("transport = %T, want the registered factory's transport", p.transport)
	}
}

func TestConfigureUnknownTransportFailsClosed(t *testing.T) {
	resetTransportFactories(t)
	// A workload configured for a transport whose provider module was not imported
	// must fail, not silently fall back to the loopback broker.
	p := Events(PluginConfig{TransportKind: "unit-nobody-registered-this"})
	if err := p.Configure(context.Background(), nil); err == nil {
		t.Fatal("Configure selected an unregistered transport, want a fail-closed error")
	}
}

func TestConfigSection_MapsTransportFromDocument(t *testing.T) {
	src := pconfig.NewMapSource("test", 100, map[string]any{
		"events": map[string]any{
			"transport": "pubsub",
			"pubsub": map[string]any{
				"projectId":     "my-project",
				"topicTemplate": "events-{topic}",
			},
		},
	})

	doc, err := pconfig.Load(configSection, src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if doc.Transport != "pubsub" {
		t.Errorf("transport = %q, want pubsub", doc.Transport)
	}
	if doc.PubSub.ProjectID != "my-project" || doc.PubSub.TopicTemplate != "events-{topic}" {
		t.Errorf("pubsub binding = %+v", doc.PubSub)
	}
}

func TestOverlayConfigDoc_MapsTransportAndPubSub(t *testing.T) {
	// The resolved events document maps onto the code-constructed PluginConfig:
	// events.transport -> TransportKind, events.pubsub -> PubSub.
	p := Events(PluginConfig{})
	if err := p.overlayConfigDoc(configDoc{
		Transport: "pubsub",
		PubSub:    PubSubBinding{ProjectID: "my-project", TopicTemplate: "events-{topic}"},
	}); err != nil {
		t.Fatalf("overlayConfigDoc: %v", err)
	}
	if p.config.TransportKind != "pubsub" {
		t.Errorf("TransportKind = %q, want pubsub", p.config.TransportKind)
	}
	if p.config.PubSub.ProjectID != "my-project" || p.config.PubSub.TopicTemplate != "events-{topic}" {
		t.Errorf("PubSub = %+v", p.config.PubSub)
	}
}

func TestOverlayConfigDoc_TransportPreservesCodeWhenUnset(t *testing.T) {
	// An empty events document leaves the programmatic transport binding intact —
	// document wins per-field only where set, code is preserved otherwise.
	p := Events(PluginConfig{
		TransportKind: "code-kind",
		PubSub:        PubSubBinding{ProjectID: "code-proj", TopicTemplate: "code-{topic}"},
	})
	if err := p.overlayConfigDoc(configDoc{}); err != nil {
		t.Fatalf("overlayConfigDoc: %v", err)
	}
	if p.config.TransportKind != "code-kind" {
		t.Errorf("TransportKind = %q, want code-kind preserved", p.config.TransportKind)
	}
	if p.config.PubSub.ProjectID != "code-proj" || p.config.PubSub.TopicTemplate != "code-{topic}" {
		t.Errorf("PubSub = %+v, want code values preserved", p.config.PubSub)
	}
}

func TestBindingTransportFactoryReceivesNeutralBinding(t *testing.T) {
	resetTransportFactories(t)
	want := &recordingTransport{}
	var got TransportBinding
	RegisterBindingTransportFactory("unit-neutral", func(binding TransportBinding) (Transport, error) {
		got = binding
		return want, nil
	})

	binding := TransportBinding{
		PubSub: PubSubBinding{ProjectID: "legacy"},
		EventServer: EventServerBinding{
			ContractVersion: EventServerContractVersionV1,
			Endpoint:        "https://events.example.com",
			Audience:        "https://events.example.com",
			Protocol:        ProtocolVersion,
		},
	}
	transport, err := buildRegisteredTransport("unit-neutral", binding)
	if err != nil {
		t.Fatalf("buildRegisteredTransport: %v", err)
	}
	if transport != want || got.EventServer.Endpoint != binding.EventServer.Endpoint || got.PubSub.ProjectID != "legacy" {
		t.Fatalf("neutral binding not preserved: got %+v", got)
	}
}

func TestRegisterBindingTransportFactory_IgnoresNil(t *testing.T) {
	resetTransportFactories(t)
	RegisterBindingTransportFactory("unit-neutral-nil", nil)
	if _, ok := lookupBindingTransportFactory("unit-neutral-nil"); ok {
		t.Fatal("RegisterBindingTransportFactory stored a nil factory")
	}
}
