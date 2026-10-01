package app

import "go.putnami.dev/errors"

// Error codes for the app lifecycle.
const (
	CodeAlreadyRunning errors.Code = "app.already_running"
	CodeConfigure      errors.Code = "app.configure"
	CodeRegister       errors.Code = "app.register"
	CodeStart          errors.Code = "app.start"
	CodeStop           errors.Code = "app.stop"
	CodeInvoke         errors.Code = "app.invoke"
	CodeRunner         errors.Code = "app.runner"
	// CodeHealth marks a health/readiness probe wiring or resolution failure on
	// the /healthz and /readyz path (bad probe signature, unbound probe, or a
	// dependency the probe could not resolve from its module container).
	CodeHealth errors.Code = "app.health"
)
