package configcli

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.putnami.dev/client"
	configapiclient "go.putnami.dev/cloud/clients/config-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// `putnami cloud telemetry show|set|reset [--app <app>]` reads and writes the
// telemetry settings Putnami Cloud keeps for a workspace and its services,
// through config-api's GET|PUT /v1/workspaces/{workspace}/config/telemetry.
// The workspace collector returns them on each export as the export directive
// of the telemetry/otlp-push-contract: while it holds, each trace setting it
// carries replaces the service's own, higher or lower, and its log level
// tightens the service's. A person tunes a running app without a redeploy.
//
// Without --app a command reads or writes the workspace row, which every
// service without a row of its own takes; with --app it reads or writes that
// service's row, named by the app's canonical name. config-api refuses a name
// that holds a folder until it accepts a workload's canonical name, and the
// collector does not send the trace budget yet.

// telemetryMaxTraces bounds the trace budget and floor, as config-api does.
const telemetryMaxTraces = 1_000_000

// telemetryLogLevels are the log levels config-api stores.
var telemetryLogLevels = []string{"debug", "info", "warn", "error"}

// TelemetrySettings is one row's fields, or the settings a service ends with.
// A nil field is unset: the directive leaves it out, and the service keeps its
// own setting.
type TelemetrySettings struct {
	TraceBudget         *int64   `json:"traceBudget,omitempty"`
	TraceRatio          *float64 `json:"traceRatio,omitempty"`
	TraceFloorPerMinute *int64   `json:"traceFloorPerMinute,omitempty"`
	LogLevel            *string  `json:"logLevel,omitempty"`
}

// empty reports whether every field is unset.
func (s TelemetrySettings) empty() bool {
	return s.TraceBudget == nil && s.TraceRatio == nil && s.TraceFloorPerMinute == nil && s.LogLevel == nil
}

// TelemetryService is one service's row and the settings it ends with, field
// by field over the workspace row.
type TelemetryService struct {
	Service   string            `json:"service"`
	Override  TelemetrySettings `json:"override"`
	Effective TelemetrySettings `json:"effective"`
	UpdatedAt *time.Time        `json:"updatedAt,omitempty"`
}

// TelemetryView is a workspace's telemetry settings, as config-api answers
// them; with --app, Services holds that service's row only.
type TelemetryView struct {
	WorkspaceID        string             `json:"workspaceId"`
	Workspace          TelemetrySettings  `json:"workspace"`
	WorkspaceUpdatedAt *time.Time         `json:"workspaceUpdatedAt,omitempty"`
	Services           []TelemetryService `json:"services"`
}

// Telemetry serves `putnami cloud telemetry`.
func Telemetry(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	positionals := clicore.Positionals(args)
	verb := ""
	if len(positionals) > 0 {
		verb = positionals[0]
	}
	switch verb {
	case "", "help":
		telemetryHelp(params, ioctx)
		return nil
	case "show", "set", "reset":
	default:
		return clicore.NewError("unknown cloud telemetry verb: "+verb+"; expected show, set or reset", clicore.ExitUsage)
	}
	if len(positionals) > 1 {
		return clicore.NewError("cloud telemetry "+verb+" takes no positional argument; name a service with --app", clicore.ExitUsage)
	}
	var update TelemetrySettings
	if verb == "set" {
		// The flags are read before anything else, so a value out of range
		// costs no call.
		var err error
		if update, err = telemetryFlags(params); err != nil {
			return err
		}
	}
	service, err := telemetryService(params, workspaceRoot)
	if err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	base := ioctx.Context
	if base == nil {
		base = context.Background()
	}
	reqCtx, stop := signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch verb {
	case "show":
		view, err := readTelemetry(reqCtx, ctx, verb)
		if err != nil {
			return err
		}
		printTelemetry(params, ioctx, view, service)
		return nil
	case "set":
		view, err := readTelemetry(reqCtx, ctx, verb)
		if err != nil {
			return err
		}
		row := mergeTelemetry(rowOf(view, service), update)
		if view, err = writeTelemetry(reqCtx, ctx, verb, service, row); err != nil {
			return err
		}
		printTelemetry(params, ioctx, view, service)
		return nil
	default:
		view, err := writeTelemetry(reqCtx, ctx, verb, service, TelemetrySettings{})
		if err != nil {
			return err
		}
		printTelemetry(params, ioctx, view, service)
		return nil
	}
}

