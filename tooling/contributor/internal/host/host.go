// Package host gives the change commands their calls into the putnami process
// that launched the executable: collaboration calls, the owners of changed
// paths, and restored verification reports.
//
// The executable never starts putnami again and never reads provider
// configuration. putnami opens a host RPC channel for an interactive
// command-group subcommand it launches, and names it in the environment. A
// run with no channel, such as a hosted run, cannot reach a provider, the
// workspace graph or the task cache.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	collab "go.putnami.dev/protocol/collaboration"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/sdk/extension/hostrpc"

	"go.putnami.dev/tooling/contributor/internal/language"
)

// Channel is the one host RPC channel of a run. Take reads it from the
// environment before the command starts any process. The first call dials
// it, every later call shares that connection, and a command that never
// calls the host never dials.
type Channel struct {
	socket, token string
	// remedy says how to launch the command with a channel. A call that
	// needs the channel and finds none reports it.
	remedy  string
	connect func(socket, token string) (*hostrpc.Client, error)

	mu     sync.Mutex
	client *hostrpc.Client
}

// Take returns the Channel the environment names, and removes the channel
// from the environment, so no process the command starts inherits the
// channel or its token. remedy says how to launch the command with a
// channel.
func Take(remedy string) *Channel {
	channel := &Channel{socket: os.Getenv(wire.SocketEnv), token: os.Getenv(wire.TokenEnv), remedy: remedy, connect: hostrpc.Connect}
	_ = os.Unsetenv(wire.SocketEnv)
	_ = os.Unsetenv(wire.TokenEnv)
	return channel
}

// Call is a language.CallProvider. A missing channel and a failed call are
// errors; an operation that fails at the provider is an envelope.
func (c *Channel) Call(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error) {
	client, err := c.dial()
	if err != nil {
		return collab.Envelope{}, c.launchError(err)
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
func (c *Channel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return nil
	}
	client := c.client
	c.client = nil
	return client.Close()
}

// dial returns the shared connection, and connects on the first call. A
// missing channel is a *hostrpc.NoHostError, and is never dialed.
func (c *Channel) dial() (*hostrpc.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	var missing []string
	if c.socket == "" {
		missing = append(missing, wire.SocketEnv)
	}
	if c.token == "" {
		missing = append(missing, wire.TokenEnv)
	}
	if len(missing) > 0 {
		return nil, &hostrpc.NoHostError{Missing: missing}
	}
	client, err := c.connect(c.socket, c.token)
	if err != nil {
		return nil, err
	}
	c.client = client
	return client, nil
}

// launchError turns a missing channel into the remedy and the missing
// variables. A channel without a remedy names only the missing variables.
// Any other error is returned unchanged.
func (c *Channel) launchError(err error) error {
	var noHost *hostrpc.NoHostError
	if !errors.As(err, &noHost) {
		return err
	}
	missing := fmt.Sprintf("a hosted run opens no channel; %s not set", strings.Join(noHost.Missing, " and "))
	if c.remedy == "" {
		return errors.New(missing)
	}
	return fmt.Errorf("%s (%s)", c.remedy, missing)
}

var _ language.CallProvider = (*Channel)(nil).Call
