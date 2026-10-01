package collaboration

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Bounds on the members of every document.
const (
	MaxTokenLength   = 200
	MaxTitleLength   = 256
	MaxBodyBytes     = 64 << 10
	MaxContentBytes  = 64 << 10
	MaxLabels        = 50
	MaxLabelLength   = 64
	MaxListMembers   = 100
	MaxCursorLength  = 1024
	MaxURLLength     = 2048
	MaxLocatorLength = 1024
)

var (
	sourcePattern         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}:\S+$`)
	idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
	commitPattern         = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	digestPattern         = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// timestampPattern is RFC 3339 with an optional fraction.
	timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`)
)

// ParseRequest strictly decodes and validates the request document of one
// operation. An empty document is the empty object.
func ParseRequest(contract string, version int, operation string, data []byte) (any, []diag.Diagnostic) {
	op, diags := lookupOperation(contract, version, operation)
	if diags != nil {
		return nil, diags
	}
	input := op.NewInput()
	if diags := strictDecode(orEmptyObject(data), input); diags != nil {
		return nil, diags
	}
	if diags := ValidateInput(input); diag.HasErrors(diags) {
		return nil, diags
	}
	return input, nil
}

// ParseResult strictly decodes and validates the result document of one
// operation.
func ParseResult(contract string, version int, operation string, data []byte) (any, []diag.Diagnostic) {
	op, diags := lookupOperation(contract, version, operation)
	if diags != nil {
		return nil, diags
	}
	result := op.NewResult()
	if diags := strictDecode(data, result); diags != nil {
		return nil, diags
	}
	if diags := ValidateResult(result); diag.HasErrors(diags) {
		return nil, diags
	}
	return result, nil
}

// ParseResponse strictly decodes and validates a provider Response. The
// result document itself is validated by ParseResult against the operation.
func ParseResponse(data []byte) (*Response, []diag.Diagnostic) {
	var response Response
	if diags := strictDecode(data, &response); diags != nil {
		return nil, diags
	}
	c := &checker{}
	c.outcome("", "", response.Outcome, response.Result, response.Error)
	if diag.HasErrors(c.diags) {
		return nil, c.diags
	}
	return &response, nil
}

// ParseEnvelope strictly decodes and validates an Envelope, including its
// result document when the outcome is ok.
func ParseEnvelope(data []byte) (*Envelope, []diag.Diagnostic) {
	var envelope Envelope
	if diags := strictDecode(data, &envelope); diags != nil {
		return nil, diags
	}
	if diags := ValidateEnvelope(&envelope); diag.HasErrors(diags) {
		return nil, diags
	}
	return &envelope, nil
}

// ValidateEnvelope checks an Envelope's vocabulary, its outcome rules and,
// on an ok outcome, its result document.
func ValidateEnvelope(envelope *Envelope) []diag.Diagnostic {
	c := &checker{}
	if !IsContract(envelope.Contract) {
		c.errorf(ErrorCodeUnknownContract, "contract", "unknown contract %q", envelope.Contract)
	}
	if strings.TrimSpace(envelope.Operation) == "" {
		c.errorf(ErrorCodeRequired, "operation", "operation is required")
	}
	if envelope.Version < 0 {
		c.errorf(ErrorCodeInvalidValue, "version", "version must be positive")
	}
	if envelope.Provider != nil && strings.TrimSpace(envelope.Provider.Name) == "" {
		c.errorf(ErrorCodeRequired, "provider.name", "provider name is required")
	}
	c.outcome("", envelope.Contract, envelope.Outcome, envelope.Result, envelope.Error)
	if diag.HasErrors(c.diags) || envelope.Outcome != OutcomeOK {
		return c.diags
	}
	if envelope.Operation == OperationCapabilities {
		var capabilities Capabilities
		if diags := strictDecode(envelope.Result, &capabilities); diags != nil {
			return prefixDiagnostics("result", diags)
		}
		return prefixDiagnostics("result", validateCapabilities(&capabilities))
	}
	if envelope.Version == 0 {
		c.errorf(ErrorCodeRequired, "version", "an ok envelope names the contract version")
		return c.diags
	}
	if _, diags := ParseResult(envelope.Contract, envelope.Version, envelope.Operation, envelope.Result); diags != nil {
		return prefixDiagnostics("result", diags)
	}
	return nil
}

