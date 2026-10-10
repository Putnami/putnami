package configcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	protocfg "go.putnami.dev/protocol/config"
	"gopkg.in/yaml.v3"
)

// PublishDefaultEnvironment is the env used when `--env` is unset, matching
// the secrets command.
const PublishDefaultEnvironment = "prod"

const publishConfigPlaceholder = "__PUTNAMI_CONFIG_REQUIRED__"

// rollDefer is the `roll` query value that suppresses the config-put revision
// roll for a single PUT /api/configs write. The server accepts "defer" or
// "skip" (rollDeferred in config-api's configs handler); `config put` and
// publish send it on every block but the last so one command rolls exactly ONE
// revision, re-resolving the now-complete config, rather than one revision per
// block.
const rollDefer = "defer"

// publishCtx mirrors secretsCtx's discovery shape — workspace/app/env from
// the link + workspace-root putnami.json + env flag, plus auth token. The
// duplication is small enough to keep as a sibling struct until a third
// caller justifies extraction.
type publishCtx struct {
	workspaceID   string
	controlPlane  string
	app           string
	environment   string
	authToken     clicore.Bearer
	workspaceRoot string
	io            clicore.IO
	params        map[string]any
	env           map[string]string
}

// blockWrite is one resolved {path, values} PUT to /api/configs the
// publish loop intends to send, built from the schema's block list
// intersected with the values tree resolved from conf/env*.yaml files.
type blockWrite struct {
	Path   string         `json:"path"`
	Values map[string]any `json:"values"`
}

// configValidationDiag is one client-side schema-validation failure for a
// planned block write, mirroring a config-server rejection.
type configValidationDiag struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// validateBlockWrites runs the framework's publish-time schema policy over the
// planned block writes on the CLIENT, so a value the config server would reject
// (a config the runtime could not load, discoverable only at boot)
// fails at publish/put instead. It uses the SAME validators the server runs in
// its PUT handler — ValidatePathInSchema then ValidateConfigEntryAgainstSchema —
// applied to the normalized schema document that is about to be published, so
// the client and server converge on identical diagnostics.
func validateBlockWrites(schemaDoc map[string]any, appName, environment string, writes []blockWrite) ([]configValidationDiag, error) {
	if schemaDoc == nil {
		return nil, nil
	}
	raw, err := json.Marshal(schemaDoc)
	if err != nil {
		return nil, fmt.Errorf("encode schema manifest for validation: %w", err)
	}
	var manifest protocfg.SchemaManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("parse schema manifest for validation: %w", err)
	}
	var out []configValidationDiag
	for _, write := range writes {
		for _, d := range protocfg.ValidatePathInSchema(write.Path, &manifest) {
			out = append(out, configValidationDiag{Path: write.Path, Message: d.Message})
		}
		entry := protocfg.ConfigEntry{
			AppName:     appName,
			Environment: environment,
			Path:        write.Path,
			Values:      write.Values,
		}
		for _, d := range protocfg.ValidateConfigEntryAgainstSchema(&entry, &manifest) {
			out = append(out, configValidationDiag{Path: write.Path, Message: d.Message})
		}
	}
	return out, nil
}

// configValidationError renders block-scoped validation diagnostics into one
// usage error (nil when there are none), so a failed publish/put reads with the
// same per-field diagnostics the server would have returned.
func configValidationError(diags []configValidationDiag) error {
	if len(diags) == 0 {
		return nil
	}
	lines := make([]string, 0, len(diags))
	for _, d := range diags {
		lines = append(lines, fmt.Sprintf("config block %q: %s", d.Path, d.Message))
	}
	return clicore.NewError(
		"config values rejected by schema validation (these would fail at the config server):\n  "+strings.Join(lines, "\n  "),
		clicore.ExitUsage,
	)
}

type schemaBlock struct {
	Path   string
	Fields []schemaField
	// Optional mirrors protocol-config Block.Optional: a block whose presence is
	// contingent (feature-gated, optional integration). A non-optional (default)
	// block that produces zero committed config values is the silent-skip
	// class the strict dry-run gate rejects; an optional one is allowed to be
	// empty.
	Optional bool
}

type schemaField struct {
	Name      string
	Type      string
	Required  bool
	Sensitive bool
	Default   any
	Fields    []schemaField
	Items     []schemaField
}

// PublishConfig backs `putnami cloud config publish`. It fills the three
// schema-gated PUT/POST endpoints — schemas first, then per-block configs —
// from artifacts the upstream `putnami publish` (which freezes at v0.1.0-
// fb17ed0b and has no extension hook) would otherwise leave unpushed.
// Secrets stay user-managed via `cloud secrets set`; see plan Phase D.
func PublishConfig(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	return publishConfig(params, args, workspaceRoot, env, ioctx)
}

