package configcli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/client"
	configapiclient "go.putnami.dev/cloud/clients/config-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// SecretsDefaultEnvironment is the env used when `--env` is unset.
const SecretsDefaultEnvironment = "prod"

// configWorkspaceHeader selects the workspace for a Google service-account
// config resolve. Config Server still requires a live exact caller grant; this
// value carries no authority and is ignored by the human scope_ref path.
const configWorkspaceHeader = "X-Putnami-Workspace-ID"

// secretsCtx is the resolved context shared by every secrets subcommand:
// the active workspace config, the active bearer, and the {app, env} the
// user targeted. Built once per command so the four subverbs share the
// same discovery/auth flow.
type secretsCtx struct {
	workspaceID  string
	controlPlane string
	app          string
	environment  string
	authToken    clicore.Bearer
	io           clicore.IO
	params       map[string]any
	env          map[string]string
}

// Secrets dispatches the `putnami cloud secrets <sub>` family. The
// dispatcher is intentionally tiny: each subverb owns its own validation
// and HTTP shape so the proxy/server contract stays close to the wire.
func Secrets(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if err := clicore.RejectAppFlag(args, "cloud secrets"); err != nil {
		return err
	}
	sub := clicore.FirstPositional(args)
	switch sub {
	case "":
		return secretsHelp(params, ioctx)
	case "help":
		return secretsHelp(params, ioctx)
	case "set":
		return secretsRunSet(params, args, workspaceRoot, env, ioctx)
	case "get":
		return secretsRunGet(params, args, workspaceRoot, env, ioctx)
	case "reveal":
		return secretsRunReveal(params, args, workspaceRoot, env, ioctx)
	case "list":
		return secretsRunList(params, args, workspaceRoot, env, ioctx)
	case "delete":
		return secretsRunDelete(params, args, workspaceRoot, env, ioctx)
	case "status":
		return secretsRunStatus(params, args, workspaceRoot, env, ioctx)
	default:
		return clicore.NewError("unknown secrets subcommand: "+sub, clicore.ExitUsage)
	}
}

func secretsHelp(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "cloud secrets status [<project>] [--strict]", "description": "compare each project's declared secrets with what each environment sets; never reads a value"},
		{"command": "cloud secrets list [<project>]", "description": "list workspace or project secret metadata"},
		{"command": "cloud secrets set <project> <key>", "description": "write a secret (--value | --from-file | --from-stdin)"},
		{"command": "cloud secrets reveal <project> [<key>]", "description": "decrypt and print plaintext"},
		{"command": "cloud secrets delete <project> <key>", "description": "delete a secret entry (idempotent)"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud secrets commands:")
	for _, c := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-45s %s", c["command"], c["description"]))
	}
	return nil
}

func secretsRunSet(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if err := adoptSecretsProjectAndKey(params, args, true); err != nil {
		return err
	}
	ctx, err := newSecretsCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	block, field, err := resolveKey(params, args)
	if err != nil {
		return err
	}
	value, err := resolveValue(params, ioctx)
	if err != nil {
		return err
	}

	body := map[string]any{
		"appName":     ctx.app,
		"environment": ctx.environment,
		"path":        block,
		"values":      map[string]any{field: value},
	}
	resp, err := ctx.configAPI().putSecret(body)
	if err != nil {
		return mapSecretsError(err, ctx, "set")
	}
	clicore.WriteResult(map[string]any{
		"status":      "encrypted",
		"app":         ctx.app,
		"environment": ctx.environment,
		"path":        block,
		"field":       field,
		"response":    resp,
	}, params, ioctx, fmt.Sprintf("Secret %s.%s stored for %s/%s.", block, field, ctx.app, ctx.environment))
	return nil
}

