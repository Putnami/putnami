package deliverycli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	ciproto "go.putnami.dev/protocol/ci"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
)

// The putnami.ci.json contract commands: `putnami cloud ci init|validate|fmt|
// explain`. They replace the framework's `putnami ci …`
// verbs of the same names and use the published protocol/ci package, so the
// document they write and the verdict they give are the framework's.

// seededCICommands is the order `ci init` writes the discovered commands in:
// the usual verification order, so two workspaces with the same jobs get the
// same file.
var seededCICommands = []string{"lint", "test", "build", "validate"}

// CIContractReport is the result of one contract command. Document and
// Explanation never hold a resolved secret.
type CIContractReport struct {
	Path        string               `json:"path"`
	Action      string               `json:"action"`
	Valid       bool                 `json:"valid"`
	Changed     bool                 `json:"changed"`
	Applied     bool                 `json:"applied,omitempty"`
	Version     int                  `json:"version,omitempty"`
	Digest      string               `json:"digest,omitempty"`
	Diagnostics []diag.Diagnostic    `json:"diagnostics"`
	Document    *ciproto.Document    `json:"document,omitempty"`
	Explanation *ciproto.Explanation `json:"explanation,omitempty"`
}

// ciContractVerb runs one contract verb. args still carry the verb as their
// first positional.
func ciContractVerb(verb string, params map[string]any, args []string, workspaceRoot string, ioctx clicore.IO) error {
	if extra := clicore.Positionals(args); len(extra) > 1 {
		return clicore.NewError("cloud ci "+verb+" takes no positional argument; got "+strings.Join(extra[1:], " "), clicore.ExitUsage)
	}
	switch verb {
	case "init":
		return ciInit(params, workspaceRoot, ioctx)
	case "validate":
		return ciValidate(params, workspaceRoot, ioctx)
	case "fmt":
		return ciFormat(params, workspaceRoot, ioctx)
	default:
		input, err := parseCIExplainInput(params)
		if err != nil {
			return err
		}
		return ciExplain(params, workspaceRoot, input, ioctx)
	}
}

// ciInit writes a version 3 document that runs the verification jobs the
// workspace's extensions declare. It declares no distribution and no
// environment, so it asks for no publication and no deployment authority.
func ciInit(params map[string]any, workspaceRoot string, ioctx clicore.IO) error {
	path := filepath.Join(workspaceRoot, ciproto.Filename)
	if _, err := os.Stat(path); err == nil && !clicore.Truthy(clicore.Param(params, "force")) {
		return clicore.NewError(ciproto.Filename+" already exists; run `putnami cloud ci init --force` to replace it", clicore.ExitUsage)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return clicore.NewError("inspect "+ciproto.Filename+": "+err.Error(), clicore.ExitUsage)
	}
	graph, err := loadCIGraph(workspaceRoot)
	if err != nil {
		return err
	}
	document := ciproto.DefaultDocument()
	document.Commands = document.Commands[:0]
	for _, command := range seededCICommands {
		if graph.known[command] != "" {
			document.Commands = append(document.Commands, ciproto.CommandEntry{Name: command})
		}
	}
	if len(document.Commands) == 0 {
		return clicore.NewError("no lint, test, build or validate job is declared by the workspace's extensions; write the commands by hand", clicore.ExitUsage)
	}
	data, err := json.Marshal(document)
	if err != nil {
		return clicore.NewError("encode "+ciproto.Filename+": "+err.Error(), clicore.ExitUsage)
	}
	data, err = ciproto.Format(data)
	if err != nil {
		return clicore.NewError("format "+ciproto.Filename+": "+err.Error(), clicore.ExitUsage)
	}
	if err := clicore.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", ciproto.Filename, err)
	}
	document, _ = ciproto.Parse(data)
	digest, _ := ciproto.Digest(document)
	return emitCIContractReport(params, ioctx, CIContractReport{
		Path: path, Action: "init", Valid: true, Changed: true, Applied: true,
		Version: ciproto.Version, Digest: digest, Diagnostics: []diag.Diagnostic{}, Document: &document,
	})
}