func publishConfig(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if clicore.StringParam(params, "config-from", "configFrom") != "" {
		return clicore.NewError("--config-from is a config write override; use `putnami cloud config put <project> --config-from <path>`", clicore.ExitUsage)
	}
	// --strict is a local, auth-free pre-publish gate that only sharpens the
	// --dry-run plan (it fails on a declared non-optional block that would
	// publish zero values — the silent-skip class). It never writes and
	// never authenticates, so it is meaningless without --dry-run.
	if clicore.Truthy(clicore.Param(params, "strict")) && !clicore.Truthy(clicore.Param(params, "dry-run", "dryRun")) {
		return clicore.NewError("--strict only applies to --dry-run; run `putnami cloud config publish --dry-run --strict`", clicore.ExitUsage)
	}
	clicore.AdoptPositionalApp(params, args)
	app, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return err
	}
	schemaPath, found, err := resolveSchemaPath(params, workspaceRoot, app)
	if err != nil {
		return err
	}
	if !found {
		// --if-present backs the publish-verb task: a project may expose a
		// migration bundle or Docker image but no config schema, and those
		// must no-op rather than fail the whole publish.
		if clicore.Truthy(clicore.Param(params, "if-present", "ifPresent")) {
			WritePublishResult(map[string]any{
				"status": "skipped",
				"app":    app,
				"reason": "no config schema for " + app,
			}, params, ioctx, fmt.Sprintf("No config schema for %s — skipping config publish.", app), nil)
			return nil
		}
		return clicore.NewError(
			fmt.Sprintf("no schema artifact for app %q (looked for schema/config.json, .gen/config-schema.json). "+
				"Run `putnami config-extract --impacted` from the app directory before publishing.", app),
			clicore.ExitUsage,
		)
	}
	ctx, err := newPublishCtxForApp(params, workspaceRoot, env, ioctx, app)
	if err != nil {
		return err
	}
	schemaDoc, schemaBlocks, err := loadSchemaManifest(schemaPath)
	if err != nil {
		return err
	}
	// The schema artifact embeds its own appName (for the TS extractor, the
	// package.json name), but the CPA keys schema rows AND config-value writes
	// by the resolved project path (ctx.app) — the same identity the value PUTs
	// below, the deploy bootstrap gate, and the running service all use. When
	// the artifact's appName diverges, the schema registers under one row while
	// values validate against another, 412-ing forever with no hint two rows
	// exist. Override it here so the publish is atomic-by-key regardless
	// of artifact staleness or extractor behavior; warn (below) when we had to
	// rewrite it so the divergence is visible.
	schemaArtifactAppName := clicore.StringValue(schemaDoc["appName"])
	schemaDoc["appName"] = ctx.app
	valuesPaths := resolveValuesPaths(workspaceRoot, ctx.app, ctx.environment)
	merged, err := loadAndMergeYAML(valuesPaths)
	if err != nil {
		return err
	}
	normalizeSchemaManifestForPublish(schemaDoc, merged)
	if !clicore.Truthy(clicore.Param(params, "dry-run", "dryRun")) {
		if err := ensurePublishValuesReady(ctx, schemaBlocks, merged, valuesPaths); err != nil {
			return err
		}
	}
	writes, warnings := planBlockWrites(schemaBlocks, merged)
	validationDiags, err := validateBlockWrites(schemaDoc, ctx.app, ctx.environment, writes)
	if err != nil {
		return err
	}
	if schemaArtifactAppName != "" && schemaArtifactAppName != ctx.app {
		warnings = append(warnings,
			fmt.Sprintf("schema artifact declares appName %q but publishing under project path %q — registering the schema under %q so it matches the config value writes and the deploy bootstrap gate.", schemaArtifactAppName, ctx.app, ctx.app),
		)
	}
	if len(schemaBlocks) > 0 && len(valuesPaths) == 0 {
		warnings = append(warnings,
			fmt.Sprintf("no publishable config value files found for %s/%s — create conf/env.yaml or conf/env.%s.yaml to publish non-secret config values.", ctx.app, ctx.environment, ctx.environment),
		)
	}
	// Warn (don't fail) when .env.local.yaml is present: publish
	// intentionally excludes it (resolveValuesPaths comment), but if the
	// user only put a value there they'd get silent omission. Surface
	// it so they can move the value into env.yaml / env.<env>.yaml
	// before re-publishing.
	if localPath := findLocalOverrideFile(workspaceRoot, ctx.app); localPath != "" {
		warnings = append(warnings,
			fmt.Sprintf("ignoring local override %s — values in this file are NOT published; move non-sensitive values to env.yaml or env.%s.yaml to ship them.", localPath, ctx.environment),
		)
	}

	if clicore.Truthy(clicore.Param(params, "dry-run", "dryRun")) {
		plan := map[string]any{
			"status":      "dry-run",
			"workspace":   ctx.workspaceID,
			"app":         ctx.app,
			"environment": ctx.environment,
			"schemaPath":  schemaPath,
			"schema":      schemaDoc,
			"valuesPaths": valuesPaths,
			"configs":     writes,
			"warnings":    warnings,
		}
		if len(validationDiags) > 0 {
			plan["validation"] = validationDiags
		}
		WritePublishResult(plan, params, ioctx, fmt.Sprintf("Dry run: would publish schema for %s and %d config block(s) for %s/%s.", ctx.app, len(writes), ctx.app, ctx.environment), warnings)
		// Surface the diagnostics after the plan so --dry-run doubles as a
		// pre-publish gate that fails loud without writing anything. --strict
		// additionally fails on a declared non-optional block that would publish
		// zero values, or on missing/placeholder committed values — the readiness
		// checks a plain --dry-run intentionally skips. It is a pure
		// read: it never scaffolds or writes.
		if clicore.Truthy(clicore.Param(params, "strict")) {
			if err := strictDryRunError(ctx, schemaBlocks, merged, writes); err != nil {
				return err
			}
		}
		return configValidationError(validationDiags)
	}

	// A value the server validator would reject fails here, before the schema
	// or any config block is written.
	if err := configValidationError(validationDiags); err != nil {
		return err
	}

	// Schema must land first; configs and secrets are schema-gated. A 4xx
	// here is a hard fail (handled by the surface) — there is no retry-with-
	// hint path because the *publish* call is what registers the schema.
	if err := ctx.configAPI().registerSchema(schemaDoc); err != nil {
		return mapPublishError(err, ctx, "register schema")
	}

	type blockResult struct {
		Path   string   `json:"path"`
		Keys   []string `json:"keys"`
		Status string   `json:"status"`
	}
	results := make([]blockResult, 0, len(writes))
	for i, write := range writes {
		body := map[string]any{
			"appName":     ctx.app,
			"environment": ctx.environment,
			"path":        write.Path,
			"values":      write.Values,
		}
		// Defer the inline-primary revision roll on every write but the
		// last, so a multi-block publish rolls exactly ONE revision carrying the
		// complete config instead of one per block. This is the server-side
		// contract (PUT /api/configs `roll=defer`, served by config-api) and
		// `config put` has always honored it — publish-config sending an empty
		// query rolled N revisions for N blocks, each one a synchronous re-stamp +
		// readiness poll inside the same request.
		roll := ""
		if i < len(writes)-1 {
			roll = rollDefer
		}
		if _, err := ctx.configAPI().putConfig(body, roll, ""); err != nil {
			return mapPublishError(err, ctx, fmt.Sprintf("write config %q", write.Path))
		}
		results = append(results, blockResult{Path: write.Path, Keys: sortedKeys(write.Values), Status: "stored"})
	}

	out := map[string]any{
		"status":      "published",
		"workspace":   ctx.workspaceID,
		"app":         ctx.app,
		"environment": ctx.environment,
		"schemaPath":  schemaPath,
		"valuesPaths": valuesPaths,
		"configs":     results,
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	WritePublishResult(out, params, ioctx, fmt.Sprintf("Published schema + %d config block(s) for %s/%s.", len(results), ctx.app, ctx.environment), warnings)
	return nil
}

