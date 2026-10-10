package configcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/config/authoredmember"
)

const (
	// AuthoredConfigEnvironment is the first qualified native Config member
	// environment. Other environments remain on the compatibility publisher.
	AuthoredConfigEnvironment = "prod"
	configMemberDirectory     = "config-member"
	configMemberFilename      = "member.json"
	maxVersionArtifactBytes   = 64 << 10
)

var (
	nativeConfigPart  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)
	fullGitRevision   = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	authoredGitOutput = func(ctx context.Context, root string, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		return command.Output()
	}
)

// ConfigNamespaceOptionLayers lists the project manifest option layers, in
// ascending precedence, that may declare the native Put namespace of the
// project's authored Config member. The workspace probe (which announces the
// member) and the package/publish steps (which build it) must agree on them:
// a project that declares no namespace has no Config member, so those steps
// skip instead of failing the workspace gate.
var ConfigNamespaceOptionLayers = []string{"publish", "@putnami/cloud", "@putnami/cloud:publish", "@putnami/cloud:publish-config"}

// ConfigNamespaceFromOptions resolves the declared Config namespace from parsed
// manifest options. It reports declared=false when no layer names one and
// fails closed on a value that is not one canonical native path segment.
func ConfigNamespaceFromOptions(options map[string]map[string]any) (string, bool, error) {
	namespace, declared := "", false
	for _, layer := range ConfigNamespaceOptionLayers {
		values, found := options[layer]
		if !found {
			continue
		}
		value, found := values["namespace"]
		if !found {
			continue
		}
		candidate, ok := value.(string)
		if !ok || candidate != strings.TrimSpace(candidate) || !nativeConfigPart.MatchString(candidate) {
			return "", false, fmt.Errorf("options.%s.namespace must be one canonical native path segment", layer)
		}
		namespace, declared = candidate, true
	}
	return namespace, declared, nil
}

