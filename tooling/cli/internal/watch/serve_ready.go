package watch

import (
	"sync"
	"sync/atomic"

	"go.putnami.dev/cli/model/jobs"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// This file is THE serve-readiness adapter of the watch loop: it turns the
// readiness claim (jobs.IsServeReadyEvent) into the signal that a serve
// iteration's server is up.
//
// A serve iteration must not arm the file watcher until the server is actually
// listening, or the watcher's own startup writes restart the server it just
// started. Readiness is the typed `ready` event of runtime event protocol v2
// (protocols/runtime/doc/05-event-v2.md), which every first-party serve wrapper
// emits through the extension SDK's shared forwarder.
//
// The consumer contract the protocol states is: arm on the FIRST `ready` event
// of the serve iteration. Restarts are covered because the forwarder holds no
// per-stream memory of having announced readiness: every serve iteration spawns
// a fresh workload whose marker travels again, which is why more than one
// `ready` per stream is legal (protocols/runtime fixtures/v2/valid/serve-restart.jsonl).
//
// The signal also keeps the port the iteration's last claim announced
// (ReadyClaim), which is the listener a `--port 0` serve waits on between
// restarts: port 0 names none.
//
// grep: jobs.IsServeReadyEvent, serveReadySignal, ReadyClaim.

// ReadyClaim is the target of a readiness claim and the port a client should
// reach it on: the first http or https endpoint, because the CLI's consumers
// speak HTTP, else the first endpoint, else 0 for a claim that carries none.
func ReadyClaim(event jobs.RawJobEvent) (target string, port int) {
	data, err := runtimeproto.ExtractReadyData(event.Data)
	if err != nil || data == nil {
		return "", 0
	}
	if len(data.Endpoints) == 0 {
		return data.Target, 0
	}
	for _, endpoint := range data.Endpoints {
		if endpoint.Scheme == runtimeproto.ReadySchemeHTTP || endpoint.Scheme == runtimeproto.ReadySchemeHTTPS {
			return data.Target, endpoint.Port
		}
	}
	return data.Target, data.Endpoints[0].Port
}

// serveReadySignal turns those events into a one-shot readiness signal for the
// current serve iteration. Reset per iteration; closed at most once, because the
// serve loop selects on the channel and a second close would panic.
type serveReadySignal struct {
	mu   sync.Mutex
	ch   chan struct{}
	once *sync.Once
	// port is the port the iteration's last claim with an endpoint announced,
	// or 0. A claim without one (a workload claim) leaves it, because it says
	// nothing about where the workload listens.
	port atomic.Int64
}

// reset arms a fresh signal for a new serve iteration and returns the channel
// the loop blocks on. It forgets the previous iteration's port: the loop has
// already waited for that port's release before it starts a new iteration.
func (s *serveReadySignal) reset() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ch = make(chan struct{})
	s.once = &sync.Once{}
	s.port.Store(0)
	return s.ch
}

// lastPort is the port the current iteration's last claim announced, or 0.
func (s *serveReadySignal) lastPort() int { return int(s.port.Load()) }

// observe inspects one job event, records the port a claim announces, and
// signals readiness on a match. Events observed before the first reset, or
// after the signal already fired, signal nothing.
func (s *serveReadySignal) observe(event jobs.RawJobEvent) {
	if !jobs.IsServeReadyEvent(event) {
		return
	}
	if _, port := ReadyClaim(event); port > 0 {
		s.port.Store(int64(port))
	}

	s.mu.Lock()
	ch, once := s.ch, s.once
	s.mu.Unlock()
	if ch == nil || once == nil {
		return
	}
	once.Do(func() { close(ch) })
}
