package client

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

// ServiceCallInfo is the stable, credential-free identity of a generated
// service call. It deliberately excludes endpoint URLs, headers, and bodies.
type ServiceCallInfo struct {
	ServiceID   string
	OperationID string
	Protocol    string
}

// ServiceCallResult is the bounded result vocabulary exposed to telemetry.
// Code is either a provider-declared stable code or a local framework code.
type ServiceCallResult struct {
	StatusCode int
	Code       string
	Attempts   int
}

// ServiceAttemptInfo identifies one attempt of a generated service call.
type ServiceAttemptInfo struct {
	ServiceCallInfo
	Attempt int
}

// ServiceAttemptResult reports one attempt without exposing its request,
// response, credential, or arbitrary transport error.
type ServiceAttemptResult struct {
	StatusCode   int
	Code         string
	AuthDuration time.Duration
}

// ServiceCallTelemetry connects generated clients to an observability
// implementation without making the client runtime depend on an SDK. The
// telemetry framework installs its OpenTelemetry bridge during configuration.
type ServiceCallTelemetry interface {
	StartServiceCall(context.Context, ServiceCallInfo) (context.Context, func(ServiceCallResult))
	StartServiceAttempt(context.Context, ServiceAttemptInfo) (context.Context, func(ServiceAttemptResult))
	InjectServiceContext(context.Context, http.Header)
}

type serviceTelemetryHolder struct {
	observer ServiceCallTelemetry
}

var installedServiceTelemetry atomic.Pointer[serviceTelemetryHolder]

// InstallServiceCallTelemetry installs the process-wide bridge used by
// generated clients and returns a compare-and-restore function for framework
// lifecycle cleanup. A concurrent replacement is never overwritten by an old
// plugin's cleanup.
func InstallServiceCallTelemetry(observer ServiceCallTelemetry) func() {
	previous := installedServiceTelemetry.Load()
	installed := &serviceTelemetryHolder{observer: observer}
	installedServiceTelemetry.Store(installed)
	return func() {
		installedServiceTelemetry.CompareAndSwap(installed, previous)
	}
}

func currentServiceTelemetry() ServiceCallTelemetry {
	holder := installedServiceTelemetry.Load()
	if holder == nil {
		return nil
	}
	return holder.observer
}
