package collaboration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
)

// ProviderMetaKey marks a manifest-declared tool as the implementation of one
// contract operation: tools.<name>._meta["putnami.dev/provider"]. A tool that
// carries it is reachable only through a workspace binding, under the
// contract's own operation name; it is never exposed under its declared name.
const ProviderMetaKey = "putnami.dev/provider"

// contractMetaKey is the extension tool contract metadata every tool carries.
const contractMetaKey = "putnami.dev/contract"

// ProviderDeclaration is the value of a tool's ProviderMetaKey.
type ProviderDeclaration struct {
	// Contract is the contract the tool implements (tasks, proposals, memory).
	Contract string `json:"contract"`
	// Version is the contract version the tool implements.
	Version int `json:"version"`
	// Operation is the operation name within Contract that the tool answers.
	Operation string `json:"operation"`
	// Preconditions states how the tool enforces expectedRevision (or, for an
	// atomic operation, exclusivity). Required exactly when the operation's
	// PreconditionRule is not PreconditionRuleNone.
	Preconditions Preconditions `json:"preconditions,omitempty"`
}

// ProvidedOperation is one tool that implements one contract operation.
type ProvidedOperation struct {
	// Tool is the tool name the extension declared.
	Tool string
	// Definition is the extension's manifest tool definition for Tool.
	Definition extension.ToolDefinition
	// Declaration is the parsed provider declaration from Definition's metadata.
	Declaration ProviderDeclaration
	// Spec is the catalog's operation specification the declaration resolved to.
	Spec OperationSpec
}

// ProviderOffer is everything one extension's manifest offers, keyed by
// contract, version and operation name. A contract version appears only when
// every one of its declarations is valid and every required operation is
// present.
type ProviderOffer map[string]map[int]map[string]ProvidedOperation

// Versions returns the versions of a contract the offer implements, ascending.
func (o ProviderOffer) Versions(contract string) []int {
	versions := make([]int, 0, len(o[contract]))
	for version := range o[contract] {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	return versions
}

// Operations returns one contract version's provided operations.
func (o ProviderOffer) Operations(contract string, version int) (map[string]ProvidedOperation, bool) {
	ops, ok := o[contract][version]
	return ops, ok
}

// ReadProviderOffer reads the provider declarations of one extension's tools
// and validates each against the catalog. It is the one check the
// orchestrator applies when it resolves a binding and the one a provider
// author runs against their own manifest.
//
// A declaration naming a contract version this package does not implement is
// reported as a warning and ignored, so a provider may offer a newer version
// beside the ones an older orchestrator speaks. Every other problem is an
// error that withdraws the affected contract version from the offer: a tool
// whose annotations disagree with its operation, two tools for one operation,
// or a missing required operation.
func ReadProviderOffer(tools map[string]extension.ToolDefinition) (ProviderOffer, []diag.Diagnostic) {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)

	type versionKey struct {
		contract string
		version  int
	}
	offered := map[versionKey]map[string]ProvidedOperation{}
	failed := map[versionKey]bool{}
	var diags []diag.Diagnostic
	for _, name := range names {
		def := tools[name]
		raw, present := def.Meta[ProviderMetaKey]
		if !present {
			continue
		}
		field := "tools." + name + "._meta." + ProviderMetaKey
		declaration, declDiags := parseDeclaration(field, raw)
		if declDiags != nil {
			diags = append(diags, declDiags...)
			if declaration != nil {
				failed[versionKey{declaration.Contract, declaration.Version}] = true
			}
			continue
		}
		key := versionKey{declaration.Contract, declaration.Version}
		spec, known := Lookup(declaration.Contract, declaration.Version)
		if !known {
			diags = append(diags, diag.Warningf(ErrorCodeUnsupportedVersion, field,
				"%s version %d is not implemented by this orchestrator; the declaration is ignored",
				declaration.Contract, declaration.Version))
			continue
		}
		op, known := spec.Operation(declaration.Operation)
		if !known {
			diags = append(diags, diag.Errorf(ErrorCodeUnknownOperation, field+".operation",
				"%s version %d has no operation %q", declaration.Contract, declaration.Version, declaration.Operation))
			failed[key] = true
			continue
		}
		if toolDiags := validateProvidedTool(field, name, def, *declaration, op); toolDiags != nil {
			diags = append(diags, toolDiags...)
			failed[key] = true
			continue
		}
		if offered[key] == nil {
			offered[key] = map[string]ProvidedOperation{}
		}
		if existing, duplicate := offered[key][op.Name]; duplicate {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidDeclaration, field,
				"tools %s and %s both implement %s.%s version %d; the choice would be ambiguous",
				existing.Tool, name, declaration.Contract, op.Name, declaration.Version))
			failed[key] = true
			continue
		}
		offered[key][op.Name] = ProvidedOperation{Tool: name, Definition: def, Declaration: *declaration, Spec: op}
	}

	offer := ProviderOffer{}
	keys := make([]versionKey, 0, len(offered))
	for key := range offered {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].contract != keys[j].contract {
			return keys[i].contract < keys[j].contract
		}
		return keys[i].version < keys[j].version
	})
	for _, key := range keys {
		if failed[key] {
			continue
		}
		spec, _ := Lookup(key.contract, key.version)
		var missing []string
		for _, required := range spec.RequiredOperations() {
			if _, ok := offered[key][required]; !ok {
				missing = append(missing, required)
			}
		}
		if len(missing) > 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidDeclaration, "tools",
				"%s version %d is declared without its required operations: %s",
				key.contract, key.version, strings.Join(missing, ", ")))
			continue
		}
		if offer[key.contract] == nil {
			offer[key.contract] = map[int]map[string]ProvidedOperation{}
		}
		offer[key.contract][key.version] = offered[key]
	}
	return offer, diags
}

