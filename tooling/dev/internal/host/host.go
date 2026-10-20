// Package host gives the version commands their one call into the putnami
// process that launched the executable: the workspace's version lines.
//
// The executable computes no version and never starts putnami again. putnami
// opens a host RPC channel for an interactive command-group subcommand it
// launches, and names it in the environment. A run with no channel, such as a
// hosted run, cannot read a version line.
package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/sdk/extension/hostrpc"
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

// VersionLines returns the version lines the host computes: every line, or
// the one line scope names. A missing channel and a failed call are errors;
// a refusal of the host is a *wire.Error.
func (c *Channel) VersionLines(ctx context.Context, scope string) (wire.VersionLinesResult, error) {
	client, err := c.dial()
	if err != nil {
		return wire.VersionLinesResult{}, c.launchError(err)
	}
	return client.VersionLines(ctx, scope)
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