// WritePublishResult writes the command result envelope and, for non-JSON
// output, appends any warning lines. Exported because the distribution-domain
// publish verbs (publish-archives, publish-migration) share this result shape.
func WritePublishResult(data any, params map[string]any, ioctx clicore.IO, message string, warnings []string) {
	clicore.WriteResult(data, params, ioctx, message)
	if clicore.StructuredOutput(params) || len(warnings) == 0 {
		return
	}
	ioctx.Stdout("Warnings:")
	for _, warning := range warnings {
		ioctx.Stdout("  - " + warning)
	}
}

func newPublishCtx(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*publishCtx, error) {
	app, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return nil, err
	}
	return newPublishCtxForApp(params, workspaceRoot, env, ioctx, app)
}

func newPublishCtxForApp(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, app string) (*publishCtx, error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, err
	}
	environment := clicore.FirstString(clicore.StringParam(params, "env", "environment"), clicore.StringValue(link["environment"]), PublishDefaultEnvironment)
	workspaceID := clicore.StringValue(link["workspace_id"])
	authToken := clicore.NewBearer("")
	if !clicore.Truthy(clicore.Param(params, "dry-run", "dryRun")) {
		auth, err := clicore.WorkspaceAuth(params, env, ioctx, workspaceID)
		if err != nil {
			return nil, err
		}
		authToken = clicore.NewBearer(auth.AccessToken)
	}
	return &publishCtx{
		workspaceID:   workspaceID,
		controlPlane:  clicore.ControlPlaneBaseURL(params, env, clicore.StringValue(link["control_plane_url"])),
		app:           app,
		environment:   environment,
		authToken:     authToken,
		workspaceRoot: workspaceRoot,
		io:            ioctx,
		params:        params,
		env:           env,
	}, nil
}

