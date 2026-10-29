// Package collaboration routes the collaboration contracts a workspace binds
// in options.collaboration to the provider extension each binding names.
//
// Two entry paths reach a provider: the `putnami <contract> <operation>`
// command and the extension host RPC method collaboration.call. Both route
// through CallProviderOperation, which resolves the binding again on every
// call, so a call always reaches the provider the workspace document names at
// that moment and never another one. A workspace without the block routes
// nothing and reserves no name.
package collaboration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// Declared reports whether the workspace document declares an
// options.collaboration block — the condition for every collaboration command
// and reserved name.
func Declared(workspaceRoot string) bool {
	document, err := readCollabDocument(workspaceRoot)
	return err == nil && document.Present
}

// Capabilities resolves one contract's binding against the extensions an
// entry path discovered and renders its capabilities document.
func Capabilities(
	workspaceRoot, contract string, extensions []*extension.ExtensionDescription, unavailable map[string]bool,
) collab.Capabilities {
	document, _ := readCollabDocument(workspaceRoot)
	return capabilitiesOf(resolveCollab(document, contract, extensions, unavailable))
}

// ProviderEnvironment is what one entry path knows when it routes a
// collaboration operation: the extensions it discovered, the ones that must
// not serve, and how it attaches the workspace view to an operation that
// declares workspaceSelection.
type ProviderEnvironment struct {
	WorkspaceRoot string
	Extensions    []*extension.ExtensionDescription
	Unavailable   map[string]bool
	AttachView    func(ctx context.Context, request *collab.ProviderRequest, args json.RawMessage) error
	// Environ is the environment the provider inherits, whose credentials are
	// redacted from the envelope. Nil means the process environment.
	Environ []string
}

// CallProviderOperation routes one collaboration operation call and answers
// with its envelope. It is the single routing path of every entry path, so
// they cannot disagree about which provider serves a contract, which requests
// reach it, or how its answer is judged.
//
// It never retries. A mutation whose provider process started and did not
// deliver a valid answer is unresolved, and the envelope names the read that
// reconciles it.
func CallProviderOperation(ctx context.Context, env ProviderEnvironment, contract, operation string, arguments json.RawMessage) collab.Envelope {
	environ := env.Environ
	if environ == nil {
		environ = os.Environ()
	}
	redactor := newRedactor(environ)
	if !collab.IsContract(contract) {
		return redactor.redactEnvelope(collab.Envelope{
			Contract: contract, Operation: operation, Outcome: collab.OutcomeInvalid,
			Error: &collab.Error{Message: fmt.Sprintf("unknown contract %q (contracts: %s)", contract, strings.Join(collab.ContractNames, ", ")),
				Reason: collab.ReasonRequestInvalid},
		}, false)
	}
	document, err := readCollabDocument(env.WorkspaceRoot)
	if err != nil {
		return redactor.redactEnvelope(collab.Envelope{
			Contract: contract, Operation: operation, Outcome: collab.OutcomeUnavailable,
			Error: &collab.Error{Message: err.Error(), Reason: collab.ReasonBindingInvalid},
		}, false)
	}
	resolution := resolveCollab(document, contract, env.Extensions, env.Unavailable)
	call, answer := prepareCall(document, resolution, operation, arguments)
	if answer != nil {
		return redactor.redactEnvelope(*answer, false)
	}
	mutating := call.Operation.Access == collab.AccessMutating
	request := collab.ProviderRequest{
		Arguments:     call.Arguments,
		WorkspaceRoot: env.WorkspaceRoot,
		ExtensionRoot: resolution.Provider.Path,
		Provider:      call.providerMember(),
	}
	if call.Provided.Operation.WorkspaceSelection {
		if env.AttachView == nil {
			return redactor.redactEnvelope(transportFailure(call, stagePrepare,
				"this entry path cannot resolve the workspace selection the provider operation declares"), mutating)
		}
		if err := env.AttachView(ctx, &request, call.Arguments); err != nil {
			envelope := collab.Envelope{
				Contract: contract, Version: resolution.Binding.Version, Operation: operation,
				Provider: &collab.ProviderIdentity{Name: resolution.Provider.Name, Version: resolution.Provider.Version},
				Outcome:  collab.OutcomeInvalid,
				Error:    &collab.Error{Message: err.Error(), Reason: collab.ReasonRequestInvalid},
			}
			return redactor.redactEnvelope(envelope, mutating)
		}
	}
	candidate := providerCandidate{ext: resolution.Provider, contract: contract, declaration: call.Provided.Provider}
	stdout, err := invokeProvider(ctx, env.WorkspaceRoot, candidate, request)
	if err != nil {
		stage, message := collaborationStage(err)
		return redactor.redactEnvelope(transportFailure(call, stage, message), mutating)
	}
	return redactor.redactEnvelope(interpretResult(call, stdout), mutating)
}

// collaborationStage reads how far a failed invocation got, and describes it
// without the provider's stderr, which never reaches an envelope.
func collaborationStage(err error) (invocationStage, string) {
	var failure *providerError
	if !errors.As(err, &failure) {
		return stagePrepare, err.Error()
	}
	return failure.stage, failure.summary()
}

// EnvelopeText encodes an envelope for an entry path: indented for a person,
// compact for a machine stream.
func EnvelopeText(envelope collab.Envelope, indent bool) (string, error) {
	var (
		encoded []byte
		err     error
	)
	if indent {
		encoded, err = json.MarshalIndent(envelope, "", "  ")
	} else {
		encoded, err = json.Marshal(envelope)
	}
	if err != nil {
		return "", fmt.Errorf("encode the %s.%s envelope: %w", envelope.Contract, envelope.Operation, err)
	}
	return string(encoded), nil
}
