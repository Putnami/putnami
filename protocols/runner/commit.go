package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Version 2 of the execution request: commit-addressed and engine-planned. A
// caller that holds a commit and no snapshot, such as a hosted CI service,
// names the commit and the selection mode it wants; the executing engine
// plans the checkout of that commit itself. Version 1 (ExecutionRequest)
// stays the snapshot-addressed request with a frozen plan.
const (
	// CommitRequestVersion is the wire version of a CommitRequest.
	CommitRequestVersion = 2
	// CommitInputDomain separates the version 2 execution-input digest from
	// the version 1 one and from every other SHA-256 value.
	CommitInputDomain = "putnami/runner/execution-input/v2"
)

// CommitRequest is the version 2 execution request. It names a commit instead
// of a snapshot and a requested selection instead of a frozen one, and carries
// no plan and no environment: a frozen plan names no commit it was planned
// from, and the checkout pins its own engine, extensions and toolchains. The
// executing engine plans the checkout, stamps versions from its Git history and
// returns the plan it ran in the session bundle.
type CommitRequest struct {
	// Version is the request wire version and must equal 2.
	Version int `json:"version"`
	// Protocol names the provider protocol and the capabilities of the submission.
	Protocol ProtocolBlock `json:"protocol"`
	// Source names the commit the executing engine's checkout must sit on.
	Source CommitSource `json:"source"`
	// Invocation is the typed projection of the command line, as in version 1.
	Invocation InvocationBlock `json:"invocation"`
	// Selection is the selection mode the executing engine resolves on the checkout.
	Selection RequestedSelection `json:"selection"`
	// Control carries submission identity and bounds; it is outside the input digest.
	Control ControlBlock `json:"control"`
}

// CommitSource addresses a commit by its full object id.
type CommitSource struct {
	// Commit is the full lowercase commit id (40 or 64 hex digits) the
	// checkout's HEAD must equal, with no modified tracked file.
	Commit string `json:"commit"`
	// Base is the full commit an impacted selection is measured against. It is
	// present exactly when Selection.Mode is impacted, and has Commit's length.
	Base string `json:"base,omitempty"`
}

// RequestedSelection is the selection a caller asks for, before any engine
// resolved it: a mode and, for the projects mode, the selectors.
type RequestedSelection struct {
	// Mode is all, impacted or projects.
	Mode string `json:"mode"`
	// Projects are the sorted, unique project selectors of a projects
	// selection, in the grammar of `--projects`: a project name, path, alias
	// or group, one per entry. Present, and non-empty, exactly in the projects
	// mode.
	Projects []string `json:"projects,omitempty"`
}

// BoundRequest is one request of the bound-request channel (BoundRequestEnv):
// a snapshot-addressed version 1 request or a commit-addressed version 2
// request. Exactly one member is set.
type BoundRequest struct {
	// Snapshot is the version 1 request, nil for a version 2 one.
	Snapshot *ExecutionRequest
	// Commit is the version 2 request, nil for a version 1 one.
	Commit *CommitRequest
}

// Invocation returns the invocation block of whichever request is set.
func (request BoundRequest) Invocation() InvocationBlock {
	if request.Commit != nil {
		return request.Commit.Invocation
	}
	if request.Snapshot != nil {
		return request.Snapshot.Invocation
	}
	return InvocationBlock{}
}

// ParseBoundRequest strictly decodes a request of either version. It reads
// the root version member once and hands the document to the parser of that
// version, so a version 1 document parses exactly as ParseExecutionRequest
// parses it.
func ParseBoundRequest(data []byte) (BoundRequest, error) {
	if len(data) > MaxRequestBytes {
		return BoundRequest{}, fmt.Errorf("runner: request exceeds %d bytes", MaxRequestBytes)
	}
	if err := strictJSON(data); err != nil {
		return BoundRequest{}, err
	}
	version, err := requestVersion(data)
	if err != nil {
		return BoundRequest{}, err
	}
	switch version {
	case ExecutionRequestVersion:
		request, err := ParseExecutionRequest(data)
		if err != nil {
			return BoundRequest{}, err
		}
		return BoundRequest{Snapshot: &request}, nil
	case CommitRequestVersion:
		request, err := ParseCommitRequest(data)
		if err != nil {
			return BoundRequest{}, err
		}
		return BoundRequest{Commit: &request}, nil
	}
	return BoundRequest{}, fmt.Errorf("runner: unsupported execution request version %d", version)
}

// requestVersion reads the integer root version member of a document that
// strictJSON already admitted.
func requestVersion(data []byte) (int, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return 0, fmt.Errorf("runner: request: %w", err)
	}
	raw, ok := root["version"]
	if !ok {
		return 0, fmt.Errorf("runner: missing field %q", "version")
	}
	var version int
	if err := json.Unmarshal(raw, &version); err != nil {
		return 0, fmt.Errorf("runner: request version must be an integer")
	}
	return version, nil
}

