package runtimecli

// env_doctor.go backs `putnami cloud env doctor [<env>]`: a read-only
// preflight that prints one row per continuous-delivery prerequisite, each ok,
// missing or unknown, with the fix for every row that is not ok. It prints the
// shared status node and exits by the shared rule: 1 when any row is
// missing, 0 otherwise. An unknown row could not be checked, so it is counted
// and printed but does not fail the command unless --strict asks for every
// row ok; a control-api that does not answer leaves its rows unknown.
// --output=json carries the node: one child per row, and the counts as
// metrics.
//
// WHERE EACH ROW COMES FROM. Control computes the server rows at
// GET /v1/workspaces/{workspace}/environments/{environment}/readiness, because
// the caller identities they check come from the control plane's own
// configuration, never from the CLI. The checkout rows (the workspace's OCI
// registry, the CI environment, the publish namespaces) come from the checkout
// (env_doctor_checkout.go). The publish-namespaces row reads Config's and
// Data's manifest conventions, which this lib does not import, so the CLI
// workload supplies it as an EnvDoctorPublish.
//
// WHY THIS LIB. runtimecli already calls control-api through its generated
// client (status, deploy), so the new call adds no dependency edge.
//
// NOTHING HERE WRITES. The command reads files and makes one GET. A server
// that cannot answer turns the server rows unknown; the checkout rows still
// print.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// EnvDoctorDefaultEnvironment is the environment checked when neither the
// command nor the workspace link names one.
const EnvDoctorDefaultEnvironment = "prod"

// Row statuses, the same three words the server answers.
const (
	EnvDoctorOK      = "ok"
	EnvDoctorMissing = "missing"
	EnvDoctorUnknown = "unknown"
)

// Checkout row identifiers.
const (
	EnvDoctorRowOCIRegistry       = "oci-registry"
	EnvDoctorRowCIEnvs            = "ci-envs"
	EnvDoctorRowPublishNamespaces = "publish-namespaces"
)

// EnvDoctorPublishNamespacesTitle is the title of the publish-namespaces row,
// shared with the EnvDoctorPublish that answers it.
const EnvDoctorPublishNamespacesTitle = "Config and migration publish namespaces"

// envDoctorTimeout bounds the one readiness read.
const envDoctorTimeout = 60 * time.Second

// envDoctorOrder is every row in the order the prerequisites fail during a
// first deploy. Server rows keep their server ids.
var envDoctorOrder = []string{
	"put-binding",
	EnvDoctorRowOCIRegistry,
	"oci-binding",
	"environment-definition",
	EnvDoctorRowCIEnvs,
	EnvDoctorRowPublishNamespaces,
	"runtime-operation-grants",
	"runtime-binding-grant-config",
	"database-ownership",
	"project-identity",
}

// envDoctorServerTitles names the server rows when the server could not
// answer, so the table still lists every prerequisite.
var envDoctorServerTitles = map[string]string{
	"put-binding":                  "Put namespace binding",
	"oci-binding":                  "OCI namespace binding",
	"environment-definition":       "Accepted environment definition",
	"runtime-operation-grants":     "Runtime operation grants",
	"runtime-binding-grant-config": "Runtime binding grant for config-api",
	"database-ownership":           "Database object ownership",
	"project-identity":             "Project identity keys",
}

// envDoctorEnvironmentPattern matches the server's environment rule, so a
// malformed name fails locally with a usage error instead of a 400.
var envDoctorEnvironmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// EnvDoctorRow is one prerequisite. Fix is empty only when Status is ok.
type EnvDoctorRow struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// EnvDoctorWorkload is one workload the environment selects, as the checkout
// declares it. Name is its manifest name; Migrated reports that it publishes
// a migration bundle.
type EnvDoctorWorkload struct {
	Path     string
	Name     string
	Migrated bool
}