func telemetryHelp(params map[string]any, ioctx clicore.IO) {
	commands := []map[string]string{
		{"command": "cloud telemetry show [--app <app>]", "description": "the workspace row, each service row and the settings each service ends with"},
		{"command": "cloud telemetry set [--app <app>] [--budget <n>] [--ratio <0..1>] [--floor <n>] [--log-level <level>]", "description": "write the given fields into the row and keep the others; the services apply the ratio, the floor and the log level within a few minutes"},
		{"command": "cloud telemetry reset [--app <app>]", "description": "remove the row: its services return to their own settings, or to the workspace row"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return
	}
	ioctx.Stdout("@putnami/cloud telemetry commands:")
	for _, c := range commands {
		ioctx.Stdout("  putnami " + c["command"])
		ioctx.Stdout("      " + c["description"])
	}
}

// telemetryService resolves --app to the service whose row a command reads or
// writes, or "" for the workspace row. The app's canonical name is the service
// its telemetry carries.
func telemetryService(params map[string]any, workspaceRoot string) (string, error) {
	if clicore.StringParam(params, "app", "appName") == "" {
		return "", nil
	}
	return clicore.ResolveApp(params, workspaceRoot)
}

// telemetryFlags reads the fields `set` writes, and refuses a value out of
// the range config-api stores before any call.
func telemetryFlags(params map[string]any) (TelemetrySettings, error) {
	var settings TelemetrySettings
	var err error
	if settings.TraceBudget, err = telemetryWhole(params, "budget"); err != nil {
		return settings, err
	}
	if settings.TraceFloorPerMinute, err = telemetryWhole(params, "floor"); err != nil {
		return settings, err
	}
	if raw, set := telemetryParam(params, "ratio"); set {
		ratio, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
			return settings, clicore.NewError("--ratio must be a number from 0 to 1, got "+raw, clicore.ExitUsage)
		}
		settings.TraceRatio = &ratio
	}
	if raw, set := telemetryParam(params, "log-level", "logLevel"); set {
		level := strings.ToLower(raw)
		if !slices.Contains(telemetryLogLevels, level) {
			return settings, clicore.NewError("--log-level must be debug, info, warn or error, got "+raw, clicore.ExitUsage)
		}
		settings.LogLevel = &level
	}
	if settings.empty() {
		return settings, clicore.NewError("cloud telemetry set needs at least one of --budget, --ratio, --floor or --log-level", clicore.ExitUsage)
	}
	return settings, nil
}

// telemetryParam returns the text of a flag and whether it was given. A
// number the flag parser read keeps its decimal form.
func telemetryParam(params map[string]any, names ...string) (string, bool) {
	value := clicore.Param(params, names...)
	switch v := value.(type) {
	case nil:
		return "", false
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		return strings.TrimSpace(clicore.StringValue(v)), true
	}
}

