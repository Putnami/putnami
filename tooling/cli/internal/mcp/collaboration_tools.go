package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// The collaboration contracts on the MCP surface.
//
// A workspace that binds contracts in options.collaboration gets, for every
// contract, a read-only `<contract>.capabilities` tool answered by the
// orchestrator, and one `<contract>.<operation>` tool per operation the
// binding serves. Every routed tool resolves the binding again when it is
// called, so a call always reaches the provider the workspace document names
// at that moment and never another one. A workspace without the block gets
// none of this and reserves no name.

// CollaborationDeclared reports whether the workspace document declares an
// options.collaboration block — the condition for every collaboration command,
// tool and reserved name.
func CollaborationDeclared(workspaceRoot string) bool {
	document, err := readCollabDocument(workspaceRoot)
	return err == nil && document.Present
}

// CollaborationCapabilities resolves one contract's binding against the
// extensions an entry path discovered and renders its capabilities document.
func CollaborationCapabilities(
	workspaceRoot, contract string, extensions []*extension.ExtensionDescription, unavailable map[string]bool,
) collab.Capabilities {
	document, _ := readCollabDocument(workspaceRoot)
	return capabilitiesOf(resolveCollab(document, contract, extensions, unavailable))
}

// inContractNamespace reports whether a tool name lies in a collaboration
// contract's namespace.
func inContractNamespace(name string) bool {
	for _, contract := range collab.ContractNames {
		if strings.HasPrefix(name, contract+".") {
			return true
		}
	}
	return false
}

// registerCollaborationTools registers the routed tools of every contract the
// workspace document declares.
func (s *Server) registerCollaborationTools(exts []*extension.ExtensionDescription) {
	document, err := readCollabDocument(s.opts.WorkspaceRoot)
	if err != nil || !document.Present {
		return
	}
	unavailable := s.unavailableExtensions()
	for _, contract := range collab.ContractNames {
		resolution := resolveCollab(document, contract, exts, unavailable)
		s.registerCollaborationTool(capabilitiesTool(contract), contract, collab.OperationCapabilities)
		for _, op := range routedOperations(resolution) {
			s.registerCollaborationTool(routedTool(resolution, op), contract, op.Name)
		}
	}
}

func (s *Server) registerCollaborationTool(def Tool, contract, operation string) {
	if _, taken := s.byName[def.Name]; taken {
		return
	}
	s.registerResult(def, func(ctx context.Context, args json.RawMessage) (callToolResult, error) {
		envelope := s.callCollaboration(ctx, contract, operation, args)
		text, err := EnvelopeText(envelope, true)
		if err != nil {
			return callToolResult{}, err
		}
		return callToolResult{
			Content: []toolContent{{Type: "text", Text: text}},
			IsError: envelope.Outcome != collab.OutcomeOK,
		}, nil
	})
}

// callCollaboration answers one routed MCP tool call from the extensions this
// session discovers now.
func (s *Server) callCollaboration(ctx context.Context, contract, operation string, args json.RawMessage) collab.Envelope {
	discovered, err := s.discoverExtensions()
	var exts []*extension.ExtensionDescription
	if err == nil && discovered != nil {
		exts = discovered.Extensions
	}
	return CallProviderOperation(ctx, ProviderEnvironment{
		WorkspaceRoot: s.opts.WorkspaceRoot,
		Extensions:    exts,
		Unavailable:   s.unavailableExtensions(),
		Agent:         s.propagatedAgentIdentity(),
		AttachView:    s.attachToolWorkspaceView,
	}, contract, operation, args)
}

func capabilitiesTool(contract string) Tool {
	schema, _ := collab.InputSchema(contract, latestVersion(contract), collab.OperationCapabilities)
	tool := readOnlyTool(Tool{
		Name: contract + "." + collab.OperationCapabilities,
		Description: fmt.Sprintf("Report the %s binding of this workspace: the provider and contract version options.collaboration.%s names, "+
			"every operation of the contract with whether that provider offers it and how it enforces preconditions, the installed "+
			"providers that are not bound, and why a binding cannot be honored. Answered by Putnami from the manifests without running "+
			"the provider. Call it before relying on an optional operation.", contract, contract),
		InputSchema: schema,
	})
	tool.Meta[collaborationMetaKey] = collaborationToolMeta{Contract: contract, Operation: collab.OperationCapabilities}
	return tool
}

// collaborationMetaKey carries the contract identity of a routed tool.
const collaborationMetaKey = "putnami.dev/collaboration"

// collaborationToolMeta is the contract identity a routed tool advertises:
// the contract, the bound version, the operation and the provider serving it.
type collaborationToolMeta struct {
	Contract  string `json:"contract"`
	Version   int    `json:"version,omitempty"`
	Operation string `json:"operation"`
	Provider  string `json:"provider,omitempty"`
}