// EnvDoctorPublish answers the publish-namespaces row for the selected
// workloads and returns them with Migrated set. The CLI workload supplies it:
// the namespace options and the migration bundle location are Config's and
// Data's conventions. namespace is the workspace's distribution namespace
// when putnami.ci.json names one, for the fix text.
type EnvDoctorPublish func(workspaceRoot, namespace string, workloads []EnvDoctorWorkload) (EnvDoctorRow, []EnvDoctorWorkload)

// EnvDoctorResult is the --output data: stable keys, rows in envDoctorOrder.
// Ready means no row is missing and control-api answered. Missing counts the
// missing rows; Unchecked counts the rows that could not be checked (every
// status but ok and missing).
type EnvDoctorResult struct {
	Workspace   string         `json:"workspace"`
	Environment string         `json:"environment"`
	Ready       bool           `json:"ready"`
	Missing     int            `json:"missing"`
	Unchecked   int            `json:"unchecked"`
	Rows        []EnvDoctorRow `json:"rows"`
}

// Env dispatches `putnami cloud env <subcommand>`.
func Env(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO, publish EnvDoctorPublish) error {
	pos := envPositionals(args)
	if len(pos) == 0 || pos[0] == "help" {
		return envHelp(params, ioctx)
	}
	switch pos[0] {
	case "doctor":
		if len(pos) > 2 {
			return clicore.NewError("cloud env doctor takes at most one environment", clicore.ExitUsage)
		}
		positional := ""
		if len(pos) == 2 {
			positional = pos[1]
		}
		return envDoctor(params, positional, workspaceRoot, env, ioctx, publish)
	case "status":
		return EnvStatus(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	case "enable":
		if len(pos) > 2 {
			return clicore.NewError("cloud env enable takes at most one environment", clicore.ExitUsage)
		}
		positional := ""
		if len(pos) == 2 {
			positional = pos[1]
		}
		return envEnable(params, positional, workspaceRoot, env, ioctx, publish)
	default:
		return clicore.NewError("unknown cloud env command; expected status, doctor or enable", clicore.ExitUsage)
	}
}

func envHelp(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "cloud env status [<env>]", "description": "compare what putnami.ci.json declares with what runs, per environment and workload, and print each channel's newest move with its release ids; exits 1 when a workload failed; --strict also fails on degraded or unknown; --health and --provenance print the deployment table of one environment (default: the linked environment, then prod)"},
		{"command": "cloud env doctor [<env>]", "description": "check every prerequisite continuous delivery needs for one environment; read-only; exits 1 when any is missing; --strict also fails on unknown"},
		{"command": "cloud env enable [<env>]", "description": "apply the fix of every workspace-owned prerequisite that is missing (namespace bindings, environment definition, Runtime grants), report the rest, then print the table again; idempotent; needs platform.workspace.manage; --source-revision <sha> accepts that commit instead of the default-branch head"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud env commands:")
	for _, command := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-26s %s", command["command"], command["description"]))
	}
	return nil
}

func envDoctor(params map[string]any, positional, workspaceRoot string, env map[string]string, ioctx clicore.IO, publish EnvDoctorPublish) error {
	result, err := collectEnvDoctor(params, positional, workspaceRoot, env, ioctx, publish)
	if err != nil {
		return err
	}
	return writeEnvDoctor(params, ioctx, result)
}

