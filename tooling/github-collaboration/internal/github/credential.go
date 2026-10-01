package github

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// TokenCommand asks an installed gh CLI for the token it stores for a host.
type TokenCommand func(ctx context.Context, host string) (string, error)

// errNoGH reports that no gh CLI is installed.
var errNoGH = errors.New("the gh CLI is not installed")

// GHAuthToken runs `gh auth token --hostname <host>`. Its standard error is
// discarded, never reported: it is not needed to explain the failure, and
// nothing the process writes may reach a caller unexamined.
func GHAuthToken(ctx context.Context, host string) (string, error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return "", errNoGH
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "auth", "token", "--hostname", host)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", errors.New("gh auth token --hostname " + host + " failed; run gh auth login --hostname " + host)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// publicHostVariables are read, in order, for github.com: the variables the
// gh CLI reads.
var publicHostVariables = []string{"GH_TOKEN", "GITHUB_TOKEN"}

// enterpriseHostVariables are read, in order, for another host, and only when
// GH_HOST names that host: a workspace that names a host cannot receive the
// enterprise credential of another one.
var enterpriseHostVariables = []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// tokenShape is what a credential may look like: letters, digits, '_', '.'
// and '-'. It can never split or inject a header, and JSON never escapes it,
// so redaction finds it verbatim in anything the provider writes.
var tokenShape = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,1000}$`)

// Credential resolves the token of one host once. The token is never part of
// an error, a message or a result: Secrets lists it for redaction.
type Credential struct {
	host    string
	getenv  func(string) string
	command TokenCommand

	mu       sync.Mutex
	resolved bool
	token    string
	err      *Error
}

// NewCredential returns the credential resolver of a host. getenv reads the
// process environment; command runs gh auth token when no variable names a
// token.
func NewCredential(host string, getenv func(string) string, command TokenCommand) *Credential {
	return &Credential{host: host, getenv: getenv, command: command}
}

// Token returns the host's token: GH_TOKEN or GITHUB_TOKEN for github.com,
// GH_ENTERPRISE_TOKEN or GITHUB_ENTERPRISE_TOKEN for the enterprise host
// GH_HOST names, and otherwise the token `gh auth token` stores for the host.
func (c *Credential) Token(ctx context.Context) (string, *Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved {
		c.token, c.err = c.resolve(ctx)
		c.resolved = true
	}
	return c.token, c.err
}

func (c *Credential) resolve(ctx context.Context) (string, *Error) {
	variables := enterpriseHostVariables
	if strings.EqualFold(c.host, DefaultHost) {
		variables = publicHostVariables
	} else if !strings.EqualFold(c.getenv("GH_HOST"), c.host) {
		variables = nil
	}
	for _, name := range variables {
		if value := strings.TrimSpace(c.getenv(name)); value != "" {
			return c.accept(value, name)
		}
	}
	if c.command == nil {
		return "", &Error{Kind: NoCredential, Message: "no GitHub credential for " + c.host + ": set GH_TOKEN or run gh auth login"}
	}
	value, err := c.command(ctx, c.host)
	value = strings.TrimSpace(value)
	switch {
	case errors.Is(err, errNoGH):
		return "", &Error{Kind: NoCredential, Message: "no GitHub credential for " + c.host + ": set GH_TOKEN, or install gh and run gh auth login"}
	case err != nil:
		return "", &Error{Kind: NoCredential, Message: err.Error()}
	case value == "":
		return "", &Error{Kind: NoCredential, Message: "gh stores no token for " + c.host + "; run gh auth login --hostname " + c.host}
	}
	return c.accept(value, "gh auth token")
}

func (c *Credential) accept(value, origin string) (string, *Error) {
	if !tokenShape.MatchString(value) {
		return "", &Error{Kind: NoCredential, Message: "the credential from " + origin + " is not a single token of letters, digits, '_', '.' and '-'"}
	}
	return value, nil
}

// Secrets lists the resolved token, for redaction. It is empty until Token
// resolved one.
func (c *Credential) Secrets() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		return nil
	}
	return []string{c.token}
}

// knownTokenPattern matches the published GitHub token formats, which are
// redacted wherever they appear, whoever's they are.
var knownTokenPattern = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,255}|github_pat_[A-Za-z0-9_]{20,255})\b`)

// RedactedMarker replaces every redacted credential.
const RedactedMarker = "[redacted]"

// Redact removes secrets and every string shaped like a GitHub token from
// text.
func Redact(text string, secrets []string) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, RedactedMarker)
		}
	}
	return knownTokenPattern.ReplaceAllString(text, RedactedMarker)
}