func secretsRunGet(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	ctx, err := newSecretsCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	block, field, err := resolveKey(params, args)
	if err != nil {
		return err
	}
	reveal := clicore.Truthy(clicore.Param(params, "reveal"))

	// Two server calls compose the get view:
	//   list    → UpdatedAt at the exact <app>/<env> dimension (the layer
	//             `secrets set` writes to). Absent if the user only wrote
	//             at a parent layer (*/<env>, <app>/*, */*).
	//   resolve → existence in the merged tree + which dimension layers
	//             contributed at all. v0 doesn't expose per-path layer
	//             provenance from the server, so the resolve response's
	//             aggregate Layers list is the best truth we can show.
	entries, err := ctx.configAPI().listSecrets(ctx.app, ctx.environment, true)
	if err != nil {
		return mapSecretsError(err, ctx, "get")
	}
	var match *secretEntry
	for i := range entries {
		if entries[i].Path == block {
			match = &entries[i]
			break
		}
	}

	resolveBody := map[string]any{"appName": ctx.app, "environment": ctx.environment}
	resolved, err := ctx.configAPI().resolveSecrets(resolveBody)
	if err != nil {
		return mapSecretsError(err, ctx, "get")
	}
	value, mergedExists := pluckField(resolved.Secrets, block, field)
	layers := resolveLayerDimensions(resolved.Layers)
	exists := mergedExists || match != nil

	out := map[string]any{
		"app":         ctx.app,
		"environment": ctx.environment,
		"path":        block,
		"field":       field,
		"exists":      exists,
		"layers":      layers,
	}
	if match != nil {
		out["updatedAt"] = wireTime(match.UpdatedAt)
	}

	if !exists {
		return clicore.NewError(fmt.Sprintf("no secret found at %s.%s for %s/%s", block, field, ctx.app, ctx.environment), clicore.ExitAPI)
	}

	if !reveal {
		message := fmt.Sprintf("Secret %s.%s exists for %s/%s", block, field, ctx.app, ctx.environment)
		if match != nil {
			message += fmt.Sprintf("; updated %s", wireTime(match.UpdatedAt))
		} else if len(layers) > 0 {
			// No entry at <app>/<env>: the value is inherited from a parent
			// dimension. Surface it so the user knows where to override.
			message += fmt.Sprintf("; inherited from %s", strings.Join(layers, ", "))
		}
		clicore.WriteResult(out, params, ioctx, message+".")
		return nil
	}

	if ok := confirmReveal(ctx.io, block, field); !ok {
		return clicore.NewError("reveal canceled", clicore.ExitUsage)
	}
	if !mergedExists {
		return clicore.NewError(fmt.Sprintf("no secret found at %s.%s for %s/%s", block, field, ctx.app, ctx.environment), clicore.ExitAPI)
	}
	out["value"] = value
	clicore.WriteResult(out, params, ioctx, fmt.Sprint(value))
	return nil
}

func secretsRunReveal(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	key := adoptRevealTarget(params, args)
	ctx, err := newSecretsCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	label := ctx.app + "/" + ctx.environment
	if key != "" {
		label += " " + key
	}
	if err := requireRevealApproval(params, ctx.io, label); err != nil {
		return err
	}

	resolved, err := ctx.configAPI().resolveSecrets(map[string]any{
		"appName":     ctx.app,
		"environment": ctx.environment,
	})
	if err != nil {
		return mapSecretsError(err, ctx, "reveal")
	}
	if key == "" {
		out := map[string]any{
			"app":         ctx.app,
			"environment": ctx.environment,
			"secrets":     resolved.Secrets,
			"layers":      resolveLayerDimensions(resolved.Layers),
		}
		if clicore.StructuredOutput(params) {
			clicore.WriteResult(out, params, ioctx, "")
		} else {
			clicore.WriteJSONText(ioctx, out["secrets"])
		}
		return nil
	}
	block, field, err := splitSecretKey(key)
	if err != nil {
		return err
	}
	value, exists := pluckField(resolved.Secrets, block, field)
	if !exists {
		return clicore.NewError(fmt.Sprintf("no secret found at %s.%s for %s/%s", block, field, ctx.app, ctx.environment), clicore.ExitAPI)
	}
	out := map[string]any{
		"app":         ctx.app,
		"environment": ctx.environment,
		"path":        block,
		"field":       field,
		"value":       value,
		"layers":      resolveLayerDimensions(resolved.Layers),
	}
	clicore.WriteResult(out, params, ioctx, fmt.Sprint(value))
	return nil
}

// putSecretResult is config-api's answer to a secret write, as `secrets set`
// prints it under "response".
type putSecretResult struct {
	AppName     string `json:"appName"`
	Environment string `json:"environment"`
	Path        string `json:"path"`
	Status      string `json:"status"`
}

