package host

import (
	"context"
	"fmt"
	"os"
	"sync"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/sdk/extension/hostrpc"

	"go.putnami.dev/tooling/contributor/internal/verify"
)

// Reports reads restored verification reports from the task cache of the
// putnami process that launched the executable, through verification.report.
// It dials on the first read, so a verification that restores no report never
// dials.
type Reports struct {
	socket, token string
	connect       func(socket, token string) (*hostrpc.Client, error)

	mu     sync.Mutex
	client *hostrpc.Client
}

// TakeReports returns a Reports for the channel the environment names, and
// removes the channel from the environment, so no process the command starts
// inherits the channel or its token.
func TakeReports() *Reports {
	reports := &Reports{socket: os.Getenv(wire.SocketEnv), token: os.Getenv(wire.TokenEnv), connect: hostrpc.Connect}
	_ = os.Unsetenv(wire.SocketEnv)
	_ = os.Unsetenv(wire.TokenEnv)
	return reports
}

// Recover is a verify.Verifier RecoverReport. The host reads the report from
// the cache of the workspace putnami launched the command in, so root is not
// sent. A missing channel is a *verify.NoChannelError; a report the cache does
// not corroborate, and a failed call, are other errors.
func (r *Reports) Recover(_ string, report features.VerificationReportRef) ([]byte, error) {
	client, err := r.dial()
	if err != nil {
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

// Close closes the channel when a read opened it.
func (r *Reports) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == nil {
		return nil
	}
	client := r.client
	r.client = nil
	return client.Close()
}

func (r *Reports) dial() (*hostrpc.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		return r.client, nil
	}
	var missing []string
	if r.socket == "" {
		missing = append(missing, wire.SocketEnv)
	}
	if r.token == "" {
		missing = append(missing, wire.TokenEnv)
	}
	if len(missing) > 0 {
		return nil, &verify.NoChannelError{Missing: missing}
	}
	client, err := r.connect(r.socket, r.token)
	if err != nil {
		return nil, err
	}
	r.client = client
	return client, nil
}

var _ = verify.Verifier{RecoverReport: (*Reports)(nil).Recover}