// collectEnvDoctor checks every prerequisite of one environment and returns the
// table without printing it.
func collectEnvDoctor(params map[string]any, positional, workspaceRoot string, env map[string]string, ioctx clicore.IO, publish EnvDoctorPublish) (EnvDoctorResult, error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return EnvDoctorResult{}, err
	}
	environment := clicore.FirstString(positional, clicore.StringParam(params, "env", "environment"),
		clicore.StringValue(link["environment"]), EnvDoctorDefaultEnvironment)
	if !envDoctorEnvironmentPattern.MatchString(environment) {
		return EnvDoctorResult{}, clicore.NewError(fmt.Sprintf("environment %q must be 1-64 letters, digits, '.', '_' or '-'", environment), clicore.ExitUsage)
	}
	local, workloads, _ := envDoctorCheckout(workspaceRoot, environment, publish)
	reqCtx, stop := runtimeCommandContext(ioctx)
	defer stop()
	wctx, err := clicore.ResolveWorkspaceContextFromLink(params, link, env, ioctx)
	var server []EnvDoctorRow
	answered := false
	workspaceID := clicore.StringValue(link["workspace_id"])
	if err != nil {
		server = envDoctorServerUnavailable("the workspace session could not be resolved: " + err.Error())
	} else {
		workspaceID = wctx.WorkspaceID
		readiness := fetchEnvReadiness(reqCtx, wctx, environment, workloads, false)
		server, answered = readiness.rows, readiness.answered
	}
	result := envDoctorMerge(workspaceID, environment, local, server)
	// A control plane that never answered checked nothing on the server side.
	// Calling that ready would pass a preflight that looked at half the table.
	result.Ready = result.Ready && answered
	// --strict is the gate for CI: every row must be ok. Without it an
	// unknown row (a machine token cannot read the Runtime grants, say) is
	// reported and counted but does not fail the command.
	if clicore.BoolParam(params, false, "strict") {
		result.Ready = result.Ready && result.Unchecked == 0
	}
	return result, nil
}

// envReadiness is one readiness read: the server rows, whether control-api
// answered, and the inputs `env enable` converges from (the callers to grant,
// the accepted revision the next acceptance must expect, and, when probed,
// whether the session may write).
type envReadiness struct {
	rows             []EnvDoctorRow
	answered         bool
	requiredCallers  []envRequiredCaller
	acceptedRevision int64
	// canManage is nil when the read did not probe it or the server does
	// not answer it (an older control-api).
	canManage *bool
}

// envRequiredCaller is one identity Runtime must grant, and the grant route
// it needs: "operation" or "binding".
type envRequiredCaller struct {
	Caller string
	Kind   string
}

// fetchEnvReadiness reads the server rows and reports whether control-api
// answered. A failed read turns every server row unknown with the reason.
// probeManage asks the server whether the session holds
// platform.workspace.manage in the workspace.
func fetchEnvReadiness(reqCtx context.Context, ctx *clicore.WorkspaceContext, environment string, workloads []EnvDoctorWorkload, probeManage bool) envReadiness {
	readCtx, cancel := context.WithTimeout(reqCtx, envDoctorTimeout)
	defer cancel()
	control, err := controlClient(ctx)
	if err != nil {
		return envReadiness{rows: envDoctorServerUnavailable("control-api could not be called: " + clicore.ServiceFailureReason(err))}
	}
	input := controlapiclient.GetV1WorkspacesEnvironmentsReadinessInput{
		Path: controlapiclient.GetV1WorkspacesEnvironmentsReadinessPath{Workspace: ctx.WorkspaceID, Environment: environment},
	}
	if list, migrated := envDoctorQuery(workloads); list != "" {
		input.Query.Workloads = &list
		if migrated != "" {
			input.Query.Migrated = &migrated
		}
	}
	if probeManage {
		probe := "manage"
		input.Query.Probe = &probe
	}
	resp, err := clicore.CallWithSession(readCtx, ctx, func(callCtx context.Context) (*controlapiclient.EnvironmentReadinessResponse, error) {
		return control.GetV1WorkspacesEnvironmentsReadiness(callCtx, input)
	})
	if err != nil {
		return envReadiness{rows: envDoctorServerUnavailable("control-api did not answer the readiness read: " + clicore.ServiceFailureReason(err))}
	}
	rows := clicore.Deref(resp.Rows)
	out := envReadiness{rows: make([]EnvDoctorRow, 0, len(rows)), answered: true, acceptedRevision: clicore.Deref(resp.AcceptedRevision)}
	if canManage, ok := resp.CallerCanManage.Value(); ok {
		out.canManage = &canManage
	}
	for _, row := range rows {
		out.rows = append(out.rows, envDoctorServerRow(EnvDoctorRow{
			ID: clicore.Deref(row.Id), Title: clicore.Deref(row.Title), Status: clicore.Deref(row.Status),
			Detail: clicore.Deref(row.Detail), Fix: clicore.Deref(row.Fix),
		}))
	}
	for _, caller := range clicore.Deref(resp.RequiredCallers) {
		email, kind := envDoctorPrintable(strings.TrimSpace(clicore.Deref(caller.Caller))), envDoctorPrintable(clicore.Deref(caller.Kind))
		if email != "" {
			out.requiredCallers = append(out.requiredCallers, envRequiredCaller{Caller: email, Kind: kind})
		}
	}
	return out
}