func lookupOperation(contract string, version int, operation string) (OperationSpec, []diag.Diagnostic) {
	if !IsContract(contract) {
		return OperationSpec{}, []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownContract, "", "unknown contract %q", contract)}
	}
	spec, ok := Lookup(contract, version)
	if !ok {
		return OperationSpec{}, []diag.Diagnostic{diag.Errorf(ErrorCodeUnsupportedVersion, "",
			"%s version %d is not supported (supported: %v)", contract, version, SupportedVersions(contract))}
	}
	op, ok := spec.Operation(operation)
	if !ok {
		return OperationSpec{}, []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownOperation, "",
			"%s version %d has no operation %q", contract, version, operation)}
	}
	return op, nil
}

func orEmptyObject(data []byte) []byte {
	if len(bytes.TrimSpace(data)) == 0 {
		return []byte("{}")
	}
	return data
}

func prefixDiagnostics(prefix string, diags []diag.Diagnostic) []diag.Diagnostic {
	out := make([]diag.Diagnostic, len(diags))
	for i, d := range diags {
		d.Field = joinPath(prefix, d.Field)
		out[i] = d
	}
	return out
}

// ValidateInput checks one decoded request document.
func ValidateInput(input any) []diag.Diagnostic {
	c := &checker{}
	switch in := input.(type) {
	case *TaskFindInput:
		c.bounded("query", in.Query, MaxTitleLength, false)
		c.taskStates("states", in.States)
		c.labels("labels", in.Labels)
		c.page(in.Page)
	case *TaskRefInput:
		c.ref("ref", in.Ref)
	case *TaskCreateInput:
		c.title("title", in.Title)
		c.bytesLimit("body", in.Body, MaxBodyBytes)
		c.labels("labels", in.Labels)
		if in.State != "" {
			c.taskState("state", in.State)
		}
		c.idempotencyKey("idempotencyKey", in.IdempotencyKey)
	case *TaskUpdateInput:
		c.ref("ref", in.Ref)
		c.optionalToken("expectedRevision", in.ExpectedRevision)
		if in.Title == nil && in.Body == nil && in.Labels == nil {
			c.errorf(ErrorCodeRequired, "", "an update changes at least one of title, body and labels")
		}
		if in.Title != nil {
			c.title("title", *in.Title)
		}
		if in.Body != nil {
			c.bytesLimit("body", *in.Body, MaxBodyBytes)
		}
		if in.Labels != nil {
			c.labels("labels", *in.Labels)
		}
	case *TaskTransitionInput:
		c.ref("ref", in.Ref)
		c.taskState("state", in.State)
		c.optionalToken("expectedRevision", in.ExpectedRevision)
		c.bounded("reason", in.Reason, MaxTitleLength, false)
	case *TaskAssignInput:
		c.ref("ref", in.Ref)
		c.names("assignees", in.Assignees)
		c.optionalToken("expectedRevision", in.ExpectedRevision)
	case *TaskLinkInput:
		c.ref("ref", in.Ref)
		switch {
		case in.Parent != nil && in.Unlink:
			c.errorf(ErrorCodeInvalidValue, "parent", "parent and unlink are exclusive")
		case in.Parent == nil && !in.Unlink:
			c.errorf(ErrorCodeRequired, "parent", "a link names a parent or sets unlink")
		case in.Parent != nil:
			c.ref("parent", *in.Parent)
		}
	case *TaskClaimInput:
		c.ref("ref", in.Ref)
		c.token("holder", in.Holder)
	case *ProposalFindInput:
		c.change("change", in.Change, false)
		c.proposalStates("states", in.States)
		c.page(in.Page)
	case *ProposalUpsertInput:
		c.change("change", in.Change, false)
		c.title("title", in.Title)
		c.bytesLimit("body", in.Body, MaxBodyBytes)
		c.optionalToken("expectedRevision", in.ExpectedRevision)
	case *ProposalRefInput:
		c.ref("ref", in.Ref)
	case *ProposalReviewInput:
		c.ref("ref", in.Ref)
		c.reviewVerdict("verdict", in.Verdict)
		c.bytesLimit("body", in.Body, MaxBodyBytes)
		if in.Commit != "" {
			c.commit("commit", in.Commit)
		}
		c.idempotencyKey("idempotencyKey", in.IdempotencyKey)
	case *ProposalMergeInput:
		c.ref("ref", in.Ref)
		c.commit("expectedHeadCommit", in.ExpectedHeadCommit)
		if in.Method != "" && !slices.Contains(ValidMergeMethods, in.Method) {
			c.errorf(ErrorCodeInvalidValue, "method", "unknown merge method %q", in.Method)
		}
	case *MemoryContextInput:
		c.identity("identity", in.Identity)
		c.memoryKinds("kinds", in.Kinds)
		c.selection(in.Projects, in.Impacted, in.Baseline)
		c.page(in.Page)
	case *MemorySearchInput:
		c.bounded("query", in.Query, MaxTitleLength, true)
		c.identity("identity", in.Identity)
		c.memoryKinds("kinds", in.Kinds)
		c.selection(in.Projects, in.Impacted, in.Baseline)
		c.page(in.Page)
	case *MemoryMissionInput:
		c.token("mission", in.Mission)
		c.identity("identity", in.Identity)
		c.sameMission(in.Mission, in.Identity)
	case *MemoryCheckpointInput:
		c.token("mission", in.Mission)
		c.identity("identity", in.Identity)
		c.sameMission(in.Mission, in.Identity)
		c.precondition("precondition", in.Precondition)
		c.idempotencyKey("idempotencyKey", in.IdempotencyKey)
		c.bounded("title", in.Title, MaxTitleLength, false)
		c.bytesLimit("content", in.Content, MaxContentBytes)
		if strings.TrimSpace(in.Content) == "" {
			c.errorf(ErrorCodeRequired, "content", "content is required")
		}
		c.refs("sources", in.Sources)
		c.evidence("evidence", in.Evidence)
	case *CapabilitiesInput:
	default:
		c.errorf(ErrorCodeUnknownOperation, "", "no request validator for %T", input)
	}
	return c.diags
}

