package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	hostrpcproto "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hostrpc"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/mcp"
)

// The host RPC channel of an interactive extension subcommand: the calls the
// launched process makes into this process instead of running putnami again
// (go.putnami.dev/protocol/extension/hostrpc). One channel serves one launch,
// and answers from what that launch's dispatch already resolved.

// hostRPCBinding is what the methods of one launch's channel answer from: the
// workspace the command runs in, its configuration, and the extensions the
// dispatch discovered. A launch outside any workspace has an empty root.
type hostRPCBinding struct {
	root       string
	config     *wsproto.Config
	extensions []*extension.ExtensionDescription
}

// startHostRPC opens the channel of one launch. It returns the variables the
// launched process receives and the function that closes the channel, which
// the caller runs once the process exits. When the channel cannot open, the
// process receives no variable and runs anyway; a warning says why, except in
// a hosted run, which never opens one.
func startHostRPC(ctx context.Context, binding hostRPCBinding, stderr io.Writer) ([]string, func()) {
	server, err := hostrpc.Start(ctx, hostrpc.Methods{
		Collaboration:      binding.collaborationCall,
		VerificationReport: binding.verificationReport,
	})
	if err != nil {
		if !errors.Is(err, hostrpc.ErrHosted) {
			iox.Fprintf(stderr, "putnami: warning: the host RPC channel did not open, so this command cannot call putnami: %v\n", err)
		}
		return nil, func() {}
	}
	return server.Env(), func() {
		if err := server.Stop(); err != nil {
			iox.Fprintf(stderr, "putnami: warning: close the host RPC channel: %v\n", err)
		}
	}
}

// dropInheritedHostRPC removes the channel variables this process inherited
// from the extension command that started it.
func dropInheritedHostRPC() {
	_ = os.Unsetenv(hostrpcproto.SocketEnv)
	_ = os.Unsetenv(hostrpcproto.TokenEnv)
}

// collaborationCall answers collaboration.call through the route
// `putnami <contract> <operation>` takes (collaborationEnvelope), and returns
// the envelope that command prints under --output=json. Where that command
// prints no envelope, the call is refused with the reason.
func (b hostRPCBinding) collaborationCall(ctx context.Context, call hostrpcproto.CollaborationCallParams) (json.RawMessage, error) {
	switch {
	case b.root == "":
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this command runs outside any workspace, so no collaboration contract is bound"}
	case !mcp.CollaborationDeclared(b.root):
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this workspace declares no options.collaboration in putnami.workspace.json, so no collaboration contract is bound"}
	case call.Contract == collab.ContractProposals && call.Operation == "finalize":
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeInvalidParams,
			Message: "proposals finalize is a command, not a contract operation"}
	}
	if owners := collaborationCommandOwners(b.extensions, call.Contract); len(owners) > 0 {
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: fmt.Sprintf("%s is a collaboration contract of this workspace and a command of %s", call.Contract, strings.Join(owners, ", "))}
	}
	envelope := collaborationEnvelope(ctx, b.root, b.config, b.extensions, call.Contract, call.Operation, call.Input)
	text, err := mcp.EnvelopeText(envelope, false)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(text), nil
}

// verificationReport answers verification.report with what
// engine.VerifyCachedVerificationReport returns for the launch's workspace.
func (b hostRPCBinding) verificationReport(_ context.Context, ref hostrpcproto.VerificationReportParams) ([]byte, error) {
	if b.root == "" {
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this command runs outside any workspace, so there is no cache to read a verification report from"}
	}
	report, err := engine.VerifyCachedVerificationReport(b.root, features.VerificationReportRef{
		Project: ref.Project,
		Task:    ref.Task,
		Path:    ref.Path,
		Digest:  ref.Digest,
	})
	if err != nil {
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeNotVerified, Message: err.Error()}
	}
	return report, nil
}