func routedTool(resolution collabResolution, op collab.OperationSpec) Tool {
	version := latestVersion(resolution.Contract)
	if resolution.Spec != nil {
		version = resolution.Spec.Version
	}
	schema, err := collab.InputSchema(resolution.Contract, version, op.Name)
	if err != nil {
		schema = schemaObject(``, nil)
	}
	readOnly := op.Access == collab.AccessRead
	annotations := &toolAnnotations{
		ReadOnlyHint:    boolPointer(readOnly),
		DestructiveHint: boolPointer(op.Destructive),
		IdempotentHint:  boolPointer(readOnly),
		OpenWorldHint:   boolPointer(true),
	}
	description := op.Summary
	meta := collaborationToolMeta{Contract: resolution.Contract, Version: version, Operation: op.Name}
	if provided, ok := resolution.Operations[op.Name]; ok && resolution.Status == collab.BindingStatusBound {
		declared := provided.Definition.Annotations
		annotations = &toolAnnotations{
			ReadOnlyHint:    boolPointer(*declared.ReadOnlyHint),
			DestructiveHint: boolPointer(*declared.DestructiveHint),
			IdempotentHint:  boolPointer(*declared.IdempotentHint),
			OpenWorldHint:   boolPointer(*declared.OpenWorldHint),
		}
		description += fmt.Sprintf(" Served by %s as bound in options.collaboration.%s: %s",
			resolution.Provider.Name, resolution.Contract, provided.Definition.Description)
		meta.Provider = resolution.Provider.Name
	} else {
		description += fmt.Sprintf(" The options.collaboration.%s binding cannot be honored, so every call answers with its failure; "+
			"%s.capabilities says why.", resolution.Contract, resolution.Contract)
	}
	access := "read"
	if !readOnly {
		access = "mutating"
	}
	tool := Tool{
		Name:        resolution.Contract + "." + op.Name,
		Description: description,
		InputSchema: schema,
		Annotations: annotations,
		Meta:        contractMeta(access, nil, false),
	}
	tool.Meta[collaborationMetaKey] = meta
	return tool
}

func latestVersion(contract string) int {
	versions := collab.SupportedVersions(contract)
	if len(versions) == 0 {
		return 0
	}
	return versions[len(versions)-1]
}

func boolPointer(value bool) *bool { return &value }

// ProviderEnvironment is what one entry path knows when it routes a
// collaboration operation: the extensions it discovered, the ones that must
// not serve, the agent identity it propagates, and how it attaches the
// workspace view to a tool that declares workspaceSelection.
type ProviderEnvironment struct {
	WorkspaceRoot string
	Extensions    []*extension.ExtensionDescription
	Unavailable   map[string]bool
	Agent         *proto.AgentIdentity
	AttachView    func(ctx context.Context, request *proto.ToolCallRequest, args json.RawMessage) error
	// Environ is the environment the provider inherits, whose credentials are
	// redacted from the envelope. Nil means the process environment.
	Environ []string
}

// CallProviderOperation routes one collaboration operation call and answers
// with its envelope. It is the single routing path of the MCP tools and the
// CLI command, so the two entry paths cannot disagree about which provider
// serves a contract, which requests reach it, or how its answer is judged.
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
	request := proto.ToolCallRequest{
		Name:          call.Provided.Tool,
		Arguments:     call.Arguments,
		WorkspaceRoot: env.WorkspaceRoot,
		ExtensionRoot: resolution.Provider.Path,
		Agent:         env.Agent,
		Provider:      call.providerMember(),
	}
	if call.Provided.Definition.WorkspaceSelection {
		if env.AttachView == nil {
			return redactor.redactEnvelope(transportFailure(call, stagePrepare,
				"this entry path cannot resolve the workspace selection the provider tool declares"), mutating)
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
	candidate := extensionToolCandidate{name: call.Provided.Tool, ext: resolution.Provider, def: call.Provided.Definition}
	result, err := invokeExtensionTool(ctx, env.WorkspaceRoot, candidate, request)
	if err != nil {
		stage, message := collaborationStage(err)
		return redactor.redactEnvelope(transportFailure(call, stage, message), mutating)
	}
	return redactor.redactEnvelope(interpretResult(call, result), mutating)
}

// collaborationStage reads how far a failed invocation got, and describes it
// without the provider's stderr, which never reaches an envelope.
func collaborationStage(err error) (invocationStage, string) {
	var failure *extensionToolError
	if !errors.As(err, &failure) {
		return stagePrepare, err.Error()
	}
	return failure.stage, failure.summary()
}

// EnvelopeText encodes an envelope for an entry path: indented for a person
// and for MCP text content, compact for a machine stream.
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