// ValidateResult checks one decoded result document.
func ValidateResult(result any) []diag.Diagnostic {
	c := &checker{}
	switch r := result.(type) {
	case *TaskListResult:
		c.itemsPresent(r.Items == nil)
		c.listBound("items", len(r.Items))
		for i := range r.Items {
			c.task(fmt.Sprintf("items[%d]", i), r.Items[i])
		}
		c.nextCursor("page.next", r.Page.Next)
	case *TaskResult:
		c.task("task", r.Task)
	case *TaskCreateResult:
		c.task("task", r.Task)
	case *TaskClaimResult:
		c.task("task", r.Task)
	case *ProposalListResult:
		c.itemsPresent(r.Items == nil)
		c.listBound("items", len(r.Items))
		for i := range r.Items {
			c.proposal(fmt.Sprintf("items[%d]", i), r.Items[i])
		}
		c.nextCursor("page.next", r.Page.Next)
	case *ProposalUpsertResult:
		c.proposal("proposal", r.Proposal)
	case *ProposalResult:
		c.proposal("proposal", r.Proposal)
	case *ProposalStatusResult:
		c.proposal("proposal", r.Proposal)
		c.checks("checks", r.Checks)
		c.listBound("reviews", len(r.Reviews))
		for i, review := range r.Reviews {
			c.review(fmt.Sprintf("reviews[%d]", i), review)
		}
	case *ProposalReviewResult:
		c.review("review", r.Review)
	case *MemoryListResult:
		c.itemsPresent(r.Items == nil)
		c.listBound("items", len(r.Items))
		for i := range r.Items {
			c.record(fmt.Sprintf("items[%d]", i), r.Items[i])
		}
		c.nextCursor("page.next", r.Page.Next)
	case *MemoryRecordResult:
		c.record("record", r.Record)
	case *MemoryCheckpointResult:
		c.record("record", r.Record)
		if r.Record.Kind != MemoryKindMission {
			c.errorf(ErrorCodeInvalidValue, "record.kind", "a checkpoint writes a mission record")
		}
	case *Capabilities:
		c.diags = append(c.diags, validateCapabilities(r)...)
	default:
		c.errorf(ErrorCodeUnknownOperation, "", "no result validator for %T", result)
	}
	return c.diags
}

