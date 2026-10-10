package runtimecli

// env_enable.go backs `putnami cloud env enable [<env>] [--source-revision
// <sha>]`: it runs the readiness read `env doctor` prints, applies
// the fix of every workspace-owned row that is missing, then reads the table
// again and prints it. It exits 1 when a step failed or a row is still
// missing.
//
// WHAT IT WRITES. Only what the workspace owns and control-api names:
//
//   - the Put and OCI namespace bindings, activated under the namespace
//     putnami.ci.json declares, with the idempotency key <ns>-<protocol>;
//   - the environment definition, accepted at --source-revision or, by
//     default, at the default-branch head Source resolves on the server (the
//     checkout's own head is never sent, so a stale clone cannot accept an
//     old declaration), with expectedRevision read from the readiness
//     response (a compare-and-set: a stale value 409s instead of replaying);
//   - one Runtime operation grant per caller control-api lists as required,
//     and the binding grant for config-api, each read first for its current
//     revision (the same compare-and-set).
//
// WHAT IT ONLY REPORTS. The checkout rows (oci-registry, ci-envs,
// publish-namespaces) are edits to committed files, and the database and
// identity rows (database-ownership, project-identity) belong to the platform:
// each prints its fix and is never executed here.
//
// IDEMPOTENT. A row that is ok makes no write, so a second run writes
// nothing. A grant that is already enabled is skipped after its read. The
// environment definition is the one row asked again when ok: the accept route
// replays a commit it accepted before and writes nothing. Any other commit,
// including one with the same putnami.ci.json, appends a revision.
//
// AUTHORIZATION. Every write needs platform.workspace.manage in the
// workspace. The readiness read probes it first (probe=manage) and the
// command refuses before its first write when the session lacks it. A
// control plane too old to answer the probe does not refuse; the first write
// a provider refuses with 403 stops the run with the same message.
//
// Every call goes through the providers' generated clients: control-api for
// the readiness read and the acceptance, distribution-api for the bindings,
// runtime-api for the grants. They are all reached through the control-plane
// host, which routes by path.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
	distributionapiclient "go.putnami.dev/cloud/clients/distribution-api/go"
	runtimeapiclient "go.putnami.dev/cloud/clients/runtime-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Step statuses.
const (
	// EnvEnableApplied: the step made its write and the provider accepted it.
	EnvEnableApplied = "applied"
	// EnvEnableSkipped: the prerequisite was already met; nothing was written.
	EnvEnableSkipped = "skipped"
	// EnvEnableFailed: the step wrote or tried to, and the provider refused or
	// did not answer. Detail names the cause.
	EnvEnableFailed = "failed"
	// EnvEnableReported: the row is not one this command writes (a checkout
	// edit, a platform-owned identity, or a row that could not be checked).
	// Detail and Fix say what to do.
	EnvEnableReported = "reported"
)

// EnvEnableManagePermission is the permission every write needs.
const EnvEnableManagePermission = "platform.workspace.manage"

// envEnableWriteTimeout bounds each write.
const envEnableWriteTimeout = 60 * time.Second

