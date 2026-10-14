package configcli

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	configapiclient "go.putnami.dev/cloud/clients/config-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"gopkg.in/yaml.v3"
)

type schemaKeySpec struct {
	Key         string
	Path        string
	Field       string
	Type        string
	Required    bool
	Default     string
	Sensitive   bool
	Description string
}

type configKeyStatus struct {
	Key       string `json:"key"`
	Type      string `json:"type,omitempty"`
	Required  bool   `json:"required,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Status    string `json:"status"`
	Value     any    `json:"value,omitempty"`
	Default   string `json:"default,omitempty"`
}

type secretKeyStatus struct {
	Key       string `json:"key"`
	Path      string `json:"path"`
	Field     string `json:"field"`
	Required  bool   `json:"required,omitempty"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	Command   string `json:"command,omitempty"`
}

func configRunInspect(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error { //nolint:unparam // command handler signature
	ctx, err := newSecretsCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	switch {
	case clicore.Truthy(clicore.Param(params, "schema")):
		schema, err := configFetchSchema(ctx)
		if err != nil {
			return err
		}
		return configWriteSchema(schema, params, ioctx)
	case clicore.Truthy(clicore.Param(params, "secret-keys", "secretKeys")):
		return configRunSecretKeys(ctx, params, ioctx)
	case clicore.Truthy(clicore.Param(params, "declared")):
		return configRunDeclared(ctx, params, ioctx)
	case clicore.Truthy(clicore.Param(params, "reveal-secrets", "revealSecrets")) && clicore.StringParam(params, "key") == "":
		resp, err := configResolveWithCtx(ctx, params, ioctx)
		if err != nil {
			return err
		}
		if clicore.StructuredOutput(params) {
			clicore.WriteResult(resp, params, ioctx, "")
		} else {
			clicore.WriteJSONText(ioctx, resp.Config)
		}
		return nil
	case clicore.StringParam(params, "key") != "":
		return configRunKey(ctx, params, ioctx)
	default:
		return configRunOverview(ctx, params, ioctx)
	}
}

func configRunOverview(ctx *secretsCtx, params map[string]any, ioctx clicore.IO) error {
	schema, err := configFetchSchema(ctx)
	if err != nil {
		return err
	}
	resp, err := configResolveWithCtx(ctx, params, ioctx)
	if err != nil {
		return err
	}
	specs := schemaKeySpecs(schema)
	rows := make([]configKeyStatus, 0, len(specs))
	secretCount := 0
	for _, spec := range specs {
		if spec.Sensitive {
			secretCount++
			continue
		}
		rows = append(rows, configStatusForSpec(spec, resp.Config))
	}

	out := map[string]any{
		"workspace":   ctx.workspaceID,
		"app":         ctx.app,
		"environment": ctx.environment,
		"schemaHash":  clicore.StringValue(schema["schemaHash"]),
		"keys":        rows,
		"layers":      resolveLayerDimensions(resp.Layers),
	}
	warnings := filterSensitiveConfigWarnings(resp.Warnings, specs)
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	if secretCount > 0 {
		out["secretKeys"] = secretCount
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(out, params, ioctx, "")
		return nil
	}

	ioctx.Stdout(fmt.Sprintf("Config for %s/%s:", ctx.app, ctx.environment))
	if len(rows) == 0 {
		ioctx.Stdout("  No non-secret config keys declared in the published schema.")
	} else {
		for _, row := range rows {
			ioctx.Stdout(formatConfigStatus(row))
		}
	}
	if len(warnings) > 0 {
		ioctx.Stdout("Warnings:")
		for _, warning := range warnings {
			ioctx.Stdout("  - " + warning)
		}
	}
	if secretCount > 0 {
		ioctx.Stdout(fmt.Sprintf("Secret keys: %d expected; run `putnami cloud config show %s --env %s --secret-keys`.", secretCount, ctx.app, ctx.environment))
	}
	return nil
}

func configRunKey(ctx *secretsCtx, params map[string]any, ioctx clicore.IO) error {
	key := clicore.StringParam(params, "key")
	schema, err := configFetchSchema(ctx)
	if err != nil {
		return err
	}
	spec, hasSpec := findSchemaKeySpec(schemaKeySpecs(schema), key)
	if hasSpec && spec.Sensitive && !clicore.Truthy(clicore.Param(params, "reveal-secrets", "revealSecrets")) {
		return clicore.NewError(fmt.Sprintf("config key %s is secret; use `putnami cloud secrets reveal %s %s --yes` or pass --reveal-secrets", key, ctx.app, key), clicore.ExitUsage)
	}
	resp, err := configResolveWithCtx(ctx, params, ioctx)
	if err != nil {
		return err
	}
	if hasSpec {
		row := configStatusForSpec(spec, resp.Config)
		if row.Status == "missing" {
			return clicore.NewError(fmt.Sprintf("config key %s is missing for %s/%s", key, ctx.app, ctx.environment), clicore.ExitAPI)
		}
		if clicore.StructuredOutput(params) {
			clicore.WriteResult(row, params, ioctx, "")
		} else {
			ioctx.Stdout(fmt.Sprintf("%s = %s (%s)", row.Key, clicore.StringValue(row.Value), row.Status))
		}
		return nil
	}
	value, ok := pluckConfigKey(resp.Config, key)
	if !ok {
		message := fmt.Sprintf("config key %s is not declared in the published schema and is not resolved for %s/%s", key, ctx.app, ctx.environment)
		if suggestion := suggestSchemaKey(schemaKeySpecs(schema), key); suggestion != "" {
			message += fmt.Sprintf(". Did you mean %s?", suggestion)
		}
		return clicore.NewError(message, clicore.ExitAPI)
	}
	row := configKeyStatus{Key: key, Status: "set", Value: value}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(row, params, ioctx, "")
	} else {
		ioctx.Stdout(fmt.Sprintf("%s = %s (set)", key, clicore.StringValue(value)))
	}
	return nil
}