// ciValidate checks the document, resolves each command against the jobs the
// workspace's extensions declare, and refuses provider sections when no
// installed extension serves release sets.
func ciValidate(params map[string]any, workspaceRoot string, ioctx clicore.IO) error {
	_, report, err := loadAndValidateCIContract(workspaceRoot, "validate")
	if emitErr := emitCIContractReport(params, ioctx, report); emitErr != nil {
		return emitErr
	}
	return err
}

// ciFormat rewrites putnami.ci.json in canonical form; --check only reports.
func ciFormat(params map[string]any, workspaceRoot string, ioctx clicore.IO) error {
	data, report, validationErr := loadAndValidateCIContract(workspaceRoot, "fmt")
	if validationErr != nil {
		if emitErr := emitCIContractReport(params, ioctx, report); emitErr != nil {
			return emitErr
		}
		return validationErr
	}
	formatted, err := ciproto.Format(data)
	if err != nil {
		return clicore.NewError("format "+ciproto.Filename+": "+err.Error(), clicore.ExitUsage)
	}
	report.Changed = !bytes.Equal(data, formatted)
	check := clicore.Truthy(clicore.Param(params, "check"))
	if check && report.Changed {
		if emitErr := emitCIContractReport(params, ioctx, report); emitErr != nil {
			return emitErr
		}
		return clicore.NewError(ciproto.Filename+" is not in canonical form; run `putnami cloud ci fmt`", clicore.ExitUsage)
	}
	if !check && report.Changed {
		if err := clicore.WriteFileAtomic(report.Path, formatted, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", ciproto.Filename, err)
		}
		report.Applied = true
	}
	return emitCIContractReport(params, ioctx, report)
}

// ciExplain evaluates the rules against facts the caller states. Nothing
// remote is read, so the answer says what the document asks for, not what the
// provider will grant.
func ciExplain(params map[string]any, workspaceRoot string, input ciproto.ExplainInput, ioctx clicore.IO) error {
	data, report, validationErr := loadAndValidateCIContract(workspaceRoot, "explain")
	if validationErr != nil {
		if emitErr := emitCIContractReport(params, ioctx, report); emitErr != nil {
			return emitErr
		}
		return validationErr
	}
	document, err := ciproto.Parse(data)
	if err != nil {
		return clicore.NewError("parse "+ciproto.Filename+": "+err.Error(), clicore.ExitUsage)
	}
	explanation, err := ciproto.Explain(document, input)
	if err != nil {
		return clicore.NewError("cloud ci explain: "+err.Error(), clicore.ExitUsage)
	}
	report.Explanation = &explanation
	return emitCIContractReport(params, ioctx, report)
}

// parseCIExplainInput turns the explain flags into the protocol's assumption.
// Each event is evaluated against exactly one fact, so a tag with a branch or
// a push with a pull-request number is a usage error, not a silently ignored
// flag. The tag is --git-tag: the framework reads --tag as its project filter
// before an extension command sees it.
func parseCIExplainInput(params map[string]any) (ciproto.ExplainInput, error) {
	input := ciproto.ExplainInput{
		Event:  ciproto.Event(clicore.StringParam(params, "event")),
		Branch: clicore.StringParam(params, "branch"),
		Tag:    clicore.StringParam(params, "git-tag", "gitTag", "tag"),
	}
	if !input.Event.Valid() {
		return input, clicore.NewError("cloud ci explain needs --event push|tag|pull_request", clicore.ExitUsage)
	}
	if raw := clicore.StringParam(params, "pr"); raw != "" {
		number, err := strconv.Atoi(raw)
		if err != nil || number <= 0 {
			return input, clicore.NewError("cloud ci explain: --pr must be a positive integer", clicore.ExitUsage)
		}
		if input.Event != ciproto.EventPullRequest {
			return input, clicore.NewError("cloud ci explain: --pr is valid only with --event pull_request", clicore.ExitUsage)
		}
		input.PullRequestNumber = number
	}
	if input.Event == ciproto.EventTag {
		if input.Tag == "" {
			return input, clicore.NewError("cloud ci explain --event tag needs --git-tag <tag>", clicore.ExitUsage)
		}
		if input.Branch != "" {
			return input, clicore.NewError("cloud ci explain: --branch is not evaluated for a tag", clicore.ExitUsage)
		}
		return input, nil
	}
	if input.Tag != "" {
		return input, clicore.NewError("cloud ci explain: --git-tag is valid only with --event tag", clicore.ExitUsage)
	}
	if input.Branch == "" {
		return input, clicore.NewError("cloud ci explain needs --branch <branch>", clicore.ExitUsage)
	}
	return input, nil
}

