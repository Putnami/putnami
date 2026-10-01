package runtime

// Runtime event protocol, version 2.
//
// Version 2 is version 1's envelope plus ONE new event type: `ready`, the
// typed readiness signal. The version exists because the vocabulary is what a
// consumer must know up front — a reader that switches on `type` cannot tell
// "this runtime never reports readiness" from "this runtime reports it and I
// do not understand the type" unless the envelope says which vocabulary it
// speaks. `v: 1` therefore keeps EXACTLY the nine v1 types, and a `ready` event
// at v1 is a contract violation, not a tolerated extra.
//
// Ownership boundary: the OUTER session-stream record — task:start /
// task:event / task:end / session:end, stamped protocolVersion: 2 — belongs to
// protocols/cli (its doc/02-result-v2.md). This package owns the INNER event
// that travels in a task:event record's `event` member, and versions it here;
// see doc/05-event-v2.md. The unversioned job:* envelopes this package used to
// declare beside it were deleted rather than frozen once their last producer was
// removed, so the outer record now has exactly one owner rather than one live
// owner and one frozen one — see
// doc/adr/0002-delete-the-producerless-stream-envelopes.md.
//
// Readiness REPLACED the log-substring probe the CLI's watch adapter used to
// decide a server was up by matching human text ("listening http(s)://"). The
// watch adapter consumes the typed event and the probe is gone, so a `ready`
// event is now the only thing that arms a serve iteration's file watcher —
// which is why a v1 stream leaves a serve job's watcher unarmed rather than
// falling back.
//
// Everything here is canonical by construction: endpoints are emitted and
// validated in one total order (CompareReadyEndpoints), so a digest over a
// readiness event does not depend on the order a workload happened to bind its
// listeners in.

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
)

// ProtocolVersion2 is the v2 runtime event protocol version, carried in the
// same `v` member v1 uses.
//
// ProtocolVersion (=1) remains the NAME of version 1 — the vocabulary v1
// documents and fixtures are validated against — and is no longer "what every
// emitter writes". The CLI advertises and accepts MaxKnownProtocolVersion only,
// and so do the events it synthesizes itself (batch member projections, replayed
// cached result events, CLI-synthesized diagnostics). A producer picks its
// version by negotiating it (negotiation.go); nothing should reach for
// ProtocolVersion as "the version to stamp".
const ProtocolVersion2 = 2

// EventReady is the typed readiness event, introduced in v2. A v1 event of this
// type is rejected as an unknown type — that is what makes v2 a version rather
// than an unannounced widening.
const EventReady EventType = "ready"

// Ready targets: WHAT became ready. The vocabulary is closed so a consumer can
// decide what to do with an event it did not itself request; a runtime that
// needs a third target is a protocol change, not a free-form string.
const (
	// ReadyTargetServer is a network listener accepting connections. It
	// REQUIRES at least one endpoint: a server nobody can address is not a
	// readiness claim a consumer can act on.
	ReadyTargetServer = "server"
	// ReadyTargetWorkload is the workload as a whole: startup finished and the
	// process is doing its job. Endpoints are optional, because a worker that
	// binds nothing is still legitimately ready.
	ReadyTargetWorkload = "workload"
)

// Ready endpoint schemes. Closed for the same reason as the targets: a watcher
// that prints or probes an endpoint has to understand its scheme.
const (
	ReadySchemeHTTP  = "http"
	ReadySchemeHTTPS = "https"
	ReadySchemeGRPC  = "grpc"
	ReadySchemeTCP   = "tcp"
)

// ValidReadyTargets is the closed ready-target vocabulary in canonical (sorted)
// order. Exported so conformance harnesses enumerate the set this package
// validates against instead of re-typing it.
var ValidReadyTargets = []string{
	ReadyTargetServer,
	ReadyTargetWorkload,
}

// ValidReadyEndpointSchemes is the closed endpoint-scheme vocabulary in
// canonical (sorted) order.
var ValidReadyEndpointSchemes = []string{
	ReadySchemeGRPC,
	ReadySchemeHTTP,
	ReadySchemeHTTPS,
	ReadySchemeTCP,
}

// validReadyTargets and validReadyEndpointSchemes are the lookup views of the
// exported slices; both are built from them so the two cannot disagree.
var (
	validReadyTargets         = setOf(ValidReadyTargets)
	validReadyEndpointSchemes = setOf(ValidReadyEndpointSchemes)
)

