package runtime

import (
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// v2 validation rules. The envelope rules live in validate.go and are shared:
// only the ready event is new, so only its payload is checked here.

// maxPort is the highest addressable TCP port.
const maxPort = 65535

// validateReadyEvent checks the readiness payload of a v2 ready event.
//
// The rules exist so a consumer can ACT on the event without re-deriving what
// the workload meant: a server claim carries an address, the address is
// well-formed, and the endpoint list is in one canonical order so two runs of
// the same workload produce the same bytes.
func validateReadyEvent(e *Event) []diag.Diagnostic {
	var diags []diag.Diagnostic

	data, err := ReadyPayload(e)
	if err != nil {
		return append(diags, diag.Errorf("invalid-ready-data", "data",
			"ready event data is not a readiness payload: %v", err))
	}
	if data == nil {
		return append(diags, diag.Errorf("required-field", "data",
			"ready event requires data"))
	}

	return append(diags, validateReadyData(data)...)
}

// validateReadyData checks a decoded readiness payload. It is split out of
// validateReadyEvent because the payload also travels outside an event
// envelope: a workload announces readiness through the reserved log marker
// (ready_marker.go), and that marker must be held to exactly the rules the
// event is, or the forwarder would turn a malformed marker into an invalid
// stream.
func validateReadyData(data *ReadyData) []diag.Diagnostic {
	var diags []diag.Diagnostic

	switch {
	case data.Target == "":
		diags = append(diags, diag.Errorf("required-field", "data.target",
			"ready event requires a target"))
	case !validReadyTargets[data.Target]:
		diags = append(diags, diag.Errorf("invalid-enum", "data.target",
			"invalid ready target %q; must be one of: %s",
			data.Target, strings.Join(ValidReadyTargets, ", ")))
	case data.Target == ReadyTargetServer && len(data.Endpoints) == 0:
		diags = append(diags, diag.Errorf("required-field", "data.endpoints",
			"ready event with target %q requires at least one endpoint", ReadyTargetServer))
	}

	if data.DurationMs < 0 {
		diags = append(diags, diag.Errorf("invalid-value", "data.durationMs",
			"durationMs must be non-negative, got %d", data.DurationMs))
	}

	diags = append(diags, validateReadyEndpoints(data.Endpoints)...)
	return diags
}

// ValidReadyEndpoint reports whether one endpoint satisfies the rules a ready
// payload holds each of its endpoints to. A producer that gathers endpoints
// from several sources drops one that fails them, instead of voiding the whole
// claim it would otherwise invalidate.
func ValidReadyEndpoint(endpoint ReadyEndpoint) bool {
	return !diag.HasErrors(validateReadyEndpoints([]ReadyEndpoint{endpoint}))
}

// validateReadyEndpoints checks each endpoint and the list's canonical order.
func validateReadyEndpoints(endpoints []ReadyEndpoint) []diag.Diagnostic {
	var diags []diag.Diagnostic

	for i, endpoint := range endpoints {
		field := func(member string) string {
			return fmt.Sprintf("data.endpoints[%d].%s", i, member)
		}
		switch {
		case endpoint.Scheme == "":
			diags = append(diags, diag.Errorf("required-field", field("scheme"),
				"endpoint requires a scheme"))
		case !validReadyEndpointSchemes[endpoint.Scheme]:
			diags = append(diags, diag.Errorf("invalid-enum", field("scheme"),
				"invalid endpoint scheme %q; must be one of: %s",
				endpoint.Scheme, strings.Join(ValidReadyEndpointSchemes, ", ")))
		}
		if endpoint.Host == "" {
			diags = append(diags, diag.Errorf("required-field", field("host"),
				"endpoint requires a host"))
		}
		if endpoint.Port < 1 || endpoint.Port > maxPort {
			diags = append(diags, diag.Errorf("invalid-value", field("port"),
				"endpoint port %d out of range [1,%d]", endpoint.Port, maxPort))
		}
		if endpoint.Path != "" && !strings.HasPrefix(endpoint.Path, "/") {
			diags = append(diags, diag.Errorf("invalid-value", field("path"),
				"endpoint path %q must start with %q", endpoint.Path, "/"))
		}
	}

	// Canonical order is a determinism rule, not a style preference: the digest
	// of a readiness event must not depend on the order a workload happened to
	// bind its listeners in. Duplicates are rejected for the same reason a
	// declared output has one owner — two identical endpoints are two claims
	// about one address.
	for i := 1; i < len(endpoints); i++ {
		switch CompareReadyEndpoints(endpoints[i-1], endpoints[i]) {
		case 0:
			diags = append(diags, diag.Errorf("duplicate-endpoint",
				fmt.Sprintf("data.endpoints[%d]", i),
				"endpoint %s is listed twice", endpoints[i].URL()))
		case 1:
			diags = append(diags, diag.Errorf("non-canonical-order",
				fmt.Sprintf("data.endpoints[%d]", i),
				"endpoints must be in canonical order; %s precedes %s",
				endpoints[i].URL(), endpoints[i-1].URL()))
		}
	}

	return diags
}

// sortedVersions renders a version set deterministically for diagnostics.
func sortedVersions(versions map[int]bool) string {
	list := make([]int, 0, len(versions))
	for v := range versions {
		list = append(list, v)
	}
	sort.Ints(list)
	parts := make([]string, len(list))
	for i, v := range list {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return strings.Join(parts, ", ")
}