func loadAndValidateCIContract(workspaceRoot, action string) ([]byte, CIContractReport, error) {
	path := filepath.Join(workspaceRoot, ciproto.Filename)
	report := CIContractReport{Path: path, Action: action, Diagnostics: []diag.Diagnostic{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, report, clicore.NewError(ciproto.Filename+" not found; run `putnami cloud ci init`", clicore.ExitUsage)
	}
	if err != nil {
		return nil, report, clicore.NewError("read "+ciproto.Filename+": "+err.Error(), clicore.ExitUsage)
	}
	document, findings := ciproto.ParseWithDiagnostics(data)
	if !diag.HasErrors(findings) {
		graph, graphErr := loadCIGraph(workspaceRoot)
		if graphErr != nil {
			return data, report, graphErr
		}
		findings = append(findings, ciproto.ValidateTaskReferences(document, graph.known)...)
		findings = append(findings, validateCIProviderSections(document, graph)...)
	}
	report.Diagnostics = findings
	report.Valid = !diag.HasErrors(findings)
	if report.Valid {
		report.Version = document.Version
		report.Digest, _ = ciproto.Digest(document)
		return data, report, nil
	}
	return data, report, clicore.NewError(fmt.Sprintf("%s has %d validation error(s)", ciproto.Filename, ciDiagnosticErrorCount(findings)), clicore.ExitUsage)
}

// validateCIProviderSections refuses `distribution` and `envs` when no
// installed extension serves release sets: without one there is no channel,
// environment, visibility or grant to serve them.
func validateCIProviderSections(document ciproto.Document, graph ciGraph) []diag.Diagnostic {
	if !ciproto.HasProviderSections(document) || graph.provider != "" {
		return nil
	}
	field := "distribution"
	if document.Distribution == nil {
		field = "envs"
	}
	message := "declares release-set authority, but no installed extension serves the %q command; install a release-set provider or delete the section"
	return []diag.Diagnostic{diag.Errorf("ci.provider_required", field, message, distribution.ProviderCommandName)}
}

// ciGraph is the workspace side of validation: the job names a command must
// resolve against, and the extension that serves release sets.
type ciGraph struct {
	known    map[string]string
	provider string
}

// loadCIGraph builds the job vocabulary the way the framework does: the union
// of the `commands` every discovered extension declares. Discovery reads three
// places, first match wins per extension name:
//
//  1. workspace projects that hold a putnami.extension.json, unless the
//     workspace pins that extension by name (the published build wins);
//  2. each extension the workspace declares: a path, else the installed copy
//     under .putnami/bin/extensions/<name>;
//  3. the same name under node_modules.
//
// It does not read the lock file's artifact directories and does not apply the
// hosted-run rules; an extension found only there is reported as unreadable.
func loadCIGraph(workspaceRoot string) (ciGraph, error) {
	refs, err := workspaceExtensionRefs(workspaceRoot)
	if err != nil {
		return ciGraph{}, err
	}
	pinnedByName := map[string]bool{}
	for _, ref := range refs {
		if !isExtensionPathRef(ref) {
			pinnedByName[ref] = true
		}
	}
	var manifests []*extproto.Manifest
	seen := map[string]bool{}
	add := func(manifest *extproto.Manifest) {
		if manifest == nil || seen[manifest.Name] {
			return
		}
		seen[manifest.Name] = true
		manifests = append(manifests, manifest)
	}
	projectManifests, err := projectExtensionManifests(workspaceRoot)
	if err != nil {
		return ciGraph{}, err
	}
	setAside := map[string]*extproto.Manifest{}
	for _, manifest := range projectManifests {
		if pinnedByName[manifest.Name] {
			setAside[manifest.Name] = manifest
			continue
		}
		add(manifest)
	}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		manifest, loadErr := loadDeclaredExtension(workspaceRoot, ref)
		if loadErr != nil {
			// A pinned build that is not installed falls back to the
			// workspace project of the same name, as the framework does.
			if project := setAside[ref]; project != nil {
				add(project)
				continue
			}
			return ciGraph{}, clicore.NewError(fmt.Sprintf("cannot resolve the CI graph while extension %q is unreadable: %v; run `putnami install`", ref, loadErr), clicore.ExitUsage)
		}
		add(manifest)
	}
	graph := ciGraph{known: map[string]string{}}
	var providers []string
	for _, manifest := range manifests {
		for name := range manifest.Commands {
			graph.known[name] = name
		}
		if _, ok := manifest.Commands[distribution.ProviderCommandName]; ok {
			providers = append(providers, manifest.Name)
		}
	}
	sort.Strings(providers)
	switch len(providers) {
	case 0:
	case 1:
		graph.provider = providers[0]
	default:
		return ciGraph{}, clicore.NewError(fmt.Sprintf("extensions %v all declare the %q command; only one may serve it", providers, distribution.ProviderCommandName), clicore.ExitUsage)
	}
	return graph, nil
}

