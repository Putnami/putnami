// Package agencycli serves the `putnami agency` commands of @putnami/cloud:
// the person's routine trigger and build routine, the organization's model
// key and the shared audit attestations of the linked workspace. Every
// command calls Putnami Cloud through a generated client under the stored
// putnami cloud login, re-minted once when a service answers 401. A secret a
// command takes is read from standard input only and never printed.
package agencycli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"

	"go.putnami.dev/client"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// secretInput is where a command reads the secret it takes. Tests replace it.
var secretInput io.Reader = os.Stdin

// Agency dispatches `putnami agency <command>`.
func Agency(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	command := clicore.FirstPositional(args)
	rest := clicore.DropFirstPositional(args)
	switch command {
	case "trigger":
		return Trigger(params, rest, workspaceRoot, env, ioctx, secretInput)
	case "model-key":
		return ModelKey(params, rest, workspaceRoot, env, ioctx, secretInput)
	case "build":
		return Build(params, workspaceRoot, env, ioctx)
	case "audit-attest":
		return AuditAttest(params, rest, workspaceRoot, env, ioctx, secretInput)
	case "", "help":
		return help(params, ioctx)
	default:
		return clicore.NewError("unknown agency command: "+command+"; expected trigger, build, model-key or audit-attest", clicore.ExitUsage)
	}
}

func help(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "agency trigger register|show|remove", "description": "your routine's API trigger in the workspace; the token is read from standard input"},
		{"command": "agency build --issue <n> [--repository owner/name]", "description": "start your build routine on one issue"},
		{"command": "agency model-key set|status|revoke", "description": "the organization's Anthropic API key and its budget per run; the key is read from standard input"},
		{"command": "agency audit-attest get|set|prefetch", "description": "the workspace's shared audit attestations"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud agency commands:")
	for _, command := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-52s %s", command["command"], command["description"]))
	}
	return nil
}

// refusal says what a service's refusal at status means for one command, or
// nil to keep the generic sentence.
type refusal func(status int) error

// session is the linked workspace a command calls, under its stored login.
type session struct {
	workspace *clicore.WorkspaceContext
	service   string
	refuse    refusal
}

// openSession resolves the linked workspace, or --workspace, and mints its
// workspace-scoped bearer. Nothing is sent to the service yet.
func openSession(params map[string]any, root string, env map[string]string, ioctx clicore.IO, service string, refuse refusal) (*session, error) {
	workspace, err := clicore.ResolveWorkspaceContext(params, root, env, ioctx)
	if err != nil {
		return nil, err
	}
	return &session{workspace: workspace, service: service, refuse: refuse}, nil
}

func (s *session) context() context.Context {
	if s.workspace.IO.Context != nil {
		return s.workspace.IO.Context
	}
	return context.Background()
}

// call sends one generated call under the session's bearer. A refusal whose
// status is in accepted answers that status and no error; any other refusal
// is the command's sentence for it, or the service's status line.
func call[T any](s *session, send func(context.Context) (*T, error), accepted ...int) (*T, int, error) {
	ctx := s.context()
	answer, err := clicore.CallWithSession(ctx, s.workspace, send)
	if err == nil {
		return answer, 0, nil
	}
	status := clicore.ServiceStatus(err)
	if status != 0 && slices.Contains(accepted, status) {
		return nil, status, nil
	}
	if status != 0 && s.refuse != nil {
		if refused := s.refuse(status); refused != nil {
			return nil, status, refused
		}
	}
	if status == http.StatusUnauthorized {
		return nil, status, clicore.NewError(s.service+" rejected the credential; run `putnami cloud login`", clicore.ExitAuth)
	}
	return nil, status, clicore.ServiceCallError(ctx, err, clicore.ServiceCallOptions{
		Target: s.workspace.ControlPlane, Prefix: s.service + " refused the request: ",
	})
}

// errNotSentAgain is the answer of a sendOnce transport to a repeat.
var errNotSentAgain = errors.New("a request that carries a secret is sent once and not again")