func setOf(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

// validEventTypesV2 is the v2 event vocabulary: every v1 type plus ready. It is
// derived from validEventTypes so a type added to v1 cannot be missing from v2.
var validEventTypesV2 = func() map[EventType]bool {
	types := make(map[EventType]bool, len(validEventTypes)+1)
	for t := range validEventTypes {
		types[t] = true
	}
	types[EventReady] = true
	return types
}()

// eventTypesForVersion returns the event vocabulary a given protocol version
// admits. An unknown version is checked against the v1 vocabulary, so an event
// with a bad `v` reports one version diagnostic instead of a version diagnostic
// plus a spurious unknown-type one.
func eventTypesForVersion(v int) map[EventType]bool {
	if v == ProtocolVersion2 {
		return validEventTypesV2
	}
	return validEventTypes
}

// IsKnownProtocolVersion reports whether v is a runtime event protocol version
// this package understands.
func IsKnownProtocolVersion(v int) bool {
	return v == ProtocolVersion || v == ProtocolVersion2
}

// ReadyEndpoint is one address a ready target accepts traffic on.
//
// The URL is DERIVED (see URL) rather than carried, so an endpoint cannot ship
// a url that disagrees with its parts. A carried view of structured members is
// a second authority for the same fact, and the two are only ever equal by
// accident; the same rule governs the job context's derived identity key.
type ReadyEndpoint struct {
	// Scheme is one of ValidReadyEndpointSchemes.
	Scheme string `json:"scheme"`
	// Host is the host the listener is reachable at, as the workload would
	// print it ("localhost", "0.0.0.0", "::1"). Required: an endpoint without a
	// host is not addressable.
	Host string `json:"host"`
	// Port is the TCP port, in [1,65535].
	Port int `json:"port"`
	// Path is the base path the server serves under, when it is not "/". Starts
	// with "/" when present.
	Path string `json:"path,omitempty"`
}

// URL returns the address the endpoint denotes. It is a pure function of the
// endpoint's members and never travels on the wire.
func (e ReadyEndpoint) URL() string {
	host := e.Host
	if e.Port != 0 {
		host = net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	}
	url := e.Scheme + "://" + host
	if e.Path != "" && e.Path != "/" {
		url += e.Path
	}
	return url
}

// ReadyData is the payload of a ready event, carried in the event's `data`.
type ReadyData struct {
	// Target is ReadyTargetServer or ReadyTargetWorkload.
	Target string `json:"target"`
	// Name disambiguates several targets of one workload ("api", "admin").
	// Optional: a workload with a single target does not need it.
	Name string `json:"name,omitempty"`
	// Endpoints are the addresses the target accepts traffic on, in canonical
	// order (CompareReadyEndpoints) and without duplicates. Required for
	// ReadyTargetServer.
	Endpoints []ReadyEndpoint `json:"endpoints,omitempty"`
	// DurationMs is the milliseconds from process start to readiness, when the
	// workload measures it.
	DurationMs int64 `json:"durationMs,omitempty"`
}

// CompareReadyEndpoints is the canonical total order on endpoints: scheme,
// then host, then port, then path. Ordering is byte order, never locale order,
// so a Go and a TypeScript emitter produce the same sequence.
func CompareReadyEndpoints(a, b ReadyEndpoint) int {
	if a.Scheme != b.Scheme {
		return compareStrings(a.Scheme, b.Scheme)
	}
	if a.Host != b.Host {
		return compareStrings(a.Host, b.Host)
	}
	if a.Port != b.Port {
		if a.Port < b.Port {
			return -1
		}
		return 1
	}
	if a.Path != b.Path {
		return compareStrings(a.Path, b.Path)
	}
	return 0
}

func compareStrings(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// SortReadyEndpoints sorts endpoints into canonical order in place.
func SortReadyEndpoints(endpoints []ReadyEndpoint) {
	sort.SliceStable(endpoints, func(i, j int) bool {
		return CompareReadyEndpoints(endpoints[i], endpoints[j]) < 0
	})
}

// ReadyPayload decodes the readiness payload of a ready event. It returns nil
// without an error when the event carries no data, so callers distinguish
// "absent" from "malformed".
func ReadyPayload(e *Event) (*ReadyData, error) {
	if e == nil || len(e.Data) == 0 {
		return nil, nil
	}
	var data ReadyData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return nil, fmt.Errorf("decoding ready payload: %w", err)
	}
	return &data, nil
}

// ExtractReadyData decodes a readiness payload from an already-decoded data
// map, the shape consumers that fold the event into a map (the CLI's session
// stream, the v2 record's `event` member) hold. It mirrors
// ExtractResultPayloads: a nil map yields a nil payload and no error.
func ExtractReadyData(data map[string]any) (*ReadyData, error) {
	if data == nil {
		return nil, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var ready ReadyData
	if err := json.Unmarshal(raw, &ready); err != nil {
		return nil, fmt.Errorf("decoding ready payload: %w", err)
	}
	return &ready, nil
}