// ParseCommitRequest strictly decodes and validates a version 2 request.
func ParseCommitRequest(data []byte) (CommitRequest, error) {
	var request CommitRequest
	if len(data) > MaxRequestBytes {
		return request, fmt.Errorf("runner: request exceeds %d bytes", MaxRequestBytes)
	}
	if err := strictJSON(data); err != nil {
		return request, err
	}
	if err := strictCommitRequestShape(data); err != nil {
		return request, err
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return request, fmt.Errorf("runner: request: %w", err)
	}
	if err := ValidateCommitRequest(request); err != nil {
		return request, err
	}
	return request, nil
}

// ErrFrozenPlan refuses a plan in a version 2 request.
var ErrFrozenPlan = errors.New("runner: a version 2 request carries no plan: a frozen plan names no commit it was planned from, and a commit-addressed request is engine-planned")

// snapshotSourceMembers are the version 1 source members. Each addresses a
// snapshot, which a version 2 source never does.
var snapshotSourceMembers = []string{"digest", "indexDigest", "git", "versions", "tree", "bound"}

// frozenSelectionMembers are the version 1 selection members a version 2
// selection does not have: each states what a submitting engine resolved.
var frozenSelectionMembers = []string{"requestedMode", "scoped", "baseline", "baselineSource", "changedPaths", "diagnostics", "noCacheProjects", "taskScopes"}

// strictCommitRequestShape rejects unknown or missing members at every object
// level, and names the version 1 member a version 2 document mixes in.
func strictCommitRequestShape(data []byte) error {
	version, err := requestVersion(data)
	if err != nil {
		return err
	}
	if version != CommitRequestVersion {
		return fmt.Errorf("runner: a commit-addressed request has version %d, not %d", CommitRequestVersion, version)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return fmt.Errorf("runner: request: %w", err)
	}
	if _, ok := members["plan"]; ok {
		return ErrFrozenPlan
	}
	if _, ok := members["environment"]; ok {
		return fmt.Errorf("runner: a version 2 request carries no environment: the executing engine runs the engine, extensions and toolchains its checkout pins")
	}
	root, err := strictObject(data, []string{"version", "protocol", "source", "invocation", "selection", "control"}, nil)
	if err != nil {
		return err
	}
	if _, err := strictObject(root["protocol"], []string{"version", "capabilities"}, nil); err != nil {
		return fmt.Errorf("runner: protocol: %w", err)
	}
	if err := strictCommitSource(root["source"]); err != nil {
		return err
	}
	if err := strictInvocationShape(root["invocation"]); err != nil {
		return err
	}
	if err := strictRequestedSelection(root["selection"]); err != nil {
		return err
	}
	if _, err := strictObject(root["control"], []string{"caller", "idempotencyKey", "deadline"}, nil); err != nil {
		return fmt.Errorf("runner: control: %w", err)
	}
	return nil
}

func strictCommitSource(raw json.RawMessage) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return fmt.Errorf("runner: source: %w", err)
	}
	for _, member := range snapshotSourceMembers {
		if _, ok := members[member]; ok {
			return fmt.Errorf("runner: source.%s addresses a snapshot; a version 2 source names a commit", member)
		}
	}
	source, err := strictObject(raw, []string{"commit"}, []string{"base"})
	if err != nil {
		return fmt.Errorf("runner: source: %w", err)
	}
	if base, ok := source["base"]; ok {
		// The canonical form omits an absent base, so an explicit empty one is
		// a second spelling of the same request and is refused.
		var value string
		if err := json.Unmarshal(base, &value); err != nil || value == "" {
			return fmt.Errorf("runner: source.base must be a full commit id when present")
		}
	}
	return nil
}

func strictRequestedSelection(raw json.RawMessage) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return fmt.Errorf("runner: selection: %w", err)
	}
	for _, member := range frozenSelectionMembers {
		if _, ok := members[member]; ok {
			return fmt.Errorf("runner: selection.%s is a frozen selection output; a version 2 selection names the requested mode, which the executing engine resolves", member)
		}
	}
	selection, err := strictObject(raw, []string{"mode"}, []string{"projects"})
	if err != nil {
		return fmt.Errorf("runner: selection: %w", err)
	}
	if projects, ok := selection["projects"]; ok {
		// Absent when empty, never an empty list: one request, one spelling.
		if items, err := strictArray(projects, MaxSelectionProjects); err != nil || len(items) == 0 {
			return fmt.Errorf("runner: selection.projects must be a non-empty array when present")
		}
	}
	return nil
}

