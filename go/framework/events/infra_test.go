package events

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"go.putnami.dev/app"
	protocaps "go.putnami.dev/protocol/capabilities"
	"go.putnami.dev/protocol/infra"
)

func TestBuildInfraManifest(t *testing.T) {
	tests := []struct {
		name           string
		publishes      []string
		subscribes     []string
		delivery       infra.Delivery
		wantNil        bool
		wantPublishes  []string
		wantSubscribes []infra.Subscription
	}{
		{
			name:    "zero topics",
			wantNil: true,
		},
		{
			name:          "publish only",
			publishes:     []string{"order.created"},
			wantPublishes: []string{"order.created"},
		},
		{
			name:           "subscribe only",
			subscribes:     []string{"order.created"},
			wantSubscribes: []infra.Subscription{{Topic: "order.created"}},
		},
		{
			name:           "mixed",
			publishes:      []string{"order.created"},
			subscribes:     []string{"payment.settled"},
			wantPublishes:  []string{"order.created"},
			wantSubscribes: []infra.Subscription{{Topic: "payment.settled"}},
		},
		{
			name:           "duplicate handlers deduped and sorted",
			publishes:      []string{"b.topic", "a.topic", "b.topic"},
			subscribes:     []string{"z.topic", "z.topic"},
			wantPublishes:  []string{"a.topic", "b.topic"},
			wantSubscribes: []infra.Subscription{{Topic: "z.topic"}},
		},
		{
			name:           "push delivery carried on subscribes",
			subscribes:     []string{"order.created"},
			delivery:       infra.DeliveryPush,
			wantSubscribes: []infra.Subscription{{Topic: "order.created", Delivery: infra.DeliveryPush}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, diags := buildInfraManifest(tt.publishes, tt.subscribes, tt.delivery)
			if len(diags) != 0 {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if tt.wantNil {
				if m != nil {
					t.Fatalf("expected nil manifest, got %+v", m)
				}
				return
			}
			if m == nil {
				t.Fatal("expected manifest, got nil")
			}
			if m.ProtocolVersion != infra.ProtocolVersion {
				t.Errorf("protocolVersion = %d, want %d", m.ProtocolVersion, infra.ProtocolVersion)
			}
			if m.Schema != infra.PerProjectSchemaURL {
				t.Errorf("schema = %q, want %q", m.Schema, infra.PerProjectSchemaURL)
			}
			if m.Events == nil {
				t.Fatal("expected events block, got nil")
			}
			if !reflect.DeepEqual(m.Events.Publishes, tt.wantPublishes) {
				t.Errorf("publishes = %v, want %v", m.Events.Publishes, tt.wantPublishes)
			}
			if !reflect.DeepEqual(m.Events.Subscribes, tt.wantSubscribes) {
				t.Errorf("subscribes = %v, want %v", m.Events.Subscribes, tt.wantSubscribes)
			}
		})
	}
}

func TestPluginDesignInfraRequirementsDeduplicatesPublishAndSubscribeTopics(t *testing.T) {
	type OrderEvent struct{ ID string }
	shared := NewTopic[OrderEvent]("order.shared")
	plugin := Events(PluginConfig{Publishes: []string{"z.topic", "order.shared"}})
	RegisterPublisher(plugin, shared)
	plugin.Register(Handle(shared, func(context.Context, *Message[OrderEvent]) error { return nil }))
	plugin.Register(Handle(NewTopic[OrderEvent]("a.topic"), func(context.Context, *Message[OrderEvent]) error { return nil }))

	requirements := plugin.DesignInfraRequirements()
	got := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		if requirement.Kind != protocaps.InfraKindEvents {
			t.Fatalf("infra kind = %q, want events", requirement.Kind)
		}
		got = append(got, requirement.Name)
	}
	want := []string{"a.topic", "order.shared", "z.topic"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event requirements = %v, want %v", got, want)
	}
}

func TestBuildInfraManifestInvalidTopicName(t *testing.T) {
	m, diags := buildInfraManifest([]string{"Not A Valid Topic"}, nil, DeliveryPull)
	if m != nil {
		t.Fatalf("expected nil manifest for invalid topic, got %+v", m)
	}
	if len(diags) == 0 {
		t.Fatal("expected a diagnostic for the invalid topic name")
	}
}

func TestPluginDescribeWritesSidecar(t *testing.T) {
	type OrderEvent struct{ ID string }
	type PaymentEvent struct{ Amount int }

	produced := NewTopic[OrderEvent]("order.created")
	consumed := NewTopic[PaymentEvent]("payment.settled")

	plugin := Events()
	plugin.Register(Handle(consumed, func(context.Context, *Message[PaymentEvent]) error { return nil }))
	// A second handler on the same topic must not duplicate the entry.
	plugin.Register(Handle(consumed, func(context.Context, *Message[PaymentEvent]) error { return nil }))
	RegisterPublisher(plugin, produced)

	out := t.TempDir()
	if err := plugin.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	path := infra.SidecarPathIn(out, "events")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	var m infra.PerProjectManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}
	if m.Events == nil {
		t.Fatal("expected events block in sidecar")
	}
	if !reflect.DeepEqual(m.Events.Publishes, []string{"order.created"}) {
		t.Errorf("publishes = %v, want [order.created]", m.Events.Publishes)
	}
	if !reflect.DeepEqual(m.Events.Subscribes, []infra.Subscription{{Topic: "payment.settled"}}) {
		t.Errorf("subscribes = %v, want [payment.settled]", m.Events.Subscribes)
	}
}

func TestPluginDescribeNoTopicsWritesNothing(t *testing.T) {
	out := t.TempDir()
	if err := Events().Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	path := infra.SidecarPathIn(out, "events")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar, stat err = %v", err)
	}
}

func TestPluginDescribeNoTopicsDoesNotTouchOtherProducerSidecars(t *testing.T) {
	out := t.TempDir()
	// Each producer owns its own sidecar; events must not read or write the
	// storage producer's file.
	storagePath := infra.SidecarPathIn(out, "storage")
	if err := infra.WriteSidecarIn(out, "storage", infra.PerProjectManifest{
		Storage: []infra.StorageBucket{{Name: "uploads"}},
	}); err != nil {
		t.Fatal(err)
	}
	storageBefore, err := os.ReadFile(storagePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := Events().Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	storageAfter, err := os.ReadFile(storagePath)
	if err != nil {
		t.Fatalf("storage sidecar disappeared: %v", err)
	}
	if !bytes.Equal(storageBefore, storageAfter) {
		t.Errorf("storage sidecar mutated by events Describe:\nbefore: %s\nafter:  %s", storageBefore, storageAfter)
	}
	if _, err := os.Stat(infra.SidecarPathIn(out, "events")); !os.IsNotExist(err) {
		t.Errorf("expected no events sidecar, stat err = %v", err)
	}
}

func TestPluginDescribeSkipsUnwantedTarget(t *testing.T) {
	type OrderEvent struct{ ID string }
	plugin := RegisterPublisher(Events(), NewTopic[OrderEvent]("order.created"))

	out := t.TempDir()
	ctx := &app.DescribeContext{OutputDir: out, Targets: []string{"openapi"}}
	if err := plugin.Describe(ctx); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	path := infra.SidecarPathIn(out, "events")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar when target not requested, stat err = %v", err)
	}
}

func TestPluginDescribeInvalidTopicReturnsError(t *testing.T) {
	plugin := Events(PluginConfig{Publishes: []string{"INVALID TOPIC"}})
	err := plugin.Describe(&app.DescribeContext{OutputDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error for invalid topic name")
	}
}