// resolveSchemaPath finds the schema artifact for the app. The upstream
// putnami-go `config-extract` task writes to <project>/schema/config.json
// (committed) and falls back to .gen/config-schema.json (generated). We
// search both. --schema-from <path> overrides discovery — absolute or
// workspace-root-relative.
// The bool result reports whether a schema artifact was discovered. A false
// with a nil error means the app exposes no config schema — the publish-verb's
// --if-present treats that as a soft skip rather than a failure. A non-nil
// error is a hard failure (an invalid --schema-from override or an
// unresolvable app).
func resolveSchemaPath(params map[string]any, workspaceRoot, appName string) (string, bool, error) {
	if override := clicore.StringParam(params, "schema-from", "schemaFrom"); override != "" {
		path := override
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspaceRoot, path)
		}
		if _, err := os.Stat(path); err != nil {
			return "", false, clicore.NewError(fmt.Sprintf("--schema-from %s: %s", override, err.Error()), clicore.ExitUsage)
		}
		return path, true, nil
	}
	appDir, err := clicore.FindAppDir(workspaceRoot, appName)
	if err != nil {
		return "", false, err
	}
	candidates := []string{
		filepath.Join(appDir, "schema", "config.json"),
		filepath.Join(appDir, ".gen", "config-schema.json"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true, nil
		}
	}
	return "", false, nil
}

// loadSchemaManifest reads the JSON schema artifact and returns both the
// raw document (we POST it verbatim — the server unmarshals into
// schemas.ConfigSchema which embeds protocol-config.SchemaManifest) and
// the parsed block list we walk to plan the values writes.
func loadSchemaManifest(path string) (map[string]any, []schemaBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, clicore.NewError(fmt.Sprintf("read schema %s: %s", path, err.Error()), clicore.ExitUsage)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, clicore.NewError(fmt.Sprintf("parse schema %s: %s", path, err.Error()), clicore.ExitUsage)
	}
	blocks, err := parseSchemaBlocks(doc, path)
	if err != nil {
		return nil, nil, err
	}
	return doc, blocks, nil
}

func parseSchemaBlocks(doc map[string]any, label string) ([]schemaBlock, error) {
	configs, _ := doc["configs"].([]any)
	blocks := make([]schemaBlock, 0, len(configs))
	for _, raw := range configs {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		bp := clicore.StringValue(block["path"])
		if bp == "" {
			continue
		}
		blocks = append(blocks, schemaBlock{Path: bp, Fields: parseSchemaFields(block["fields"]), Optional: clicore.Truthy(block["optional"])})
	}
	if clicore.StringValue(doc["appName"]) == "" {
		return nil, clicore.NewError(fmt.Sprintf("schema %s missing appName", label), clicore.ExitUsage)
	}
	return blocks, nil
}

func parseSchemaFields(raw any) []schemaField {
	fields, _ := raw.([]any)
	out := make([]schemaField, 0, len(fields))
	for _, item := range fields {
		fieldMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := clicore.StringValue(fieldMap["name"])
		if name == "" {
			continue
		}
		field := schemaField{
			Name:      name,
			Type:      clicore.StringValue(fieldMap["type"]),
			Required:  clicore.Truthy(fieldMap["required"]),
			Sensitive: clicore.Truthy(fieldMap["sensitive"]),
			Fields:    parseSchemaFields(fieldMap["fields"]),
		}
		if defaultValue, ok := fieldMap["default"]; ok {
			field.Default = defaultValue
		}
		if items, ok := fieldMap["items"].(map[string]any); ok {
			field.Items = parseSchemaFields(items["fields"])
		}
		out = append(out, field)
	}
	return out
}

var publishSchemaFieldTypes = map[string]bool{
	"string":   true,
	"int":      true,
	"float":    true,
	"bool":     true,
	"duration": true,
	"object":   true,
	"array":    true,
	"map":      true,
}

// normalizeSchemaManifestForPublish keeps publish compatible with older
// extractors that emitted language-specific type names such as ServerConfig
// or Level instead of the protocol vocabulary. The source artifact is left
// untouched; only the POST body is normalized.
func normalizeSchemaManifestForPublish(doc map[string]any, values map[string]any) {
	configs, _ := doc["configs"].([]any)
	for _, raw := range configs {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		blockValues := extractBlockValues(values, clicore.StringValue(block["path"]))
		normalizeSchemaFieldsForPublish(block["fields"], blockValues)
	}
}

