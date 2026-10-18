package host

import (
	"context"
	"errors"
	"fmt"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/sdk/extension/hostrpc"

	"go.putnami.dev/tooling/contributor/internal/verify"
)

// Recover is a verify.Verifier RecoverReport: it reads a restored
// verification report from the task cache of the putnami process that
// launched the executable, through verification.report. The host reads the
// report from the cache of the workspace putnami launched the command in, so
// root is not sent. A missing channel is a *verify.NoChannelError; a report
// the cache does not corroborate, and a failed call, are other errors.
func (c *Channel) Recover(_ string, report features.VerificationReportRef) ([]byte, error) {
	client, err := c.dial()
	if err != nil {
		var noHost *hostrpc.NoHostError
		if errors.As(err, &noHost) {
			return nil, &verify.NoChannelError{Missing: noHost.Missing}
		}
		return nil, err
	}
	data, err := client.VerificationReport(context.Background(), wire.VerificationReportParams{
		Project: report.Project,
		Task:    report.Task,
		Path:    report.Path,
		Digest:  report.Digest,
	})
	if err != nil {
		return nil, fmt.Errorf("verification.report through the putnami host: %w", err)
	}
	return data, nil
}

var _ = verify.Verifier{RecoverReport: (*Channel)(nil).Recover}
