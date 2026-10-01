package infra

import (
	"encoding/json"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestSubscriptionMarshalForms(t *testing.T) {
	cases := []struct {
		name string
		sub  Subscription
		want string
	}{
		{"default empty collapses to string", Subscription{Topic: "a.b"}, `"a.b"`},
		{"explicit pull collapses to string", Subscription{Topic: "a.b", Delivery: DeliveryPull}, `"a.b"`},
		{"push expands to object", Subscription{Topic: "a.b", Delivery: DeliveryPush}, `{"topic":"a.b","delivery":"push"}`},
		{"stream expands to object", Subscription{Topic: "a.b", Delivery: DeliveryStream}, `{"topic":"a.b","delivery":"stream"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.sub)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSubscriptionUnmarshalForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Subscription
	}{
		{"bare string", `"a.b"`, Subscription{Topic: "a.b"}},
		{"object without delivery", `{"topic":"a.b"}`, Subscription{Topic: "a.b"}},
		{"object pull canonicalizes to empty", `{"topic":"a.b","delivery":"pull"}`, Subscription{Topic: "a.b"}},
		{"object push", `{"topic":"a.b","delivery":"push"}`, Subscription{Topic: "a.b", Delivery: DeliveryPush}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Subscription
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Unmarshal(%s) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSubscriptionUnmarshalRejectsUnknownField(t *testing.T) {
	var s Subscription
	if err := json.Unmarshal([]byte(`{"topic":"a.b","foo":1}`), &s); err == nil {
		t.Error("expected error for unknown field in subscription object")
	}
}

func TestEventsSubscribesBackCompatBytes(t *testing.T) {
	// All-pull subscribes must marshal byte-identically to the legacy string
	// array so existing manifests and goldens do not churn.
	ev := Events{Subscribes: []Subscription{{Topic: "a.b"}, {Topic: "c.d"}}}
	got, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"subscribes":["a.b","c.d"]}`
	if string(got) != want {
		t.Errorf("Events marshal = %s, want %s", got, want)
	}
}

func TestMergeResolvesDeliveryByPrecedence(t *testing.T) {
	// Two projects subscribe the same topic with different deliveries; the
	// aggregated entry takes the strongest, independent of contribution order.
	push := ProjectContribution{
		Project:     "b",
		Contributor: ContributorManual,
		Manifest:    PerProjectManifest{Events: &Events{Subscribes: []Subscription{{Topic: "t", Delivery: DeliveryPush}}}},
	}
	pull := ProjectContribution{
		Project:     "a",
		Contributor: ContributorManual,
		Manifest:    PerProjectManifest{Events: &Events{Subscribes: []Subscription{{Topic: "t"}}}},
	}
	for _, order := range [][]ProjectContribution{{push, pull}, {pull, push}} {
		m, diags := Merge("w", order)
		if diag.HasErrors(diags) {
			t.Fatalf("merge errors: %v", diags)
		}
		if m.Events == nil || len(m.Events.Subscribes) != 1 {
			t.Fatalf("expected 1 subscribe, got %+v", m.Events)
		}
		if got := m.Events.Subscribes[0].Delivery; got != DeliveryPush {
			t.Errorf("merged delivery = %q, want push", got)
		}
	}
}

func TestMergePullSubscribeOmitsDelivery(t *testing.T) {
	// A pull-only subscribe must leave delivery empty so the aggregated shape
	// stays byte-identical to the pre-push manifest.
	c := ProjectContribution{
		Project:     "a",
		Contributor: ContributorManual,
		Manifest:    PerProjectManifest{Events: &Events{Subscribes: []Subscription{{Topic: "t"}}}},
	}
	m, _ := Merge("w", []ProjectContribution{c})
	if m.Events.Subscribes[0].Delivery != "" {
		t.Errorf("pull subscribe delivery = %q, want empty", m.Events.Subscribes[0].Delivery)
	}
}

func TestValidatePerProjectManifestDelivery(t *testing.T) {
	valid := &PerProjectManifest{
		ProtocolVersion: ProtocolVersion,
		Events:          &Events{Subscribes: []Subscription{{Topic: "t", Delivery: DeliveryPush}}},
	}
	if diags := ValidatePerProjectManifest(valid); diag.HasErrors(diags) {
		t.Errorf("valid push subscription rejected: %v", diags)
	}

	bad := &PerProjectManifest{
		ProtocolVersion: ProtocolVersion,
		Events:          &Events{Subscribes: []Subscription{{Topic: "t", Delivery: "webhook"}}},
	}
	if diags := ValidatePerProjectManifest(bad); !diag.HasErrors(diags) {
		t.Error("expected error for invalid delivery")
	}
}
