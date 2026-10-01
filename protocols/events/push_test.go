package events

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestPushConformance_ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/push/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid push fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseAndValidatePushEnvelope(data); diag.HasErrors(diags) {
				t.Errorf("valid push fixture %s produced errors: %v", path, diags)
			}
		})
	}
}

func TestPushConformance_InvalidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/push/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid push fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseAndValidatePushEnvelope(data); !diag.HasErrors(diags) {
				t.Errorf("invalid push fixture %s should produce errors but none found", path)
			}
		})
	}
}

func TestPushOutcomeForStatus(t *testing.T) {
	cases := map[int]PushAckOutcome{
		200: PushAck,
		201: PushAck,
		204: PushAck,
		299: PushAck,
		400: PushDLQ,
		404: PushDLQ,
		422: PushDLQ,
		499: PushDLQ,
		500: PushRetry,
		503: PushRetry,
		301: PushRetry,
		102: PushRetry,
		0:   PushRetry,
	}
	for status, want := range cases {
		if got := PushOutcomeForStatus(status); got != want {
			t.Errorf("PushOutcomeForStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestDefaultDeliveryProfileIsPull(t *testing.T) {
	if DefaultDeliveryProfile != DeliveryProfilePull {
		t.Fatalf("DefaultDeliveryProfile = %q, want %q", DefaultDeliveryProfile, DeliveryProfilePull)
	}
}

func TestValidateDeliveryProfile(t *testing.T) {
	for _, p := range []DeliveryProfile{DeliveryProfilePull, DeliveryProfileStream, DeliveryProfilePush} {
		if !ValidateDeliveryProfile(p) {
			t.Errorf("ValidateDeliveryProfile(%q) = false, want true", p)
		}
	}
	if ValidateDeliveryProfile("webhook") {
		t.Error(`ValidateDeliveryProfile("webhook") = true, want false`)
	}
}

func TestDecodePushEnvelope(t *testing.T) {
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	valid := &PushEnvelope{
		Message:      PushMessage{Data: encode(`{"id":"x","topic":"t","payload":{"a":1},"timestamp":"2026-05-01T12:00:00Z","attempt":1}`)},
		Subscription: "s",
	}
	env, diags := DecodePushEnvelope(valid)
	if diag.HasErrors(diags) || env == nil {
		t.Fatalf("expected valid decode, got env=%v diags=%v", env, diags)
	}
	if env.Topic != "t" {
		t.Errorf("decoded topic = %q, want %q", env.Topic, "t")
	}

	if _, diags := DecodePushEnvelope(&PushEnvelope{Message: PushMessage{Data: "%%%"}, Subscription: "s"}); !diag.HasErrors(diags) {
		t.Error("expected error for bad base64")
	}

	missingTopic := encode(`{"id":"x","payload":{"a":1},"timestamp":"2026-05-01T12:00:00Z","attempt":1}`)
	if _, diags := DecodePushEnvelope(&PushEnvelope{Message: PushMessage{Data: missingTopic}, Subscription: "s"}); !diag.HasErrors(diags) {
		t.Error("expected error for envelope missing topic")
	}

	if _, diags := DecodePushEnvelope(nil); !diag.HasErrors(diags) {
		t.Error("expected error for nil push envelope")
	}
}

func TestValidatePushEnvelopeInvariants(t *testing.T) {
	if diags := ValidatePushEnvelope(nil); !diag.HasErrors(diags) {
		t.Error("expected error for nil push envelope")
	}

	// Missing data short-circuits.
	if diags := ValidatePushEnvelope(&PushEnvelope{Subscription: "s"}); !diag.HasErrors(diags) {
		t.Error("expected error for missing message.data")
	}

	// Bad publishTime is reported.
	data := base64.StdEncoding.EncodeToString([]byte(`{"id":"x","topic":"t","payload":{"a":1},"timestamp":"2026-05-01T12:00:00Z","attempt":1}`))
	diags := ValidatePushEnvelope(&PushEnvelope{
		Message:      PushMessage{Data: data, PublishTime: "not-a-timestamp"},
		Subscription: "s",
	})
	if !diag.HasErrors(diags) {
		t.Error("expected error for invalid publishTime")
	}
}

func TestCapabilitiesDeliveryProfiles(t *testing.T) {
	base := func(profiles []DeliveryProfile) *EventServerCapabilities {
		return &EventServerCapabilities{
			Protocol:         Protocol,
			Transports:       []EventServerTransport{EventServerTransportHTTP},
			DeliveryProfiles: profiles,
			Features:         EventServerFeatures{Publish: true, Payload: []PayloadEncoding{PayloadJSON}},
			Endpoints:        EventServerEndpoints{Publish: &EventServerEndpoint{Method: "POST", Path: "/events/publish"}},
		}
	}

	if diags := ValidateEventServerCapabilities(base([]DeliveryProfile{DeliveryProfilePush})); diag.HasErrors(diags) {
		t.Errorf("valid push delivery profile rejected: %v", diags)
	}
	if diags := ValidateEventServerCapabilities(base([]DeliveryProfile{"webhook"})); !diag.HasErrors(diags) {
		t.Error("expected error for unknown delivery profile")
	}
	if diags := ValidateEventServerCapabilities(base([]DeliveryProfile{DeliveryProfilePush, DeliveryProfilePush})); !diag.HasErrors(diags) {
		t.Error("expected error for duplicate delivery profile")
	}
}
