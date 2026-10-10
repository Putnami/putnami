// Package configcli holds the config-domain command implementations for the
// @putnami/cloud CLI extension: config, secrets, and publish-config. The
// extension binary registers these commands; the shared toolkit they build
// on lives in internal/clicore.
package configcli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.putnami.dev/client"
	configapiclient "go.putnami.dev/cloud/clients/config-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Config dispatches the `putnami cloud config <sub>` family.
func Config(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if err := clicore.RejectAppFlag(args, "cloud config"); err != nil {
		return err
	}
	sub := clicore.FirstPositional(args)
	switch sub {
	case "":
		if clicore.StringParam(params, "app", "appName") != "" {
			return configRunInspect(params, args, workspaceRoot, env, ioctx)
		}
		return configHelp(params, ioctx)
	case "help":
		return configHelp(params, ioctx)
	case "put":
		adoptConfigResolvePositionalApp(params, args)
		return configRunPut(params, args, workspaceRoot, env, ioctx)
	case "list":
		adoptConfigResolvePositionalApp(params, args)
		return configRunList(params, args, workspaceRoot, env, ioctx)
	case "show":
		adoptConfigResolvePositionalApp(params, args)
		return configRunShow(params, args, workspaceRoot, env, ioctx)
	case "resolve":
		adoptConfigResolvePositionalApp(params, args)
		return configRunResolve(params, args, workspaceRoot, env, ioctx)
	case "drift":
		adoptConfigResolvePositionalApp(params, args)
		return configRunDrift(params, args, workspaceRoot, env, ioctx)
	case "status":
		return configRunStatus(params, args, workspaceRoot, env, ioctx)
	default:
		if clicore.StringParam(params, "app", "appName") == "" {
			params["app"] = sub
			return configRunInspect(params, args, workspaceRoot, env, ioctx)
		}
		return clicore.NewError("unknown config subcommand: "+sub, clicore.ExitUsage)
	}
}

func configHelp(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "cloud config status [<project>] [--strict]", "description": "compare each project's declared keys with what each environment sets"},
		{"command": "cloud config show [<project>]", "description": "show the resolved config; --with-secrets redacts secret values; --require-complete fails if a declared block was never published"},
		{"command": "cloud config show <project> --declared", "description": "show every declared key and whether it is set"},
		{"command": "cloud config show <project> --schema [--format table|json|yaml]", "description": "show the published config schema"},
		{"command": "cloud config show <project> --key <key>", "description": "show one resolved non-secret value"},
		{"command": "cloud config show <project> --secret-keys", "description": "list the declared secret keys and whether each is set"},
		{"command": "cloud config show <project> --reveal-secrets", "description": "show the resolved config with plaintext secrets, after confirmation"},
		{"command": "cloud config show [<project>] --keys", "description": "list the resolved key names"},
		{"command": "cloud config show [<project>] --metadata", "description": "show the merge metadata; --require-complete fails if a declared block was never published"},
		{"command": "cloud config validate [<project>] --env <env>", "description": "validate the local config inputs, with no network access"},
		{"command": "cloud config drift <project>", "description": "compare the committed conf/env*.yaml with the published config; exits non-zero on drift"},
		{"command": "cloud config put <project> --config-from <yaml>", "description": "force-write non-secret values from a YAML file"},
		{"command": "cloud config publish [<project>]", "description": "publish a project's config artifacts (putnami publish does this for you)"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud config commands:")
	for _, c := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-62s %s", c["command"], c["description"]))
	}
	return nil
}

