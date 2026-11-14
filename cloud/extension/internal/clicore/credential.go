package clicore

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// CloudTokenEnv carries the workspace machine token: the one secret of a CI
// job or a runner, which a hosted agent session may hold beside a signed-in
// session.
const CloudTokenEnv = "PUTNAMI_CLOUD_TOKEN"

// CloudCredentialEnv chooses the credential of workspace commands for a whole
// process, as --credential does for one command. The build cache provider,
// which takes no flag, reads it too.
const CloudCredentialEnv = "PUTNAMI_CLOUD_CREDENTIAL" // #nosec G101 -- environment variable name, not a credential

// The credential choices of --credential and PUTNAMI_CLOUD_CREDENTIAL.
const (
	// CredentialSession uses the signed-in session and never the machine token.
	CredentialSession = "session"
	// CredentialMachine uses PUTNAMI_CLOUD_TOKEN even when a session is stored.
	CredentialMachine = "machine"
)

// Credential is the credential a workspace command sends, and why.
type Credential struct {
	// Machine reports that the command sends PUTNAMI_CLOUD_TOKEN; otherwise it
	// sends the signed-in session.
	Machine bool
	// Explicit reports that --credential or PUTNAMI_CLOUD_CREDENTIAL chose it.
	Explicit bool
	// TokenSet reports that PUTNAMI_CLOUD_TOKEN is set, used or not.
	TokenSet bool
}

// String names the credential and why, in the words every surface uses.
func (c Credential) String() string {
	switch {
	case c.Machine && c.Explicit:
		return CloudTokenEnv + ", the workspace machine token, chosen with --credential=machine"
	case c.Machine:
		return CloudTokenEnv + ", the workspace machine token: no signed-in session is stored on this machine"
	case c.TokenSet && c.Explicit:
		return "the signed-in session, chosen with --credential=session; " + CloudTokenEnv + " is not used"
	case c.TokenSet:
		return "the signed-in session, which wins over " + CloudTokenEnv
	default:
		return "the signed-in session"
	}
}

// CredentialChoice is the explicit credential choice: --credential, else
// PUTNAMI_CLOUD_CREDENTIAL, else "". Any other value is a usage error.
func CredentialChoice(params map[string]any, env map[string]string) (string, error) {
	choice := strings.TrimSpace(StringParam(params, "credential"))
	source := "--credential"
	if choice == "" {
		choice = strings.TrimSpace(EnvGet(env, CloudCredentialEnv))
		source = CloudCredentialEnv
	}
	switch choice {
	case "", CredentialSession, CredentialMachine:
		return choice, nil
	default:
		return "", NewError(fmt.Sprintf("%s %q is not %q or %q", source, choice, CredentialSession, CredentialMachine), ExitUsage)
	}
}

// SessionUsable reports whether auth is a session a command can authenticate
// with: a fresh access token, or a refresh token that renews it.
func SessionUsable(auth *StoredToken, now time.Time) bool {
	if auth == nil || auth.AccessToken == "" {
		return false
	}
	return auth.RefreshToken != "" || AccessTokenFresh(auth.AccessToken, auth.ExpiresAt, now)
}

// StoredSessionUsable reports whether ~/.putnami/auth.json holds a usable
// session (SessionUsable).
func StoredSessionUsable(env map[string]string, now time.Time) bool {
	auth, err := ReadAuth(env, false)
	return err == nil && SessionUsable(auth, now)
}

// ResolveCredential is the credential workspace commands send. A usable
// stored session wins; PUTNAMI_CLOUD_TOKEN is sent only when no usable
// session is stored, or when the command chose it with --credential=machine
// or PUTNAMI_CLOUD_CREDENTIAL=machine. Choosing the machine token without
// PUTNAMI_CLOUD_TOKEN is an authentication error.
func ResolveCredential(params map[string]any, env map[string]string, now time.Time) (Credential, error) {
	choice, err := CredentialChoice(params, env)
	if err != nil {
		return Credential{}, err
	}
	cred := Credential{Explicit: choice != "", TokenSet: EnvGet(env, CloudTokenEnv) != ""}
	switch choice {
	case CredentialMachine:
		if !cred.TokenSet {
			return Credential{}, NewError("--credential=machine needs "+CloudTokenEnv+", which is not set", ExitAuth)
		}
		cred.Machine = true
	case CredentialSession:
	default:
		cred.Machine = cred.TokenSet && !StoredSessionUsable(env, now)
	}
	return cred, nil
}

// ioctxNow is ioctx's clock, or the wall clock when it has none.
func ioctxNow(ioctx IO) time.Time {
	if ioctx.Now != nil {
		return ioctx.Now()
	}
	return time.Now()
}

// credentialAnnounced keeps the --verbose credential line to one per process.
var credentialAnnounced atomic.Bool

// announceCredential prints, once per process and only under --verbose, the
// credential a workspace command sends.
func announceCredential(params map[string]any, ioctx IO, cred Credential) {
	if !Truthy(Param(params, "verbose")) || ioctx.Stderr == nil || !credentialAnnounced.CompareAndSwap(false, true) {
		return
	}
	ioctx.Stderr("credential: " + cred.String())
}

// NameCredential adds the credential to an authentication error of a
// workspace command, when the machine holds both credentials, so the person
// sees which one the server refused and how to choose the other. A refusal is
// an ExitAuth error or an HTTP 403. Any other error, and any error on a
// machine with only one credential, is returned as is.
func NameCredential(err error, params map[string]any, env map[string]string, now time.Time) error {
	if err == nil || EnvGet(env, CloudTokenEnv) == "" || !refusal(err) {
		return err
	}
	cred, credErr := ResolveCredential(params, env, now)
	if credErr != nil {
		return err
	}
	hint := "pass --credential=machine to use " + CloudTokenEnv
	if cred.Machine {
		hint = "run putnami cloud login to use a signed-in session"
		if cred.Explicit {
			hint = "drop --credential=machine to use the signed-in session"
		}
	}
	return &credentialError{err: err, note: fmt.Sprintf("%s (credential: %s; %s)", err.Error(), cred, hint)}
}

// refusal reports whether err is a refused credential: an ExitAuth error, or
// a request the server answered with HTTP 403.
func refusal(err error) bool {
	if ExitCode(err) == ExitAuth {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
		return true
	}
	return strings.Contains(err.Error(), StatusLine(http.StatusForbidden))
}

// credentialError is an error whose message names the credential; it keeps
// the exit code and the class of the error it wraps.
type credentialError struct {
	err  error
	note string
}

func (e *credentialError) Error() string { return e.note }

func (e *credentialError) Unwrap() error { return e.err }
