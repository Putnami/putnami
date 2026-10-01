package runtime

// Reserved readiness log marker.
//
// A served workload's stdout is a LOG stream, not a runtime event stream: the
// extension that spawned it re-emits each line as a `log` event, so a workload
// that printed a raw runtime event would have it forwarded verbatim into the
// user's console at v1 negotiation, and would still be unable to reach the
// event stream it does not own. The workload therefore announces readiness the
// only way it legitimately can — as a machine-readable member of the log record
// it already writes when it starts listening — under one reserved key whose
// name and payload shape are defined here and mirrored in TypeScript
// (typescript/framework/runtime/src/jobs/events.ts).
//
// The extension's forwarder (tooling/extension-sdk/jsonl) recognizes the key
// and, when its own stream is negotiated at v2, emits the typed `ready` event
// alongside the log event. Because the forwarder is shared, every first-party
// extension — including Python's, which has no framework of its own — gains
// typed readiness from one place, and any workload that logs this key gets it
// for free.

import (
	"encoding/json"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ReadyLogKey is the reserved top-level member of a structured log record that
// carries a ReadyData payload. It is namespaced so it cannot collide with a
// workload's own context fields, and it is stripped from the forwarded log
// event's context: it is a machine channel, so the human log line a user reads
// is byte-identical to the one written before readiness was typed.
const ReadyLogKey = "putnami.ready"

// ReadyMarker returns the value a workload attaches to its listening log record
// under ReadyLogKey. It canonicalizes the endpoint order — the same
// normalization Emitter.Ready applies — so the marker is already deterministic
// on the workload's side and two runs that bound their listeners in different
// orders produce the same log bytes.
//
// The caller's slice is never reordered.
func ReadyMarker(data ReadyData) ReadyData {
	if len(data.Endpoints) > 0 {
		endpoints := make([]ReadyEndpoint, len(data.Endpoints))
		copy(endpoints, data.Endpoints)
		SortReadyEndpoints(endpoints)
		data.Endpoints = endpoints
	}
	return data
}

// ReadyMarkerFromLogRecord extracts the readiness payload from a decoded
// structured log record, returning false when the record carries no marker.
//
// The payload is canonicalized and then validated against the same rules a
// `ready` event's data is held to. A marker that fails them yields false rather
// than a payload, which is the fail-closed direction for a forwarder: a
// workload with a malformed marker loses its typed readiness signal instead of
// poisoning the whole event stream with a line its consumer must reject.
func ReadyMarkerFromLogRecord(record map[string]any) (*ReadyData, bool) {
	if record == nil {
		return nil, false
	}
	raw, ok := record[ReadyLogKey]
	if !ok || raw == nil {
		return nil, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var data ReadyData
	if err := json.Unmarshal(encoded, &data); err != nil {
		return nil, false
	}
	data = ReadyMarker(data)
	if diag.HasErrors(validateReadyData(&data)) {
		return nil, false
	}
	return &data, true
}
