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

// Credential is the credential a workspace command sends, and why.
type Credential struct {
	// Machine reports that the command sends PUTNAMI_CLOUD_TOKEN, because no
	// usable session is stored; otherwise it sends the signed-in session.
	Machine bool
	// TokenSet reports that PUTNAMI_CLOUD_TOKEN is set, used or not.
	TokenSet bool
	// Stored reports that ~/.putnami/auth.json holds a session, usable or not.
	Stored bool
}

// String names the credential and why, in the words every surface uses.
func (c Credential) String() string {
	switch {
	case c.Machine && c.Stored:
		return CloudTokenEnv + ", the workspace machine token: the stored session expired and cannot renew"
	case c.Machine:
		return CloudTokenEnv + ", the workspace machine token: no signed-in session is stored on this machine"
	case c.TokenSet:
		return "the signed-in session, which wins over " + CloudTokenEnv
	default:
		return "the signed-in session"
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

// ResolveCredential is the credential workspace commands send. A usable
// stored session always wins; PUTNAMI_CLOUD_TOKEN is sent only when no usable
// session is stored. No flag or variable chooses the machine token over a
// usable session.
func ResolveCredential(env map[string]string, now time.Time) Credential {
	tokenSet := EnvGet(env, CloudTokenEnv) != ""
	auth, err := ReadAuth(env, false)
	if err != nil {
		auth = nil
	}
	return Credential{
		Machine:  tokenSet && !SessionUsable(auth, now),
		TokenSet: tokenSet,
		Stored:   auth != nil && auth.AccessToken != "",
	}
}

// usedCredential is the credential WorkspaceAuth last sent in this process,
// nil before any. NameCredential reads it, so a note names what was sent,
// never what a command that picks its own bearer might have sent.
var usedCredential atomic.Pointer[Credential]

// recordCredential records the credential a workspace command sends.
func recordCredential(cred Credential) {
	usedCredential.Store(&cred)
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

// NameCredential adds to a refused request the credential WorkspaceAuth sent,
// when the machine holds both a stored session and PUTNAMI_CLOUD_TOKEN, so
// the person sees which one the server refused. A refusal is an ExitAuth
// error or an HTTP 403. Any other error, an error of a command that sent no
// credential through WorkspaceAuth, and any error on a machine with only one
// credential (a CI job with the token alone) is returned as is.
func NameCredential(err error) error {
	cred := usedCredential.Load()
	if err == nil || cred == nil || !cred.TokenSet || !cred.Stored || !refusal(err) {
		return err
	}
	return &credentialError{err: err, note: fmt.Sprintf("%s (credential: %s; run putnami cloud login to renew the session)", err.Error(), *cred)}
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