// DeclaredConfigNamespace reads the project's manifest and resolves the Config
// namespace it declares through ConfigNamespaceFromOptions.
func DeclaredConfigNamespace(workspaceRoot, project string) (string, bool, error) {
	dir, err := clicore.FindAppDir(workspaceRoot, project)
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "putnami.json"))
	if err != nil {
		return "", false, fmt.Errorf("read %s manifest: %w", project, err)
	}
	var manifest struct {
		Options map[string]map[string]any `json:"options"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", false, fmt.Errorf("parse %s manifest: %w", project, err)
	}
	return ConfigNamespaceFromOptions(manifest.Options)
}

// PreparedAuthoredConfig is the exact local Config member identity used by
// both the package and publish steps. Member bytes are immutable and contain
// no secret values.
//
// ProjectID is the workspace project id the release-set plan carries for the
// member: "/" + the project's workspace-relative path, never its manifest
// name. The two differ for every nested project (name "putnami.dev", path
// "sites/putnami.dev"), and the managed publish step matches the selected
// member on this id. ProjectPath holds the same value.
type PreparedAuthoredConfig struct {
	ProjectPath        string
	ProjectID          string
	Namespace          string
	Package            string
	Coordinate         string
	Version            string
	SchemaPath         string
	ValuesPaths        []string
	Member             authoredmember.AuthoredMember
	PackagedMemberPath string
}

// AuthoredConfigCoordinate derives the one native Put coordinate owned by a
// Config project. It has no dependency on language bundle naming.
func AuthoredConfigCoordinate(namespace, project string) (string, error) {
	namespace = strings.TrimSpace(namespace)
	if !nativeConfigPart.MatchString(namespace) || project == "" || project != strings.TrimSpace(project) ||
		strings.HasPrefix(project, "/") || strings.HasSuffix(project, "/") || strings.Contains(project, "//") || strings.Contains(project, "..") {
		return "", fmt.Errorf("Config publication requires a canonical namespace and project")
	}
	packageName := strings.ReplaceAll(project, "/", "-") + "-config"
	if !nativeConfigPart.MatchString(packageName) {
		return "", fmt.Errorf("Config project does not map to a native Put package")
	}
	return namespace + "/" + packageName, nil
}

// PrepareAuthoredConfig builds the canonical, secret-free member from local
// schema and production values. It performs no network request and returns nil
// only for an absent schema combined with --if-present.
func PrepareAuthoredConfig(params map[string]any, args []string, workspaceRoot string, ioctx clicore.IO) (*PreparedAuthoredConfig, error) {
	clicore.AdoptPositionalApp(params, args)
	project, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return nil, err
	}
	schemaPath, found, err := resolveSchemaPath(params, workspaceRoot, project)
	if err != nil {
		return nil, err
	}
	if !found {
		if clicore.Truthy(clicore.Param(params, "if-present", "ifPresent")) {
			return nil, nil
		}
		return nil, clicore.NewError(fmt.Sprintf("no schema artifact for app %q", project), clicore.ExitUsage)
	}
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, err
	}
	environment := clicore.FirstString(
		clicore.StringParam(params, "env", "environment"),
		clicore.StringValue(link["environment"]),
		PublishDefaultEnvironment,
	)
	if environment != AuthoredConfigEnvironment {
		return nil, clicore.NewError("native Config member publication currently supports env=prod only", clicore.ExitUsage)
	}
	workspaceID := clicore.StringValue(link["workspace_id"])
	// An explicit --namespace wins; otherwise the project's own manifest
	// declaration decides, exactly as the workspace probe decides whether the
	// project has a Config member at all. Undeclared means "no member": the
	// package step's --if-present skips, and only an explicit request fails.
	namespace := clicore.StringParam(params, "namespace")
	if namespace == "" {
		declared, found, err := DeclaredConfigNamespace(workspaceRoot, project)
		if err != nil {
			return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
		}
		if !found {
			if clicore.Truthy(clicore.Param(params, "if-present", "ifPresent")) {
				return nil, nil
			}
			return nil, clicore.NewError(fmt.Sprintf("app %q declares no native Config namespace: set options[\"@putnami/cloud:publish-config\"].namespace or pass --namespace", project), clicore.ExitUsage)
		}
		namespace = declared
	}
	coordinate, err := AuthoredConfigCoordinate(namespace, project)
	if err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	packageName := strings.TrimPrefix(coordinate, namespace+"/")

	schemaDoc, _, err := loadSchemaManifest(schemaPath)
	if err != nil {
		return nil, err
	}
	valuesPaths := resolveValuesPaths(workspaceRoot, project, environment)
	merged, err := loadAndMergeYAML(valuesPaths)
	if err != nil {
		return nil, err
	}
	normalizeSchemaManifestForPublish(schemaDoc, merged)
	blocks, err := parseSchemaBlocks(schemaDoc, schemaPath)
	if err != nil {
		return nil, err
	}
	if err := validateAuthoredArraySafety(blocks); err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	writes, _ := planBlockWrites(blocks, merged)
	validation, err := validateBlockWrites(schemaDoc, project, environment, writes)
	if err != nil {
		return nil, err
	}
	if err := configValidationError(validation); err != nil {
		return nil, err
	}
	placeholders, missing := publishReadinessProblems(blocks, merged)
	if len(placeholders) != 0 || len(missing) != 0 {
		problems := append([]string(nil), placeholders...)
		problems = append(problems, missing...)
		return nil, clicore.NewError("native Config member is missing committed production values: "+strings.Join(problems, ", "), clicore.ExitUsage)
	}

	schema, references, err := normalizeAuthoredSchema(blocks)
	if err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	projectDir, err := clicore.FindAppDir(workspaceRoot, project)
	if err != nil {
		return nil, err
	}
	schema, err = extendAuthoredSchema(projectDir, schema)
	if err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	authoredValues, err := authoredValues(blocks, writes)
	if err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	repository, revision, err := authoredSourceProvenance(ioctx.Context, workspaceRoot)
	if err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	version, err := authoredConfigVersion(workspaceRoot, project)
	if err != nil {
		return nil, err
	}
	projectPath, err := workspaceProjectPath(workspaceRoot, projectDir)
	if err != nil {
		return nil, err
	}
	member, err := (authoredmember.Publisher{}).Build(contextOrBackground(ioctx.Context), authoredmember.BuildAuthoredMemberRequest{
		WorkspaceID: workspaceID,
		Project:     memberProject(projectPath, project),
		Environment: environment,
		Schema:      schema,
		AuthoredLayers: []authoredmember.AuthoredLayer{{
			Name: "application", Pin: revision, Values: authoredValues,
		}},
		SecretReferences: references,
		SourceProvenance: authoredmember.SourceProvenance{Repository: repository, Revision: revision},
	})
	if err != nil {
		return nil, clicore.NewError("build native Config member: "+err.Error(), clicore.ExitUsage)
	}
	return &PreparedAuthoredConfig{
		ProjectPath: projectPath, ProjectID: projectPath,
		Namespace: namespace, Package: packageName, Coordinate: coordinate,
		Version: version, SchemaPath: schemaPath, ValuesPaths: append([]string(nil), valuesPaths...),
		Member: member, PackagedMemberPath: AuthoredConfigMemberPath(workspaceRoot, project),
	}, nil
}

// PackageAuthoredConfig writes the exact canonical bytes consumed later by the
// managed publish step. The atomic rename prevents a partial package artifact.
func PackageAuthoredConfig(params map[string]any, args []string, workspaceRoot string, ioctx clicore.IO) (*PreparedAuthoredConfig, error) {
	prepared, err := PrepareAuthoredConfig(params, args, workspaceRoot, ioctx)
	if err != nil || prepared == nil {
		return prepared, err
	}
	if err := clicore.WriteFileAtomic(prepared.PackagedMemberPath, prepared.Member.Bytes, 0o644); err != nil {
		return nil, clicore.NewError("write native Config member: "+err.Error(), clicore.ExitAPI)
	}
	return prepared, nil
}

// VerifyPackagedAuthoredConfig proves that publish consumes the package step's
// complete artifact and that rebuilding the current local inputs is byte exact.
func VerifyPackagedAuthoredConfig(ctx context.Context, prepared *PreparedAuthoredConfig) error {
	if prepared == nil {
		return clicore.NewError("native Config member is unavailable", clicore.ExitUsage)
	}
	file, err := os.Open(prepared.PackagedMemberPath)
	if err != nil {
		return clicore.NewError("read packaged Config member: "+err.Error(), clicore.ExitUsage)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, authoredmember.MaxMemberSize+1))
	if err != nil {
		return clicore.NewError("read packaged Config member: "+err.Error(), clicore.ExitUsage)
	}
	if len(data) > authoredmember.MaxMemberSize {
		return clicore.NewError("packaged Config member exceeds the size limit", clicore.ExitUsage)
	}
	if _, err := (authoredmember.Publisher{}).Validate(contextOrBackground(ctx), authoredmember.AuthoredMember{Bytes: data}); err != nil {
		return clicore.NewError("validate packaged Config member: "+err.Error(), clicore.ExitUsage)
	}
	if !bytes.Equal(data, prepared.Member.Bytes) {
		return clicore.NewError("packaged Config member differs from current canonical inputs", clicore.ExitUsage)
	}
	return nil
}

// AuthoredConfigMemberPath is the deterministic package artifact location.
func AuthoredConfigMemberPath(workspaceRoot, project string) string {
	return filepath.Join(workspaceRoot, ".putnami", "out", filepath.FromSlash(strings.TrimPrefix(project, "/")), configMemberDirectory, configMemberFilename)
}

func normalizeAuthoredSchema(blocks []schemaBlock) (authoredmember.NormalizedSchema, []authoredmember.SecretReference, error) {
	fields := make([]authoredmember.SchemaField, 0)
	references := make([]authoredmember.SecretReference, 0)
	for _, block := range blocks {
		for _, field := range block.Fields {
			if err := appendAuthoredSchemaField(block.Path+"."+field.Name, field, &fields, &references, false, block.Optional); err != nil {
				return authoredmember.NormalizedSchema{}, nil, err
			}
		}
	}
	return authoredmember.NormalizedSchema{Fields: fields}, references, nil
}

// appendAuthoredSchemaField normalizes one schema field. optionalBlock reports
// that the schema marks the field's block optional.
func appendAuthoredSchemaField(path string, field schemaField, fields *[]authoredmember.SchemaField, references *[]authoredmember.SecretReference, inArray, optionalBlock bool) error {
	kind, err := authoredFieldType(field.Type)
	if err != nil {
		return fmt.Errorf("Config field %s: %w", path, err)
	}
	if field.Sensitive {
		// Every secret is a reference, so a prepared boot binding delivers it
		// when it is set. The schema field says whether the boot needs its
		// value: Config refuses to prepare a member whose required reference
		// is not set, and skips an optional one that is not set. Neither
		// runtime needs a value for a field with a default: both apply the
		// default. Neither needs a block the schema marks optional.
		needed := field.Required && !optionalBlock && field.Default == nil
		*fields = append(*fields, authoredmember.SchemaField{Path: path, Type: kind, Required: needed, Sensitive: true})
		*references = append(*references, authoredmember.SecretReference{Path: path, Reference: "config-secret://" + strings.ReplaceAll(path, ".", "/")})
		return nil
	}
	if len(field.Fields) != 0 {
		if inArray {
			*fields = append(*fields, authoredmember.SchemaField{Path: path, Type: kind, Required: field.Required})
		}
		for _, child := range field.Fields {
			if err := appendAuthoredSchemaField(path+"."+child.Name, child, fields, references, inArray, optionalBlock); err != nil {
				return err
			}
		}
		return nil
	}
	*fields = append(*fields, authoredmember.SchemaField{Path: path, Type: kind, Required: field.Required})
	if len(field.Items) != 0 {
		for _, child := range field.Items {
			if err := appendAuthoredSchemaField(path+"."+child.Name, child, fields, references, true, optionalBlock); err != nil {
				return err
			}
		}
	}
	return nil
}

func authoredFieldType(kind string) (string, error) {
	switch kind {
	case "string", "duration":
		return "string", nil
	case "int":
		return "integer", nil
	case "float":
		return "number", nil
	case "bool":
		return "boolean", nil
	case "object", "map":
		return "object", nil
	case "array":
		return "array", nil
	default:
		return "", fmt.Errorf("unsupported normalized type %q", kind)
	}
}

func authoredValues(blocks []schemaBlock, writes []blockWrite) (map[string]any, error) {
	byBlock := make(map[string]map[string]any, len(writes))
	for _, write := range writes {
		byBlock[write.Path] = write.Values
	}
	values := map[string]any{}
	for _, block := range blocks {
		for _, field := range block.Fields {
			if err := appendAuthoredValue(block.Path+"."+field.Name, field, byBlock[block.Path][field.Name], values); err != nil {
				return nil, err
			}
		}
	}
	return values, nil
}

func validateAuthoredArraySafety(blocks []schemaBlock) error {
	var validateFields func(string, []schemaField) error
	validateFields = func(prefix string, fields []schemaField) error {
		for _, field := range fields {
			path := prefix + "." + field.Name
			if len(field.Items) != 0 && hasSensitiveSchemaField(field.Items) {
				return fmt.Errorf("Config field %s mixes array values with sensitive descendants and cannot be published safely", path)
			}
			if err := validateFields(path, field.Fields); err != nil {
				return err
			}
		}
		return nil
	}
	for _, block := range blocks {
		if err := validateFields(block.Path, block.Fields); err != nil {
			return err
		}
	}
	return nil
}

func appendAuthoredValue(path string, field schemaField, value any, values map[string]any) error {
	if field.Sensitive || value == nil {
		return nil
	}
	if len(field.Fields) != 0 {
		nested, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("Config field %s is not a canonical object", path)
		}
		for _, child := range field.Fields {
			if err := appendAuthoredValue(path+"."+child.Name, child, nested[child.Name], values); err != nil {
				return err
			}
		}
		return nil
	}
	if len(field.Items) != 0 && hasSensitiveSchemaField(field.Items) {
		return fmt.Errorf("Config field %s mixes array values with sensitive descendants and cannot be published safely", path)
	}
	values[path] = value
	return nil
}

func hasSensitiveSchemaField(fields []schemaField) bool {
	for _, field := range fields {
		if field.Sensitive || hasSensitiveSchemaField(field.Fields) || hasSensitiveSchemaField(field.Items) {
			return true
		}
	}
	return false
}

func authoredSourceProvenance(parent context.Context, workspaceRoot string) (string, string, error) {
	ctx, cancel := context.WithTimeout(contextOrBackground(parent), 10*time.Second)
	defer cancel()
	revisionBytes, err := authoredGitOutput(ctx, workspaceRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("resolve Config source revision: %w", err)
	}
	revision := strings.TrimSpace(string(revisionBytes))
	if !fullGitRevision.MatchString(revision) {
		return "", "", fmt.Errorf("Config source revision is not a full Git object id")
	}
	remoteBytes, err := authoredGitOutput(ctx, workspaceRoot, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("resolve Config source repository: %w", err)
	}
	provider, repositoryKey, err := clicore.RepositoryKeyFromRemote(strings.TrimSpace(string(remoteBytes)))
	if err != nil || provider != "github" {
		return "", "", fmt.Errorf("resolve Config source repository: canonical GitHub origin required")
	}
	return "github.com/" + repositoryKey, revision, nil
}

func authoredConfigVersion(workspaceRoot, project string) (string, error) {
	projectDir, err := clicore.FindAppDir(workspaceRoot, project)
	if err != nil {
		return "", err
	}
	path := filepath.Join(projectDir, ".gen", "version.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", clicore.NewError("read native Config version: "+err.Error(), clicore.ExitUsage)
	}
	if len(data) == 0 || len(data) > maxVersionArtifactBytes {
		return "", clicore.NewError("native Config version artifact is empty or oversized", clicore.ExitUsage)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return "", clicore.NewError("parse native Config version: "+err.Error(), clicore.ExitUsage)
	}
	version := clicore.StringValue(document["version"])
	if version == "" || len(version) > 256 || version != strings.TrimSpace(version) || strings.ContainsAny(version, "/\x00\r\n") {
		return "", clicore.NewError("native Config version is not canonical", clicore.ExitUsage)
	}
	return version, nil
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// memberProject is the project the member descriptor names. Control prepares
// the member for the release-set project, its workspace-relative path
// ("sites/putnami.dev"), and Config refuses a descriptor that names anything
// else, so the manifest name ("putnami.dev") would fail every nested project.
// The root project keeps its manifest name.
func memberProject(projectPath, name string) string {
	if path := strings.TrimPrefix(projectPath, "/"); path != "" {
		return path
	}
	return name
}

// workspaceProjectPath returns the project id the framework's release-set
// plan records for a project directory: "/" + its path relative to the
// workspace root without grouping folders such as "(web)", "/" for the root
// project (clicore.ProjectIDFromPath). Symlinked roots resolve first so a
// checkout reached through an alias yields the same id as the real path.
func workspaceProjectPath(workspaceRoot, projectDir string) (string, error) {
	resolvedRoot := workspaceRoot
	if evaluated, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		resolvedRoot = evaluated
	}
	rel, err := filepath.Rel(resolvedRoot, projectDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", clicore.NewError("Config project is outside the workspace", clicore.ExitUsage)
	}
	return clicore.ProjectIDFromPath(filepath.ToSlash(rel)), nil
}