func configRunPut(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error { //nolint:unparam // command handler signature
	ctx, err := newConfigPutCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	valuesPaths, err := resolveConfigFromPaths(params, workspaceRoot)
	if err != nil {
		return err
	}
	merged, err := loadAndMergeYAML(valuesPaths)
	if err != nil {
		return err
	}
	if markers := placeholderPaths(merged); len(markers) > 0 {
		return clicore.NewError(
			fmt.Sprintf("config file still contains %s marker(s) at %s; edit the file before writing config.",
				publishConfigPlaceholder, strings.Join(markers, ", ")),
			clicore.ExitUsage,
		)
	}
	schemaDoc, blocks, schemaSource, err := configPutSchema(params, ctx, workspaceRoot)
	if err != nil {
		return err
	}
	normalizeSchemaManifestForPublish(schemaDoc, merged)
	writes, warnings := planBlockWrites(blocks, merged)
	if len(writes) == 0 {
		return clicore.NewError(fmt.Sprintf("--config-from produced no schema-declared non-secret config values for %s/%s", ctx.app, ctx.environment), clicore.ExitUsage)
	}
	// Validate every block against the schema BEFORE writing — the same policy
	// the config server enforces on PUT — so a rejected value fails client-side
	// here rather than at the workload's next boot.
	validationDiags, err := validateBlockWrites(schemaDoc, ctx.app, ctx.environment, writes)
	if err != nil {
		return err
	}
	dryRun := clicore.Truthy(clicore.Param(params, "dry-run", "dryRun"))
	if !dryRun {
		if err := configValidationError(validationDiags); err != nil {
			return err
		}
	}

	type blockResult struct {
		Path   string   `json:"path"`
		Keys   []string `json:"keys"`
		Status string   `json:"status"`
	}
	results := make([]blockResult, 0, len(writes))
	var roll *configRoll
	if !clicore.Truthy(clicore.Param(params, "dry-run", "dryRun")) {
		for i, write := range writes {
			body := map[string]any{
				"appName":     ctx.app,
				"environment": ctx.environment,
				"path":        write.Path,
				"values":      write.Values,
			}
			// Defer the inline-primary revision roll on every write but the
			// last, so a multi-block put rolls exactly ONE revision carrying the
			// complete config rather than one per block. On the last write,
			// --roll-shared opts a shared/overlay-block ("*") write into the bounded
			// fleet roll (a no-op for a concrete-app write).
			rollQuery, rollSharedQuery := "", ""
			switch {
			case i < len(writes)-1:
				rollQuery = rollDefer
			case clicore.Truthy(clicore.Param(params, "roll-shared", "rollShared")):
				rollSharedQuery = "true"
			}
			resp, err := ctx.configAPI().putConfig(body, rollQuery, rollSharedQuery)
			if err != nil {
				return mapConfigError(err, ctx, fmt.Sprintf("write config %q", write.Path))
			}
			if status, ok := resp.Roll.Value(); ok {
				roll = configRollFrom(status)
			}
			results = append(results, blockResult{Path: write.Path, Keys: sortedKeys(write.Values), Status: "stored"})
		}
	} else {
		for _, write := range writes {
			results = append(results, blockResult{Path: write.Path, Keys: sortedKeys(write.Values), Status: "dry-run"})
		}
	}

	out := map[string]any{
		"status":       "stored",
		"workspace":    ctx.workspaceID,
		"app":          ctx.app,
		"environment":  ctx.environment,
		"schemaSource": schemaSource,
		"valuesPaths":  valuesPaths,
		"configs":      results,
	}
	if dryRun {
		out["status"] = "dry-run"
	}
	if len(validationDiags) > 0 {
		out["validation"] = validationDiags
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	message := fmt.Sprintf("Stored %d config block(s) for %s/%s.", len(results), ctx.app, ctx.environment)
	if dryRun {
		message = fmt.Sprintf("Dry run: would store %d config block(s) for %s/%s.", len(results), ctx.app, ctx.environment)
	}
	if roll != nil {
		// An inline-primary workload freezes its config into the running
		// revision, so the write also rolls a re-stamped revision. Surface the
		// outcome — a failed/skipped roll means the store and the running revision
		// have diverged (no self-heal).
		out["roll"] = roll
		if roll.Message != "" {
			message += " " + roll.Message
		}
		for _, f := range roll.Failed {
			warnings = append(warnings, fmt.Sprintf("roll failed for %s: %s", f.App, f.Reason))
		}
		for _, s := range roll.Skipped {
			warnings = append(warnings, fmt.Sprintf("roll skipped for %s: %s", s.App, s.Reason))
		}
	}
	WritePublishResult(out, params, ioctx, message, warnings)
	// --dry-run doubles as a pre-publish gate: emit the plan (with diagnostics)
	// above, then fail loud without having written anything.
	if dryRun {
		return configValidationError(validationDiags)
	}
	return nil
}

// configRoll is the revision roll a config write triggered, as `config put`
// prints it. The local type keeps the member order and the always-written
// `app` member of the structured output.
type configRoll struct {
	Rolled  []configRollWorkload `json:"rolled,omitempty"`
	Failed  []configRollWorkload `json:"failed,omitempty"`
	Skipped []configRollWorkload `json:"skipped,omitempty"`
	Message string               `json:"message,omitempty"`
}

// configRollWorkload names one workload the roll acted on, or chose not to.
type configRollWorkload struct {
	App      string `json:"app"`
	Revision string `json:"revision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func configRollFrom(status configapiclient.ConfigRollStatus) *configRoll {
	return &configRoll{
		Rolled:  configRollWorkloads(status.Rolled),
		Failed:  configRollWorkloads(status.Failed),
		Skipped: configRollWorkloads(status.Skipped),
		Message: clicore.Deref(status.Message),
	}
}

func configRollWorkloads(workloads *[]configapiclient.ConfigRollWorkload) []configRollWorkload {
	if workloads == nil {
		return nil
	}
	out := make([]configRollWorkload, 0, len(*workloads))
	for _, workload := range *workloads {
		out = append(out, configRollWorkload{
			App:      clicore.Deref(workload.App),
			Revision: clicore.Deref(workload.Revision),
			Reason:   clicore.Deref(workload.Reason),
		})
	}
	return out
}

func newConfigPutCtx(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*secretsCtx, error) {
	if !clicore.Truthy(clicore.Param(params, "dry-run", "dryRun")) {
		return newSecretsCtx(params, workspaceRoot, env, ioctx)
	}
	ctx, err := newPublishCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, err
	}
	return &secretsCtx{
		workspaceID:  ctx.workspaceID,
		controlPlane: ctx.controlPlane,
		app:          ctx.app,
		environment:  ctx.environment,
		authToken:    ctx.authToken,
		io:           ctx.io,
		params:       ctx.params,
		env:          ctx.env,
	}, nil
}

func configPutSchema(params map[string]any, ctx *secretsCtx, workspaceRoot string) (map[string]any, []schemaBlock, string, error) {
	schemaPath, found, err := resolveSchemaPath(params, workspaceRoot, ctx.app)
	if err == nil && found {
		doc, blocks, err := loadSchemaManifest(schemaPath)
		return doc, blocks, schemaPath, err
	}
	if clicore.StringParam(params, "schema-from", "schemaFrom") != "" {
		return nil, nil, "", err
	}
	if clicore.Truthy(clicore.Param(params, "dry-run", "dryRun")) {
		return nil, nil, "", clicore.NewError("config put --dry-run requires a local schema artifact; run without --dry-run to use the published schema", clicore.ExitUsage)
	}
	schemaDoc, err := configFetchSchema(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	blocks, err := parseSchemaBlocks(schemaDoc, "published schema")
	if err != nil {
		return nil, nil, "", err
	}
	return schemaDoc, blocks, "published", nil
}

func configRunResolve(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error { //nolint:unparam // command handler signature
	ctx, resp, err := configResolve(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	resp.MissingBlocks = configMissingDeclaredBlocks(ctx, resp.Config)
	message := configResolveMessage(ctx, resp)
	if lines := missingDeclaredBlocksLines(resp.MissingBlocks); len(lines) > 0 {
		message += "\n" + strings.Join(lines, "\n")
	}
	clicore.WriteResult(resp, params, ioctx, message)
	return requireCompleteError(params, ctx, resp.MissingBlocks)
}

func configRunList(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error { //nolint:unparam // command handler signature
	ctx, resp, err := configResolve(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	keys := flattenConfigKeys(resp.Config)
	out := map[string]any{
		"workspace":   ctx.workspaceID,
		"app":         ctx.app,
		"environment": ctx.environment,
		"keys":        keys,
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(out, params, ioctx, "")
		return nil
	}
	if len(keys) == 0 {
		ioctx.Stdout(fmt.Sprintf("No resolved config keys for %s/%s.", ctx.app, ctx.environment))
		return nil
	}
	ioctx.Stdout(fmt.Sprintf("Config keys for %s/%s:", ctx.app, ctx.environment))
	for _, key := range keys {
		ioctx.Stdout("  " + key)
	}
	return nil
}

func configRunShow(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error { //nolint:unparam // command handler signature
	ctx, resp, err := configResolve(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	resp.MissingBlocks = configMissingDeclaredBlocks(ctx, resp.Config)
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(resp, params, ioctx, "")
		return requireCompleteError(params, ctx, resp.MissingBlocks)
	}
	clicore.WriteJSONText(ioctx, resp.Config)
	for _, line := range missingDeclaredBlocksLines(resp.MissingBlocks) {
		ioctx.Stdout(line)
	}
	return requireCompleteError(params, ctx, resp.MissingBlocks)
}

// configMissingDeclaredBlocks fetches the published schema and returns the
// non-secret config keys that resolved to "missing" — declared in the schema
// but never published. It mirrors configRunOverview's schema-fetch +
// per-spec status walk, and skips sensitive specs the same way (secrets never
// appear in a default resolve, so they are not "never published" here). It is
// best-effort: a schema-fetch failure surfaces nothing rather than failing an
// otherwise-successful resolve.
func configMissingDeclaredBlocks(ctx *secretsCtx, config map[string]any) []string {
	schema, err := configFetchSchema(ctx)
	if err != nil {
		return nil
	}
	var missing []string
	for _, spec := range schemaKeySpecs(schema) {
		if spec.Sensitive {
			continue
		}
		if configStatusForSpec(spec, config).Status == "missing" {
			missing = append(missing, spec.Key)
		}
	}
	return missing
}

// missingDeclaredBlocksLines renders the loud, clearly-labeled human section
// listing every declared-but-never-published config key.
func missingDeclaredBlocksLines(missing []string) []string {
	if len(missing) == 0 {
		return nil
	}
	lines := make([]string, 0, len(missing)+1)
	lines = append(lines, fmt.Sprintf("Declared in schema but NEVER published (%d):", len(missing)))
	for _, key := range missing {
		lines = append(lines, "  - "+key)
	}
	return lines
}

// requireCompleteError fails the command non-zero when --require-complete is set
// and at least one declared block was never published. Without the flag the
// missing set is purely informational and the exit code is unchanged.
func requireCompleteError(params map[string]any, ctx *secretsCtx, missing []string) error {
	if len(missing) == 0 || !clicore.Truthy(clicore.Param(params, "require-complete", "requireComplete")) {
		return nil
	}
	return clicore.NewError(
		fmt.Sprintf("%d declared config block(s) never published for %s/%s: %s",
			len(missing), ctx.app, ctx.environment, strings.Join(missing, ", ")),
		clicore.ExitAPI,
	)
}

// resolvedConfig is config-api's resolve answer as the config commands read
// and print it. The local type keeps the member order and the always-written
// members of the structured output.
type resolvedConfig struct {
	Config         map[string]any `json:"config"`
	Resolved       bool           `json:"resolved"`
	SchemaMatch    bool           `json:"schemaMatch"`
	Layers         []layerInfo    `json:"layers"`
	Warnings       []string       `json:"warnings,omitempty"`
	SecretsApplied bool           `json:"secretsApplied,omitempty"`
	SecretsLayers  []layerInfo    `json:"secretsLayers,omitempty"`
}

// layerInfo is one config dimension that contributed to a resolved config or
// secrets tree.
type layerInfo struct {
	Dimension string `json:"dimension"`
	Priority  int    `json:"priority"`
}

// layerInfosFrom converts the layers config-api answered. Absent layers stay
// nil and an empty list stays empty, so the output shows null or [] as before.
func layerInfosFrom(layers []configapiclient.LayerInfo, present bool) []layerInfo {
	if !present || layers == nil {
		return nil
	}
	out := make([]layerInfo, 0, len(layers))
	for _, layer := range layers {
		out = append(out, layerInfo{Dimension: clicore.Deref(layer.Dimension), Priority: int(clicore.Deref(layer.Priority))})
	}
	return out
}

// optionalLayers reads an optional layer list config-api answered.
func optionalLayers(layers client.Optional[[]configapiclient.LayerInfo]) []layerInfo {
	value, ok := layers.Value()
	return layerInfosFrom(value, ok)
}

// pointerLayers reads a layer list config-api answered as a pointer.
func pointerLayers(layers *[]configapiclient.LayerInfo) []layerInfo {
	if layers == nil {
		return nil
	}
	return layerInfosFrom(*layers, true)
}

// resolvedConfigFrom converts config-api's resolve answer into the shape the
// config commands read.
func resolvedConfigFrom(target string, resp *configapiclient.ResolveConfigsResponse) (*resolvedConfig, error) {
	tree, err := jsonTree(target, resp.Config)
	if err != nil {
		return nil, err
	}
	var warnings []string
	if resp.Warnings != nil {
		warnings = *resp.Warnings
	}
	return &resolvedConfig{
		Config:         tree,
		Resolved:       clicore.Deref(resp.Resolved),
		SchemaMatch:    clicore.Deref(resp.SchemaMatch),
		Layers:         optionalLayers(resp.Layers),
		Warnings:       warnings,
		SecretsApplied: clicore.Deref(resp.SecretsApplied),
		SecretsLayers:  pointerLayers(resp.SecretsLayers),
	}, nil
}

// configResolveResult is the CLI projection of config-api's resolve answer plus
// the target context the command has always added to structured output.
type configResolveResult struct {
	resolvedConfig
	Workspace   string `json:"workspace"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	Version     string `json:"version,omitempty"`
	// MissingBlocks lists the non-secret config keys declared in the published
	// schema that resolved to "missing" — declared but never published.
	// Additive: emitted in structured output alongside the resolved config so a
	// silent never-published block is surfaced.
	MissingBlocks []string `json:"missingBlocks,omitempty"`
}

func configResolve(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*secretsCtx, *configResolveResult, error) {
	ctx, err := newSecretsCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, nil, err
	}
	resp, err := configResolveWithCtx(ctx, params, ioctx)
	if err != nil {
		return nil, nil, err
	}
	return ctx, resp, nil
}

func configResolveWithCtx(ctx *secretsCtx, params map[string]any, ioctx clicore.IO) (*configResolveResult, error) {
	body := map[string]any{
		"appName":     ctx.app,
		"environment": ctx.environment,
	}
	if version := clicore.StringParam(params, "version"); version != "" {
		body["version"] = version
	}
	if clicore.Truthy(clicore.Param(params, "reveal-secrets", "revealSecrets")) {
		if err := requireRevealApproval(params, ioctx, ctx.app+"/"+ctx.environment+" config"); err != nil {
			return nil, err
		}
		body["secretsMode"] = "reveal"
	} else if clicore.Truthy(clicore.Param(params, "with-secrets", "withSecrets")) {
		body["secretsMode"] = "redacted"
	} else if clicore.Truthy(clicore.Param(params, "include-secrets", "includeSecrets")) {
		body["includeSecrets"] = true
	}

	resp, err := ctx.configAPI().resolveConfigs(body, ctx.workspaceID)
	if err != nil {
		return nil, mapConfigError(err, ctx, "resolve")
	}
	return &configResolveResult{
		resolvedConfig: *resp,
		Workspace:      ctx.workspaceID,
		App:            ctx.app,
		Environment:    ctx.environment,
		Version:        clicore.StringParam(params, "version"),
	}, nil
}

func configResolveMessage(ctx *secretsCtx, resp *configResolveResult) string {
	keys := sortedKeys(resp.Config)
	status := "unresolved"
	if resp.Resolved {
		status = "resolved"
	}
	lines := []string{fmt.Sprintf("Config for %s/%s: %s (%d top-level key(s)).", ctx.app, ctx.environment, status, len(keys))}
	if len(keys) > 0 {
		lines = append(lines, "Keys: "+strings.Join(keys, ", "))
	}
	if layers := resolveLayerDimensions(resp.Layers); len(layers) > 0 {
		lines = append(lines, "Layers: "+strings.Join(layers, ", "))
	}
	if warnings := resp.Warnings; len(warnings) > 0 {
		lines = append(lines, "Warnings:")
		for _, warning := range warnings {
			lines = append(lines, "  - "+warning)
		}
	}
	if resp.SecretsApplied {
		lines = append(lines, "Secrets applied: true.")
	}
	return strings.Join(lines, "\n")
}

func mapConfigError(err error, ctx *secretsCtx, verb string) error {
	var apiErr *configAPIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.schemaNotRegistered || apiErr.status == http.StatusNotFound {
		return clicore.NewError(
			fmt.Sprintf("cannot %s config for app %q: %s. Run `putnami publish %s` to register its schema first.",
				verb, ctx.app, apiErr.message, ctx.app),
			clicore.ExitAPI,
		)
	}
	return clicore.NewError(fmt.Sprintf("cannot %s config for app %q: %s", verb, ctx.app, apiErr.message), clicore.ExitAPI)
}

func adoptConfigResolvePositionalApp(params map[string]any, args []string) {
	if clicore.StringParam(params, "app", "appName") != "" {
		return
	}
	if app := positionalAfterFirst(args); app != "" {
		params["app"] = app
	}
}

func positionalAfterFirst(args []string) string {
	seenFirst := false
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
		if !seenFirst {
			seenFirst = true
			continue
		}
		return raw
	}
	return ""
}

func flattenConfigKeys(config map[string]any) []string {
	var out []string
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		switch v := value.(type) {
		case map[string]any:
			if len(v) == 0 {
				out = append(out, prefix)
				return
			}
			for _, key := range sortedKeys(v) {
				next := key
				if prefix != "" {
					next = prefix + "." + key
				}
				walk(next, v[key])
			}
		default:
			if prefix != "" {
				out = append(out, prefix)
			}
		}
	}
	walk("", config)
	return out
}