// envDoctorServerRow makes a server row safe to print and to judge. It drops
// control characters other than newline and tab, so a server string cannot
// drive the terminal, and it reads a status this CLI does not know as
// unknown, so a newer server can never turn a row ok by accident; only
// --strict makes that unknown row fail the command.
func envDoctorServerRow(row EnvDoctorRow) EnvDoctorRow {
	row.ID, row.Title = envDoctorPrintable(row.ID), envDoctorPrintable(row.Title)
	row.Status = envDoctorPrintable(row.Status)
	row.Detail, row.Fix = envDoctorPrintable(row.Detail), envDoctorPrintable(row.Fix)
	switch row.Status {
	case EnvDoctorOK, EnvDoctorMissing, EnvDoctorUnknown:
	default:
		row.Detail = fmt.Sprintf("the server answered status %q: %s", row.Status, row.Detail)
		row.Status = EnvDoctorUnknown
		if strings.TrimSpace(row.Fix) == "" {
			row.Fix = "upgrade the Putnami CLI, then run `putnami cloud env doctor` again"
		}
	}
	return row
}

func envDoctorPrintable(value string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

// envDoctorQuery encodes the selection the way the route reads it:
// `<path>=<manifest name>`, comma-separated, and the migrated paths. The
// name goes out even when it equals the path, because a bare `<path>` tells
// the route the name is not known; only a manifest without a name sends one.
func envDoctorQuery(workloads []EnvDoctorWorkload) (list, migrated string) {
	entries := make([]string, 0, len(workloads))
	var migratedPaths []string
	for _, workload := range workloads {
		entry := workload.Path
		if workload.Name != "" {
			entry += "=" + workload.Name
		}
		entries = append(entries, entry)
		if workload.Migrated {
			migratedPaths = append(migratedPaths, workload.Path)
		}
	}
	return strings.Join(entries, ","), strings.Join(migratedPaths, ",")
}

// envDoctorServerUnavailable is every server row, unknown for one reason.
func envDoctorServerUnavailable(reason string) []EnvDoctorRow {
	rows := make([]EnvDoctorRow, 0, len(envDoctorServerTitles))
	for _, id := range envDoctorOrder {
		title, server := envDoctorServerTitles[id]
		if !server {
			continue
		}
		rows = append(rows, EnvDoctorRow{
			ID: id, Title: title, Status: EnvDoctorUnknown, Detail: reason,
			Fix: "resolve the failure in the detail, then run `putnami cloud env doctor` again",
		})
	}
	return rows
}

// envDoctorMerge orders the checkout and server rows. A known row neither
// side answered is unknown; a row this CLI does not know yet (a newer
// server) keeps its place after the known ones.
func envDoctorMerge(workspaceID, environment string, local, server []EnvDoctorRow) EnvDoctorResult {
	byID := map[string]EnvDoctorRow{}
	var extra []EnvDoctorRow
	for _, row := range append(append([]EnvDoctorRow(nil), local...), server...) {
		if _, seen := byID[row.ID]; seen {
			continue
		}
		byID[row.ID] = row
		if !envDoctorKnown(row.ID) {
			extra = append(extra, row)
		}
	}
	result := EnvDoctorResult{Workspace: workspaceID, Environment: environment}
	for _, id := range envDoctorOrder {
		row, answered := byID[id]
		if !answered {
			title := envDoctorServerTitles[id]
			if title == "" {
				title = id
			}
			row = EnvDoctorRow{ID: id, Title: title, Status: EnvDoctorUnknown,
				Detail: "no check answered this row", Fix: "upgrade the Putnami CLI and control-api, then run `putnami cloud env doctor` again"}
		}
		result.Rows = append(result.Rows, row)
	}
	result.Rows = append(result.Rows, extra...)
	for _, row := range result.Rows {
		switch row.Status {
		case EnvDoctorOK:
		case EnvDoctorMissing:
			result.Missing++
		default:
			result.Unchecked++
		}
	}
	result.Ready = result.Missing == 0
	return result
}

func envDoctorKnown(id string) bool {
	for _, known := range envDoctorOrder {
		if known == id {
			return true
		}
	}
	return false
}

// writeEnvDoctor prints the result as a status node and returns the exit
// decision every status command shares: a missing row fails the
// command with exit 1; a row that could not be checked fails it only under
// --strict. Structured output holds the node, on success and failure alike.
func writeEnvDoctor(params map[string]any, ioctx clicore.IO, result EnvDoctorResult) error {
	return clicore.WriteStatus(params, ioctx, EnvDoctorStatusNodeFrom(result))
}

// envDoctorCounts is the summary both the verdict line and the error use.
func envDoctorCounts(result EnvDoctorResult) string {
	return fmt.Sprintf("%d missing, %d could not be checked", result.Missing, result.Unchecked)
}

// renderEnvDoctor is the human view: a heading, one line per row with the
// fix of each row that is not ok indented beneath it, and a summary line.
//
//	Environment prod of workspace ws-acme
//
//	ok       put-binding        active put namespace acme
//	missing  oci-registry       putnami.workspace.json has no registries.oci.publish
//	         fix: set "registries": {"oci": {"publish": "oci.putnami.dev/<ns>"}} in putnami.workspace.json
//
//	not ready: 1 missing, 2 could not be checked
func renderEnvDoctor(result EnvDoctorResult) string {
	var b strings.Builder
	workspace := result.Workspace
	if workspace == "" {
		workspace = "(unresolved)"
	}
	fmt.Fprintf(&b, "Environment %s of workspace %s\n\n", result.Environment, workspace)
	idWidth := 0
	for _, row := range result.Rows {
		idWidth = max(idWidth, len(row.ID))
	}
	indent := strings.Repeat(" ", 9+idWidth+2)
	for _, row := range result.Rows {
		fmt.Fprintf(&b, "%-7s  %-*s  %s\n", row.Status, idWidth, row.ID, statusOneLine(row.Detail))
		if row.Status == EnvDoctorOK || strings.TrimSpace(row.Fix) == "" {
			continue
		}
		for i, line := range strings.Split(strings.TrimRight(row.Fix, "\n"), "\n") {
			prefix := "fix: "
			if i > 0 {
				prefix = "     "
			}
			b.WriteString(indent + prefix + line + "\n")
		}
	}
	verdict := "ready"
	if !result.Ready {
		verdict = "not ready"
	}
	fmt.Fprintf(&b, "\n%s: %s\n", verdict, envDoctorCounts(result))
	return b.String()
}

// envPositionals is the non-flag arguments, skipping the parent CLI's
// --putnamiContext token and each value a string flag consumes.
func envPositionals(args []string) []string {
	var out []string
	skipNext := false
	for i, raw := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if raw == "--putnamiContext" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(raw, "--") {
			name := strings.TrimPrefix(raw, "--")
			if strings.Contains(name, "=") || strings.HasPrefix(name, "no-") || clicore.IsBooleanFlag(name) {
				continue
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				skipNext = true
			}
			continue
		}
		out = append(out, raw)
	}
	return out
}
