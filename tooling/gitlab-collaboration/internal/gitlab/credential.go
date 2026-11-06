package gitlab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/sdk/extension/collabhelper"
)

// Variables the credential resolver reads.
const (
	// TokenVariable holds a token for the host HostVariable names, GitLab.com
	// when it is unset: the variable glab reads.
	TokenVariable = "GITLAB_TOKEN"
	// HostVariable names the instance TokenVariable belongs to, as a host or
	// an https URL.
	HostVariable = "GITLAB_HOST"
	// CIVariable is "true" inside a GitLab CI/CD job.
	CIVariable = "GITLAB_CI"
	// JobKeyVariable is the token of the running CI/CD job.
	JobKeyVariable = "CI_JOB_TOKEN"
	// CIHostVariable and CIFQDNVariable name the instance running the job:
	// its host, and its host with a non-default port.
	CIHostVariable = "CI_SERVER_HOST"
	CIFQDNVariable = "CI_SERVER_FQDN"
)

// Scheme is how a token is sent.
type Scheme int

// Token schemes.
const (
	// Bearer sends Authorization: Bearer, which GitLab accepts for personal,
	// project and group access tokens and OAuth tokens.
	Bearer Scheme = iota + 1
	// JobToken sends JOB-TOKEN, the only header a CI/CD job token is read
	// from.
	JobToken
)

// Token is one resolved credential.
type Token struct {
	Value  string
	Scheme Scheme
	// Origin names where the token came from, never the token itself.
	Origin string
}

// TokenCommand asks an installed glab CLI for the token it stores for a host.
type TokenCommand func(ctx context.Context, host string) (string, error)

// errNoGlab reports that no glab CLI is installed.
var errNoGlab = errors.New("the glab CLI is not installed")

// glabTokenVariables are the variables glab reads a token from before its
// configuration. They are removed from the command's environment, so glab
// answers with the token it stores for the host asked, never one an
// environment variable holds for another instance.
var glabTokenVariables = []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "GITLAB_HOST", "GL_HOST"}

// GlabConfigToken runs `glab config get token --host <host>`, which prints
// the token glab auth login stored for the host, or nothing. Its standard
// error is discarded, never reported: nothing the process writes may reach a
// caller unexamined.
func GlabConfigToken(ctx context.Context, host string) (string, error) {
	path, err := exec.LookPath("glab")
	if err != nil {
		return "", errNoGlab
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "config", "get", "token", "--host", host)
	command.Env = withoutVariables(os.Environ(), glabTokenVariables)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", errors.New("glab config get token --host " + host + " failed")
	}
	return strings.TrimSpace(stdout.String()), nil
}

func withoutVariables(environment, names []string) []string {
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		drop := false
		for _, removed := range names {
			if name == removed {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, entry)
		}
	}
	return kept
}

// tokenShape is what a credential may look like: letters, digits, '_', '.'
// and '-'. It can never split or inject a header, and JSON never escapes it,
// so redaction finds it verbatim in anything the provider writes.
var tokenShape = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,1000}$`)

// Credential resolves the token of one instance once. The token is never part
// of an error, a message or a result: Secrets lists it for redaction.
type Credential struct {
	host    string
	getenv  func(string) string
	command TokenCommand

	mu       sync.Mutex
	resolved bool
	token    Token
	err      *Error
}

// NewCredential returns the credential resolver of an instance. getenv reads
// the process environment; command runs glab when no variable names a token.
func NewCredential(host string, getenv func(string) string, command TokenCommand) *Credential {
	return &Credential{host: host, getenv: getenv, command: command}
}

// Token returns the instance's token: GITLAB_TOKEN when GITLAB_HOST names the
// instance (GitLab.com when it is unset), then the token glab stores for it,
// then, inside a CI/CD job the instance runs, CI_JOB_TOKEN.
func (c *Credential) Token(ctx context.Context) (Token, *Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved {
		c.token, c.err = c.resolve(ctx)
		c.resolved = true
	}
	return c.token, c.err
}

func (c *Credential) resolve(ctx context.Context) (Token, *Error) {
	if value := strings.TrimSpace(c.getenv(TokenVariable)); value != "" && SameHost(hostOf(c.getenv(HostVariable)), c.host) {
		return c.accept(value, TokenVariable, Bearer)
	}
	tried := TokenVariable + " for " + c.host
	if c.command != nil {
		value, err := c.command(ctx, c.host)
		value = strings.TrimSpace(value)
		switch {
		case err == nil && value != "":
			return c.accept(value, "glab config get token", Bearer)
		case errors.Is(err, errNoGlab):
			tried += ", glab (not installed)"
		case err != nil:
			tried += ", glab (" + err.Error() + ")"
		default:
			tried += ", glab (no token stored; run glab auth login --hostname " + c.host + ")"
		}
	}
	if c.getenv(CIVariable) == "true" {
		ciHost := c.getenv(CIHostVariable)
		if SameHost(ciHost, c.host) || SameHost(c.getenv(CIFQDNVariable), c.host) {
			if value := strings.TrimSpace(c.getenv(JobKeyVariable)); value != "" {
				return c.accept(value, JobKeyVariable, JobToken)
			}
			tried += ", " + JobKeyVariable + " (empty)"
		} else {
			tried += ", " + JobKeyVariable + " (the job runs on another instance)"
		}
	}
	return Token{}, &Error{Kind: NoCredential, Message: "no GitLab credential for " + c.host + "; tried " + tried + ": set " + TokenVariable + " or run glab auth login"}
}

func (c *Credential) accept(value, origin string, scheme Scheme) (Token, *Error) {
	if !tokenShape.MatchString(value) {
		return Token{}, &Error{Kind: NoCredential, Message: "the credential from " + origin + " is not a single token of letters, digits, '_', '.' and '-'"}
	}
	return Token{Value: value, Scheme: scheme, Origin: origin}, nil
}

// hostOf reads the host GITLAB_HOST names, as a bare host or a URL; it is
// GitLab.com when the variable is unset.
func hostOf(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultHost
	}
	if _, rest, found := strings.Cut(value, "://"); found {
		value = rest
	}
	host, _, _ := strings.Cut(value, "/")
	return host
}

// SameHost compares two host names, ignoring case.
func SameHost(a, b string) bool {
	return a != "" && strings.EqualFold(a, b)
}

// origin names where the resolved token came from, empty before one was.
func (c *Credential) origin() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token.Origin
}

// Secrets lists the resolved token, for redaction. It is empty until Token
// resolved one.
func (c *Credential) Secrets() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token.Value == "" {
		return nil
	}
	return []string{c.token.Value}
}

// knownTokenPattern matches the published GitLab token prefixes, which are
// redacted wherever they appear, whoever's they are.
var knownTokenPattern = regexp.MustCompile(`\bgl(?:pat|oas|dt|rt|rtr|cbt|ptt|ft|imt|agent|soat|ffct|wt|sst)-[A-Za-z0-9_.-]{16,255}`)

// Redact removes secrets and every string shaped like a GitLab token from
// text.
func Redact(text string, secrets []string) string {
	return collabhelper.Redact(text, secrets, knownTokenPattern)
}
