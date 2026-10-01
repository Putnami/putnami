package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ciproto "go.putnami.dev/protocol/ci"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// seededCommands is the order `ci init` writes the discovered commands in. It
// is the conventional putnami verification order, not the discovery order, so
// two workspaces with the same jobs get the same file.
var seededCommands = []string{"lint", "test", "build", "validate"}

// CICommandReport is the machine-readable result shared by the local CI
// contract commands. Document and Explanation never contain resolved secrets.
type CICommandReport struct {
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

// CIInit creates a safe version 3 document from graph jobs actually discovered
// in the workspace. It declares no distribution and no environment, so it
// requests no publication and no deployment authority.
func CIInit(wsRoot string, cfg *wsproto.Config, force bool, outputFormat string) error {
	path := filepath.Join(wsRoot, ciproto.Filename)
	if _, err := os.Stat(path); err == nil && !force {
		return cmderr.InvalidConfigf("%s already exists; use `putnami ci init --force` to replace it", ciproto.Filename)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cmderr.InvalidConfigf("inspect %s: %v", ciproto.Filename, err)
	}
	graph, err := loadCIGraph(wsRoot, cfg)
	if err != nil {
		return err
	}
	document := ciproto.DefaultDocument()
	document.Commands = document.Commands[:0]
	for _, command := range seededCommands {
		if _, exists := graph.known[command]; exists {
			document.Commands = append(document.Commands, ciproto.CommandEntry{Name: command})
		}
	}
	if len(document.Commands) == 0 {
		return cmderr.InvalidConfigf("no lint, test, build, or validate job is declared by the workspace's extensions; author the commands by hand")
	}
	data, err := json.Marshal(document)
	if err != nil {
		return cmderr.InvalidConfigf("encode %s: %v", ciproto.Filename, err)
	}
	data, err = ciproto.Format(data)
	if err != nil {
		return cmderr.InvalidConfigf("format %s: %v", ciproto.Filename, err)
	}
	if err := shared.AtomicWriteFile(path, data); err != nil {
		return fmt.Errorf("write %s: %w", ciproto.Filename, err)
	}
	document, _ = ciproto.Parse(data)
	digest, _ := ciproto.Digest(document)
	report := CICommandReport{
		Path: path, Action: "init", Valid: true, Changed: true, Applied: true,
		Version: ciproto.Version, Digest: digest, Diagnostics: []diag.Diagnostic{}, Document: &document,
	}
	return emitCIReport(report, outputFormat)
}

// CIValidate validates contract syntax, resolves graph command references, and
// refuses provider sections in a workspace with no release-set provider.
func CIValidate(wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	_, report, err := loadAndValidateCI(wsRoot, cfg, "validate")
	if emitErr := emitCIReport(report, outputFormat); emitErr != nil {
		return emitErr
	}
	return err
}

// CIFormat writes canonical JSON, or checks it without mutation.
func CIFormat(wsRoot string, cfg *wsproto.Config, check bool, outputFormat string) error {
	data, report, validationErr := loadAndValidateCI(wsRoot, cfg, "fmt")
	if validationErr != nil {
		if emitErr := emitCIReport(report, outputFormat); emitErr != nil {
			return emitErr
		}
		return validationErr
	}
	formatted, err := ciproto.Format(data)
	if err != nil {
		return cmderr.InvalidConfigf("format %s: %v", ciproto.Filename, err)
	}
	report.Changed = !bytes.Equal(data, formatted)
	if check && report.Changed {
		runErr := shared.WithResultData(cmderr.InvalidConfigf("%s is not canonically formatted; run `putnami ci fmt`", ciproto.Filename), report)
		if emitErr := emitCIReport(report, outputFormat); emitErr != nil {
			return emitErr
		}
		return runErr
	}
	if !check && report.Changed {
		if err := shared.AtomicWriteFile(report.Path, formatted); err != nil {
			return fmt.Errorf("write %s: %w", ciproto.Filename, err)
		}
		report.Applied = true
	}
	return emitCIReport(report, outputFormat)
}

// CIExplain evaluates ordered rules against explicitly local Source facts.
func CIExplain(wsRoot string, cfg *wsproto.Config, input ciproto.ExplainInput, outputFormat string) error {
	data, report, validationErr := loadAndValidateCI(wsRoot, cfg, "explain")
	if validationErr != nil {
		if emitErr := emitCIReport(report, outputFormat); emitErr != nil {
			return emitErr
		}
		return validationErr
	}
	document, err := ciproto.Parse(data)
	if err != nil {
		return cmderr.InvalidConfigf("parse %s: %v", ciproto.Filename, err)
	}
	explanation, err := ciproto.Explain(document, input)
	if err != nil {
		return cmderr.Usagef("ci explain: %v", err)
	}
	report.Explanation = &explanation
	return emitCIReport(report, outputFormat)
}

func loadAndValidateCI(wsRoot string, cfg *wsproto.Config, action string) ([]byte, CICommandReport, error) {
	path := filepath.Join(wsRoot, ciproto.Filename)
	report := CICommandReport{Path: path, Action: action, Diagnostics: []diag.Diagnostic{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		runErr := shared.WithResultData(cmderr.NotFoundf("%s not found; run `putnami ci init`", ciproto.Filename), report)
		return nil, report, runErr
	}
	if err != nil {
		runErr := shared.WithResultData(cmderr.InvalidConfigf("read %s: %v", ciproto.Filename, err), report)
		return nil, report, runErr
	}
	document, findings := ciproto.ParseWithDiagnostics(data)
	if !diag.HasErrors(findings) {
		graph, graphErr := loadCIGraph(wsRoot, cfg)
		if graphErr != nil {
			return data, report, graphErr
		}
		findings = append(findings, ciproto.ValidateTaskReferences(document, graph.known)...)
		findings = append(findings, validateProviderSections(document, graph)...)
	}
	report.Diagnostics = findings
	report.Valid = !diag.HasErrors(findings)
	if report.Valid {
		report.Version = document.Version
		report.Digest, _ = ciproto.Digest(document)
		return data, report, nil
	}
	runErr := shared.WithResultData(cmderr.InvalidConfigf("%s has %d validation error(s)", ciproto.Filename, diagnosticErrorCount(findings)), report)
	return data, report, runErr
}

// validateProviderSections refuses `distribution` and `envs` in a workspace
// that installs no release-set provider. Without one there is no channel, no
// environment, no visibility, and no grant, so the sections would describe
// authority nothing can serve.
func validateProviderSections(document ciproto.Document, graph ciGraph) []diag.Diagnostic {
	if !ciproto.HasProviderSections(document) || graph.provider != nil {
		return nil
	}
	field := "distribution"
	if document.Distribution == nil {
		field = "envs"
	}
	message := "declares release-set authority, but no installed extension serves the %q command; install a release-set provider or delete the section"
	return []diag.Diagnostic{diag.Errorf("ci.provider_required", field, message, distribution.ProviderCommandName)}
}

// ciGraph is the workspace side of validation: the job vocabulary a command
// name must resolve against, and the release-set provider a distribution or
// envs section needs.
type ciGraph struct {
	known    map[string]string
	provider *extension.ResolvedProvider
}

func loadCIGraph(wsRoot string, cfg *wsproto.Config) (ciGraph, error) {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return ciGraph{}, cmderr.InvalidConfigf("load workspace graph: %v", err)
	}
	projectPaths := make([]string, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		projectPaths = append(projectPaths, project.Path)
	}
	discovered, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil {
		return ciGraph{}, cmderr.InvalidConfigf("discover workspace jobs: %v", err)
	}
	if len(discovered.Skipped) > 0 {
		first := discovered.Skipped[0]
		return ciGraph{}, cmderr.InvalidConfigf("cannot resolve CI graph while extension %q is unreadable: %v", first.Name, first.Reason)
	}
	known := map[string]string{}
	for name := range extension.BuildJobMap(discovered.Extensions) {
		known[name] = name
	}
	provider, err := extension.ResolveReservedProvider(discovered.Extensions, distribution.ProviderCommandName)
	if err != nil {
		return ciGraph{}, cmderr.InvalidConfigf("resolve release-set provider: %v", err)
	}
	return ciGraph{known: known, provider: provider}, nil
}

func emitCIReport(report CICommandReport, outputFormat string) error {
	if outputFormat == "jsonl" {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		iox.Fprintln(os.Stdout, string(data))
		return nil
	}
	for _, finding := range report.Diagnostics {
		iox.Fprintf(os.Stdout, "  %s\n", finding.String())
	}
	if !report.Valid {
		iox.Fprintf(os.Stdout, "  ✗ %s is invalid\n", report.Path)
		return nil
	}
	switch report.Action {
	case "init":
		iox.Fprintf(os.Stdout, "  ✓ Created %s\n", report.Path)
	case "fmt":
		if report.Applied {
			iox.Fprintf(os.Stdout, "  ✓ Formatted %s\n", report.Path)
		} else if report.Changed {
			iox.Fprintf(os.Stdout, "  ✗ %s needs formatting\n", report.Path)
		} else {
			iox.Fprintf(os.Stdout, "  ✓ %s is canonically formatted\n", report.Path)
		}
	case "explain":
		emitHumanCIExplanation(report.Explanation)
	default:
		iox.Fprintf(os.Stdout, "  ✓ %s is valid\n", report.Path)
	}
	if report.Digest != "" {
		iox.Fprintf(os.Stdout, "  Digest: sha256:%s\n", report.Digest)
	}
	return nil
}

func emitHumanCIExplanation(explanation *ciproto.Explanation) {
	if explanation == nil {
		return
	}
	iox.Fprintf(os.Stdout, "  Assumption: %s (local, not a trust proof)\n", describeCIAssumption(explanation.Assumption))
	iox.Fprintf(os.Stdout, "  Commands: putnami %s --impacted%s\n",
		strings.Join(explainedCommandNames(explanation.Commands), ","), explainedFlags(explanation.Flags))
	for _, command := range explanation.Commands {
		if !command.Blocking() {
			iox.Fprintf(os.Stdout, "  Advisory: %s reports its failures as warnings\n", command.Name)
		}
	}
	switch {
	case explanation.Matched:
		iox.Fprintf(os.Stdout, "  First match: rules[%d]\n", *explanation.RuleIndex)
	case explanation.ImplicitTag:
		iox.Fprintln(os.Stdout, "  No tags rule matched; the tag publishes by convention on its own immutable channel.")
	default:
		iox.Fprintln(os.Stdout, "  No rule matched; the run executes the commands only.")
	}
	if len(explanation.Publish) == 0 {
		iox.Fprintln(os.Stdout, "  Publish request: none")
	}
	for _, channel := range explanation.Publish {
		followers := explanation.Environments[channel]
		if len(followers) == 0 {
			iox.Fprintf(os.Stdout, "  Publish request: channel=%s (no environment follows it; authorization unresolved)\n", channel)
			continue
		}
		iox.Fprintf(os.Stdout, "  Publish request: channel=%s followed by %s (authorization/approval unresolved)\n",
			channel, strings.Join(followers, ", "))
	}
	if len(explanation.Publish) > 0 {
		iox.Fprintf(os.Stdout, "  Channel lifetime: %s\n", describeChannelLifetime(explanation.Retain))
		iox.Fprintf(os.Stdout, "  Impact baseline: %s\n", describePublishBaseline(explanation.Baseline))
	}
	iox.Fprintf(os.Stdout, "  Remote decisions unresolved: %s\n", strings.Join(explanation.Unresolved, "; "))
}

// describePublishBaseline states which head the publish measures impact
// against on its FIRST run into a channel that has none yet, and says so
// explicitly when the rule declares no baseline: republishing every member on
// the first run of every pull request is the cost the member exists to remove,
// so it is printed rather than left to silence. The named channel is read and
// never advanced, which is why it is reported apart from the publish request.
func describePublishBaseline(baseline string) string {
	if baseline == "" {
		return "none declared; a first publish into an empty channel republishes every member"
	}
	return baseline + "; read and never advanced, and only when the first published channel has no head yet"
}

// describeChannelLifetime states what the rule's declared lifetime asks a
// provider to do with the channels it publishes, and says so explicitly when
// the rule declares none: a channel nothing ever retracts is the gap the
// member closes, so it is printed rather than left to silence. Nothing here is
// resolved — expiry belongs to the provider, like every other remote decision
// this report names.
func describeChannelLifetime(retain string) string {
	switch retain {
	case "":
		return "none declared; the channel is kept until someone removes it"
	case ciproto.RetainWhileOpen:
		return retain + "; the provider is expected to retract the channel when the pull request closes or merges (unresolved)"
	default:
		return fmt.Sprintf("%s; the provider is expected to expire the channel %s after its last move (unresolved)",
			retain, humanizeRetainDays(retain))
	}
}

// humanizeRetainDays spells the declared duration in words. The authored value
// is the contract; this is the sentence around it.
func humanizeRetainDays(retain string) string {
	days := strings.TrimSuffix(retain, "d")
	if days == "1" {
		return "1 day"
	}
	return days + " days"
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

func explainedCommandNames(commands []ciproto.CommandEntry) []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.Name)
	}
	return names
}

// explainedFlags renders the appended flags in the document's own order,
// because that is the invocation the runner builds.
func explainedFlags(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	return " " + strings.Join(flags, " ")
}

func diagnosticErrorCount(findings []diag.Diagnostic) int {
	count := 0
	for _, finding := range findings {
		if finding.Severity == diag.Error {
			count++
		}
	}
	return count
}
