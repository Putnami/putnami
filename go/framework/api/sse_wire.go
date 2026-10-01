package api

import (
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// sseWire returns the negotiated SSE wire of a first-party server stream that
// declares a continuation (clientcontract ADR 0013), or nil. A route without a
// client contract, a route an external authority owns, a provider-owned wire
// and a stream that declares no continuation keep the legacy framing for
// every request, so an old consumer never reads a control event.
func (d EndpointDefinition) sseWire(document *clientcontract.DocumentV1) *phttp.SSEWire {
	b := d.builder
	if document == nil || b.clientOptions == nil || b.clientOptions.SSEContinuation == nil ||
		b.streamMode() != phttp.StreamModeServer || b.providerWire() != nil {
		return nil
	}
	return &phttp.SSEWire{
		Header:     clientcontract.SSEWireHeader,
		Token:      clientcontract.SSEWireV1,
		Negotiates: clientcontract.NegotiatesSSEWire,
		Complete:   clientcontract.SSECompleteFrame,
	}
}
