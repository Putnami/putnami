package clicore

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
)

// Delegation is what a delegated session's access token says about it: a
// person's MCP connection approved this CLI's sign-in, so the token carries
// an actor (`act.sub`, for example agent-cli) and is bound to one workspace
// (`scope_ref.workspace_id`). Such a token admits only the routes delegated
// to that connection; the issuer's userinfo endpoint is not one of them.
// Only claims are read here, never verified: the routes verify the token.
type Delegation struct {
	// Subject is the person the session acts for (`sub`).
	Subject string
	// Actor is the client acting for the person (`act.sub`).
	Actor string
	// WorkspaceID is the one workspace the token is bound to.
	WorkspaceID string
	// Scopes are the token's granted scopes (`scope`).
	Scopes []string
}

// DelegationFromClaims reads a delegated session from access token claims.
// It reports false for a person's own session: a token without an actor or
// without a workspace scope is not delegated.
func DelegationFromClaims(claims map[string]any) (Delegation, bool) {
	act, _ := claims["act"].(map[string]any)
	actor := strings.TrimSpace(StringValue(act["sub"]))
	workspaceID := strings.TrimSpace(ClaimedWorkspaceID(claims))
	if actor == "" || workspaceID == "" {
		return Delegation{}, false
	}
	return Delegation{
		Subject:     strings.TrimSpace(StringValue(claims["sub"])),
		Actor:       actor,
		WorkspaceID: workspaceID,
		Scopes:      strings.Fields(StringValue(claims["scope"])),
	}, true
}

// DelegationOf reads a delegated session from a stored access token.
func DelegationOf(auth *StoredToken) (Delegation, bool) {
	if auth == nil {
		return Delegation{}, false
	}
	return DelegationFromClaims(DecodeJWT(auth.AccessToken))
}

// sentDelegation is the delegated session ActiveAuth or WorkspaceAuth last
// read in this process, nil when the last credential read was a person's own
// session or PUTNAMI_CLOUD_TOKEN. A refusal is named a delegated-route
// refusal only when the session that sent the request was delegated.
var sentDelegation atomic.Pointer[Delegation]

// recordDelegation records whether the session about to be sent is delegated.
func recordDelegation(auth *StoredToken) {
	if delegation, ok := DelegationOf(auth); ok {
		sentDelegation.Store(&delegation)
		return
	}
	sentDelegation.Store(nil)
}

// SentDelegation returns the delegated session the last request of this
// process was sent with, and false when it was not a delegated one.
func SentDelegation() (Delegation, bool) {
	if delegation := sentDelegation.Load(); delegation != nil {
		return *delegation, true
	}
	return Delegation{}, false
}

// ForgetSentDelegation clears the record of the session sent, so a test
// that signed in with a delegated session leaves no trace for the next one.
func ForgetSentDelegation() {
	sentDelegation.Store(nil)
}

// delegatedReadTools names, per command or status entry, the MCP read tool
// that reads the same data a delegated session cannot read here.
var delegatedReadTools = map[string]string{
	"logs":    "query_logs, or list_errors for the errors",
	"metrics": "query_metrics",
	"traces":  "query_traces",
	"ci":      "list_runs or get_run",
	"env":     "list_environments or list_releases",
	"config":  "get_configuration",
	"db":      "list_databases",
}

// DelegatedReadTool is the MCP read tool for what a command or status entry
// reads, "" when none reads it. name is the command (`logs`, `ci`) or the
// status entry id (`ci`, `env.production`); a dotted id is read by its root.
func DelegatedReadTool(name string) string {
	root, _, _ := strings.Cut(strings.TrimSpace(name), ".")
	return delegatedReadTools[root]
}

// DelegatedRouteError is a refusal a delegated session got from a route its
// token does not admit. The session itself works: signing in again changes
// nothing, and the MCP connection reads what it admits.
type DelegatedRouteError struct {
	Delegation Delegation
	// Status is the refusal's HTTP status, 401 or 403.
	Status int
	// Tool is the MCP read tool for the same data, "" when none reads it.
	Tool  string
	cause error
	exit  *ExitError
}

func (e *DelegatedRouteError) Error() string { return e.exit.Error() }

// Unwrap keeps the exit class of the refusal and the refusal itself.
func (e *DelegatedRouteError) Unwrap() []error { return []error{e.exit, e.cause} }

// DelegatedRefusal names a 401 or 403 that a delegated session got from a
// route it does not admit: the reason, and the MCP read tool for name's data
// when one exists. Any other error, and any refusal a person's own session or
// PUTNAMI_CLOUD_TOKEN got, is returned as is.
func DelegatedRefusal(err error, name string) error {
	if err == nil {
		return nil
	}
	var already *DelegatedRouteError
	if errors.As(err, &already) {
		return err
	}
	delegation, ok := SentDelegation()
	if !ok {
		return err
	}
	status := RefusalStatus(err)
	if status == 0 {
		return err
	}
	tool := DelegatedReadTool(name)
	message := fmt.Sprintf("refused (%s): this session is delegated through an MCP connection (%s) for workspace %s, and its token admits only the routes delegated to that connection",
		StatusLine(status), delegation.Actor, delegation.WorkspaceID)
	if cred := usedCredential.Load(); cred != nil && cred.TokenSet && cred.Stored {
		message += " (credential: " + cred.String() + ")"
	}
	if tool != "" {
		message += "; read it with the MCP tool " + tool
	} else {
		message += "; a person's own `putnami cloud login` session reaches it"
	}
	return &DelegatedRouteError{
		Delegation: delegation,
		Status:     status,
		Tool:       tool,
		cause:      err,
		exit:       newExitError(message, ExitCode(err)),
	}
}

// IsDelegatedRefusal reports whether err is a delegated-route refusal.
func IsDelegatedRefusal(err error) bool {
	var refusal *DelegatedRouteError
	return errors.As(err, &refusal)
}

// RefusalStatus is the 401 or 403 a request was refused with, 0 when err is
// no such refusal. A failed sign-in refresh is not a route refusal: it ends
// the session.
func RefusalStatus(err error) int {
	if err == nil {
		return 0
	}
	if isRefreshError(err) {
		return 0
	}
	refused := func(status int) bool {
		return status == http.StatusUnauthorized || status == http.StatusForbidden
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if refused(apiErr.StatusCode) {
			return apiErr.StatusCode
		}
		return 0
	}
	if status := ServiceStatus(err); status != 0 {
		if refused(status) {
			return status
		}
		return 0
	}
	// A command that renders a provider refusal itself keeps only its
	// status line in the message.
	message := err.Error()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if strings.Contains(message, StatusLine(status)) {
			return status
		}
	}
	if strings.Contains(message, "Authentication required") {
		return http.StatusUnauthorized
	}
	return 0
}
