package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// ReleasePlanCallback is the release plan callback a trusted process captured
// (InternalReleasePlanCallbackEnv), with the runner capability that authorizes
// a call to it. The capability stays in this package: a caller puts it on a
// request with SetAuthHeader and never reads it.
type ReleasePlanCallback struct {
	capabilities *processCapabilities
}

// String keeps an accidental diagnostic from rendering the callback or its
// capability.
func (*ReleasePlanCallback) String() string { return "<redacted release plan callback>" }

// ReleasePlanCallbackFrom is the release plan callback ctx carries, nil when
// the process captured none.
func ReleasePlanCallbackFrom(ctx context.Context) *ReleasePlanCallback {
	capabilities := processCapabilitiesFromContext(ctx)
	if capabilities == nil || capabilities.planCallback == "" {
		return nil
	}
	return &ReleasePlanCallback{capabilities: capabilities}
}

// Endpoint is the callback URL as the process captured it. The caller
// validates it before any request.
func (callback *ReleasePlanCallback) Endpoint() string {
	return callback.capabilities.planCallback
}

// Authorized reports whether the process captured a runner capability the
// callback can carry: an AFTER contract and a bearer of at most 64 KiB with no
// space, tab, line break or NUL.
func (callback *ReleasePlanCallback) Authorized() bool {
	capabilities := callback.capabilities
	return capabilities.after != "" && capabilities.cloudToken != "" && len(capabilities.cloudToken) <= 64<<10 &&
		!strings.ContainsAny(capabilities.cloudToken, " \t\r\n\x00")
}

// SetAuthHeader sets the captured runner capability as the bearer of request.
func (callback *ReleasePlanCallback) SetAuthHeader(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+callback.capabilities.cloudToken)
}

// Bind binds the callback to one plan digest per process. Under the
// callback's lock, it returns nil without running handoff when digest is
// already bound, an error when another digest is, and otherwise runs handoff
// and binds digest when handoff returns nil.
func (callback *ReleasePlanCallback) Bind(digest string, handoff func() error) error {
	capabilities := callback.capabilities
	capabilities.planHandoffMu.Lock()
	defer capabilities.planHandoffMu.Unlock()
	if capabilities.planHandoffDigest != "" {
		if capabilities.planHandoffDigest != digest {
			return fmt.Errorf("runner capability is already bound to another release plan")
		}
		return nil
	}
	if err := handoff(); err != nil {
		return err
	}
	capabilities.planHandoffDigest = digest
	return nil
}
