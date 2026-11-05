package linear

import (
	"regexp"
	"strings"
	"sync"

	"go.putnami.dev/sdk/extension/collabhelper"
)

// KeyVariable names the only place the API key is read from.
const KeyVariable = "LINEAR_API_KEY"

// keyShape is what a key may look like: letters, digits, '_', '.' and '-'. It
// can never split or inject a header, and JSON never escapes it, so redaction
// finds it verbatim in anything the provider writes.
var keyShape = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,1000}$`)

// Credential resolves the API key once. The key is never part of an error, a
// message or a result: Secrets lists it for redaction.
type Credential struct {
	getenv func(string) string

	mu       sync.Mutex
	resolved bool
	key      string
	err      *Error
}

// NewCredential returns the resolver of the key getenv reads.
func NewCredential(getenv func(string) string) *Credential {
	return &Credential{getenv: getenv}
}

// Key returns the value of LINEAR_API_KEY.
func (c *Credential) Key() (string, *Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved {
		c.key, c.err = c.resolve()
		c.resolved = true
	}
	return c.key, c.err
}

func (c *Credential) resolve() (string, *Error) {
	value := strings.TrimSpace(c.getenv(KeyVariable))
	if value == "" {
		return "", &Error{Kind: NoCredential, Message: "no Linear credential: set " + KeyVariable + " to a Linear API key"}
	}
	if !keyShape.MatchString(value) {
		return "", &Error{Kind: NoCredential, Message: KeyVariable + " is not a single key of letters, digits, '_', '.' and '-'"}
	}
	return value, nil
}

// authorization is the Authorization header value of a key: an OAuth access
// token is a bearer token, a personal API key is sent as it is.
func authorization(key string) string {
	if strings.HasPrefix(key, "lin_oauth_") {
		return "Bearer " + key
	}
	return key
}

// Secrets lists the resolved key, for redaction. It is empty until Key
// resolved one.
func (c *Credential) Secrets() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == "" {
		return nil
	}
	return []string{c.key}
}

// knownKeyPattern matches the published Linear key formats, which are
// redacted wherever they appear, whoever's they are.
var knownKeyPattern = regexp.MustCompile(`\blin_(?:api|oauth)_[A-Za-z0-9]{16,255}\b`)

// Redact removes secrets and every string shaped like a Linear key from text.
func Redact(text string, secrets []string) string {
	return collabhelper.Redact(text, secrets, knownKeyPattern)
}
