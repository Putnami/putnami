// Package host gives the change commands their calls into the putnami process
// that launched the executable: the language checker's collaboration call and
// the verifier's restored verification reports.
//
// The executable never starts putnami again and never reads provider
// configuration. putnami opens a host RPC channel for an interactive
// command-group subcommand it launches, and names it in the environment. A
// run with no channel, such as a hosted run, cannot reach a provider or the
// task cache.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/hostrpc"

	"go.putnami.dev/tooling/contributor/internal/language"
)

// Caller sends collaboration calls over one host RPC channel. It dials on the
// first call, so a command that never calls a provider never dials.
type Caller struct {
	dial func() (*hostrpc.Client, error)

	mu     sync.Mutex
	client *hostrpc.Client
}

// NewCaller returns a Caller that dials the channel the environment names.
func NewCaller() *Caller {
	return &Caller{dial: hostrpc.Dial}
}

// Call is a language.CallProvider. A missing channel and a failed call are
// errors; an operation that fails at the provider is an envelope.
func (c *Caller) Call(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error) {
	client, err := c.connect()
	if err != nil {
		return collab.Envelope{}, err
	}
	raw, err := client.CallCollaboration(ctx, contract, operation, input)
	if err != nil {
		return collab.Envelope{}, fmt.Errorf("%s %s through the putnami host: %w", contract, operation, err)
	}
	var envelope collab.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return collab.Envelope{}, fmt.Errorf("%s %s answered a document that is not an envelope: %w", contract, operation, err)
	}
	return envelope, nil
}

// Close closes the channel when a call opened it.
func (c *Caller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return nil
	}
	client := c.client
	c.client = nil
	return client.Close()
}

func (c *Caller) connect() (*hostrpc.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	client, err := c.dial()
	if err != nil {
		var noHost *hostrpc.NoHostError
		if errors.As(err, &noHost) {
			return nil, fmt.Errorf("the tasks mode needs an interactive launch by putnami, which opens the host channel it reads the tasks provider through; run `putnami change language tasks` (a hosted run opens no channel; %s not set)",
				strings.Join(noHost.Missing, " and "))
		}
		return nil, err
	}
	c.client = client
	return client, nil
}

var _ language.CallProvider = (*Caller)(nil).Call
