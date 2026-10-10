// Package distributioncli holds the distribution-domain command implementations
// for the @putnami/cloud CLI extension: per-registry credential management
// (registries / registry tokens), archive publishing, and the narrow credential
// resolver used by Data's migration publication boundary. The extension
// binary registers these commands; the shared toolkit they build on lives in
// internal/clicore.
package distributioncli

import (
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// writePublishResult writes the standard command result for the distribution
// publishers. It carries no "warnings" channel: a publish leg that does not
// land — notably a channel move — fails the command instead of degrading to a
// line of advisory text a caller can miss.
func writePublishResult(data any, params map[string]any, ioctx clicore.IO, message string) {
	clicore.WriteResult(data, params, ioctx, message)
}

// nowOrDefault returns the supplied clock or time.Now when it is nil, so the
// registry-token JWT cache can stamp expiries deterministically under test while
// defaulting to the wall clock in production.
func nowOrDefault(now func() time.Time) func() time.Time {
	if now != nil {
		return now
	}
	return time.Now
}