func parseDeclaration(field string, raw any) (*ProviderDeclaration, []diag.Diagnostic) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDeclaration, field, "the declaration does not encode: %v", err)}
	}
	var declaration ProviderDeclaration
	if diags := strictDecode(encoded, &declaration); diags != nil {
		return nil, prefixDiagnostics(field, diags)
	}
	if !IsContract(declaration.Contract) {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownContract, field+".contract",
			"unknown contract %q", declaration.Contract)}
	}
	if declaration.Version < 1 {
		return &declaration, []diag.Diagnostic{diag.Errorf(ErrorCodeRequired, field+".version",
			"a provider declaration names its contract version")}
	}
	return &declaration, nil
}

func validateProvidedTool(field, name string, def extension.ToolDefinition, declaration ProviderDeclaration, op OperationSpec) []diag.Diagnostic {
	c := &checker{}
	if op.Name == OperationCapabilities {
		c.errorf(ErrorCodeInvalidDeclaration, field+".operation", "capabilities is answered by the orchestrator")
	}
	contract, _ := def.Meta[contractMetaKey].(map[string]any)
	access, _ := contract["access"].(string)
	if Access(access) != op.Access {
		c.errorf(ErrorCodeInvalidDeclaration, "tools."+name+"._meta."+contractMetaKey+".access",
			"%s.%s is a %s operation, but the tool declares access %q", declaration.Contract, op.Name, op.Access, access)
	}
	if dryRun, _ := contract["supportsDryRun"].(bool); dryRun {
		c.errorf(ErrorCodeInvalidDeclaration, "tools."+name+"._meta."+contractMetaKey+".supportsDryRun",
			"collaboration operations have no dry-run request member")
	}
	annotations := def.Annotations
	switch {
	case annotations == nil || annotations.ReadOnlyHint == nil || annotations.DestructiveHint == nil ||
		annotations.IdempotentHint == nil || annotations.OpenWorldHint == nil:
		c.errorf(ErrorCodeInvalidDeclaration, "tools."+name+".annotations", "all four safety annotations are required")
	default:
		readOnly := op.Access == AccessRead
		if *annotations.ReadOnlyHint != readOnly {
			c.errorf(ErrorCodeInvalidDeclaration, "tools."+name+".annotations.readOnlyHint",
				"%s.%s is %s; readOnlyHint must be %v", declaration.Contract, op.Name, op.Access, readOnly)
		}
		if readOnly && *annotations.DestructiveHint {
			c.errorf(ErrorCodeInvalidDeclaration, "tools."+name+".annotations.destructiveHint",
				"a read operation is never destructive")
		}
		if op.Destructive && !*annotations.DestructiveHint {
			c.errorf(ErrorCodeInvalidDeclaration, "tools."+name+".annotations.destructiveHint",
				"%s.%s may modify existing state; destructiveHint must be true", declaration.Contract, op.Name)
		}
	}
	switch op.Preconditions {
	case PreconditionRuleNone:
		if declaration.Preconditions != "" {
			c.errorf(ErrorCodeInvalidDeclaration, field+".preconditions",
				"%s.%s takes no revision precondition", declaration.Contract, op.Name)
		}
	case PreconditionRuleDeclared:
		if !validPreconditions(declaration.Preconditions) {
			c.errorf(ErrorCodeInvalidDeclaration, field+".preconditions",
				"%s.%s must declare preconditions: atomic, checked or none", declaration.Contract, op.Name)
		}
	case PreconditionRuleAtomic:
		if declaration.Preconditions != PreconditionsAtomic {
			c.errorf(ErrorCodeInvalidDeclaration, field+".preconditions",
				"%s.%s is offered only by a provider that enforces it atomically; declare preconditions: atomic",
				declaration.Contract, op.Name)
		}
	}
	if op.WorkspaceSelection && !def.WorkspaceSelection {
		c.errorf(ErrorCodeInvalidDeclaration, "tools."+name+".workspaceSelection",
			"%s.%s takes the canonical selection arguments; the tool must declare workspaceSelection so the orchestrator resolves them",
			declaration.Contract, op.Name)
	}
	return c.diags
}

