package jira

import (
	"encoding/base64"
	"regexp"
	"strings"
	"sync"
)

// The environment variables the credential is read from. The binding never
// carries a credential: settings are committed and echoed by capability
// discovery.
const (
	EmailVariable = "JIRA_EMAIL"
	TokenVariable = "JIRA_API_TOKEN" //nolint:gosec // G101: the name of the variable, not a credential
)

// emailShape is what the account's email may look like: no whitespace, no
// control character and no ':', which would split the Basic credential.
var emailShape = regexp.MustCompile(`^[^\s:\x00-\x1f\x7f]{3,320}$`)

// tokenShape is what an API token may look like: letters, digits and
// "_.=+/-". It can never split or inject a header, and JSON never escapes it,
// so redaction finds it verbatim in anything the provider writes.
var tokenShape = regexp.MustCompile(`^[A-Za-z0-9_.=+/-]{8,1000}$`)

// Credential resolves the account email and API token once. Neither the token
// nor the Basic credential built from it is ever part of an error, a message
// or a result: Secrets lists both for redaction.
type Credential struct {
	getenv func(string) string

	mu       sync.Mutex
	resolved bool
	header   string
	token    string
	err      *Error
}

// NewCredential returns the credential resolver. getenv reads the process
// environment.
func NewCredential(getenv func(string) string) *Credential {
	return &Credential{getenv: getenv}
}

// Authorization returns the value of the Authorization header: HTTP Basic
// with JIRA_EMAIL and JIRA_API_TOKEN.
func (c *Credential) Authorization() (string, *Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved {
		c.header, c.err = c.resolve()
		c.resolved = true
	}
	return c.header, c.err
}

func (c *Credential) resolve() (string, *Error) {
	email := strings.TrimSpace(c.getenv(EmailVariable))
	token := strings.TrimSpace(c.getenv(TokenVariable))
	switch {
	case email == "" || token == "":
		return "", &Error{Kind: NoCredential, Message: "no Jira credential: set " + EmailVariable + " and " + TokenVariable}
	case !emailShape.MatchString(email):
		return "", &Error{Kind: NoCredential, Message: EmailVariable + " is not a single account email without whitespace or ':'"}
	case !tokenShape.MatchString(token):
		return "", &Error{Kind: NoCredential, Message: TokenVariable + " is not a single token of letters, digits and _.=+/-"}
	}
	c.token = token
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token)), nil
}

// Secrets lists the resolved token and the Basic credential, for redaction.
// It is empty until Authorization resolved them.
func (c *Credential) Secrets() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		return nil
	}
	return []string{c.token, strings.TrimPrefix(c.header, "Basic ")}
}

// knownTokenPattern matches the published Atlassian API token format, which
// is redacted wherever it appears, whoever's it is.
var knownTokenPattern = regexp.MustCompile(`\bATATT[A-Za-z0-9_\-=+/]{20,1000}`)

// RedactedMarker replaces every redacted credential.
const RedactedMarker = "[redacted]"

// Redact removes secrets and every string shaped like an Atlassian API token
// from text.
func Redact(text string, secrets []string) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, RedactedMarker)
		}
	}
	return knownTokenPattern.ReplaceAllString(text, RedactedMarker)
}