func normalizeSchemaFieldsForPublish(rawFields any, values map[string]any) {
	fields, _ := rawFields.([]any)
	for _, raw := range fields {
		field, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fieldValue := any(nil)
		if values != nil {
			fieldValue = values[clicore.StringValue(field["name"])]
		}
		fieldType := clicore.StringValue(field["type"])
		if !publishSchemaFieldTypes[fieldType] {
			originalType := fieldType
			fieldType = inferPublishSchemaFieldType(field, fieldValue)
			field["type"] = fieldType
			if originalType == "array" || originalType == "map" {
				delete(field, "items")
				delete(field, "keys")
				delete(field, "values")
			}
		}

		if fieldType == "object" {
			nested, _ := fieldValue.(map[string]any)
			normalizeSchemaFieldsForPublish(field["fields"], nested)
		}
	}
}

func inferPublishSchemaFieldType(field map[string]any, value any) string {
	if inferred := inferPublishValueType(value); inferred != "" {
		return inferred
	}
	if defaultValue := clicore.StringValue(field["default"]); defaultValue != "" {
		return inferPublishDefaultType(defaultValue)
	}
	if fields, ok := field["fields"].([]any); ok && len(fields) > 0 {
		return "object"
	}
	typeName := strings.ToLower(clicore.StringValue(field["type"]))
	fieldName := strings.ToLower(clicore.StringValue(field["name"]))
	if strings.Contains(typeName, "duration") || strings.Contains(fieldName, "duration") || strings.Contains(fieldName, "timeout") || strings.Contains(fieldName, "ttl") {
		return "duration"
	}
	switch typeName {
	case "level", "loglevel", "url", "dsn":
		return "string"
	}
	// Prefer object for unknown legacy type names; it preserves structured blocks
	// instead of flattening potentially nested config into a scalar.
	return "object"
}

func inferPublishValueType(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return "string"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "int"
	case float32:
		return "float"
	case float64:
		if v == float64(int64(v)) {
			return "int"
		}
		return "float"
	case map[string]any, map[any]any:
		return "object"
	case []any:
		return "object"
	default:
		return ""
	}
}

func inferPublishDefaultType(value string) string {
	switch strings.ToLower(value) {
	case "true", "false":
		return "bool"
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return "int"
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return "float"
	}
	if _, err := time.ParseDuration(value); err == nil {
		return "duration"
	}
	return "string"
}