func validPreconditions(value Preconditions) bool {
	for _, known := range ValidPreconditions {
		if value == known {
			return true
		}
	}
	return false
}

// OperationKey names one operation of one contract version.
type OperationKey struct {
	// Contract is the contract name (tasks, proposals, memory).
	Contract string
	// Version is the contract version.
	Version int
	// Operation is the operation name within Contract.
	Operation string
}

// String renders the key as "<contract>.<operation>@v<version>".
func (k OperationKey) String() string {
	return fmt.Sprintf("%s.%s@v%d", k.Contract, k.Operation, k.Version)
}

// Call is one validated operation request, as a provider handler receives it.
type Call struct {
	OperationKey
	// Input is the validated request document: a pointer to the operation's
	// input type (for example *TaskFindInput).
	Input any
	// Settings is the binding's settings object, verbatim.
	Settings json.RawMessage
	// Request is the whole tool call, for the workspace root and, when the
	// tool declared workspaceSelection, the resolved workspace view.
	Request extension.ToolCallRequest
}

// Failure is a non-ok outcome a handler returns.
type Failure struct {
	// Outcome is the closed non-ok result; see the Outcome* constants.
	Outcome Outcome
	// Error explains Outcome.
	Error Error
}

// Fail builds a Failure. Retryable defaults to false; an unavailable outcome
// the caller may retry sets it explicitly.
func Fail(outcome Outcome, reason, format string, args ...any) *Failure {
	return &Failure{Outcome: outcome, Error: Error{Message: fmt.Sprintf(format, args...), Reason: reason}}
}

// Handler performs one operation and returns its result document, or a
// Failure.
type Handler func(ctx context.Context, call Call) (any, *Failure)