func validateCapabilities(capabilities *Capabilities) []diag.Diagnostic {
	c := &checker{}
	if !IsContract(capabilities.Contract) {
		c.errorf(ErrorCodeUnknownContract, "contract", "unknown contract %q", capabilities.Contract)
	}
	if !slices.Contains(ValidBindingStatuses, capabilities.Status) {
		c.errorf(ErrorCodeInvalidValue, "status", "unknown binding status %q", capabilities.Status)
	}
	if capabilities.Status == BindingStatusBound && (capabilities.Binding == nil || capabilities.Provider == nil) {
		c.errorf(ErrorCodeRequired, "binding", "a bound contract names its binding and provider")
	}
	if capabilities.Status == BindingStatusInvalid && len(capabilities.Issues) == 0 {
		c.errorf(ErrorCodeRequired, "issues", "an invalid binding says why")
	}
	for i, op := range capabilities.Operations {
		field := fmt.Sprintf("operations[%d]", i)
		c.token(field+".name", op.Name)
		if op.Access != AccessRead && op.Access != AccessMutating {
			c.errorf(ErrorCodeInvalidValue, field+".access", "unknown access %q", op.Access)
		}
		if op.Preconditions != "" && !slices.Contains(ValidPreconditions, op.Preconditions) {
			c.errorf(ErrorCodeInvalidValue, field+".preconditions", "unknown preconditions %q", op.Preconditions)
		}
	}
	return c.diags
}

// checker accumulates diagnostics for one document.
type checker struct {
	diags []diag.Diagnostic
}

func (c *checker) errorf(code, field, format string, args ...any) {
	c.diags = append(c.diags, diag.Errorf(code, field, format, args...))
}

// outcome checks the outcome rules of a Response or an Envelope. contract is
// the Envelope's contract, and "" for a Response, which names none.
func (c *checker) outcome(field, contract string, outcome Outcome, result []byte, errorValue *Error) {
	if !slices.Contains(ValidOutcomes, outcome) {
		c.errorf(ErrorCodeInvalidOutcome, joinPath(field, "outcome"), "unknown outcome %q", outcome)
		return
	}
	if outcome == OutcomeOK {
		if len(bytes.TrimSpace(result)) == 0 || bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
			c.errorf(ErrorCodeInvalidOutcome, joinPath(field, "result"), "an ok outcome carries a result")
		}
		if errorValue != nil {
			c.errorf(ErrorCodeInvalidOutcome, joinPath(field, "error"), "an ok outcome carries no error")
		}
		return
	}
	if len(result) > 0 {
		c.errorf(ErrorCodeInvalidOutcome, joinPath(field, "result"), "a %s outcome carries no result", outcome)
	}
	if errorValue == nil {
		c.errorf(ErrorCodeInvalidOutcome, joinPath(field, "error"), "a %s outcome carries an error", outcome)
		return
	}
	if strings.TrimSpace(errorValue.Message) == "" {
		c.errorf(ErrorCodeRequired, joinPath(field, "error.message"), "an error carries a message")
	}
	if outcome == OutcomeUnresolved && errorValue.Retryable {
		c.errorf(ErrorCodeInvalidOutcome, joinPath(field, "error.retryable"),
			"an unresolved mutation is never retryable: reconcile first")
	}
	if errorValue.Current != "" {
		if outcome != OutcomeConflict {
			c.errorf(ErrorCodeInvalidOutcome, joinPath(field, "error.current"), "only a conflict reports a current revision")
		}
		c.token(joinPath(field, "error.current"), errorValue.Current)
	}
	switch {
	case errorValue.Reconcile == "":
	case outcome != OutcomeUnresolved:
		c.errorf(ErrorCodeInvalidOutcome, joinPath(field, "error.reconcile"), "only an unresolved outcome names a reconciliation")
	default:
		if _, _, ok := ReconcileOperation(contract, errorValue.Reconcile); !ok {
			scope := "an operation of the contract catalog"
			if contract != "" {
				scope = "an operation of the " + contract + " contract"
			}
			c.errorf(ErrorCodeInvalidValue, joinPath(field, "error.reconcile"),
				"reconcile names the read or repeat that settles the write as <contract>.<operation>, %s", scope)
		}
	}
}