// findLocalOverrideFile returns the absolute path of <appDir>/conf/
// .env.local.yaml or env.local.yaml when present, or "" when absent or
// the app dir can't be resolved. Used by the publish command to warn that
// local values will not be shipped — see resolveValuesPaths for why it's
// excluded.
func findLocalOverrideFile(workspaceRoot, appName string) string {
	appDir := workspaceRoot
	if found, err := clicore.FindAppDir(workspaceRoot, appName); err == nil {
		appDir = found
	}
	for _, candidate := range []string{
		filepath.Join(appDir, "conf", ".env.local.yaml"),
		filepath.Join(appDir, "conf", "env.local.yaml"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// resolveValuesPaths returns the ordered list of value-source files the
// publish loop merges, from least specific to most specific. Generated env
// artifacts win over source files because they are the framework's
// build-merged runtime view for the target env when present. Visible
// env.yaml/env.<env>.yaml files are preferred for committed non-secret Cloud
// config; hidden .env*.yaml files remain accepted for compatibility with the
// local config loader. The local-override file (.env.local.yaml) is
// intentionally excluded — its purpose is local-machine development overrides
// and pushing it would silently poison the server-side layer with secrets/dev
// URLs.
func resolveValuesPaths(workspaceRoot, appName, environment string) []string {
	appDir := workspaceRoot
	if found, err := clicore.FindAppDir(workspaceRoot, appName); err == nil {
		appDir = found
	}
	candidates := []string{
		filepath.Join(appDir, "conf", "env.yaml"),
		filepath.Join(appDir, "conf", ".env.yaml"),
		filepath.Join(appDir, "conf", fmt.Sprintf("env.%s.yaml", environment)),
		filepath.Join(appDir, "conf", fmt.Sprintf(".env.%s.yaml", environment)),
		filepath.Join(appDir, ".gen", "conf", fmt.Sprintf("env.%s.yaml", environment)),
		filepath.Join(appDir, ".gen", "conf", fmt.Sprintf(".env.%s.yaml", environment)),
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			out = append(out, candidate)
		}
	}
	return out
}

func resolveConfigFromPaths(params map[string]any, workspaceRoot string) ([]string, error) {
	pattern := clicore.StringParam(params, "config-from", "configFrom")
	if pattern == "" {
		return nil, clicore.NewError("config put requires --config-from <path-or-glob>", clicore.ExitUsage)
	}
	pathPattern := pattern
	if !filepath.IsAbs(pathPattern) {
		pathPattern = filepath.Join(workspaceRoot, pathPattern)
	}
	var matches []string
	if strings.ContainsAny(pathPattern, "*?[") {
		globMatches, _ := filepath.Glob(pathPattern)
		matches = globMatches
	} else if _, err := os.Stat(pathPattern); err == nil {
		matches = []string{pathPattern}
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return nil, clicore.NewError(fmt.Sprintf("--config-from %s matched no files", pattern), clicore.ExitUsage)
	}
	return matches, nil
}

// loadAndMergeYAML deep-merges YAML files in the given order. Later files
// overwrite earlier ones — same precedence the framework uses on boot
// (base env first, then env-specific values).
func loadAndMergeYAML(paths []string) (map[string]any, error) {
	merged := map[string]any{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, clicore.NewError(fmt.Sprintf("read %s: %s", path, err.Error()), clicore.ExitUsage)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, clicore.NewError(fmt.Sprintf("parse YAML %s: %s", path, err.Error()), clicore.ExitUsage)
		}
		doc = normalizeMapAny(doc)
		deepMerge(merged, doc)
	}
	return merged, nil
}

// publishReadinessProblems returns the two READ-ONLY readiness signals the
// publish flow checks, without writing anything: placeholder markers already
// present in the committed values, and the required non-secret fields that have
// no committed value (as dotted paths). It is the pure checking half of
// ensurePublishValuesReady — the strict --dry-run gate uses it to fail
// loud without ever scaffolding a file, and ensurePublishValuesReady layers its
// scaffold-writing behavior on top of the same signals so the two never drift.
func publishReadinessProblems(blocks []schemaBlock, merged map[string]any) (placeholders, missing []string) {
	placeholders = placeholderPaths(merged)
	missing = placeholderPaths(missingPublishScaffold(blocks, merged))
	return placeholders, missing
}

func ensurePublishValuesReady(ctx *publishCtx, blocks []schemaBlock, merged map[string]any, valuesPaths []string) error {
	if markers, _ := publishReadinessProblems(blocks, merged); len(markers) > 0 {
		return clicore.NewError(
			fmt.Sprintf("publish config for %s/%s still contains %s marker(s) at %s; edit the generated env file before publishing.",
				ctx.app, ctx.environment, publishConfigPlaceholder, strings.Join(markers, ", ")),
			clicore.ExitUsage,
		)
	}
	scaffold := missingPublishScaffold(blocks, merged)
	if len(scaffold) == 0 {
		return nil
	}
	missing := placeholderPaths(scaffold)
	target, err := publishEnvPath(ctx.workspaceRoot, ctx.app, ctx.environment)
	if err != nil {
		return err
	}
	targetExists := false
	for _, path := range valuesPaths {
		if clicore.SamePath(path, target) {
			targetExists = true
			break
		}
	}
	if !targetExists {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
		}
		data, err := yaml.Marshal(scaffold)
		if err != nil {
			return fmt.Errorf("render %s scaffold: %w", target, err)
		}
		header := []byte("# Putnami Cloud non-secret config scaffold.\n# Replace every " + publishConfigPlaceholder + " marker before publishing.\n\n")
		if err := os.WriteFile(target, append(header, data...), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		return clicore.NewError(
			fmt.Sprintf("created %s with placeholder values for %s; edit every %s marker and rerun `putnami publish %s --env %s`.",
				target, strings.Join(missing, ", "), publishConfigPlaceholder, ctx.app, ctx.environment),
			clicore.ExitUsage,
		)
	}
	return clicore.NewError(
		fmt.Sprintf("missing non-secret config values for %s/%s: %s. Edit %s before publishing.",
			ctx.app, ctx.environment, strings.Join(missing, ", "), target),
		clicore.ExitUsage,
	)
}

// strictDryRunError is the `--dry-run --strict` gate. It is a
// PURE read over the already-computed plan — it never scaffolds or writes — and
// fails when:
//
//   - a declared NON-OPTIONAL schema block would publish zero config values (the
//     committed-but-unpublished class a plain --dry-run passes silently), or
//   - the committed values still carry placeholder markers, or
//   - a required non-secret value has no committed value.
//
// It returns nil when the plan is publish-ready. Optional blocks with no values
// are not failures. Callers still surface validation diagnostics separately.
func strictDryRunError(ctx *publishCtx, blocks []schemaBlock, merged map[string]any, writes []blockWrite) error {
	placeholders, missing := publishReadinessProblems(blocks, merged)
	written := make(map[string]bool, len(writes))
	for _, w := range writes {
		written[w.Path] = true
	}
	var problems []string
	for _, p := range placeholders {
		problems = append(problems, fmt.Sprintf("%s still holds the %s marker", p, publishConfigPlaceholder))
	}
	for _, m := range missing {
		problems = append(problems, fmt.Sprintf("required non-secret value %s has no committed value", m))
	}
	for _, block := range blocks {
		if block.Optional || written[block.Path] {
			continue
		}
		// A block whose emptiness is already explained field-level (a missing
		// required value under it) is not reported again as a block-level line.
		if blockHasMissingField(block.Path, missing) {
			continue
		}
		problems = append(problems, fmt.Sprintf("declared block %q produces no config values — nothing committed for it", block.Path))
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return clicore.NewError(
		fmt.Sprintf("strict publish check failed for %s/%s:\n  %s\nCommit the missing values, or mark the block optional in the schema, before publishing.",
			ctx.app, ctx.environment, strings.Join(problems, "\n  ")),
		clicore.ExitUsage,
	)
}

// blockHasMissingField reports whether any dotted missing-value path falls under
// blockPath (equal to it or prefixed by "blockPath."), so the strict gate can
// suppress a redundant block-level "no values" line when a field-level line for
// the same block is already present.
func blockHasMissingField(blockPath string, missing []string) bool {
	for _, m := range missing {
		if m == blockPath || strings.HasPrefix(m, blockPath+".") {
			return true
		}
	}
	return false
}

func publishEnvPath(workspaceRoot, appName, environment string) (string, error) {
	appDir, err := clicore.FindAppDir(workspaceRoot, appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(appDir, "conf", fmt.Sprintf("env.%s.yaml", environment)), nil
}

func placeholderPaths(tree map[string]any) []string {
	var out []string
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		switch v := value.(type) {
		case string:
			if v == publishConfigPlaceholder && prefix != "" {
				out = append(out, prefix)
			}
		case map[string]any:
			for _, key := range sortedKeys(v) {
				next := key
				if prefix != "" {
					next = prefix + "." + key
				}
				walk(next, v[key])
			}
		case []any:
			for i, item := range v {
				next := fmt.Sprintf("%s[%d]", prefix, i)
				walk(next, item)
			}
		}
	}
	walk("", tree)
	sort.Strings(out)
	return out
}

func missingPublishScaffold(blocks []schemaBlock, merged map[string]any) map[string]any {
	out := map[string]any{}
	for _, block := range blocks {
		values := extractBlockValues(merged, block.Path)
		scaffold := missingFieldScaffold(block.Fields, values, false)
		if len(scaffold) == 0 {
			continue
		}
		insertBlockValues(out, block.Path, scaffold)
	}
	return out
}

func missingFieldScaffold(fields []schemaField, values map[string]any, parentRequired bool) map[string]any {
	out := map[string]any{}
	for _, field := range fields {
		if field.Sensitive || field.Default != nil {
			continue
		}
		required := parentRequired || field.Required
		value, exists := values[field.Name]
		if exists {
			if len(field.Fields) == 0 {
				continue
			}
			nested, ok := value.(map[string]any)
			if !ok {
				continue
			}
			child := missingFieldScaffold(field.Fields, nested, required)
			if len(child) > 0 {
				out[field.Name] = child
			}
			continue
		}
		if !required {
			if len(field.Fields) > 0 {
				child := missingFieldScaffold(field.Fields, nil, false)
				if len(child) > 0 {
					out[field.Name] = child
				}
			}
			continue
		}
		if scaffold, ok := fullFieldScaffold(field); ok {
			out[field.Name] = scaffold
		}
	}
	return out
}

func fullFieldScaffold(field schemaField) (any, bool) {
	if field.Sensitive || field.Default != nil {
		return nil, false
	}
	if len(field.Fields) > 0 {
		child := fullFieldsScaffold(field.Fields)
		if len(child) > 0 {
			return child, true
		}
		return publishConfigPlaceholder, true
	}
	if len(field.Items) > 0 {
		child := fullFieldsScaffold(field.Items)
		if len(child) > 0 {
			return []any{child}, true
		}
		return []any{publishConfigPlaceholder}, true
	}
	return publishConfigPlaceholder, true
}

func fullFieldsScaffold(fields []schemaField) map[string]any {
	out := map[string]any{}
	for _, field := range fields {
		if scaffold, ok := fullFieldScaffold(field); ok {
			out[field.Name] = scaffold
		}
	}
	return out
}

func insertBlockValues(tree map[string]any, blockPath string, values map[string]any) {
	current := tree
	segments := strings.Split(blockPath, ".")
	for _, segment := range segments[:len(segments)-1] {
		next, _ := current[segment].(map[string]any)
		if next == nil {
			next = map[string]any{}
			current[segment] = next
		}
		current = next
	}
	current[segments[len(segments)-1]] = values
}

// normalizeMapAny converts yaml.v3's `map[interface{}]interface{}` (which
// it never emits for JSON-safe shapes but does for inline anchors and
// some integer keys) into pure `map[string]any` recursively. We need this
// so the JSON encoder can serialize the values map on the wire.
func normalizeMapAny(value any) map[string]any {
	out := map[string]any{}
	switch v := value.(type) {
	case map[string]any:
		for k, sub := range v {
			out[k] = normalizeAny(sub)
		}
	case map[any]any:
		for k, sub := range v {
			out[fmt.Sprint(k)] = normalizeAny(sub)
		}
	}
	return out
}

func normalizeAny(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return normalizeMapAny(v)
	case map[any]any:
		return normalizeMapAny(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalizeAny(item)
		}
		return out
	default:
		return v
	}
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := dst[k].(map[string]any); ok {
				deepMerge(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}

// planBlockWrites projects the merged values tree onto the schema's
// declared block paths. Each block.path is a dotted address (e.g.
// "core.database"); we navigate the tree and emit a PUT for the leaf
// values map if non-empty. Top-level YAML keys not declared in the schema
// are surfaced as warnings — they would silently fall through at runtime
// (no schema → no resolution) so flagging them at publish saves the user
// hours.
func planBlockWrites(blocks []schemaBlock, merged map[string]any) ([]blockWrite, []string) {
	writes := make([]blockWrite, 0, len(blocks))
	declared := map[string]bool{}
	for _, block := range blocks {
		declared[topSegment(block.Path)] = true
		values := extractBlockValues(merged, block.Path)
		values = publishValuesForSchema(block.Fields, values)
		if len(values) == 0 {
			continue
		}
		writes = append(writes, blockWrite{Path: block.Path, Values: values})
	}
	var warnings []string
	for key := range merged {
		if !declared[key] {
			warnings = append(warnings, fmt.Sprintf("top-level YAML key %q is not declared in the schema and will not be published", key))
		}
	}
	sort.Strings(warnings)
	sort.Slice(writes, func(i, j int) bool { return writes[i].Path < writes[j].Path })
	return writes, warnings
}

func publishValuesForSchema(fields []schemaField, values map[string]any) map[string]any {
	out := map[string]any{}
	for _, field := range fields {
		if field.Sensitive {
			continue
		}
		value, hasValue := values[field.Name]
		if hasValue {
			switch {
			case len(field.Fields) > 0:
				if nested, ok := value.(map[string]any); ok {
					out[field.Name] = publishValuesForSchema(field.Fields, nested)
					continue
				}
			case len(field.Items) > 0:
				if list, ok := value.([]any); ok {
					out[field.Name] = publishListValuesForSchema(field.Items, list)
					continue
				}
			}
			out[field.Name] = value
			continue
		}
		if field.Default != nil {
			out[field.Name] = publishDefaultValue(field)
		}
	}
	return out
}

func publishListValuesForSchema(fields []schemaField, values []any) []any {
	out := make([]any, len(values))
	for i, item := range values {
		if nested, ok := item.(map[string]any); ok {
			out[i] = publishValuesForSchema(fields, nested)
			continue
		}
		out[i] = item
	}
	return out
}

func publishDefaultValue(field schemaField) any {
	if v, ok := field.Default.(string); ok {
		switch field.Type {
		case "bool":
			if parsed, err := strconv.ParseBool(v); err == nil {
				return parsed
			}
		case "int":
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
				return parsed
			}
		case "float":
			if parsed, err := strconv.ParseFloat(v, 64); err == nil {
				return parsed
			}
		}
	}
	return field.Default
}

func topSegment(path string) string {
	if head, _, ok := strings.Cut(path, "."); ok {
		return head
	}
	return path
}

func extractBlockValues(tree map[string]any, blockPath string) map[string]any {
	current := any(tree)
	for segment := range strings.SplitSeq(blockPath, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = m[segment]
		if !ok {
			return nil
		}
	}
	leaf, ok := current.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(leaf))
	maps.Copy(out, leaf)
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// mapPublishError attaches the verb context and, for schema-gating 400s
// during the configs phase, a hint pointing at the failed schema step.
// Schema-gating should not fire because we POST /api/schemas first — but
// if it does, surfacing the upstream message + hint cuts straight to the
// fix.
func mapPublishError(err error, ctx *publishCtx, verb string) error {
	var apiErr *configAPIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.schemaNotRegistered {
		return clicore.NewError(
			fmt.Sprintf("cannot %s for app %q: %s. The publish flow registers the schema before writing configs; this means the schema POST failed silently or the server rejected it.",
				verb, ctx.app, apiErr.message),
			clicore.ExitAPI,
		)
	}
	return clicore.NewError(fmt.Sprintf("%s: %s%s", verb, apiErr.message, formatPublishAPIDetails(apiErr.details)), clicore.ExitAPI)
}

// formatPublishAPIDetails renders the problems config-api listed for a refused
// schema manifest.
func formatPublishAPIDetails(details []string) string {
	parts := make([]string, 0, len(details))
	for _, detail := range details {
		if detail = strings.TrimSpace(detail); detail != "" {
			parts = append(parts, detail)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, "; ")
}