// Serve answers one routed tool call: it reads one ToolCallRequest from in,
// validates the request against the operation the orchestrator named, runs the
// handler, validates the result, and writes exactly one ToolCallResult whose
// text block is a Response. A request that did not come through a binding (no
// provider member) is refused as invalid.
//
// A handler that panics or returns a result the contract refuses answers
// unresolved for a mutation — the write may have happened — and unavailable
// for a read. A list page too large for the document bound is requested again
// with fewer items (see fitResult).
func Serve(ctx context.Context, in io.Reader, out io.Writer, handlers map[OperationKey]Handler) error {
	var request extension.ToolCallRequest
	decoder := json.NewDecoder(in)
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode collaboration tool request: %w", err)
	}
	return writeResponse(out, serveOne(ctx, request, handlers))
}

func serveOne(ctx context.Context, request extension.ToolCallRequest, handlers map[OperationKey]Handler) Response {
	if request.Provider == nil {
		return failureResponse(Fail(OutcomeInvalid, ReasonRequestInvalid,
			"tool %s implements a collaboration operation and is called only through a workspace binding", request.Name))
	}
	key := OperationKey{Contract: request.Provider.Contract, Version: request.Provider.Version, Operation: request.Provider.Operation}
	handler, ok := handlers[key]
	if !ok {
		return failureResponse(Fail(OutcomeUnsupported, ReasonOperationUnsupported,
			"this provider does not implement %s", key))
	}
	spec, _ := Lookup(key.Contract, key.Version)
	op, _ := spec.Operation(key.Operation)
	input, diags := ParseRequest(key.Contract, key.Version, key.Operation, request.Arguments)
	if diags != nil {
		return failureResponse(Fail(OutcomeInvalid, ReasonRequestInvalid, "%s", formatDiagnostics(diags)))
	}
	call := Call{
		OperationKey: key,
		Input:        input,
		Settings:     request.Provider.Settings,
		Request:      request,
	}
	result, failure := runHandler(ctx, handler, call, op)
	if failure != nil {
		return failureResponse(failure)
	}
	encoded, failure := fitResult(ctx, handler, call, op, result)
	if failure != nil {
		return failureResponse(failure)
	}
	if _, diags := ParseResult(key.Contract, key.Version, key.Operation, encoded); diags != nil {
		return failureResponse(uncertain(op, "the %s result violates the contract: %s", key, formatDiagnostics(diags)))
	}
	return Response{Outcome: OutcomeOK, Result: encoded}
}

func runHandler(ctx context.Context, handler Handler, call Call, op OperationSpec) (result any, failure *Failure) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			failure = uncertain(op, "the %s handler panicked: %v", call.OperationKey, recovered)
		}
	}()
	result, failure = handler(ctx, call)
	if failure != nil && failure.Outcome == OutcomeOK {
		return nil, uncertain(op, "the %s handler returned a failure with outcome ok", call.OperationKey)
	}
	return result, failure
}

// maxResultBytes bounds the result document Serve answers with. The rest of
// MaxDocumentBytes holds the Response around it and the members the
// orchestrator adds to make it an Envelope.
const maxResultBytes = MaxDocumentBytes - 4<<10

// listOverheadBytes bounds a list result without its items: the members
// around them and a page.next cursor of MaxCursorLength characters, each
// escaped to at most six bytes.
const listOverheadBytes = 64 + 6*MaxCursorLength

// fitResult encodes a handler's result. A list page that would encode above
// maxResultBytes is requested again with a smaller page size, the number of
// its first items that fit, so the provider issues the cursor that follows
// the shorter page and a traversal still covers every item once. The page
// size strictly shrinks on every repeat, and only a read returns a list, so
// repeating the handler has no effect a caller could see.
func fitResult(ctx context.Context, handler Handler, call Call, op OperationSpec, result any) ([]byte, *Failure) {
	for asked := 0; ; {
		emptyListsAsEmpty(result)
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, uncertain(op, "encode the %s result: %v", call.OperationKey, err)
		}
		if len(encoded) <= maxResultBytes {
			return encoded, nil
		}
		sizes, err := listItemSizes(result)
		if err != nil {
			return nil, uncertain(op, "encode the %s result: %v", call.OperationKey, err)
		}
		page := pageRequestOf(call.Input)
		if sizes == nil || page == nil {
			return encoded, nil
		}
		if asked > 0 && len(sizes) > asked {
			return nil, uncertain(op, "the %s handler returned %d items for a page of %d", call.OperationKey, len(sizes), asked)
		}
		fit, total := 0, listOverheadBytes
		for _, size := range sizes {
			if total += size + 1; total > maxResultBytes {
				break
			}
			fit++
		}
		if fit == 0 {
			return nil, uncertain(op, "one %s item encodes to %d bytes, above the %d-byte result bound", call.OperationKey, sizes[0], maxResultBytes)
		}
		asked = fit
		page.Size = fit
		var failure *Failure
		if result, failure = runHandler(ctx, handler, call, op); failure != nil {
			return nil, failure
		}
	}
}