// ReconcileOperation returns the first operation an unresolved error's
// reconcile hint names, spelled "<contract>.<operation>" as the catalog
// defines it. contract restricts the match to that contract; "" accepts any.
// It is the operation a caller runs, before anything else, to learn whether
// the uncertain write happened.
func ReconcileOperation(contract, hint string) (string, string, bool) {
	words := strings.FieldsFunc(hint, func(r rune) bool { return (r < 'a' || r > 'z') && r != '.' })
	for _, word := range words {
		name, operation, found := strings.Cut(strings.Trim(word, "."), ".")
		if !found || (contract != "" && name != contract) {
			continue
		}
		for _, version := range SupportedVersions(name) {
			spec, _ := Lookup(name, version)
			if _, ok := spec.Operation(operation); ok {
				return name, operation, true
			}
		}
	}
	return "", "", false
}

func (c *checker) ref(field string, ref Ref) {
	switch {
	case ref.Source == "":
		c.errorf(ErrorCodeInvalidReference, field+".source", "a reference names its source")
	case len(ref.Source) > MaxTokenLength || !sourcePattern.MatchString(ref.Source):
		c.errorf(ErrorCodeInvalidReference, field+".source", "source %q is not \"<kind>:<locator>\"", ref.Source)
	}
	switch {
	case ref.ID == "":
		c.errorf(ErrorCodeInvalidReference, field+".id", "a reference names its id")
	case !isToken(ref.ID):
		c.errorf(ErrorCodeInvalidReference, field+".id", "id must be at most %d printable characters without whitespace", MaxTokenLength)
	}
}

func (c *checker) refs(field string, refs []Ref) {
	c.listBound(field, len(refs))
	for i, ref := range refs {
		c.ref(fmt.Sprintf("%s[%d]", field, i), ref)
	}
}

func (c *checker) token(field, value string) {
	if value == "" {
		c.errorf(ErrorCodeRequired, field, "%s is required", field)
		return
	}
	if !isToken(value) {
		c.errorf(ErrorCodeInvalidValue, field, "%s must be at most %d printable characters without whitespace", field, MaxTokenLength)
	}
}

func (c *checker) optionalToken(field, value string) {
	if value != "" {
		c.token(field, value)
	}
}

func (c *checker) idempotencyKey(field, value string) {
	if value == "" {
		c.errorf(ErrorCodeRequired, field, "%s is required: it is what makes a retry safe", field)
		return
	}
	if !idempotencyKeyPattern.MatchString(value) {
		c.errorf(ErrorCodeInvalidValue, field, "%s must match %s", field, idempotencyKeyPattern)
	}
}

func (c *checker) title(field, value string) {
	if strings.TrimSpace(value) == "" {
		c.errorf(ErrorCodeRequired, field, "%s is required", field)
		return
	}
	c.bounded(field, value, MaxTitleLength, true)
}

func (c *checker) bounded(field, value string, limit int, required bool) {
	if required && strings.TrimSpace(value) == "" {
		c.errorf(ErrorCodeRequired, field, "%s is required", field)
		return
	}
	if utf8.RuneCountInString(value) > limit {
		c.errorf(ErrorCodeTooLarge, field, "%s exceeds %d characters", field, limit)
	}
	if strings.ContainsAny(value, "\n\r\x00") {
		c.errorf(ErrorCodeInvalidValue, field, "%s is one line", field)
	}
}

func (c *checker) bytesLimit(field, value string, limit int) {
	if len(value) > limit {
		c.errorf(ErrorCodeTooLarge, field, "%s exceeds %d bytes", field, limit)
	}
	if strings.ContainsRune(value, 0) {
		c.errorf(ErrorCodeInvalidValue, field, "%s contains a NUL character", field)
	}
}

func (c *checker) labels(field string, labels []string) {
	if len(labels) > MaxLabels {
		c.errorf(ErrorCodeTooLarge, field, "at most %d labels", MaxLabels)
	}
	seen := map[string]bool{}
	for i, label := range labels {
		item := fmt.Sprintf("%s[%d]", field, i)
		if strings.TrimSpace(label) == "" || label != strings.TrimSpace(label) {
			c.errorf(ErrorCodeInvalidValue, item, "a label is non-empty and carries no surrounding whitespace")
			continue
		}
		c.bounded(item, label, MaxLabelLength, true)
		if seen[label] {
			c.errorf(ErrorCodeInvalidValue, item, "label %q is repeated", label)
		}
		seen[label] = true
	}
}