// secretEntry is the metadata-only view of one stored secret, as the secrets
// commands sort and print it. It never carries plaintext.
type secretEntry struct {
	AppName     string    `json:"appName"`
	Environment string    `json:"environment"`
	Version     string    `json:"version,omitempty"`
	Path        string    `json:"path"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// secretEntriesFrom converts the entries config-api listed. Absent entries
// stay nil and an empty list stays empty, so the output shows null or [] as
// before.
func secretEntriesFrom(entries client.Optional[[]configapiclient.ListEntry]) []secretEntry {
	listed, ok := entries.Value()
	if !ok || listed == nil {
		return nil
	}
	out := make([]secretEntry, 0, len(listed))
	for _, entry := range listed {
		out = append(out, secretEntry{
			AppName:     clicore.Deref(entry.AppName),
			Environment: clicore.Deref(entry.Environment),
			Version:     clicore.Deref(entry.Version),
			Path:        clicore.Deref(entry.Path),
			UpdatedAt:   clicore.Deref(entry.UpdatedAt),
		})
	}
	return out
}

// resolvedSecrets is config-api's secret resolve answer: the plaintext tree
// and the dimensions that contributed to it.
type resolvedSecrets struct {
	Secrets map[string]any
	Layers  []layerInfo
}

// resolveLayerDimensions extracts the dimension labels from a resolve
// response. Each LayerInfo carries Dimension (e.g. "my-app/prod") and
// Priority; we surface the ordered Dimension list since the priority
// numbers leak server-internal merge semantics that aren't meaningful to
// CLI users.
func resolveLayerDimensions(layers []layerInfo) []string {
	if layers == nil {
		return nil
	}
	out := make([]string, 0, len(layers))
	for _, layer := range layers {
		if layer.Dimension != "" {
			out = append(out, layer.Dimension)
		}
	}
	return out
}

func secretsRunList(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	project := secretsProjectPositional(args)
	if project != "" {
		params["app"] = project
	}
	if project == "" && clicore.StringParam(params, "_contextApp") != "" {
		delete(params, "app")
	}
	if clicore.StringParam(params, "app", "appName") == "" {
		return secretsRunListWorkspace(params, workspaceRoot, env, ioctx)
	}
	ctx, err := newSecretsCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	entries, err := ctx.configAPI().listSecrets(ctx.app, ctx.environment, true)
	if err != nil {
		return mapSecretsError(err, ctx, "list")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{
			"app":         ctx.app,
			"environment": ctx.environment,
			"entries":     entries,
		}, params, ioctx, "")
		return nil
	}
	if len(entries) == 0 {
		ioctx.Stdout(fmt.Sprintf("No secrets set for %s/%s.", ctx.app, ctx.environment))
		return nil
	}
	ioctx.Stdout(fmt.Sprintf("Secrets for %s/%s:", ctx.app, ctx.environment))
	for _, entry := range entries {
		ioctx.Stdout(fmt.Sprintf("  %-32s updated %s", entry.Path, wireTime(entry.UpdatedAt)))
	}
	return nil
}

func secretsRunListWorkspace(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	ctx, err := newSecretsWorkspaceCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	entries, err := ctx.configAPI().listSecrets("", "", false)
	if err != nil {
		return mapSecretsError(err, ctx, "list")
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].AppName != entries[j].AppName {
			return entries[i].AppName < entries[j].AppName
		}
		if entries[i].Environment != entries[j].Environment {
			return entries[i].Environment < entries[j].Environment
		}
		if entries[i].Version != entries[j].Version {
			return entries[i].Version < entries[j].Version
		}
		return entries[i].Path < entries[j].Path
	})
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{
			"workspace": ctx.workspaceID,
			"entries":   entries,
		}, params, ioctx, "")
		return nil
	}
	if len(entries) == 0 {
		ioctx.Stdout(fmt.Sprintf("No secrets set in workspace %s.", ctx.workspaceID))
		return nil
	}
	ioctx.Stdout(fmt.Sprintf("Secrets in workspace %s:", ctx.workspaceID))
	for _, entry := range entries {
		dim := entry.AppName + "/" + entry.Environment
		if entry.Version != "" {
			dim += "/" + entry.Version
		}
		ioctx.Stdout(fmt.Sprintf("  %-40s %-32s updated %s", dim, entry.Path, wireTime(entry.UpdatedAt)))
	}
	return nil
}

func secretsRunDelete(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if err := adoptSecretsProjectAndKey(params, args, true); err != nil {
		return err
	}
	ctx, err := newSecretsCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	block, field, err := resolveKey(params, args)
	if err != nil {
		return err
	}
	if err := ctx.configAPI().deleteSecret(ctx.app, ctx.environment, block, field); err != nil {
		if configStatus(err) == http.StatusNotFound {
			// kubectl-style: deleting a missing entry succeeds idempotently.
			clicore.WriteResult(map[string]any{
				"status":      "absent",
				"app":         ctx.app,
				"environment": ctx.environment,
				"path":        block,
				"field":       field,
			}, params, ioctx, fmt.Sprintf("Secret %s.%s already absent for %s/%s.", block, field, ctx.app, ctx.environment))
			return nil
		}
		return mapSecretsError(err, ctx, "delete")
	}
	clicore.WriteResult(map[string]any{
		"status":      "deleted",
		"app":         ctx.app,
		"environment": ctx.environment,
		"path":        block,
		"field":       field,
	}, params, ioctx, fmt.Sprintf("Secret %s.%s deleted for %s/%s.", block, field, ctx.app, ctx.environment))
	return nil
}

func newSecretsCtx(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*secretsCtx, error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, err
	}
	app, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return nil, err
	}
	environment := clicore.FirstString(clicore.StringParam(params, "env", "environment"), clicore.StringValue(link["environment"]), SecretsDefaultEnvironment)
	workspaceID := clicore.StringValue(link["workspace_id"])
	auth, err := clicore.WorkspaceAuth(params, env, ioctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return &secretsCtx{
		workspaceID:  workspaceID,
		controlPlane: clicore.StringValue(link["control_plane_url"]),
		app:          app,
		environment:  environment,
		authToken:    clicore.NewBearer(auth.AccessToken),
		io:           ioctx,
		params:       params,
		env:          env,
	}, nil
}

func newSecretsWorkspaceCtx(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*secretsCtx, error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, err
	}
	workspaceID := clicore.StringValue(link["workspace_id"])
	auth, err := clicore.WorkspaceAuth(params, env, ioctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return &secretsCtx{
		workspaceID:  workspaceID,
		controlPlane: clicore.StringValue(link["control_plane_url"]),
		environment:  clicore.FirstString(clicore.StringParam(params, "env", "environment"), clicore.StringValue(link["environment"]), SecretsDefaultEnvironment),
		authToken:    clicore.NewBearer(auth.AccessToken),
		io:           ioctx,
		params:       params,
		env:          env,
	}, nil
}

// resolveKey turns the user-facing key into the (block, field) tuple the
// secrets API expects. Long form (--block + --field) wins; otherwise the
// first positional arg after the subcommand is split on the LAST dot.
//
// Edge cases:
//   - Missing/empty key → usage error.
//   - Key without a dot → require --block/--field (we don't guess that
//     the whole token is a block name with no field; the framework
//     schema is path-indexed, so a bare top-level field has no
//     unambiguous block).
func resolveKey(params map[string]any, args []string) (block, field string, err error) {
	block = clicore.StringParam(params, "block")
	field = clicore.StringParam(params, "field")
	if block != "" && field != "" {
		return block, field, nil
	}
	if block != "" || field != "" {
		return "", "", clicore.NewError("--block and --field must be passed together", clicore.ExitUsage)
	}
	key := clicore.FirstString(clicore.StringParam(params, "key"), secretsKeyPositional(args))
	if key == "" {
		return "", "", clicore.NewError("secrets command requires a key (e.g. database.password) or --block/--field", clicore.ExitUsage)
	}
	return splitSecretKey(key)
}

func splitSecretKey(key string) (block, field string, err error) {
	idx := strings.LastIndex(key, ".")
	if idx <= 0 || idx == len(key)-1 {
		return "", "", clicore.NewError("key must be in block.field form (e.g. database.password) — or pass --block/--field", clicore.ExitUsage)
	}
	return key[:idx], key[idx+1:], nil
}

func adoptSecretsProjectAndKey(params map[string]any, args []string, keyRequired bool) error {
	pos := secretsPositionals(args)
	if len(pos) == 0 {
		return nil
	}
	tail := pos[1:]
	if len(tail) >= 2 {
		params["app"] = tail[0]
		params["key"] = tail[1]
		return nil
	}
	if len(tail) == 1 {
		token := tail[0]
		hasExplicitApp := clicore.StringParam(params, "app", "appName") != ""
		hasExplicitKey := clicore.StringParam(params, "block") != "" || clicore.StringParam(params, "field") != ""
		if !hasExplicitApp && !strings.Contains(token, ".") && hasExplicitKey {
			params["app"] = token
			return nil
		}
		if !hasExplicitApp && !strings.Contains(token, ".") && !hasExplicitKey {
			params["app"] = token
			if keyRequired {
				return clicore.NewError("secrets command requires a key after the project (e.g. putnami cloud secrets set apps/api database.password)", clicore.ExitUsage)
			}
			return nil
		}
		params["key"] = token
	}
	return nil
}

func adoptRevealTarget(params map[string]any, args []string) string {
	pos := secretsPositionals(args)
	tail := []string{}
	if len(pos) > 0 {
		tail = pos[1:]
	}
	if len(tail) >= 2 {
		params["app"] = tail[0]
		return tail[1]
	}
	if len(tail) == 1 {
		token := tail[0]
		if clicore.StringParam(params, "app", "appName") != "" || strings.Contains(token, ".") {
			return token
		}
		params["app"] = token
	}
	return ""
}

func secretsProjectPositional(args []string) string {
	pos := secretsPositionals(args)
	if len(pos) < 2 {
		return ""
	}
	return pos[1]
}

func secretsPositionals(args []string) []string {
	out := []string{}
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

// secretsKeyPositional returns the positional arg AFTER the subcommand
// keyword. clicore.FirstPositional returns the subcommand itself, so we skip
// the first non-flag token and use the second.
func secretsKeyPositional(args []string) string {
	seenSub := false
	skipNext := false
	for _, raw := range args {
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
			// flag with value: skip its value token below
			skipNext = true
			continue
		}
		if !seenSub {
			seenSub = true
			continue
		}
		return raw
	}
	return ""
}

// resolveValue reads the secret value from one of three sources:
//
//   - --value <plaintext>: convenient but puts the secret in argv +
//     shell history. We emit a TTY warning when this is used
//     interactively so the user can switch to a safer mode.
//   - --from-file <path>: reads from disk, trims one trailing newline.
//     `--from-file -` is the established sentinel for stdin (matches
//     tar, kubectl, gpg). Useful for piping: `vault read -field=… | …`.
//   - --from-stdin: explicit alternative to `--from-file -` for users
//     who find the dash form confusing.
//
// The TTY warning is intentionally a Stderr line, not a hard reject —
// CI/non-interactive flows already use --value safely (no shell
// history to leak to), and we don't want to break those.
func resolveValue(params map[string]any, ioctx clicore.IO) (string, error) {
	value := clicore.StringParam(params, "value")
	fromFile := clicore.StringParam(params, "from-file", "fromFile")
	fromStdin := clicore.Truthy(clicore.Param(params, "from-stdin", "fromStdin"))
	sourceCount := 0
	for _, isSet := range []bool{value != "", fromFile != "", fromStdin} {
		if isSet {
			sourceCount++
		}
	}
	if sourceCount > 1 {
		return "", clicore.NewError("pass only one of --value, --from-file, or --from-stdin", clicore.ExitUsage)
	}
	if value != "" {
		// TTY indicates interactive shell: --value lands in shell
		// history and argv. Stderr warning is the lightest-weight
		// nudge — non-interactive callers (CI, scripts) never see it.
		if ioctx.TTY != nil && ioctx.Stderr != nil {
			ioctx.Stderr("warning: --value puts the secret in argv and shell history. Prefer --from-file or --from-stdin for sensitive values.")
		}
		return value, nil
	}
	if fromStdin || fromFile == "-" {
		// Trim one trailing newline so `echo $secret | … set` works
		// without leaking the implicit newline into storage.
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read --from-stdin: %w", err)
		}
		return strings.TrimRight(string(data), "\n"), nil
	}
	if fromFile != "" {
		data, err := os.ReadFile(fromFile)
		if err != nil {
			return "", fmt.Errorf("read --from-file %s: %w", fromFile, err)
		}
		// Trim a single trailing newline so `--from-file token.txt` matches
		// the natural shell `echo > file` output without surprises.
		return strings.TrimRight(string(data), "\n"), nil
	}
	return "", clicore.NewError("secrets set requires --value, --from-file <path>, or --from-stdin", clicore.ExitUsage)
}

func wireTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

func pluckField(secrets map[string]any, block, field string) (any, bool) {
	if secrets == nil {
		return nil, false
	}
	current := any(secrets)
	for _, segment := range strings.Split(block, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[segment]
		if !ok {
			return nil, false
		}
	}
	leaf, ok := current.(map[string]any)
	if !ok {
		return nil, false
	}
	value, ok := leaf[field]
	return value, ok
}

func mapSecretsError(err error, ctx *secretsCtx, verb string) error {
	var apiErr *configAPIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.schemaNotRegistered {
		return clicore.NewError(
			fmt.Sprintf("cannot %s secrets for app %q: %s. Run `putnami publish` from the app workspace to register its schema first.",
				verb, ctx.app, apiErr.message),
			clicore.ExitAPI,
		)
	}
	return clicore.NewError(apiErr.message, clicore.ExitAPI)
}

func confirmReveal(ioctx clicore.IO, block, field string) bool {
	question := fmt.Sprintf("Reveal plaintext for %s.%s? [y/N]: ", block, field)
	if ioctx.Confirm != nil {
		answer, ok := ioctx.Confirm(question)
		if !ok {
			// Non-interactive or test stub opted out → behave as
			// CI/pipe mode: print without prompting.
			return true
		}
		return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes")
	}
	// Default: try /dev/tty (mac/linux). If unavailable, treat as
	// non-interactive (CI) and proceed without prompting.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return true
	}
	defer tty.Close()
	if _, err := tty.WriteString(question); err != nil {
		return true
	}
	answer, ok := readConfirmAnswer(tty)
	if !ok {
		return false
	}
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes")
}

func requireRevealApproval(params map[string]any, ioctx clicore.IO, label string) error {
	if clicore.Truthy(clicore.Param(params, "yes")) {
		return nil
	}
	question := fmt.Sprintf("Reveal plaintext secrets for %s? [y/N]: ", label)
	if ioctx.Confirm != nil {
		answer, ok := ioctx.Confirm(question)
		if !ok {
			return clicore.NewError("reveal requires --yes in non-interactive environments", clicore.ExitUsage)
		}
		if strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
			return nil
		}
		return clicore.NewError("reveal canceled", clicore.ExitUsage)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return clicore.NewError("reveal requires --yes in non-interactive environments", clicore.ExitUsage)
	}
	defer tty.Close()
	if _, err := tty.WriteString(question); err != nil {
		return clicore.NewError("reveal requires --yes in non-interactive environments", clicore.ExitUsage)
	}
	answer, ok := readConfirmAnswer(tty)
	if !ok {
		return clicore.NewError("reveal canceled", clicore.ExitUsage)
	}
	if strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
		return nil
	}
	return clicore.NewError("reveal canceled", clicore.ExitUsage)
}

// readConfirmAnswer reads a single trimmed line from the given reader,
// reporting whether a line was read at all. Domain-local copy of the
// stdin-line helper the aggregator also keeps (cli.go); it is provider/
// domain agnostic and small enough to duplicate rather than couple to.
func readConfirmAnswer(input io.Reader) (string, bool) {
	reader := bufio.NewReader(input)
	var answer strings.Builder
	for answer.Len() < 1024 {
		ch, err := reader.ReadByte()
		if err != nil {
			if answer.Len() == 0 {
				return "", false
			}
			break
		}
		if ch == '\n' || ch == '\r' {
			break
		}
		answer.WriteByte(ch)
	}
	return strings.TrimSpace(answer.String()), true
}