// listItemSizes returns the encoded size of every item of a list result, or
// nil for any other result.
func listItemSizes(result any) ([]int, error) {
	switch list := result.(type) {
	case *TaskListResult:
		return encodedSizes(list.Items)
	case *ProposalListResult:
		return encodedSizes(list.Items)
	case *MemoryListResult:
		return encodedSizes(list.Items)
	}
	return nil, nil
}

func encodedSizes[T any](items []T) ([]int, error) {
	sizes := make([]int, len(items))
	for i, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		sizes[i] = len(encoded)
	}
	return sizes, nil
}

// pageRequestOf returns the page request of a list operation's input,
// creating it when the request carried none, or nil for any other input.
func pageRequestOf(input any) *PageRequest {
	var page **PageRequest
	switch in := input.(type) {
	case *TaskFindInput:
		page = &in.Page
	case *ProposalFindInput:
		page = &in.Page
	case *MemoryContextInput:
		page = &in.Page
	case *MemorySearchInput:
		page = &in.Page
	default:
		return nil
	}
	if *page == nil {
		*page = &PageRequest{}
	}
	return *page
}

// emptyListsAsEmpty turns a list result's nil items into the empty list the
// contract requires, so a handler that found nothing may return a nil slice.
func emptyListsAsEmpty(result any) {
	switch list := result.(type) {
	case *TaskListResult:
		if list.Items == nil {
			list.Items = []Task{}
		}
	case *ProposalListResult:
		if list.Items == nil {
			list.Items = []Proposal{}
		}
	case *MemoryListResult:
		if list.Items == nil {
			list.Items = []MemoryRecord{}
		}
	}
}

// uncertain is the failure of a handler whose effect is unknown: unresolved
// for a mutation, unavailable for a read.
func uncertain(op OperationSpec, format string, args ...any) *Failure {
	if op.Access == AccessMutating {
		return Fail(OutcomeUnresolved, ReasonProviderInvalidResponse, format, args...)
	}
	return Fail(OutcomeUnavailable, ReasonProviderInvalidResponse, format, args...)
}

func failureResponse(failure *Failure) Response {
	errorValue := failure.Error
	if failure.Outcome == OutcomeUnresolved {
		errorValue.Retryable = false
	}
	return Response{Outcome: failure.Outcome, Error: &errorValue}
}

func writeResponse(out io.Writer, response Response) error {
	text, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode collaboration response: %w", err)
	}
	result := extension.ToolCallResult{
		Content: []extension.ToolContent{{Type: "text", Text: string(text)}},
		IsError: response.Outcome != OutcomeOK,
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(result); err != nil {
		return fmt.Errorf("encode collaboration tool result: %w", err)
	}
	_, err = out.Write(buffer.Bytes())
	return err
}

// formatDiagnostics renders error diagnostics on one line.
func formatDiagnostics(diags []diag.Diagnostic) string {
	parts := make([]string, 0, len(diags))
	for _, d := range diag.Errors(diags) {
		if d.Field != "" {
			parts = append(parts, d.Field+": "+d.Message)
		} else {
			parts = append(parts, d.Message)
		}
	}
	return strings.Join(parts, "; ")
}

// FormatDiagnostics renders error diagnostics on one line, for messages.
func FormatDiagnostics(diags []diag.Diagnostic) string { return formatDiagnostics(diags) }