func configRunSecretKeys(ctx *secretsCtx, params map[string]any, ioctx clicore.IO) error {
	rows, err := secretKeyRows(ctx)
	if err != nil {
		return err
	}
	return configWriteSecretKeyRows(ctx, params, ioctx, rows)
}

// secretKeyRows answers the secret keys the published schema declares for
// ctx.app and whether ctx.environment sets each: GET /api/secrets/status, or,
// on a config-api that does not serve it, the schema and the secret list. It
// reads metadata only; the error is the one the command prints.
func secretKeyRows(ctx *secretsCtx) ([]secretKeyStatus, error) {
	statusResp, err := ctx.configAPI().secretStatus(ctx.app, ctx.environment)
	if err == nil {
		return secretStatusRows(statusResp, ctx), nil
	}
	if configStatus(err) != http.StatusNotFound {
		return nil, mapSecretsError(err, ctx, "status")
	}
	schema, err := configFetchSchema(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := ctx.configAPI().listSecrets(ctx.app, ctx.environment, true)
	if err != nil {
		return nil, mapSecretsError(err, ctx, "list")
	}
	return legacySecretKeyRows(ctx, schema, entries), nil
}

// legacySecretKeyRows computes the secret-key status a config-api without
// GET /api/secrets/status cannot answer: each sensitive key of the schema is
// set when a secret entry exists at its path. It reads metadata only.
func legacySecretKeyRows(ctx *secretsCtx, schema map[string]any, entries []secretEntry) []secretKeyStatus {
	byPath := map[string]secretEntry{}
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	var rows []secretKeyStatus
	for _, spec := range schemaKeySpecs(schema) {
		if !spec.Sensitive {
			continue
		}
		row := secretKeyStatus{
			Key:      spec.Key,
			Path:     spec.Path,
			Field:    spec.Field,
			Required: spec.Required,
			Status:   "missing",
			Command:  secretSetCommand(ctx, spec.Key),
		}
		if entry, ok := byPath[spec.Path]; ok {
			row.Status = "set"
			row.UpdatedAt = wireTime(entry.UpdatedAt)
			row.Command = ""
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows
}

func configWriteSecretKeyRows(ctx *secretsCtx, params map[string]any, ioctx clicore.IO, rows []secretKeyStatus) error {
	out := map[string]any{
		"workspace":   ctx.workspaceID,
		"app":         ctx.app,
		"environment": ctx.environment,
		"keys":        rows,
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(out, params, ioctx, "")
		return nil
	}

	ioctx.Stdout(fmt.Sprintf("Secret keys for %s/%s:", ctx.app, ctx.environment))
	if len(rows) == 0 {
		ioctx.Stdout("  No secret keys declared in the published schema.")
		return nil
	}
	for _, row := range rows {
		detail := ""
		if row.Status == "set" && row.UpdatedAt != "" {
			detail = " updated " + row.UpdatedAt
		} else if row.Required {
			detail = " required"
		}
		ioctx.Stdout(fmt.Sprintf("  %-40s %-8s%s", row.Key, row.Status, detail))
		if row.Command != "" {
			ioctx.Stdout("    set: " + row.Command)
		}
	}
	return nil
}

// declaredKeyStatus is one key the published schema declares, as
// `config show <project> --declared` prints it: whether it is a config or a
// secret key, whether it is required, and whether the environment sets it.
// It never carries a value.
type declaredKeyStatus struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required,omitempty"`
	Status   string `json:"status"`
}

// configRunDeclared lists every key the published schema declares, config and
// secret keys alike, and whether ctx.environment sets it. A config key is
// set, default or missing in the resolved config; a secret key is set or
// missing as the secret status answers it. No value is read for a secret, and
// none is printed.
func configRunDeclared(ctx *secretsCtx, params map[string]any, ioctx clicore.IO) error {
	schema, err := configFetchSchema(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{"appName": ctx.app, "environment": ctx.environment}
	if version := clicore.StringParam(params, "version"); version != "" {
		body["version"] = version
	}
	resolved, err := ctx.configAPI().resolveConfigs(body, ctx.workspaceID)
	if err != nil {
		return mapConfigError(err, ctx, "resolve")
	}
	secretRows, err := secretKeyRows(ctx)
	if err != nil {
		return err
	}
	rows := make([]declaredKeyStatus, 0)
	for _, spec := range schemaKeySpecs(schema) {
		if spec.Sensitive {
			continue
		}
		status := configStatusForSpec(spec, resolved.Config)
		rows = append(rows, declaredKeyStatus{Key: spec.Key, Kind: "config", Type: spec.Type, Required: spec.Required, Status: status.Status})
	}
	for _, row := range secretRows {
		rows = append(rows, declaredKeyStatus{Key: row.Key, Kind: "secret", Required: row.Required, Status: row.Status})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{
			"workspace":   ctx.workspaceID,
			"app":         ctx.app,
			"environment": ctx.environment,
			"keys":        rows,
		}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout(fmt.Sprintf("Declared keys for %s/%s:", ctx.app, ctx.environment))
	if len(rows) == 0 {
		ioctx.Stdout("  No key declared in the published schema.")
		return nil
	}
	for _, row := range rows {
		required := ""
		if row.Required {
			required = " required"
		}
		ioctx.Stdout(fmt.Sprintf("  %-40s %-7s %-8s%s", row.Key, row.Kind, row.Status, required))
	}
	return nil
}

func configWriteSchema(schema map[string]any, params map[string]any, ioctx clicore.IO) error {
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(schema, params, ioctx, "")
		return nil
	}
	format := schemaFormat(params)
	switch format {
	case "json":
		clicore.WriteJSONText(ioctx, schema)
	case "yaml":
		data, err := yaml.Marshal(schema)
		if err != nil {
			return clicore.NewError("marshal schema yaml: "+err.Error(), clicore.ExitAPI)
		}
		ioctx.Stdout(strings.TrimRight(string(data), "\n"))
	case "table":
		configWriteSchemaTable(schema, ioctx)
	default:
		return clicore.NewError("unsupported schema format "+format+"; use table, json, or yaml", clicore.ExitUsage)
	}
	return nil
}

func schemaFormat(params map[string]any) string {
	format := strings.ToLower(strings.TrimSpace(clicore.StringParam(params, "format")))
	if format == "" {
		return "table"
	}
	return format
}

func configWriteSchemaTable(schema map[string]any, ioctx clicore.IO) {
	app := clicore.FirstString(clicore.StringValue(schema["appName"]), "<unknown>")
	ioctx.Stdout(fmt.Sprintf("Config schema for %s:", app))
	specs := schemaKeySpecs(schema)
	if len(specs) == 0 {
		ioctx.Stdout("  No config keys declared in the published schema.")
		return
	}
	for _, spec := range specs {
		ioctx.Stdout(formatSchemaSpec(spec))
	}
}

func formatSchemaSpec(spec schemaKeySpec) string {
	tags := []string{}
	if spec.Required {
		tags = append(tags, "required")
	}
	if spec.Default != "" {
		tags = append(tags, "default "+spec.Default)
	}
	if spec.Sensitive {
		tags = append(tags, "secret")
	}
	if len(tags) == 0 {
		tags = append(tags, "optional")
	}
	return fmt.Sprintf("  %-40s %-10s %s", spec.Key, spec.Type, strings.Join(tags, ", "))
}

func secretStatusRows(resp *configapiclient.SecretStatusResponse, ctx *secretsCtx) []secretKeyStatus {
	keys := clicore.Deref(resp.Keys)
	rows := make([]secretKeyStatus, 0, len(keys))
	for _, entry := range keys {
		updatedAt := ""
		if value, ok := entry.UpdatedAt.Value(); ok {
			updatedAt = wireTime(value)
		}
		row := secretKeyStatus{
			Key:       clicore.Deref(entry.Key),
			Path:      clicore.Deref(entry.Path),
			Field:     clicore.Deref(entry.Field),
			Required:  clicore.Deref(entry.Required),
			Status:    clicore.FirstString(clicore.Deref(entry.Status), "missing"),
			UpdatedAt: updatedAt,
		}
		if row.Status == "missing" && row.Key != "" {
			row.Command = secretSetCommand(ctx, row.Key)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows
}

func configFetchSchema(ctx *secretsCtx) (map[string]any, error) {
	schema, err := ctx.configAPI().fetchSchema(ctx.app)
	if err != nil {
		return nil, mapConfigError(err, ctx, "fetch schema")
	}
	return schema, nil
}

func schemaKeySpecs(schema map[string]any) []schemaKeySpec {
	configs, _ := schema["configs"].([]any)
	specs := make([]schemaKeySpec, 0)
	for _, raw := range configs {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		path := clicore.StringValue(block["path"])
		if path == "" {
			continue
		}
		specs = append(specs, schemaFieldSpecs(path, block["fields"], false)...)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Key < specs[j].Key })
	return specs
}

func schemaFieldSpecs(prefix string, rawFields any, inheritedSensitive bool) []schemaKeySpec {
	fields, _ := rawFields.([]any)
	specs := make([]schemaKeySpec, 0, len(fields))
	for _, raw := range fields {
		field, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := clicore.StringValue(field["name"])
		if name == "" {
			continue
		}
		key := prefix + "." + name
		sensitive := inheritedSensitive || clicore.Truthy(field["sensitive"])
		if nested, ok := field["fields"].([]any); ok && len(nested) > 0 {
			specs = append(specs, schemaFieldSpecs(key, nested, sensitive)...)
			continue
		}
		path, leaf := splitConfigKey(key)
		specs = append(specs, schemaKeySpec{
			Key:         key,
			Path:        path,
			Field:       leaf,
			Type:        clicore.StringValue(field["type"]),
			Required:    clicore.Truthy(field["required"]),
			Default:     clicore.StringValue(field["default"]),
			Sensitive:   sensitive,
			Description: clicore.StringValue(field["description"]),
		})
	}
	return specs
}

func splitConfigKey(key string) (path, field string) {
	idx := strings.LastIndex(key, ".")
	if idx <= 0 || idx == len(key)-1 {
		return key, ""
	}
	return key[:idx], key[idx+1:]
}

func configStatusForSpec(spec schemaKeySpec, config map[string]any) configKeyStatus {
	row := configKeyStatus{
		Key:       spec.Key,
		Type:      spec.Type,
		Required:  spec.Required,
		Sensitive: spec.Sensitive,
		Status:    "missing",
	}
	if value, ok := pluckConfigKey(config, spec.Key); ok {
		row.Status = "set"
		row.Value = value
		return row
	}
	if spec.Default != "" {
		row.Status = "default"
		row.Default = spec.Default
		row.Value = spec.Default
	}
	return row
}

func formatConfigStatus(row configKeyStatus) string {
	detail := ""
	switch row.Status {
	case "set", "default":
		detail = " " + clicore.StringValue(row.Value)
	case "missing":
		if row.Required {
			detail = " required"
		}
	}
	return fmt.Sprintf("  %-40s %-8s%s", row.Key, row.Status, detail)
}

func filterSensitiveConfigWarnings(warnings []string, specs []schemaKeySpec) []string {
	if len(warnings) == 0 {
		return nil
	}
	sensitive := map[string]bool{}
	for _, spec := range specs {
		if spec.Sensitive {
			sensitive[spec.Key] = true
		}
	}
	if len(sensitive) == 0 {
		return warnings
	}
	out := warnings[:0]
	for _, warning := range warnings {
		skip := false
		for key := range sensitive {
			if strings.HasPrefix(warning, key+":") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, warning)
		}
	}
	return out
}

func findSchemaKeySpec(specs []schemaKeySpec, key string) (schemaKeySpec, bool) {
	for _, spec := range specs {
		if spec.Key == key {
			return spec, true
		}
	}
	return schemaKeySpec{}, false
}

func suggestSchemaKey(specs []schemaKeySpec, key string) string {
	if key == "" {
		return ""
	}
	_, leaf := splitConfigKey(key)
	if leaf == "" {
		leaf = key
	}
	var match string
	for _, spec := range specs {
		if spec.Sensitive {
			continue
		}
		if spec.Field != leaf {
			continue
		}
		if match != "" {
			return ""
		}
		match = spec.Key
	}
	return match
}

func pluckConfigKey(config map[string]any, key string) (any, bool) {
	current := any(config)
	for _, segment := range strings.Split(key, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func secretSetCommand(ctx *secretsCtx, key string) string {
	return fmt.Sprintf("putnami cloud secrets set %s %s --env %s --from-stdin", ctx.app, key, ctx.environment)
}
