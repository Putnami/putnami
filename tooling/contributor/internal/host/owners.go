package host

import (
	"context"
	"fmt"

	wire "go.putnami.dev/protocol/extension/hostrpc"
)

// Owners asks the host which workspace projects own each path, through
// workspace.owners: one entry for each path, in the order given. A missing
// channel and a failed call are errors.
func (c *Channel) Owners(ctx context.Context, paths []string) ([]wire.PathOwners, error) {
	client, err := c.dial()
	if err != nil {
		return nil, c.launchError(err)
	}
	owners, err := client.WorkspaceOwners(ctx, paths)
	if err != nil {
		return nil, fmt.Errorf("workspace.owners through the putnami host: %w", err)
	}
	return owners, nil
}