// workspaceExtensionRefs lists the `extensions` of the workspace manifest: an
// object keyed by extension name or path, or an array of them.
func workspaceExtensionRefs(workspaceRoot string) ([]string, error) {
	path := clicore.ManifestPath(workspaceRoot)
	if path == "" {
		return nil, clicore.NewError("no putnami.workspace.json in "+workspaceRoot, clicore.ExitUsage)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, clicore.NewError("read "+filepath.Base(path)+": "+err.Error(), clicore.ExitUsage)
	}
	var manifest struct {
		Extensions json.RawMessage `json:"extensions"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, clicore.NewError("parse "+filepath.Base(path)+": "+err.Error(), clicore.ExitUsage)
	}
	if len(manifest.Extensions) == 0 || string(manifest.Extensions) == "null" {
		return nil, nil
	}
	var keyed map[string]json.RawMessage
	if err := json.Unmarshal(manifest.Extensions, &keyed); err == nil {
		refs := make([]string, 0, len(keyed))
		for ref := range keyed {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		return refs, nil
	}
	var listed []string
	if err := json.Unmarshal(manifest.Extensions, &listed); err != nil {
		return nil, clicore.NewError(filepath.Base(path)+" extensions must be an object or a list of names", clicore.ExitUsage)
	}
	return listed, nil
}

// isExtensionPathRef reports whether a workspace extension key names a path
// rather than a published extension.
func isExtensionPathRef(ref string) bool {
	return strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../")
}

// loadDeclaredExtension loads one declared extension from where the framework
// would find it.
func loadDeclaredExtension(workspaceRoot, ref string) (*extproto.Manifest, error) {
	var candidates []string
	if isExtensionPathRef(ref) {
		if filepath.IsAbs(ref) {
			candidates = append(candidates, ref)
		}
		candidates = append(candidates, filepath.Join(workspaceRoot, strings.TrimPrefix(ref, "/")))
	} else {
		candidates = append(candidates,
			filepath.Join(workspaceRoot, "node_modules", filepath.FromSlash(ref)),
			filepath.Join(workspaceRoot, ".putnami", "bin", "extensions", encodeExtensionName(ref)),
		)
	}
	var lastErr error
	for _, dir := range candidates {
		manifestPath := filepath.Join(dir, "putnami.extension.json")
		if _, err := os.Stat(manifestPath); err != nil {
			if lastErr == nil {
				lastErr = fmt.Errorf("not installed (looked in %s)", strings.Join(candidates, ", "))
			}
			continue
		}
		manifest, err := extproto.LoadManifest(manifestPath)
		if err != nil {
			lastErr = err
			continue
		}
		return manifest, nil
	}
	return nil, lastErr
}

// encodeExtensionName is the install layout's directory name for an
// extension: `@scope/name` becomes `scope-name`.
func encodeExtensionName(name string) string {
	if strings.HasPrefix(name, "@") {
		return strings.Replace(strings.TrimPrefix(name, "@"), "/", "-", 1)
	}
	return name
}

// projectExtensionManifests finds the workspace projects that hold an
// extension manifest: a directory with both putnami.json and
// putnami.extension.json. Hidden directories and the directories core always
// excludes are skipped.
func projectExtensionManifests(workspaceRoot string) ([]*extproto.Manifest, error) {
	var manifests []*extproto.Manifest
	err := filepath.WalkDir(workspaceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == workspaceRoot {
				return walkErr
			}
			return fs.SkipDir
		}
		if !entry.IsDir() {
			return nil
		}
		if path != workspaceRoot && (strings.HasPrefix(entry.Name(), ".") || extproto.IsAlwaysExcludedDir(entry.Name())) {
			return fs.SkipDir
		}
		manifestPath := filepath.Join(path, "putnami.extension.json")
		if _, err := os.Stat(manifestPath); err != nil {
			return nil
		}
		if _, err := os.Stat(filepath.Join(path, "putnami.json")); err != nil {
			return nil
		}
		manifest, err := extproto.LoadManifest(manifestPath)
		if err != nil {
			// A project manifest that does not load is the framework's
			// skipped extension; a pinned build may still serve the name.
			return nil
		}
		manifests = append(manifests, manifest)
		return nil
	})
	if err != nil {
		return nil, clicore.NewError("scan workspace projects: "+err.Error(), clicore.ExitUsage)
	}
	return manifests, nil
}

func emitCIContractReport(params map[string]any, ioctx clicore.IO, report CIContractReport) error {
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(report, params, ioctx, "")
		return nil
	}
	var lines []string
	for _, finding := range report.Diagnostics {
		lines = append(lines, "  "+finding.String())
	}
	if !report.Valid {
		lines = append(lines, fmt.Sprintf("  ✗ %s is invalid", report.Path))
		ioctx.Stdout(strings.Join(lines, "\n"))
		return nil
	}
	switch report.Action {
	case "init":
		lines = append(lines, "  ✓ Created "+report.Path)
	case "fmt":
		switch {
		case report.Applied:
			lines = append(lines, "  ✓ Formatted "+report.Path)
		case report.Changed:
			lines = append(lines, fmt.Sprintf("  ✗ %s needs formatting", report.Path))
		default:
			lines = append(lines, fmt.Sprintf("  ✓ %s is in canonical form", report.Path))
		}
	case "explain":
		lines = append(lines, ciExplanationLines(report.Explanation)...)
	default:
		lines = append(lines, fmt.Sprintf("  ✓ %s is valid", report.Path))
	}
	if report.Digest != "" {
		lines = append(lines, "  Digest: sha256:"+report.Digest)
	}
	ioctx.Stdout(strings.Join(lines, "\n"))
	return nil
}

func ciExplanationLines(explanation *ciproto.Explanation) []string {
	if explanation == nil {
		return nil
	}
	names := make([]string, 0, len(explanation.Commands))
	for _, command := range explanation.Commands {
		names = append(names, command.Name)
	}
	flags := ""
	if len(explanation.Flags) > 0 {
		flags = " " + strings.Join(explanation.Flags, " ")
	}
	lines := []string{
		fmt.Sprintf("  Assumption: %s (local, not a trust proof)", describeCIAssumption(explanation.Assumption)),
		fmt.Sprintf("  Commands: putnami %s --impacted%s", strings.Join(names, ","), flags),
	}
	for _, command := range explanation.Commands {
		if !command.Blocking() {
			lines = append(lines, fmt.Sprintf("  Advisory: %s reports its failures as warnings", command.Name))
		}
	}
	switch {
	case explanation.Matched && explanation.RuleIndex != nil:
		lines = append(lines, fmt.Sprintf("  First match: rules[%d]", *explanation.RuleIndex))
	case explanation.ImplicitTag:
		lines = append(lines, "  No tags rule matched; the tag publishes by convention on its own immutable channel.")
	default:
		lines = append(lines, "  No rule matched; the run executes the commands only.")
	}
	if len(explanation.Publish) == 0 {
		lines = append(lines, "  Publish request: none")
	}
	for _, channel := range explanation.Publish {
		followers := explanation.Environments[channel]
		if len(followers) == 0 {
			lines = append(lines, fmt.Sprintf("  Publish request: channel=%s (no environment follows it; authorization unresolved)", channel))
			continue
		}
		lines = append(lines, fmt.Sprintf("  Publish request: channel=%s followed by %s (authorization/approval unresolved)", channel, strings.Join(followers, ", ")))
	}
	if len(explanation.Publish) > 0 {
		lines = append(lines, "  Channel lifetime: "+describeCIChannelLifetime(explanation.Retain))
		lines = append(lines, "  Impact baseline: "+describeCIPublishBaseline(explanation.Baseline))
	}
	return append(lines, "  Remote decisions unresolved: "+strings.Join(explanation.Unresolved, "; "))
}

// describeCIPublishBaseline states which head the first publish into an empty
// channel measures impact against, and says so when the rule declares none.
func describeCIPublishBaseline(baseline string) string {
	if baseline == "" {
		return "none declared; a first publish into an empty channel republishes every member"
	}
	return baseline + "; read and never advanced, and only when the first published channel has no head yet"
}

// describeCIChannelLifetime states what the rule asks the provider to do with
// the channels it publishes.
func describeCIChannelLifetime(retain string) string {
	switch retain {
	case "":
		return "none declared; the channel is kept until someone removes it"
	case ciproto.RetainWhileOpen:
		return retain + "; the provider is expected to retract the channel when the pull request closes or merges (unresolved)"
	default:
		days := strings.TrimSuffix(retain, "d")
		unit := days + " days"
		if days == "1" {
			unit = "1 day"
		}
		return fmt.Sprintf("%s; the provider is expected to expire the channel %s after its last move (unresolved)", retain, unit)
	}
}

func describeCIAssumption(assumption ciproto.ExplainInput) string {
	parts := []string{"event=" + string(assumption.Event)}
	if assumption.Branch != "" {
		parts = append(parts, "branch="+assumption.Branch)
	}
	if assumption.Tag != "" {
		parts = append(parts, "tag="+assumption.Tag)
	}
	if assumption.PullRequestNumber > 0 {
		parts = append(parts, fmt.Sprintf("pr=%d", assumption.PullRequestNumber))
	}
	return strings.Join(parts, " ")
}

func ciDiagnosticErrorCount(findings []diag.Diagnostic) int {
	count := 0
	for _, finding := range findings {
		if finding.Severity == diag.Error {
			count++
		}
	}
	return count
}
