package infra

import (
	"bytes"
	"encoding/json"

	pevents "go.putnami.dev/protocol/events"
)

// Delivery is the delivery model a subscribed topic uses. The canonical
// vocabulary (pull | stream | push) is owned by go.putnami.dev/protocol/events;
// infra carries the value so a deployer knows which subscribed topics need a
// provider push subscription provisioned. The empty value is the back-compatible
// default, pull.
type Delivery = pevents.DeliveryProfile

// Delivery values, re-exported from the events protocol so infra consumers do
// not need a second import for the common case.
const (
	DeliveryPull   = pevents.DeliveryProfilePull
	DeliveryStream = pevents.DeliveryProfileStream
	DeliveryPush   = pevents.DeliveryProfilePush
)

// Subscription is one subscribed topic in a project's events requirement.
//
// To stay backwards compatible with the original string-only shape, a
// Subscription marshals to a bare JSON string when its delivery is the default
// (pull) — so a workload that does not use push delivery emits byte-for-byte the
// same manifest it always has — and to an object { "topic", "delivery" } only
// when it carries an explicit non-default delivery. Unmarshalling accepts both
// forms: a bare string reads as { topic, delivery: pull }.
//
// This keeps the infra protocol version unchanged: pull/stream workloads stay
// readable by existing v1 readers, and only push subscriptions introduce the
// enriched object form (which the deployer that provisions push subscriptions
// understands).
type Subscription struct {
	Topic    string   `json:"topic"`
	Delivery Delivery `json:"delivery,omitempty"`
}

// subscriptionObject is the object wire form. It mirrors Subscription's fields
// but has no custom (un)marshaling, so it is used to encode/decode the object
// branch without recursing through Subscription's own methods.
type subscriptionObject struct {
	Topic    string   `json:"topic"`
	Delivery Delivery `json:"delivery,omitempty"`
}

// MarshalJSON emits the canonical wire form: a bare topic string for the default
// (pull) delivery, an object otherwise. Pull and the empty value are equivalent
// and both collapse to the bare string, so the default never widens the shape.
func (s Subscription) MarshalJSON() ([]byte, error) {
	if s.Delivery == "" || s.Delivery == DeliveryPull {
		return json.Marshal(s.Topic)
	}
	return json.Marshal(subscriptionObject(s))
}

// UnmarshalJSON accepts either a bare topic string (delivery defaults to pull,
// kept as the empty value) or a strict { topic, delivery } object (unknown
// fields rejected, mirroring the manifest's strict parsing). An explicit
// delivery of "pull" canonicalizes to the empty value so it round-trips to a
// bare string.
func (s *Subscription) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var topic string
		if err := json.Unmarshal(trimmed, &topic); err != nil {
			return err
		}
		s.Topic = topic
		s.Delivery = ""
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var obj subscriptionObject
	if err := dec.Decode(&obj); err != nil {
		return err
	}
	s.Topic = obj.Topic
	s.Delivery = canonicalDelivery(obj.Delivery)
	return nil
}

// canonicalDelivery maps the default (pull) to the empty value so equivalent
// declarations — a bare string, an absent delivery, and an explicit "pull" —
// all normalize to one canonical form. This keeps merge resolution and wire
// output deterministic.
func canonicalDelivery(d Delivery) Delivery {
	if d == DeliveryPull {
		return ""
	}
	return d
}