func (c *checker) names(field string, names []string) {
	c.listBound(field, len(names))
	seen := map[string]bool{}
	for i, name := range names {
		item := fmt.Sprintf("%s[%d]", field, i)
		c.token(item, name)
		if seen[name] {
			c.errorf(ErrorCodeInvalidValue, item, "%q is repeated", name)
		}
		seen[name] = true
	}
}

// itemsPresent refuses a list result whose items are absent or null: an empty
// page is the empty list, so a reader never mistakes a missing answer for no
// match.
func (c *checker) itemsPresent(missing bool) {
	if missing {
		c.errorf(ErrorCodeRequired, "items", "items is required; an empty page is []")
	}
}

func (c *checker) listBound(field string, count int) {
	if count > MaxListMembers {
		c.errorf(ErrorCodeTooLarge, field, "at most %d members", MaxListMembers)
	}
}

func (c *checker) page(page *PageRequest) {
	const field = "page"
	if page == nil {
		return
	}
	if page.Size < 0 || page.Size > MaxPageSize {
		c.errorf(ErrorCodeInvalidPage, field+".size", "page size must be between 0 (the default, %d) and %d", DefaultPageSize, MaxPageSize)
	}
	if len(page.Cursor) > MaxCursorLength || strings.ContainsAny(page.Cursor, " \t\n\r") {
		c.errorf(ErrorCodeInvalidPage, field+".cursor", "a cursor is at most %d characters without whitespace", MaxCursorLength)
	}
}

func (c *checker) nextCursor(field, cursor string) {
	if len(cursor) > MaxCursorLength || strings.ContainsAny(cursor, " \t\n\r") {
		c.errorf(ErrorCodeInvalidPage, field, "a cursor is at most %d characters without whitespace", MaxCursorLength)
	}
}

func (c *checker) taskState(field string, state TaskState) {
	if !slices.Contains(ValidTaskStates, state) {
		c.errorf(ErrorCodeInvalidState, field, "unknown task state %q", state)
	}
}

func (c *checker) taskStates(field string, states []TaskState) {
	for i, state := range states {
		c.taskState(fmt.Sprintf("%s[%d]", field, i), state)
	}
}

func (c *checker) proposalStates(field string, states []ProposalState) {
	for i, state := range states {
		if !slices.Contains(ValidProposalStates, state) {
			c.errorf(ErrorCodeInvalidState, fmt.Sprintf("%s[%d]", field, i), "unknown proposal state %q", state)
		}
	}
}

func (c *checker) memoryKinds(field string, kinds []MemoryKind) {
	for i, kind := range kinds {
		if !slices.Contains(ValidMemoryKinds, kind) {
			c.errorf(ErrorCodeInvalidValue, fmt.Sprintf("%s[%d]", field, i), "unknown memory kind %q", kind)
		}
	}
}

func (c *checker) reviewVerdict(field string, verdict ReviewVerdict) {
	if !slices.Contains(ValidReviewVerdicts, verdict) {
		c.errorf(ErrorCodeInvalidValue, field, "unknown review verdict %q", verdict)
	}
}

func (c *checker) commit(field, commit string) {
	if commit == "" {
		c.errorf(ErrorCodeRequired, field, "%s is required", field)
		return
	}
	if !commitPattern.MatchString(commit) {
		c.errorf(ErrorCodeInvalidValue, field, "%s must be 7 to 64 lowercase hexadecimal characters", field)
	}
}

func (c *checker) change(field string, change Change, requireRepository bool) {
	if requireRepository || change.Repository != "" {
		c.token(field+".repository", change.Repository)
	}
	c.token(field+".base", change.Base)
	c.token(field+".head", change.Head)
	if change.HeadCommit != "" {
		c.commit(field+".headCommit", change.HeadCommit)
	}
}

