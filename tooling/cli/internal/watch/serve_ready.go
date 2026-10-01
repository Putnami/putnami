package watch

import (
	"sync"

	"go.putnami.dev/cli/model/jobs"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// This file is THE serve-readiness adapter: the one place that decides a serve
// iteration's server is up.
//
// A serve iteration must not arm the file watcher until the server is actually
// listening, or the watcher's own startup writes restart the server it just
// started. That fact used to be inferred from a substring of the workload's
// LOG text ("listening http(s)://") — a compatibility seam that
// read a human message as a machine signal, could not say WHAT became ready,
// and discarded the address it had just matched. It is gone: readiness is now
// the typed `ready` event of runtime event protocol v2
// (protocols/runtime/doc/05-event-v2.md), which every first-party serve wrapper
// emits through the extension SDK's shared forwarder.
//
// The consumer contract the protocol states is: arm on the FIRST `ready` event
// of the serve iteration — exactly the semantics of the first-matching-line
// probe it replaces. Restarts are covered because the forwarder holds no
// per-stream memory of having announced readiness: every serve iteration spawns
// a fresh workload whose marker travels again, which is why more than one
// `ready` per stream is legal (protocols/runtime fixtures/v2/valid/serve-restart.jsonl).
//
// grep: isServeReadyEvent, serveReadySignal.

// isServeReadyEvent reports whether a job event is a readiness claim this
// iteration can act on.
//
// The three conditions are the protocol's, not this package's. The type must be
// `ready`; the envelope must be v2, because a `ready` stamped v1 is a wire
// violation rather than an early adopter (only v2 admits the type); and the
// payload must decode to a claim inside the closed target vocabulary, because a
// consumer that cannot tell WHAT became ready has not been told anything.
//
// Every rejection fails CLOSED — the watcher stays unarmed — which is the same
// direction the log probe failed in and the safe one: an unarmed watcher costs
// a serve iteration its hot reload, while arming early restarts a server that
// is not listening yet and races the port it has not bound.
func isServeReadyEvent(event jobs.RawJobEvent) bool {
	if event.Type != jobs.EventTypeReady || event.Version != runtimeproto.ProtocolVersion2 {
		return false
	}
	data, err := runtimeproto.ExtractReadyData(event.Data)
	if err != nil || data == nil {
		return false
	}
	return data.Target == runtimeproto.ReadyTargetServer || data.Target == runtimeproto.ReadyTargetWorkload
}

// serveReadySignal turns those events into a one-shot readiness signal for the
// current serve iteration. Reset per iteration; closed at most once, because the
// serve loop selects on the channel and a second close would panic.
type serveReadySignal struct {
	mu   sync.Mutex
	ch   chan struct{}
	once *sync.Once
}

// reset arms a fresh signal for a new serve iteration and returns the channel
// the loop blocks on.
func (s *serveReadySignal) reset() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ch = make(chan struct{})
	s.once = &sync.Once{}
	return s.ch
}

// observe inspects one job event and signals readiness on a match. Events
// observed before the first reset, or after the signal already fired, are
// no-ops.
func (s *serveReadySignal) observe(event jobs.RawJobEvent) {
	if !isServeReadyEvent(event) {
		return
	}

	s.mu.Lock()
	ch, once := s.ch, s.once
	s.mu.Unlock()
	if ch == nil || once == nil {
		return
	}
	once.Do(func() { close(ch) })
}