// envEnableSourceRevisionPattern is the immutable revision the acceptance
// route accepts: a full git or sha256 object id.
var envEnableSourceRevisionPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// EnvEnableStep is one converged prerequisite. Row is the doctor row it
// converges; Action names the write in the provider's terms.
type EnvEnableStep struct {
	Row    string `json:"row"`
	Action string `json:"action"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// EnvEnableResult is the --output data. Ready is the readiness verdict after
// the writes with no failed step; Readiness is the same table `env doctor`
// prints, read after the writes.
type EnvEnableResult struct {
	Workspace   string          `json:"workspace"`
	Environment string          `json:"environment"`
	Ready       bool            `json:"ready"`
	Applied     int             `json:"applied"`
	Failed      int             `json:"failed"`
	Steps       []EnvEnableStep `json:"steps"`
	Readiness   EnvDoctorResult `json:"readiness"`
}

// envEnableClients are the generated clients the writes go through, bound to
// the workspace's control-plane host.
type envEnableClients struct {
	control      *controlapiclient.ControlClient
	distribution *distributionapiclient.DistributionClient
	runtime      *runtimeapiclient.RuntimeClient
}

func newEnvEnableClients(ctx *clicore.WorkspaceContext) (envEnableClients, error) {
	control, err := controlClient(ctx)
	if err != nil {
		return envEnableClients{}, err
	}
	distribution, err := clicore.NewServiceClient[distributionapiclient.DistributionClient](distributionapiclient.RegisterDistributionClient, ctx.ServiceBinding())
	if err != nil {
		return envEnableClients{}, err
	}
	runtime, err := clicore.NewServiceClient[runtimeapiclient.RuntimeClient](runtimeapiclient.RegisterRuntimeClient, ctx.ServiceBinding())
	if err != nil {
		return envEnableClients{}, err
	}
	return envEnableClients{control: control, distribution: distribution, runtime: runtime}, nil
}

// envEnable is the command. It resolves the environment the way doctor does,
// refuses a malformed --source-revision before any call, and needs a
// resolved session: without one there is nothing to converge.
func envEnable(params map[string]any, positional, workspaceRoot string, env map[string]string, ioctx clicore.IO, publish EnvDoctorPublish) error {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return err
	}
	environment := clicore.FirstString(positional, clicore.StringParam(params, "env", "environment"),
		clicore.StringValue(link["environment"]), EnvDoctorDefaultEnvironment)
	if !envDoctorEnvironmentPattern.MatchString(environment) {
		return clicore.NewError(fmt.Sprintf("environment %q must be 1-64 letters, digits, '.', '_' or '-'", environment), clicore.ExitUsage)
	}
	sourceRevision := strings.TrimSpace(clicore.StringParam(params, "source-revision", "sourceRevision"))
	if sourceRevision != "" && !envEnableSourceRevisionPattern.MatchString(sourceRevision) {
		return clicore.NewError("--source-revision must be a full commit id (40 hex characters); leave it out to accept the default-branch head", clicore.ExitUsage)
	}
	local, workloads, namespace := envDoctorCheckout(workspaceRoot, environment, publish)
	reqCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	wctx, err := clicore.ResolveWorkspaceContextFromLink(params, link, env, ioctx)
	if err != nil {
		return err
	}
	clients, err := newEnvEnableClients(wctx)
	if err != nil {
		return err
	}
	before := fetchEnvReadiness(reqCtx, wctx, environment, workloads, true)
	if !before.answered {
		return clicore.NewError(fmt.Sprintf("environment %s cannot be enabled: %s", environment, envEnableReadFailure(before.rows)), clicore.ExitFailure)
	}
	if before.canManage != nil && !*before.canManage {
		return envEnableRefusal(wctx.WorkspaceID)
	}
	run := envEnableRun{
		reqCtx: reqCtx, wctx: wctx, clients: clients, environment: environment,
		namespace: namespace, sourceRevision: sourceRevision, acceptedRevision: before.acceptedRevision,
		callers: before.requiredCallers,
	}
	result := EnvEnableResult{Workspace: wctx.WorkspaceID, Environment: environment}
	rows := envDoctorMerge(wctx.WorkspaceID, environment, local, before.rows).Rows
	for _, row := range rows {
		steps, refused := run.converge(row)
		result.Steps = append(result.Steps, steps...)
		if refused {
			return clicore.WithResultData(envEnableRefusal(wctx.WorkspaceID), envEnableCount(result))
		}
	}
	after := fetchEnvReadiness(reqCtx, wctx, environment, workloads, false)
	result.Readiness = envDoctorMerge(wctx.WorkspaceID, environment, local, after.rows)
	result.Readiness.Ready = result.Readiness.Ready && after.answered
	return writeEnvEnable(params, ioctx, envEnableCount(result))
}

// envEnableReadFailure is the one reason every server row carries when the
// readiness read failed.
func envEnableReadFailure(rows []EnvDoctorRow) string {
	for _, row := range rows {
		if row.Status == EnvDoctorUnknown && row.Detail != "" {
			return row.Detail
		}
	}
	return "control-api did not answer the readiness read"
}

// envEnableRefusal is the one-line refusal for a session without the manage
// permission. Nothing was written after it.
func envEnableRefusal(workspaceID string) error {
	return clicore.NewError(fmt.Sprintf("the session does not hold %s in workspace %s; ask a workspace owner to run `putnami cloud env enable` (no further write was made)",
		EnvEnableManagePermission, workspaceID), clicore.ExitAuth)
}

// envEnableCount fills the counts and the verdict from the steps and the
// readiness table.
func envEnableCount(result EnvEnableResult) EnvEnableResult {
	result.Applied, result.Failed = 0, 0
	for _, step := range result.Steps {
		switch step.Status {
		case EnvEnableApplied:
			result.Applied++
		case EnvEnableFailed:
			result.Failed++
		}
	}
	result.Ready = result.Failed == 0 && result.Readiness.Ready
	return result
}

// envEnableRun carries what every step needs.
type envEnableRun struct {
	reqCtx           context.Context
	wctx             *clicore.WorkspaceContext
	clients          envEnableClients
	environment      string
	namespace        string
	sourceRevision   string
	acceptedRevision int64
	callers          []envRequiredCaller
}

// converge turns one doctor row into its steps. refused reports a 403 on a
// write: the caller stops the run.
func (r envEnableRun) converge(row EnvDoctorRow) (steps []EnvEnableStep, refused bool) {
	switch row.ID {
	case "put-binding", "oci-binding":
		protocol := strings.TrimSuffix(row.ID, "-binding")
		step, refused := r.activateBinding(row, protocol)
		return []EnvEnableStep{step}, refused
	case "environment-definition":
		step, refused := r.acceptDefinition(row)
		return []EnvEnableStep{step}, refused
	case "runtime-operation-grants":
		return r.grants(row, "operation")
	case "runtime-binding-grant-config":
		return r.grants(row, "binding")
	default:
		return []EnvEnableStep{r.report(row, "not written by this command")}, false
	}
}

// report is a step this command never executes: ok rows say so; the others
// carry the row's own fix.
func (r envEnableRun) report(row EnvDoctorRow, action string) EnvEnableStep {
	step := EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableReported, Detail: row.Detail, Fix: row.Fix}
	if row.Status == EnvDoctorOK {
		step.Status, step.Fix = EnvEnableSkipped, ""
	}
	return step
}

// unchecked is a row the server could not judge: this command does not write
// on a guess.
func (r envEnableRun) unchecked(row EnvDoctorRow, action string) EnvEnableStep {
	return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableReported,
		Detail: "not written: the row could not be checked (" + row.Detail + ")", Fix: row.Fix}
}

func (r envEnableRun) activateBinding(row EnvDoctorRow, protocol string) (EnvEnableStep, bool) {
	action := "activate the " + protocol + " namespace binding"
	switch row.Status {
	case EnvDoctorOK:
		return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableSkipped, Detail: row.Detail}, false
	case EnvDoctorMissing:
	default:
		return r.unchecked(row, action), false
	}
	if r.namespace == "" {
		return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableFailed,
			Detail: "putnami.ci.json names no distribution.namespace to bind", Fix: row.Fix}, false
	}
	key := r.namespace + "-" + protocol
	namespace := r.namespace
	writeCtx, cancel := context.WithTimeout(r.reqCtx, envEnableWriteTimeout)
	defer cancel()
	result, err := clicore.CallWithSession(writeCtx, r.wctx, func(callCtx context.Context) (*distributionapiclient.CreateV1WorkspacesDistributionBindingsResult, error) {
		return r.clients.distribution.CreateV1WorkspacesDistributionBindings(callCtx, distributionapiclient.CreateV1WorkspacesDistributionBindingsInput{
			Path: distributionapiclient.CreateV1WorkspacesDistributionBindingsPath{Workspace: r.wctx.WorkspaceID},
			Body: distributionapiclient.ActivateBindingRequest{Protocol: &protocol, Namespace: &namespace, IdempotencyKey: &key},
		})
	})
	if err != nil {
		return r.failed(row, action, "distribution-api", err)
	}
	if result == nil || result.Status != http.StatusCreated {
		status := 0
		if result != nil {
			status = result.Status
		}
		return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableFailed,
			Detail: fmt.Sprintf("distribution-api answered status %d, want 201", status), Fix: row.Fix}, false
	}
	return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableApplied,
		Detail: fmt.Sprintf("activated %s namespace %s (idempotency key %s)", protocol, namespace, key)}, false
}

// acceptDefinition accepts the environment definition at --source-revision
// or at the default-branch head. It also runs when the row is ok: the row is
// ok once any accepted definition declares the environment, so it cannot tell
// whether putnami.ci.json changed since. The accept route decides by commit,
// not by content: a commit it accepted before replays its receipt and writes
// nothing, and any other commit appends the next revision, even when its
// putnami.ci.json is unchanged. The step is skipped on a replay and applied on
// an append. A replay of an older commit names the latest revision, which
// stays the one that applies.
func (r envEnableRun) acceptDefinition(row EnvDoctorRow) (EnvEnableStep, bool) {
	action := "accept the environment definition"
	switch row.Status {
	case EnvDoctorOK, EnvDoctorMissing:
	default:
		return r.unchecked(row, action), false
	}
	expected := r.acceptedRevision
	body := controlapiclient.SourceAcceptInput{ExpectedRevision: &expected}
	source := "the default-branch head"
	if r.sourceRevision != "" {
		revision := r.sourceRevision
		body.SourceRevision = &revision
		source = "--source-revision"
	}
	writeCtx, cancel := context.WithTimeout(r.reqCtx, envEnableWriteTimeout)
	defer cancel()
	receipt, err := clicore.CallWithSession(writeCtx, r.wctx, func(callCtx context.Context) (*controlapiclient.EnvironmentDefinitionAcceptance, error) {
		return r.clients.control.CreateV1WorkspacesEnvironmentDefinitionsAccept(callCtx, controlapiclient.CreateV1WorkspacesEnvironmentDefinitionsAcceptInput{
			Path: controlapiclient.CreateV1WorkspacesEnvironmentDefinitionsAcceptPath{Workspace: r.wctx.WorkspaceID},
			Body: body,
		})
	})
	if err != nil {
		return r.failed(row, action, "control-api", err)
	}
	if receipt == nil {
		return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableFailed, Detail: "control-api answered no acceptance receipt", Fix: row.Fix}, false
	}
	revision, accepted := clicore.Deref(receipt.Revision), envDoctorPrintable(clicore.Deref(receipt.SourceRevision))
	if revision <= expected {
		detail := fmt.Sprintf("source revision %s (%s) is already accepted as revision %d", accepted, source, revision)
		if revision < expected {
			detail += fmt.Sprintf("; the latest accepted revision stays %d", expected)
		}
		return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableSkipped, Detail: detail}, false
	}
	return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableApplied,
		Detail: fmt.Sprintf("accepted revision %d at source revision %s (%s; expected revision %d)", revision, accepted, source, expected)}, false
}

// grants converges every required caller of one kind. An ok row is skipped
// without a read. A missing or unknown row reads each grant here, with the
// session's own bearer, because control-api may not hold a forwarded
// credential to read Runtime while this session does.
func (r envEnableRun) grants(row EnvDoctorRow, kind string) ([]EnvEnableStep, bool) {
	action := "enable the Runtime " + kind + " grant"
	if row.Status == EnvDoctorOK {
		return []EnvEnableStep{{Row: row.ID, Action: action, Status: EnvEnableSkipped, Detail: row.Detail}}, false
	}
	var callers []string
	for _, caller := range r.callers {
		if caller.Kind == kind {
			callers = append(callers, caller.Caller)
		}
	}
	if len(callers) == 0 {
		return []EnvEnableStep{{Row: row.ID, Action: action, Status: EnvEnableFailed,
			Detail: "the control plane names no required " + kind + " caller; upgrade control-api, or apply the fix by hand", Fix: row.Fix}}, false
	}
	steps := make([]EnvEnableStep, 0, len(callers))
	for _, caller := range callers {
		step, refused := r.grant(row, kind, caller)
		steps = append(steps, step)
		if refused {
			return steps, true
		}
	}
	return steps, false
}

// grant reads one caller's grant, then enables it at the revision it read.
// Runtime answers 404 for a caller that has no row; that is revision 0.
func (r envEnableRun) grant(row EnvDoctorRow, kind, caller string) (EnvEnableStep, bool) {
	action := "enable the Runtime " + kind + " grant for " + caller
	readCtx, cancel := context.WithTimeout(r.reqCtx, envEnableWriteTimeout)
	defer cancel()
	enabled, revision, err := r.readGrant(readCtx, kind, caller)
	if err != nil {
		step, _ := r.failed(row, action, "runtime-api (read)", err)
		return step, false
	}
	if enabled {
		return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableSkipped,
			Detail: fmt.Sprintf("already enabled (revision %d)", revision)}, false
	}
	written, err := r.writeGrant(readCtx, kind, caller, revision)
	if err != nil {
		return r.failed(row, action, "runtime-api", err)
	}
	return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableApplied,
		Detail: fmt.Sprintf("enabled at revision %d (expected revision %d)", written, revision)}, false
}

// grantPath is the generated path shape of the four grant operations (the
// field order is the generated one, so the conversions below hold).
type grantPath struct {
	Caller    string
	Workspace string
}

func (r envEnableRun) readGrant(ctx context.Context, kind, caller string) (enabled bool, revision int64, err error) {
	path := grantPath{Caller: caller, Workspace: r.wctx.WorkspaceID}
	var enabledPtr *bool
	var revisionPtr *int64
	switch kind {
	case "operation":
		grant, callErr := clicore.CallWithSession(ctx, r.wctx, func(callCtx context.Context) (*runtimeapiclient.Grant2, error) {
			return r.clients.runtime.GetV1WorkspacesRuntimeOperationGrants(callCtx, runtimeapiclient.GetV1WorkspacesRuntimeOperationGrantsInput{
				Path: runtimeapiclient.GetV1WorkspacesRuntimeOperationGrantsPath(path),
			})
		})
		err = callErr
		if grant != nil {
			enabledPtr, revisionPtr = grant.Enabled, grant.Revision
		}
	default:
		grant, callErr := clicore.CallWithSession(ctx, r.wctx, func(callCtx context.Context) (*runtimeapiclient.Grant, error) {
			return r.clients.runtime.GetV1WorkspacesRuntimeBindingGrants(callCtx, runtimeapiclient.GetV1WorkspacesRuntimeBindingGrantsInput{
				Path: runtimeapiclient.GetV1WorkspacesRuntimeBindingGrantsPath(path),
			})
		})
		err = callErr
		if grant != nil {
			enabledPtr, revisionPtr = grant.Enabled, grant.Revision
		}
	}
	if clicore.ServiceStatus(err) == http.StatusNotFound {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return clicore.Deref(enabledPtr), clicore.Deref(revisionPtr), nil
}

func (r envEnableRun) writeGrant(ctx context.Context, kind, caller string, expected int64) (int64, error) {
	enabled := true
	path := grantPath{Caller: caller, Workspace: r.wctx.WorkspaceID}
	switch kind {
	case "operation":
		grant, err := clicore.CallWithSession(ctx, r.wctx, func(callCtx context.Context) (*runtimeapiclient.Grant2, error) {
			return r.clients.runtime.UpdateV1WorkspacesRuntimeOperationGrants(callCtx, runtimeapiclient.UpdateV1WorkspacesRuntimeOperationGrantsInput{
				Path: runtimeapiclient.UpdateV1WorkspacesRuntimeOperationGrantsPath(path),
				Body: runtimeapiclient.PutOperationGrant{Enabled: &enabled, ExpectedRevision: &expected},
			})
		})
		if err != nil {
			return 0, err
		}
		return clicore.Deref(grant.Revision), nil
	default:
		grant, err := clicore.CallWithSession(ctx, r.wctx, func(callCtx context.Context) (*runtimeapiclient.Grant, error) {
			return r.clients.runtime.UpdateV1WorkspacesRuntimeBindingGrants(callCtx, runtimeapiclient.UpdateV1WorkspacesRuntimeBindingGrantsInput{
				Path: runtimeapiclient.UpdateV1WorkspacesRuntimeBindingGrantsPath(path),
				Body: runtimeapiclient.PutBindingGrant{Enabled: &enabled, ExpectedRevision: &expected},
			})
		})
		if err != nil {
			return 0, err
		}
		return clicore.Deref(grant.Revision), nil
	}
}

// failed is a step whose write the provider refused or did not answer. A 403
// is the manage permission missing: the run stops there.
func (r envEnableRun) failed(row EnvDoctorRow, action, provider string, err error) (EnvEnableStep, bool) {
	status := clicore.ServiceStatus(err)
	detail := provider + " refused: " + clicore.ServiceFailureReason(err)
	if message := clicore.ServiceMessage(err); message != "" {
		detail += ": " + envDoctorPrintable(message)
	}
	switch status {
	case http.StatusForbidden:
		detail = provider + " refused the write (403): the session lacks " + EnvEnableManagePermission
	case http.StatusConflict:
		detail += " (the revision moved; run `putnami cloud env enable` again)"
	}
	return EnvEnableStep{Row: row.ID, Action: action, Status: EnvEnableFailed, Detail: detail, Fix: row.Fix}, status == http.StatusForbidden
}

// writeEnvEnable prints the steps and the readiness table, and returns the
// exit decision: exit 1 when a step failed or a row is still missing, with
// the result on the failure envelope.
func writeEnvEnable(params map[string]any, ioctx clicore.IO, result EnvEnableResult) error {
	if result.Ready {
		clicore.WriteResult(result, params, ioctx, renderEnvEnable(result))
		return nil
	}
	if !clicore.StructuredOutput(params) {
		clicore.WriteTextLines(ioctx, renderEnvEnable(result))
	}
	return clicore.WithResultData(clicore.NewError(fmt.Sprintf("environment %s is not enabled: %d step(s) failed; %s",
		result.Environment, result.Failed, envDoctorCounts(result.Readiness)), clicore.ExitFailure), result)
}

// renderEnvEnable is the human view: the steps, one per line with the fix of
// each failed or reported step beneath it, then the doctor table.
//
//	Enable prod of workspace ws-acme
//
//	applied   put-binding             activated put namespace acme (idempotency key acme-put)
//	skipped   oci-binding             active oci namespace acme
//	reported  database-ownership      objects owned outside the database owner role…
//	                                  fix: the platform re-owns them at bootstrap…
//
//	2 applied, 0 failed
//
//	Environment prod of workspace ws-acme
//	…
func renderEnvEnable(result EnvEnableResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Enable %s of workspace %s\n\n", result.Environment, result.Workspace)
	rowWidth := 0
	for _, step := range result.Steps {
		rowWidth = max(rowWidth, len(step.Row))
	}
	indent := strings.Repeat(" ", 9+rowWidth+2)
	for _, step := range result.Steps {
		fmt.Fprintf(&b, "%-8s  %-*s  %s\n", step.Status, rowWidth, step.Row, statusOneLine(step.Action+": "+step.Detail))
		if step.Status == EnvEnableSkipped || step.Status == EnvEnableApplied || strings.TrimSpace(step.Fix) == "" {
			continue
		}
		for i, line := range strings.Split(strings.TrimRight(step.Fix, "\n"), "\n") {
			prefix := "fix: "
			if i > 0 {
				prefix = "     "
			}
			b.WriteString(indent + prefix + line + "\n")
		}
	}
	fmt.Fprintf(&b, "\n%d applied, %d failed\n\n", result.Applied, result.Failed)
	b.WriteString(renderEnvDoctor(result.Readiness))
	return b.String()
}