// ValidateCommitRequest checks the semantic rules of a version 2 request. The
// invocation block follows the version 1 rules. ValidatePublication does not
// run, because there is no plan: the executing engine checks the plan it makes
// against invocation.publication instead.
func ValidateCommitRequest(request CommitRequest) error {
	if request.Version != CommitRequestVersion {
		return fmt.Errorf("runner: unsupported commit request version %d", request.Version)
	}
	if request.Protocol.Version != ProviderProtocolVersion {
		return fmt.Errorf("runner: unsupported provider protocol version %d", request.Protocol.Version)
	}
	if err := sortedUnique(request.Protocol.Capabilities, MaxDiagnostics); err != nil {
		return fmt.Errorf("runner: protocol.capabilities: %w", err)
	}
	if err := validateCommitSource(request.Source); err != nil {
		return err
	}
	if err := validateInvocation(request.Invocation); err != nil {
		return err
	}
	if err := validateRequestedSelection(request.Selection, request.Source); err != nil {
		return err
	}
	return validateControl(request.Control, CommitRequestVersion)
}

func validateCommitSource(source CommitSource) error {
	if !validObjectID(source.Commit) {
		return fmt.Errorf("runner: source.commit must be a full lowercase commit id of 40 or 64 hex digits")
	}
	if source.Base == "" {
		return nil
	}
	if !validObjectID(source.Base) {
		return fmt.Errorf("runner: source.base must be a full lowercase commit id of 40 or 64 hex digits")
	}
	if len(source.Base) != len(source.Commit) {
		return fmt.Errorf("runner: source.base and source.commit must be commit ids of one object format")
	}
	return nil
}

func validObjectID(value string) bool {
	return validHex(value, 40) || validHex(value, 64)
}

func validateRequestedSelection(selection RequestedSelection, source CommitSource) error {
	switch selection.Mode {
	case SelectionModeAll, SelectionModeImpacted, SelectionModeProjects:
	default:
		return fmt.Errorf("runner: selection mode %q is not all, impacted or projects", selection.Mode)
	}
	impacted := selection.Mode == SelectionModeImpacted
	if impacted && source.Base == "" {
		return fmt.Errorf("runner: an impacted selection needs source.base, the full commit it is measured against")
	}
	if !impacted && source.Base != "" {
		return fmt.Errorf("runner: source.base is the impacted baseline; a selection in mode %s has none", selection.Mode)
	}
	if selection.Mode != SelectionModeProjects {
		if selection.Projects != nil {
			return fmt.Errorf("runner: selection.projects belongs to the projects mode; a selection in mode %s lists none", selection.Mode)
		}
		return nil
	}
	if len(selection.Projects) == 0 {
		return fmt.Errorf("runner: a projects selection must list its selectors in selection.projects")
	}
	if err := sortedUnique(selection.Projects, MaxSelectionProjects); err != nil {
		return fmt.Errorf("runner: selection.projects: %w", err)
	}
	for _, selector := range selection.Projects {
		if err := validateSelector(selector); err != nil {
			return fmt.Errorf("runner: selection.projects: %w", err)
		}
	}
	return nil
}

// validateSelector refuses a selector the executing engine would read as more
// than one project selector, or as a selection mode.
func validateSelector(selector string) error {
	switch {
	case strings.ContainsRune(selector, ','):
		return fmt.Errorf("selector %q holds a comma; each entry is one selector", selector)
	case selector == "*" || selector == "[impacted]":
		return fmt.Errorf("selector %q spells a selection mode, not a project", selector)
	case strings.TrimSpace(selector) != selector:
		return fmt.Errorf("selector %q has surrounding whitespace", selector)
	}
	return nil
}

// CanonicalCommitRequest returns the compact canonical JSON bytes of a
// validated version 2 request, under the version 1 rules: struct member order
// and Go JSON escaping.
func CanonicalCommitRequest(request CommitRequest) ([]byte, error) {
	if err := ValidateCommitRequest(request); err != nil {
		return nil, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRequestBytes {
		return nil, fmt.Errorf("runner: request exceeds %d bytes", MaxRequestBytes)
	}
	return data, nil
}

type commitInput struct {
	Domain     string             `json:"domain"`
	Protocol   int                `json:"protocolVersion"`
	Source     CommitSource       `json:"source"`
	Invocation InvocationBlock    `json:"invocation"`
	Selection  RequestedSelection `json:"selection"`
}

// CommitInputDigest is the execution-input digest of a version 2 request:
// commit, invocation and requested selection under CommitInputDomain. It
// excludes the control block (per submission) and the negotiated
// capabilities, and is never a task key.
func CommitInputDigest(request CommitRequest) (string, error) {
	if err := ValidateCommitRequest(request); err != nil {
		return "", err
	}
	data, err := json.Marshal(commitInput{
		Domain: CommitInputDomain, Protocol: request.Protocol.Version,
		Source: request.Source, Invocation: request.Invocation, Selection: request.Selection,
	})
	if err != nil {
		return "", err
	}
	return BlobDigest(data), nil
}