func (c *checker) url(field, value string) {
	if value == "" {
		return
	}
	parsed, err := url.Parse(value)
	switch {
	case err != nil || len(value) > MaxURLLength:
		c.errorf(ErrorCodeInvalidURL, field, "%s is not a URL of at most %d characters", field, MaxURLLength)
	case (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "":
		c.errorf(ErrorCodeInvalidURL, field, "%s is not an absolute http or https display link", field)
	case parsed.User != nil:
		c.errorf(ErrorCodeInvalidURL, field, "%s carries credentials; a display link never does", field)
	}
}

func (c *checker) timestamp(field, value string, required bool) {
	if value == "" {
		if required {
			c.errorf(ErrorCodeRequired, field, "%s is required", field)
		}
		return
	}
	if !timestampPattern.MatchString(value) {
		c.errorf(ErrorCodeInvalidTimestamp, field, "%s is not an RFC 3339 timestamp", field)
	}
}

func (c *checker) identity(field string, identity MemoryIdentity) {
	c.optionalToken(field+".workspace", identity.Workspace)
	c.optionalToken(field+".repository", identity.Repository)
	c.optionalToken(field+".scope", identity.Scope)
	c.optionalToken(field+".mission", identity.Mission)
}

func (c *checker) sameMission(mission string, identity MemoryIdentity) {
	if identity.Mission != "" && identity.Mission != mission {
		c.errorf(ErrorCodeInvalidValue, "identity.mission", "identity.mission %q differs from mission %q", identity.Mission, mission)
	}
}

func (c *checker) selection(projects []string, impacted bool, baseline string) {
	if impacted && len(projects) > 0 {
		c.errorf(ErrorCodeInvalidValue, "projects", "projects and impacted select the same thing two ways: pass one")
	}
	if baseline != "" && !impacted {
		c.errorf(ErrorCodeInvalidValue, "baseline", "baseline applies to impacted only")
	}
	c.listBound("projects", len(projects))
	for i, project := range projects {
		c.token(fmt.Sprintf("projects[%d]", i), project)
	}
	c.optionalToken("baseline", baseline)
}

func (c *checker) precondition(field string, precondition Precondition) {
	switch {
	case precondition.ExpectedRevision != "" && precondition.MustNotExist:
		c.errorf(ErrorCodeInvalidPrecondition, field, "expectedRevision and mustNotExist are exclusive")
	case precondition.ExpectedRevision == "" && !precondition.MustNotExist:
		c.errorf(ErrorCodeInvalidPrecondition, field,
			"a write states expectedRevision, or mustNotExist to create; an unconditional write could overwrite another agent's")
	case precondition.ExpectedRevision != "":
		c.token(field+".expectedRevision", precondition.ExpectedRevision)
	}
}

func (c *checker) evidence(field string, items []Evidence) {
	c.listBound(field, len(items))
	for i, item := range items {
		entry := fmt.Sprintf("%s[%d]", field, i)
		if !slices.Contains(ValidEvidenceKinds, item.Kind) {
			c.errorf(ErrorCodeInvalidValue, entry+".kind", "unknown evidence kind %q", item.Kind)
		}
		if strings.TrimSpace(item.Locator) == "" {
			c.errorf(ErrorCodeRequired, entry+".locator", "evidence names its locator")
		} else {
			c.bounded(entry+".locator", item.Locator, MaxLocatorLength, true)
			if parsed, err := url.Parse(item.Locator); err == nil && parsed.User != nil {
				c.errorf(ErrorCodeInvalidURL, entry+".locator", "an evidence locator carries no credentials")
			}
		}
		if item.Digest != "" && !digestPattern.MatchString(item.Digest) {
			c.errorf(ErrorCodeInvalidValue, entry+".digest", "digest is sha256: plus 64 lowercase hexadecimal characters")
		}
	}
}

func (c *checker) task(field string, task Task) {
	c.ref(field+".ref", task.Ref)
	c.token(field+".revision", task.Revision)
	c.url(field+".url", task.URL)
	c.title(field+".title", task.Title)
	c.bytesLimit(field+".body", task.Body, MaxBodyBytes)
	c.taskState(field+".state", task.State)
	c.bounded(field+".providerState", task.ProviderState, MaxLabelLength, false)
	c.labels(field+".labels", task.Labels)
	c.names(field+".assignees", task.Assignees)
	if task.Parent != nil {
		c.ref(field+".parent", *task.Parent)
	}
	c.optionalToken(field+".holder", task.Holder)
	c.timestamp(field+".updatedAt", task.UpdatedAt, false)
}

func (c *checker) proposal(field string, proposal Proposal) {
	c.ref(field+".ref", proposal.Ref)
	c.token(field+".revision", proposal.Revision)
	c.url(field+".url", proposal.URL)
	c.change(field+".change", proposal.Change, true)
	c.title(field+".title", proposal.Title)
	c.bytesLimit(field+".body", proposal.Body, MaxBodyBytes)
	if !slices.Contains(ValidProposalStates, proposal.State) {
		c.errorf(ErrorCodeInvalidState, field+".state", "unknown proposal state %q", proposal.State)
	}
	c.bounded(field+".providerState", proposal.ProviderState, MaxLabelLength, false)
	c.labels(field+".labels", proposal.Labels)
	c.names(field+".assignees", proposal.Assignees)
	c.timestamp(field+".updatedAt", proposal.UpdatedAt, false)
}

func (c *checker) checks(field string, checks Checks) {
	if !slices.Contains(ValidChecksStates, checks.State) {
		c.errorf(ErrorCodeInvalidState, field+".state", "unknown checks state %q", checks.State)
	}
	if checks.Commit != "" {
		c.commit(field+".commit", checks.Commit)
	}
	if checks.State == ChecksStateUnsupported && len(checks.Items) > 0 {
		c.errorf(ErrorCodeInvalidState, field+".items", "a provider without hosted checks reports no check")
	}
	if checks.State == ChecksStateUnsupported && strings.TrimSpace(checks.Detail) == "" {
		c.errorf(ErrorCodeRequired, field+".detail", "unsupported checks say why")
	}
	c.bytesLimit(field+".detail", checks.Detail, MaxTitleLength*4)
	c.listBound(field+".items", len(checks.Items))
	for i, check := range checks.Items {
		entry := fmt.Sprintf("%s.items[%d]", field, i)
		c.bounded(entry+".name", check.Name, MaxTitleLength, true)
		if !slices.Contains(ValidCheckStates, check.State) {
			c.errorf(ErrorCodeInvalidState, entry+".state", "unknown check state %q", check.State)
		}
		c.url(entry+".url", check.URL)
	}
}

func (c *checker) review(field string, review Review) {
	c.ref(field+".ref", review.Ref)
	c.url(field+".url", review.URL)
	c.reviewVerdict(field+".verdict", review.Verdict)
	if review.Commit != "" {
		c.commit(field+".commit", review.Commit)
	}
	c.optionalToken(field+".author", review.Author)
	c.timestamp(field+".submittedAt", review.SubmittedAt, false)
}

func (c *checker) record(field string, record MemoryRecord) {
	c.ref(field+".ref", record.Ref)
	c.token(field+".revision", record.Revision)
	if !slices.Contains(ValidMemoryKinds, record.Kind) {
		c.errorf(ErrorCodeInvalidValue, field+".kind", "unknown memory kind %q", record.Kind)
	}
	c.identity(field+".identity", record.Identity)
	if record.Kind == MemoryKindMission && record.Identity.Mission == "" {
		c.errorf(ErrorCodeRequired, field+".identity.mission", "a mission record names its mission")
	}
	c.bounded(field+".title", record.Title, MaxTitleLength, false)
	c.bytesLimit(field+".content", record.Content, MaxContentBytes)
	c.refs(field+".sources", record.Sources)
	c.evidence(field+".evidence", record.Evidence)
	c.timestamp(field+".provenance.recordedAt", record.Provenance.RecordedAt, true)
	c.optionalToken(field+".provenance.recordedBy", record.Provenance.RecordedBy)
	c.timestamp(field+".freshness.updatedAt", record.Freshness.UpdatedAt, true)
	c.timestamp(field+".freshness.retrievedAt", record.Freshness.RetrievedAt, true)
}

// isToken reports whether value is a bounded run of printable characters
// without whitespace.
func isToken(value string) bool {
	if value == "" || utf8.RuneCountInString(value) > MaxTokenLength {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// formatVersions renders a version list for a message.
func formatVersions(versions []int) string {
	parts := make([]string, len(versions))
	for i, v := range versions {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}
