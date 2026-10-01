package client

import (
	"context"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
)

// CodeClientClosed identifies use of a service registry the application already
// stopped. It is the stable typed error every late acquisition returns: binding
// a client, acquiring a credential, and tracking a stream session all fail with
// this code rather than with a bare context cancellation.
const CodeClientClosed errors.Code = "client.closed"

func errClosedRegistry() error {
	return errors.New(CodeClientClosed, "generated service client registry is closed")
}

// Ownership
//
// One application owns one ServicesPlugin, which publishes one ServiceBindings
// registry into that application's dependency-injection container. Everything
// derived from the registry has the registry's lifetime:
//
//   - Credentials. The registry owns the credential cache and every in-flight
//     refresh. Modules of the same application share it, so one refresh serves
//     every module; two applications never share it, whatever their config
//     says.
//   - Token sources. A built-in source is framework-owned. A CredentialBinding
//     that carries an explicit Provider stays caller-owned: the registry stops
//     calling it at Close but never closes it.
//   - Responses. The registry owns the response cache of every generated
//     operation that declares one (ADR 0007 of the client contract), keyed by
//     service, operation, forwarded identity and request, and every upstream
//     call in flight for it. Closing the registry drops both.
//   - Stream sessions. A stream transport registers its session with
//     TrackStream, so stopping the application closes streams the caller never
//     closed. The transport keeps ownership of the terminal: the session
//     records cancel itself when the registry closes it without one.
//
// Close is the single point where all four end, and it is idempotent.

// Close releases everything the application registry owns: it cancels the
// credential refreshes still running, drops every cached credential and every
// cached response, cancels the cached calls in flight, closes the stream
// sessions still open, and refuses every later acquisition with
// CodeClientClosed. Calling it more than once is a no-op.
func (bindings *ServiceBindings) Close() error {
	if bindings == nil {
		return nil
	}
	bindings.mu.Lock()
	if bindings.closed {
		bindings.mu.Unlock()
		return nil
	}
	bindings.closed = true
	streams := make([]*StreamSession, 0, len(bindings.streams))
	for session := range bindings.streams {
		streams = append(streams, session)
	}
	clear(bindings.streams)
	binaryStreams := make([]*ownedResponseStream, 0, len(bindings.binaryStreams))
	for stream := range bindings.binaryStreams {
		binaryStreams = append(binaryStreams, stream)
	}
	clear(bindings.binaryStreams)
	bindings.mu.Unlock()

	// Credentials end first: a session that reaches its send-point re-check
	// while it is being closed sees the closed registry rather than acquiring a
	// bearer nothing will ever use.
	bindings.credentials.close()
	bindings.responses.close()
	var errs []error
	for _, stream := range binaryStreams {
		if err := stream.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, session := range streams {
		if err := session.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.NewAggregate("close service client registry", errs)
	}
	return nil
}

// TrackStream registers an open stream session with the application registry so
// stopping the application closes it. The returned release detaches the session
// when its transport closes it first; it is idempotent and safe to defer.
//
// A registry that is already closed refuses the session with CodeClientClosed:
// the transport must fail the stream instead of opening one that nothing will
// ever stop.
func (bindings *ServiceBindings) TrackStream(session *StreamSession) (func(), error) {
	if bindings == nil || session == nil {
		return nil, errors.New(CodeClientConfig, "tracking a stream session requires a registry and a session")
	}
	bindings.mu.Lock()
	if bindings.closed {
		bindings.mu.Unlock()
		return nil, errClosedRegistry()
	}
	bindings.streams[session] = struct{}{}
	bindings.mu.Unlock()

	var released bool
	return func() {
		bindings.mu.Lock()
		defer bindings.mu.Unlock()
		if released {
			return
		}
		released = true
		delete(bindings.streams, session)
	}, nil
}

func (bindings *ServiceBindings) isClosed() bool {
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	return bindings.closed
}

func (bindings *ServiceBindings) trackBinaryStream(stream *ownedResponseStream) error {
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	if bindings.closed {
		return errClosedRegistry()
	}
	if bindings.binaryStreams == nil {
		bindings.binaryStreams = make(map[*ownedResponseStream]struct{})
	}
	bindings.binaryStreams[stream] = struct{}{}
	return nil
}

// publish records a registry this plugin created so the application's stop
// phase can close it.
func (p *ServicesPlugin) publish(bindings *ServiceBindings) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, bindings)
}

// Stop implements app.Stopper. It closes every registry this plugin published,
// which ends the credential cache, the refreshes in flight, and the stream
// sessions still open. The plugin needs no Starter: the stop phase runs every
// Stopper in reverse registration order whether or not the plugin started
// anything, and the registry is built by dependency injection rather than by a
// start hook.
func (p *ServicesPlugin) Stop(_ context.Context, _ *app.Module) error {
	p.mu.Lock()
	published := p.published
	p.published = nil
	p.mu.Unlock()

	var errs []error
	for _, bindings := range published {
		if err := bindings.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.NewAggregate("stop generated service clients", errs)
	}
	return nil
}
