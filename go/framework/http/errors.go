package http

import "go.putnami.dev/errors"

// Error codes for the HTTP package.
const (
	CodeListen errors.Code = "http.listen"
	CodeBody   errors.Code = "http.body"
	CodeScope  errors.Code = "http.scope"
)

// Request-scope outcome sentinels passed to DetachedScope.Finalize so a
// scope-scoped transaction (a database UnitOfWork) rolls back. Their identity is
// what matters — a non-nil outcome means "failure, roll back"; the text is for
// server-side logging only and never reaches the client.
var (
	errRequestPanic        = errors.New(CodeScope, "request handler panicked; rolling back request scope")
	errServerErrorResponse = errors.New(CodeScope, "handler returned a server-error response; rolling back request scope")
)
