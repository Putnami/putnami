package job

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Job context error codes for validation failures.
const (
	ErrorCodeInvalidContext = "job.invalid_context"
	ErrorCodeMissingField   = "job.missing_field"
)

// Parse decodes a job context document leniently: unknown fields are
// ignored so an older extension binary keeps working when a newer
// orchestrator adds fields. SDKs use this entry point.
func Parse(data []byte) (*Context, error) {
	var ctx Context
	if err := json.Unmarshal(data, &ctx); err != nil {
		return nil, fmt.Errorf("parsing job context: %w", err)
	}
	return &ctx, nil
}

// ParseFile reads and leniently parses a job context JSON file.
func ParseFile(path string) (*Context, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading context file: %w", err)
	}
	ctx, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing context file: %w", err)
	}
	return ctx, nil
}

// ParseStrict decodes a job context document in strict mode (unknown
// fields rejected). Producers and conformance suites use this entry
// point to catch fields that are not part of the contract.
func ParseStrict(data []byte) (*Context, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var ctx Context
	if err := dec.Decode(&ctx); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(
			ErrorCodeInvalidContext, "", "invalid job context JSON: %v", err,
		)}
	}
	presence, err := parseContextPresence(data)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(
			ErrorCodeInvalidContext, "", "invalid job context JSON: %v", err,
		)}
	}
	ctx.presence = presence
	return &ctx, nil
}

// parseContextPresence records member presence after strict decoding has
// established the document's shape. This raw pass answers only absent vs
// present (including present null); it never replaces DisallowUnknownFields.
func parseContextPresence(data []byte) (contextPresence, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return contextPresence{}, err
	}
	nestedHas := func(object, member string) bool {
		raw, ok := root[object]
		if !ok {
			return false
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			return false
		}
		_, ok = members[member]
		return ok
	}
	_, invocation := root["invocation"]
	_, selection := root["selection"]
	_, workspaceProjects := root["workspaceProjects"]
	_, userScope := root["userScope"]
	return contextPresence{
		workspaceOptions:         nestedHas("workspace", "options"),
		workspaceProjects:        workspaceProjects,
		extensionRuntimePath:     nestedHas("extension", "runtimePath"),
		extensionCacheRoot:       nestedHas("extension", "cacheRoot"),
		projectMetadata:          nestedHas("project", "metadata"),
		projectType:              nestedHas("project", "type"),
		projectDependencyClosure: nestedHas("project", "dependencyClosure"),
		invocation:               invocation,
		selection:                selection,
		userScope:                userScope,
	}, nil
}

// Validate checks the structural invariants every produced context
// must satisfy.
func Validate(ctx *Context) []diag.Diagnostic {
	if ctx == nil {
		return []diag.Diagnostic{diag.Errorf(
			ErrorCodeInvalidContext, "", "job context is nil",
		)}
	}

	var diags []diag.Diagnostic
	require := func(field, value string) {
		if value == "" {
			diags = append(diags, diag.Errorf(
				ErrorCodeMissingField, field, "%s is required", field,
			))
		}
	}

	require("workspaceRoot", ctx.WorkspaceRoot)
	require("outputPath", ctx.OutputPath)
	require("cacheRoot", ctx.CacheRoot)
	require("extension.name", ctx.Extension.Name)
	require("job.name", ctx.Job.Name)
	require("project.name", ctx.Project.Name)
	for i, project := range ctx.SelectedProjects {
		prefix := fmt.Sprintf("selectedProjects[%d]", i)
		require(prefix+".name", project.Name)
		require(prefix+".path", project.Path)
		require(prefix+".fullPath", project.FullPath)
	}

	// Version-scoped rules (strict_v2.go). Everything above applies to every
	// version, so a v1 document is checked exactly as it was before v2 existed.
	diags = append(diags, validateVersioned(ctx)...)
	return diags
}

// ParseAndValidate runs strict parsing followed by validation.
func ParseAndValidate(data []byte) (*Context, []diag.Diagnostic) {
	ctx, diags := ParseStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return ctx, append(diags, Validate(ctx)...)
}