func telemetryWhole(params map[string]any, name string) (*int64, error) {
	raw, set := telemetryParam(params, name)
	if !set {
		return nil, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 || value > telemetryMaxTraces {
		return nil, clicore.NewError(fmt.Sprintf("--%s must be a whole number from 0 to %d, got %s", name, telemetryMaxTraces, raw), clicore.ExitUsage)
	}
	return &value, nil
}

// rowOf returns the stored row of service, or of the workspace for "".
func rowOf(view *TelemetryView, service string) TelemetrySettings {
	if service == "" {
		return view.Workspace
	}
	for _, row := range view.Services {
		if row.Service == service {
			return row.Override
		}
	}
	return TelemetrySettings{}
}

// mergeTelemetry returns row with each field update sets in its place.
func mergeTelemetry(row, update TelemetrySettings) TelemetrySettings {
	if update.TraceBudget != nil {
		row.TraceBudget = update.TraceBudget
	}
	if update.TraceRatio != nil {
		row.TraceRatio = update.TraceRatio
	}
	if update.TraceFloorPerMinute != nil {
		row.TraceFloorPerMinute = update.TraceFloorPerMinute
	}
	if update.LogLevel != nil {
		row.LogLevel = update.LogLevel
	}
	return row
}

func readTelemetry(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb string) (*TelemetryView, error) {
	return telemetryCall(reqCtx, ctx, verb, "", func(callCtx context.Context, api *configapiclient.ConfigClient) (*configapiclient.TelemetrySettingsView, error) {
		return api.GetV1WorkspacesConfigTelemetry(callCtx, configapiclient.GetV1WorkspacesConfigTelemetryInput{
			Path: configapiclient.GetV1WorkspacesConfigTelemetryPath{Workspace: ctx.WorkspaceID},
		})
	})
}

// writeTelemetry replaces the row of service, or of the workspace for "",
// with row; a row with every field unset removes it.
func writeTelemetry(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, service string, row TelemetrySettings) (*TelemetryView, error) {
	body := configapiclient.TelemetrySettingsWriteRequest{
		TraceBudget:         optional(row.TraceBudget),
		TraceRatio:          optional(row.TraceRatio),
		TraceFloorPerMinute: optional(row.TraceFloorPerMinute),
		LogLevel:            optional(row.LogLevel),
	}
	if service != "" {
		body.Service = &service
	}
	return telemetryCall(reqCtx, ctx, verb, service, func(callCtx context.Context, api *configapiclient.ConfigClient) (*configapiclient.TelemetrySettingsView, error) {
		return api.UpdateV1WorkspacesConfigTelemetry(callCtx, configapiclient.UpdateV1WorkspacesConfigTelemetryInput{
			Path: configapiclient.UpdateV1WorkspacesConfigTelemetryPath{Workspace: ctx.WorkspaceID},
			Body: body,
		})
	})
}

// telemetryCall sends one generated config-api call and maps a refusal onto
// a message that names the verb and config-api's reason. config-api answers a
// refusal of this route as {"error": …}, so the call goes through the same
// legacy-error transport as the config and secrets commands, which carries
// that reason to the message.
func telemetryCall(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, service string,
	call func(context.Context, *configapiclient.ConfigClient) (*configapiclient.TelemetrySettingsView, error),
) (*TelemetryView, error) {
	binding := clicore.ServiceBindingFor(ctx.ControlPlane, withLegacyConfigErrors(ctx.IO.Client))
	api, err := clicore.NewServiceClient[configapiclient.ConfigClient](configapiclient.RegisterConfigClient, binding)
	if err != nil {
		return nil, err
	}
	answer, err := clicore.CallWithSession(reqCtx, ctx, func(callCtx context.Context) (*configapiclient.TelemetrySettingsView, error) {
		return call(callCtx, api)
	})
	if err != nil {
		return nil, telemetryRefusal(verb, service, ctx, err)
	}
	return telemetryViewOf(answer), nil
}

func telemetryRefusal(verb, service string, ctx *clicore.WorkspaceContext, err error) error {
	status := clicore.ServiceStatus(err)
	if status == 0 {
		return clicore.NewError(fmt.Sprintf("cloud telemetry %s: request failed for %s: %v", verb, ctx.WorkspaceURL("/config/telemetry"), err), clicore.ExitAPI)
	}
	message := clicore.ServiceMessage(err)
	if message == "" {
		message = clicore.StatusLine(status)
	}
	prefix := "cloud telemetry " + verb + ": " + message
	switch status {
	case 400:
		if strings.Contains(service, "/") {
			prefix += " (config-api does not accept a service name with a folder, such as " + service + ", yet)"
		}
		return clicore.NewError(prefix, clicore.ExitUsage)
	case 401:
		return clicore.NewError(prefix+": run `putnami cloud login`", clicore.ExitAuth)
	case 403:
		need := "platform.workspace.read"
		if verb != "show" {
			need = "platform.workspace.manage, as a person; a machine token such as PUTNAMI_CLOUD_TOKEN cannot"
		}
		return clicore.NewError(prefix+" (403 — this needs "+need+")", clicore.ExitAuth)
	default:
		return clicore.NewError(prefix, clicore.ExitAPI)
	}
}

func telemetryViewOf(answer *configapiclient.TelemetrySettingsView) *TelemetryView {
	view := &TelemetryView{Services: []TelemetryService{}}
	if answer == nil {
		return view
	}
	if answer.WorkspaceId != nil {
		view.WorkspaceID = *answer.WorkspaceId
	}
	view.Workspace = settingsOf(answer.Workspace)
	if at, ok := answer.WorkspaceUpdatedAt.Value(); ok {
		view.WorkspaceUpdatedAt = &at
	}
	if answer.Services != nil {
		for _, row := range *answer.Services {
			if row.Service == nil {
				continue
			}
			view.Services = append(view.Services, TelemetryService{
				Service: *row.Service, Override: settingsOf(row.Override),
				Effective: settingsOf(row.Effective), UpdatedAt: row.UpdatedAt,
			})
		}
	}
	return view
}

func settingsOf(values *configapiclient.TelemetrySettingValues) TelemetrySettings {
	if values == nil {
		return TelemetrySettings{}
	}
	return TelemetrySettings{
		TraceBudget:         present(values.TraceBudget),
		TraceRatio:          present(values.TraceRatio),
		TraceFloorPerMinute: present(values.TraceFloorPerMinute),
		LogLevel:            present(values.LogLevel),
	}
}

func present[T any](value client.Optional[T]) *T {
	if v, ok := value.Value(); ok {
		return &v
	}
	return nil
}

func optional[T any](value *T) client.Optional[T] {
	if value == nil {
		return client.Optional[T]{}
	}
	return client.Some(*value)
}

// printTelemetry prints the view, or with service only the workspace row and
// that service's row.
func printTelemetry(params map[string]any, ioctx clicore.IO, view *TelemetryView, service string) {
	if service != "" {
		rows := []TelemetryService{}
		for _, row := range view.Services {
			if row.Service == service {
				rows = append(rows, row)
			}
		}
		view.Services = rows
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(view, params, ioctx, "")
		return
	}
	ioctx.Stdout("Telemetry settings of workspace " + view.WorkspaceID + ". An unset field keeps the service's own setting.")
	ioctx.Stdout("workspace: " + describeTelemetry(view.Workspace) + updatedSuffix(view.WorkspaceUpdatedAt))
	for _, row := range view.Services {
		ioctx.Stdout(row.Service + ": " + describeTelemetry(row.Override) + updatedSuffix(row.UpdatedAt))
		ioctx.Stdout("  in force: " + describeTelemetry(row.Effective))
	}
	if service != "" && len(view.Services) == 0 {
		ioctx.Stdout(service + ": no row; it takes the workspace row")
	}
}

// describeTelemetry renders the fields a row sets, in the order of the flags.
func describeTelemetry(s TelemetrySettings) string {
	var fields []string
	if s.TraceBudget != nil {
		fields = append(fields, "budget "+strconv.FormatInt(*s.TraceBudget, 10))
	}
	if s.TraceRatio != nil {
		fields = append(fields, "ratio "+strconv.FormatFloat(*s.TraceRatio, 'f', -1, 64))
	}
	if s.TraceFloorPerMinute != nil {
		fields = append(fields, "floor "+strconv.FormatInt(*s.TraceFloorPerMinute, 10)+"/min")
	}
	if s.LogLevel != nil {
		fields = append(fields, "log level "+*s.LogLevel)
	}
	if len(fields) == 0 {
		return "nothing set"
	}
	return strings.Join(fields, ", ")
}

func updatedSuffix(at *time.Time) string {
	if at == nil {
		return ""
	}
	return " (updated " + at.UTC().Format(time.RFC3339) + ")"
}
