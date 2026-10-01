package credentialprovider

import (
	"context"
	"fmt"

	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// ReadCredential returns the broker's read credential, whatever hosts it
// names. A nil credential with a nil error means the broker does not serve
// the read purpose or the provider holds no read credential. An error is a
// refusal (*RefusalError) or a provider failure.
func (b *Broker) ReadCredential(ctx context.Context) (*registry.Credential, error) {
	if !b.Enabled(registry.PurposeRead) {
		return nil, nil
	}
	credential, err := b.credential(ctx, registry.PurposeRead)
	if err != nil && b.source != "" {
		err = fmt.Errorf("%s: %w", b.source, err)
	}
	return credential, err
}

// InstallJobRead makes the broker's read credential the one a hosted run hands
// its fetch job (jobs.InstallJobReadCredential), and returns the function that
// restores the previous source. A broker that does not serve the read purpose
// installs nothing: the job then receives absence.
func (b *Broker) InstallJobRead() (restore func()) {
	if !b.Enabled(registry.PurposeRead) {
		return func() {}
	}
	return jobs.InstallJobReadCredential(b.ReadCredential)
}
