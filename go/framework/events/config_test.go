package events

import (
	"context"
	"testing"

	pconfig "go.putnami.dev/config"
)

func TestOverlayConfigDoc_DocumentWinsPerFieldOverCode(t *testing.T) {
	// Code sets the base (handlers + a JWKS override); the document supplies the
	// delivery mode and the push binding. Document values win where set; an unset
	// field keeps the code value.
	p := Events(PluginConfig{
		Delivery: DeliveryPull,                             // code default — overridden by the document
		Push:     PushConfig{JWKSURL: "https://code-jwks"}, // kept (the document leaves JWKSURL unset)
	})

	if err := p.overlayConfigDoc(configDoc{
		Delivery: string(DeliveryPush),
		Push: PushConfig{
			Issuer:                 "https://accounts.google.com",
			Audience:               "https://svc.run.app",
			AllowedServiceAccounts: []string{"runtime@proj.iam.gserviceaccount.com"},
		},
	}); err != nil {
		t.Fatalf("overlayConfigDoc: %v", err)
	}

	if p.deliveryProfile() != DeliveryPush {
		t.Errorf("delivery = %q, want push (the document overrides the code value)", p.deliveryProfile())
	}
	if p.config.Push.Issuer != "https://accounts.google.com" {
		t.Errorf("issuer = %q", p.config.Push.Issuer)
	}
	if p.config.Push.Audience != "https://svc.run.app" {
		t.Errorf("audience = %q", p.config.Push.Audience)
	}
	if len(p.config.Push.AllowedServiceAccounts) != 1 || p.config.Push.AllowedServiceAccounts[0] != "runtime@proj.iam.gserviceaccount.com" {
		t.Errorf("allowedServiceAccounts = %v", p.config.Push.AllowedServiceAccounts)
	}
	if p.config.Push.JWKSURL != "https://code-jwks" {
		t.Errorf("jwksUrl = %q, want the code value preserved (the document left it unset)", p.config.Push.JWKSURL)
	}
}

func TestOverlayConfigDoc_EmptyDocumentPreservesCode(t *testing.T) {
	// A workload with no events config section (a zero document) keeps its
	// code-constructed config untouched — existing code-only usage is unchanged.
	p := Events(PluginConfig{
		Delivery: DeliveryPush,
		Push:     PushConfig{Issuer: "https://code", AllowedServiceAccounts: []string{"a@x"}},
	})

	if err := p.overlayConfigDoc(configDoc{}); err != nil {
		t.Fatalf("overlayConfigDoc: %v", err)
	}

	if p.deliveryProfile() != DeliveryPush {
		t.Errorf("delivery = %q, want push (code preserved)", p.deliveryProfile())
	}
	if p.config.Push.Issuer != "https://code" || len(p.config.Push.AllowedServiceAccounts) != 1 {
		t.Errorf("code push config clobbered by an empty document: %+v", p.config.Push)
	}
}

func TestOverlayConfigDoc_UnknownDeliveryRejected(t *testing.T) {
	// Delivery now arrives as deploy-time config, so a typo must fail the load
	// rather than silently degrade to pull — which would leave a push workload's
	// receiver route unmounted with no error. The code value is preserved.
	p := Events(PluginConfig{Delivery: DeliveryPush})

	err := p.overlayConfigDoc(configDoc{Delivery: "Push"}) // capital P — not a known mode
	if err == nil {
		t.Fatal("overlayConfigDoc accepted an unknown delivery mode, want error")
	}
	if p.deliveryProfile() != DeliveryPush {
		t.Errorf("delivery = %q, want the code value preserved on a rejected document", p.deliveryProfile())
	}
}

func TestConfigSection_MapsEventsBlockFromDocument(t *testing.T) {
	// The deployer-injected events.push block maps onto configDoc via the json
	// tags and the "events" config path — the contract the cloud overlay writes.
	src := pconfig.NewMapSource("test", 100, map[string]any{
		"events": map[string]any{
			"delivery": "push",
			"push": map[string]any{
				"issuer":                 "https://accounts.google.com",
				"audience":               "https://svc.run.app",
				"allowedServiceAccounts": []any{"runtime@proj.iam.gserviceaccount.com"},
			},
		},
	})

	doc, err := pconfig.Load(configSection, src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if doc.Delivery != string(DeliveryPush) {
		t.Errorf("delivery = %q, want push", doc.Delivery)
	}
	if doc.Push.Issuer != "https://accounts.google.com" || doc.Push.Audience != "https://svc.run.app" {
		t.Errorf("push issuer/audience not mapped: %+v", doc.Push)
	}
	if len(doc.Push.AllowedServiceAccounts) != 1 || doc.Push.AllowedServiceAccounts[0] != "runtime@proj.iam.gserviceaccount.com" {
		t.Errorf("allowedServiceAccounts not mapped: %v", doc.Push.AllowedServiceAccounts)
	}
}

func TestOverlayConfigDoc_ExplicitlyDisablesPushBootstrap(t *testing.T) {
	enabled := true
	disabled := false
	p := Events(PluginConfig{Delivery: DeliveryPush, Push: PushConfig{Enabled: &enabled}})
	if err := p.overlayConfigDoc(configDoc{Push: PushConfig{Enabled: &disabled}}); err != nil {
		t.Fatal(err)
	}
	if p.pushEnabled() {
		t.Fatal("events.push.enabled=false did not disable push admission")
	}
}

func TestConfigSectionMapsEventServerBinding(t *testing.T) {
	src := pconfig.NewMapSource("test", 100, map[string]any{
		"events": map[string]any{
			"transport": "eventserver",
			"eventServer": map[string]any{
				"contractVersion":      1,
				"endpoint":             "https://events.example.com",
				"audience":             "https://events.example.com",
				"protocol":             ProtocolVersion,
				"workspaceId":          "workspace-1",
				"environment":          "prod",
				"workload":             "api",
				"topologyGenerationId": "generation-1",
			},
		},
	})
	doc, err := pconfig.Load(configSection, src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if doc.Transport != TransportKindEventServer || doc.EventServer.ContractVersion != EventServerContractVersionV1 {
		t.Fatalf("transport/eventServer = %q %+v", doc.Transport, doc.EventServer)
	}
	if doc.EventServer.Endpoint != "https://events.example.com" || doc.EventServer.TopologyGenerationID != "generation-1" {
		t.Fatalf("eventServer binding = %+v", doc.EventServer)
	}
}

type staticTraceContextSource struct{}

func (staticTraceContextSource) TraceContext(context.Context) W3CTraceContext {
	return W3CTraceContext{}
}

func TestOverlayConfigDocMapsEventServerAndPreservesHooks(t *testing.T) {
	credentials := staticEventServerCredential()
	trace := staticTraceContextSource{}
	p := Events(PluginConfig{EventServer: EventServerBinding{
		CredentialSource:   credentials,
		TraceContextSource: trace,
	}})
	if err := p.overlayConfigDoc(configDoc{
		Transport: TransportKindEventServer,
		EventServer: EventServerBinding{
			ContractVersion: EventServerContractVersionV1,
			Endpoint:        "https://events.example.com",
			Audience:        "https://events.example.com",
			Protocol:        ProtocolVersion,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if p.config.TransportKind != TransportKindEventServer || p.config.EventServer.Endpoint != "https://events.example.com" {
		t.Fatalf("eventServer config = %+v", p.config.EventServer)
	}
	if p.config.EventServer.CredentialSource == nil || p.config.EventServer.TraceContextSource == nil {
		t.Fatal("document overlay discarded programmatic credential/trace hooks")
	}
}