// sendOnce lets one request through and refuses every later one. A command
// that sends a secret binds its client to it: the generated client retries an
// idempotent call on 429 and 5xx, and the secret must leave the machine once.
// status is the HTTP status the one request was answered with.
type sendOnce struct {
	base   http.RoundTripper
	mu     sync.Mutex
	sent   bool
	status int
}

func (o *sendOnce) RoundTrip(request *http.Request) (*http.Response, error) {
	o.mu.Lock()
	if o.sent {
		o.mu.Unlock()
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, errNotSentAgain
	}
	o.sent = true
	o.mu.Unlock()
	response, err := o.base.RoundTrip(request)
	if response != nil {
		o.mu.Lock()
		o.status = response.StatusCode
		o.mu.Unlock()
	}
	return response, err
}

// answered is the status of the one request, or 0 when none was answered.
func (o *sendOnce) answered() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status
}

// onceBinding is the session's binding with a transport that sends one
// request only.
func (s *session) onceBinding() (client.ServiceBinding, *sendOnce) {
	binding := s.workspace.ServiceBinding()
	httpClient := http.DefaultClient
	if binding.HTTPClient != nil {
		httpClient = binding.HTTPClient
	}
	copied := *httpClient
	base := copied.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	once := &sendOnce{base: base}
	copied.Transport = once
	binding.HTTPClient = &copied
	return binding, once
}

// callOnce is call for a request that carries a secret: it is sent once,
// with no re-mint and no retry. A refusal the client would have retried is
// read from the status of the one answer.
func callOnce[T any](s *session, once *sendOnce, what string, send func(context.Context) (*T, error)) (*T, error) {
	answer, err := clicore.CallWithSession(s.context(), &clicore.WorkspaceContext{
		WorkspaceID: s.workspace.WorkspaceID, ControlPlane: s.workspace.ControlPlane, AuthToken: s.workspace.AuthToken, IO: s.workspace.IO,
	}, send)
	if err == nil {
		return answer, nil
	}
	status := clicore.ServiceStatus(err)
	if status == 0 {
		status = once.answered()
	}
	if status != 0 && s.refuse != nil {
		if refused := s.refuse(status); refused != nil {
			return nil, refused
		}
	}
	switch {
	case status == http.StatusUnauthorized:
		return nil, clicore.NewError(s.service+" rejected the credential; run `putnami cloud login`", clicore.ExitAuth)
	case status != 0:
		return nil, clicore.NewError(fmt.Sprintf("%s refused the request (HTTP %d); the %s was sent once and not again: run the command again", s.service, status, what), clicore.ExitAPI)
	}
	return nil, clicore.NewError(fmt.Sprintf("%s did not answer: %v; the %s was sent at most once: run the command again", s.service, err, what), clicore.ExitAPI)
}

// readSecret reads one line of printable ASCII from input, at most limit
// bytes, with no space. The caller clears the bytes once they are sent; the
// copies the request encoding makes are left to the garbage collector.
func readSecret(input io.Reader, minimum, limit int) ([]byte, bool) {
	if input == nil {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(input, int64(limit)+2))
	secret := trimLineEnd(raw)
	if err != nil || len(secret) < minimum || len(secret) > limit {
		clear(raw)
		return nil, false
	}
	for _, b := range secret {
		if b <= ' ' || b > '~' {
			clear(raw)
			return nil, false
		}
	}
	return secret, true
}

func trimLineEnd(raw []byte) []byte {
	for len(raw) > 0 && (raw[len(raw)-1] == '\n' || raw[len(raw)-1] == '\r') {
		raw = raw[:len(raw)-1]
	}
	return raw
}

// refuseSecretFlags refuses any flag that would carry a secret on the command
// line, before anything is read or sent.
func refuseSecretFlags(params map[string]any, names []string, message string) error {
	for _, name := range names {
		if _, present := params[name]; present {
			return clicore.NewError(message, clicore.ExitUsage)
		}
	}
	return nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefInt(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
