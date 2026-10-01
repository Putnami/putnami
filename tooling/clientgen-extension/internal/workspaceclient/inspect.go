package workspaceclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	diag "go.putnami.dev/protocol/diagnostic"
)

type artifactReader interface {
	Read(path string) ([]byte, error)
	List(prefix string) ([]string, error)
}

type worktreeReader struct{ root string }

func (r worktreeReader) Read(path string) ([]byte, error) {
	clean, err := safeWorkspacePath(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(r.root, filepath.FromSlash(clean))) //nolint:gosec // validated workspace-relative path
}

func (r worktreeReader) List(prefix string) ([]string, error) {
	clean, err := safeWorkspacePath(prefix)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(r.root, filepath.FromSlash(clean))
	var paths []string
	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(r.root, path)
		if relErr != nil {
			return relErr
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

// Inspect discovers provider contracts from the built tree and verifies the
// generated target closure of the worktree against itself: manifests parse,
// hashes match bytes, operations cover the contract. It renders nothing.
func Inspect(workspaceRoot string, mode Mode) Report {
	return inspect(workspaceRoot, discover, "", mode)
}

// InspectCommitted is the guard's inspection (`clientgen-check`, and the
// `validate` contribution): every input is a COMMITTED file — the project
// index, each provider's committed contract sidecar, its committed
// client.putnami.json manifests and the two inventories — so a cold tree and a
// tree the session has just built reach the same verdict, and no provider is
// built or rendered to reach it.
//
// What it judges is what no generator task can: a manifest whose recorded
// hashes no longer match the bytes beside it (a hand edit), a manifest cut from
// a contract other than the committed one (a provider change nobody
// regenerated after), an operation the contract has and the client lacks, a
// generated-marked file the manifest does not list, and the handwritten
// transports the consumer scan finds. Whether the committed bytes are what the
// current inputs GENERATE is the generator task's verdict — the engine compares
// each generator's declared output with the bytes present before it wrote
// (protocols/extension ADR 0004), which is why this check no longer needs a
// fresh render or a pre-session capture to compare against (ADR 0003).
func InspectCommitted(workspaceRoot string) Report {
	return inspect(workspaceRoot, discoverCommitted, "", ModeCheck)
}

// InspectRendered compares a fresh ephemeral generator render with the worktree
// bytes. expectedRoot must mirror provider paths below workspaceRoot. It is the
// sync and adopt commands' verification: they regenerated in place, so the
// worktree and the render are expected to agree byte for byte.
func InspectRendered(workspaceRoot, expectedRoot string, mode Mode) Report {
	return inspect(workspaceRoot, discover, expectedRoot, mode)
}

func inspect(workspaceRoot string, discovery func(string) ([]provider, []Finding), expectedRoot string, mode Mode) (report Report) {
	inspectStarted := time.Now()
	providers, findings := discovery(workspaceRoot)
	var committed artifactReader = worktreeReader{root: workspaceRoot}
	expected := committed
	if expectedRoot != "" {
		expected = worktreeReader{root: expectedRoot}
	}
	report = inspectWithReader(workspaceRoot, mode, providers, findings, committed, expected)
	report.ExternalContracts, findings = loadAndValidateExternalInventory(workspaceRoot, providers)
	report.Findings = append(report.Findings, findings...)
	report.FrameworkTransports, findings = loadAndValidateFrameworkInventory(workspaceRoot)
	report.Findings = append(report.Findings, findings...)
	// ONE workspace source scan answers both questions below. The scan reads
	// and parses every indexed project's production sources, which is the
	// single most expensive thing this check does; running it twice doubled
	// that for nothing, because both callers asked for the same set. The set
	// is a function of the worktree and of the generated-file inventory, and
	// the inventory is derived from report.Providers alone — which
	// inspectWithReader has already settled and nothing below changes. Passing
	// the records in rather than letting each caller re-derive them keeps that
	// equality checkable at the call site instead of inside two scanners.
	report.recordTiming(PhaseInspect, time.Since(inspectStarted))
	// The scan is timed on every exit below, including the failed-scan return,
	// because a scan that failed still cost what it cost.
	scanStarted := time.Now()
	defer func() { report.recordTiming(PhaseScan, time.Since(scanStarted)) }()
	records, scanErr := workspaceSourceRecords(workspaceRoot, report)
	if scanErr != nil {
		// A scan that failed read NO source, which is not the same fact as a
		// workspace that contains no transport. Every verdict below is about
		// what the scan saw: an inventory callsite is stale because nothing
		// matched it, a censused entry is stale because the workspace no longer
		// holds the callsite it exempts. Classifying against an empty record set
		// turns one unreadable tree into one finding per inventory entry — 114
		// of them on this repository — and buries the single fact that explains
		// them all. So both scanners are skipped, exactly as each skipped itself
		// while it owned its own failing scan, and the only verdict that
		// survives is the one needing no observation: whether the census
		// document itself is valid.
		report.Findings = append(report.Findings, Finding{Code: "clientgen.source-scan",
			Path: workspaceRoot, Message: scanErr.Error()})
		report.Findings = append(report.Findings, loadPendingCensus(workspaceRoot).invalid...)
		canonicalizeReport(&report)
		return report
	}
	report.ConsumerEdges = scanConsumerEdges(records, providers, report)
	manual, manualFindings := scanManualClients(workspaceRoot, providers, records, report)
	report.AdaptationQueue = manual
	// The census splits handwritten-transport verdicts into the ones this
	// workspace already carried when the guard landed and the ones this change
	// introduced. Only the second kind fails; the first is reported so the
	// remaining debt stays visible and countable (see pending.go).
	blocking, pending := classifyPendingTransports(manualFindings, callsiteIndex(manual), loadPendingCensus(workspaceRoot))
	report.Findings = append(report.Findings, blocking...)
	report.PendingTransports = pending
	canonicalizeReport(&report)
	return report
}

// inspectWithReader judges every first-party provider's configured targets:
// committed is the tree under judgment (the worktree), expected is the render
// it must equal — the same reader when nothing was rendered.
func inspectWithReader(
	workspaceRoot string,
	mode Mode,
	providers []provider,
	findings []Finding,
	committed artifactReader,
	expected artifactReader,
) Report {
	report := Report{ProtocolVersion: 1, Mode: mode, Findings: append([]Finding(nil), findings...)}
	for _, item := range providers {
		providerReport := ProviderReport{
			Project: item.rel, Classification: item.classification, ContractSHA256: item.contractHash,
			EmptyContract: item.emptyFirstPartyContract(),
		}
		if item.document != nil {
			providerReport.ServiceID = item.document.Service.ID
			providerReport.Audience = item.document.Service.Audience
		}
		if item.classification != ClassificationFirstParty || item.document == nil || item.config == nil {
			report.Providers = append(report.Providers, providerReport)
			continue
		}
		targets, targetFindings := configuredTargets(item)
		report.Findings = append(report.Findings, targetFindings...)
		// A contract that declares no first-party operation has nothing for a
		// target to carry, so requiring one of it says nothing. Every
		// target it DOES configure is still judged below: a client generated
		// when the contract still had operations is drift, not an absence.
		if len(targets) == 0 && !item.emptyFirstPartyContract() {
			report.Findings = append(report.Findings, Finding{Code: "clientgen.no-targets", Path: item.rel,
				ServiceID: item.document.Service.ID, Message: "first-party provider must configure at least one generated target"})
		}
		for _, target := range targets {
			report.Coverage.Required += len(item.operations)
			targetReport, manifest, targetFindings := inspectTarget(item, target, committed, expected)
			report.Findings = append(report.Findings, targetFindings...)
			if targetReport == nil {
				// The declared target generated nothing because the contract
				// declares no first-party operation. Reporting it anyway would
				// put a target with no binding in the report, and bindingAdvice
				// would tell a consumer to regenerate a client that cannot
				// exist (ADR 0004 decision 5).
				continue
			}
			report.Coverage.Covered += targetReport.Covered
			providerReport.Targets = append(providerReport.Targets, *targetReport)
			if manifest != nil {
				outputPrefix := joinRel(item.rel, target.output)
				report.Findings = append(report.Findings, detectExtraGeneratedFiles(outputPrefix, manifest, committed)...)
			}
		}
		report.Providers = append(report.Providers, providerReport)
	}
	if report.Coverage.Required == 0 {
		report.Coverage.Percent = 100
	} else {
		report.Coverage.Percent = report.Coverage.Covered * 100 / report.Coverage.Required
	}
	_ = workspaceRoot
	_ = mode
	return report
}

type configuredTarget struct {
	language clientcontract.GeneratedLanguage
	output   string
}

func configuredTargets(item provider) ([]configuredTarget, []Finding) {
	seen := map[string]bool{}
	var targets []configuredTarget
	var findings []Finding
	for _, raw := range item.config.Targets {
		if seen[raw] {
			findings = append(findings, Finding{Code: "clientgen.duplicate-target", Path: item.rel,
				ServiceID: item.document.Service.ID, Message: fmt.Sprintf("target %q is configured more than once", raw)})
			continue
		}
		seen[raw] = true
		switch raw {
		case "go":
			targets = append(targets, configuredTarget{language: clientcontract.GeneratedLanguageGo,
				output: defaultOutput(item.config.Go.Output, "clients/go")})
		case "ts":
			targets = append(targets, configuredTarget{language: clientcontract.GeneratedLanguageTypeScript,
				output: defaultOutput(item.config.TS.Output, "clients/ts")})
		default:
			findings = append(findings, Finding{Code: "clientgen.unsupported-target", Path: item.rel,
				ServiceID: item.document.Service.ID, Message: fmt.Sprintf("target %q is unsupported", raw)})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].language < targets[j].language })
	return targets, findings
}

// inspectTarget judges one configured target. The manifest under judgment is
// the expected one (a fresh render when there is one, the committed manifest
// otherwise); every committed byte is then compared with it, and every
// recorded hash with the bytes it describes.
//
// It returns a nil report for the one case where the declared target does not
// exist and is not supposed to: a contract that declares no first-party
// operation generated nothing for it. Every other absence is a finding, and a
// target whose manifest IS committed is judged in full whatever the contract
// declares (ADR 0004 decisions 4 and 5).
func inspectTarget(
	item provider,
	target configuredTarget,
	committed artifactReader,
	expectedReader artifactReader,
) (*TargetReport, *clientcontract.GeneratedClientManifestV1, []Finding) {
	manifestRel := joinRel(item.rel, target.output, clientcontract.GeneratedManifestFile)
	report := TargetReport{Language: target.language, Output: target.output, Manifest: manifestRel, Operations: len(item.operations)}
	expectedData, err := expectedReader.Read(manifestRel)
	if err != nil {
		if item.emptyFirstPartyContract() {
			// The contract declares no first-party operation, so the provider's
			// generator staged nothing for this target and there is no manifest
			// to find. That is the declared empty contract, not a generation
			// that broke: a contract with even one operation, or one discovery
			// could not read, still fails below.
			return nil, nil, nil
		}
		return &report, nil, []Finding{{Code: "clientgen.missing-manifest", Path: manifestRel,
			ServiceID: item.document.Service.ID, Message: "generated target has no client.putnami.json"}}
	}
	manifest, diags := clientcontract.ParseAndValidateGeneratedManifest(expectedData)
	findings := manifestFindings(manifestRel, item.document.Service.ID, diags)
	if manifest == nil || diag.HasErrors(diags) {
		return &report, manifest, findings
	}
	committedData, committedErr := committed.Read(manifestRel)
	if committedErr != nil {
		findings = append(findings, Finding{Code: "clientgen.missing-manifest", Path: manifestRel,
			ServiceID: item.document.Service.ID, Message: "generated target has no committed client.putnami.json"})
	} else if !bytes.Equal(committedData, expectedData) {
		findings = append(findings, Finding{Code: "clientgen.manifest-drift", Path: manifestRel,
			ServiceID: item.document.Service.ID, Message: "committed generated manifest differs from a fresh render"})
	}
	if manifest.Language != target.language {
		findings = append(findings, Finding{Code: "clientgen.target-language-drift", Path: manifestRel,
			ServiceID: item.document.Service.ID, Message: fmt.Sprintf("manifest language is %q, configured target is %q", manifest.Language, target.language)})
	}
	report.Binding = &manifest.Binding
	report.GeneratedOperations = append([]clientcontract.GeneratedOperation(nil), manifest.Operations...)
	if manifest.Service != item.document.Service {
		findings = append(findings, Finding{Code: "clientgen.service-drift", Path: manifestRel,
			ServiceID: item.document.Service.ID, Message: "generated service identity or audience differs from provider contract"})
	}
	if manifest.ContractSHA256 != item.contractHash {
		findings = append(findings, Finding{Code: "clientgen.contract-drift", Path: manifestRel,
			ServiceID: item.document.Service.ID, Message: "generated contract hash differs from the provider OpenAPI artifact; regenerate the client"})
	}
	// An operation the target's configuration leaves out is named by the
	// committed manifest itself, so a cold clone and a built tree reach the same
	// verdict. It is excluded from what the target must generate and never
	// counted as covered; naming one the provider does not declare is drift.
	expected, unknown := targetOperations(item.operations, manifest.OmittedOperations)
	for _, operationID := range unknown {
		findings = append(findings, Finding{Code: "clientgen.omitted-operation-drift", Path: manifestRel,
			ServiceID: item.document.Service.ID, Message: fmt.Sprintf("generated manifest leaves out operation %s, which the provider contract does not declare", operationID)})
	}
	report.Omitted = append([]string(nil), manifest.OmittedOperations...)
	if !operationSemanticsEqual(manifest.Operations, expected) {
		findings = append(findings, Finding{Code: "clientgen.operation-coverage-drift", Path: manifestRel,
			ServiceID: item.document.Service.ID, Message: "generated operation/stream/transport/cache inventory differs from the provider contract"})
	} else {
		report.Covered = len(expected)
	}
	for _, file := range manifest.Files {
		fileRel := joinRel(item.rel, target.output, file.Path)
		expectedContent, expectedErr := expectedReader.Read(fileRel)
		if expectedErr != nil {
			findings = append(findings, Finding{Code: "clientgen.invalid-generated-inventory", Path: fileRel,
				ServiceID: item.document.Service.ID, Message: "the manifest lists a file that is not present beside it"})
			continue
		}
		expectedSum := sha256.Sum256(expectedContent)
		if hex.EncodeToString(expectedSum[:]) != file.SHA256 {
			// With a fresh render this is an emitter that lied about its own
			// bytes; on the committed tree it is a hand edit of a generated
			// file whose manifest nobody regenerated.
			findings = append(findings, Finding{Code: "clientgen.forged-generated-hash", Path: fileRel,
				ServiceID: item.document.Service.ID, Message: "file bytes do not match the hash recorded in client.putnami.json"})
		}
		content, readErr := committed.Read(fileRel)
		if readErr != nil {
			findings = append(findings, Finding{Code: "clientgen.missing-generated-file", Path: fileRel,
				ServiceID: item.document.Service.ID, Message: "file listed by generated client manifest is absent"})
		} else if !bytes.Equal(content, expectedContent) {
			findings = append(findings, Finding{Code: "clientgen.generated-file-drift", Path: fileRel,
				ServiceID: item.document.Service.ID, Message: "committed file bytes differ from a fresh generator render"})
		}
	}
	return &report, manifest, findings
}

// targetOperations returns the provider operations a target must generate —
// every one its manifest does not name as omitted — and the omitted IDs the
// provider does not declare.
func targetOperations(provider []clientcontract.GeneratedOperation, omitted []string) ([]clientcontract.GeneratedOperation, []string) {
	if len(omitted) == 0 {
		return provider, nil
	}
	left := make(map[string]bool, len(omitted))
	for _, operationID := range omitted {
		left[operationID] = true
	}
	expected := make([]clientcontract.GeneratedOperation, 0, len(provider))
	declared := make(map[string]bool, len(provider))
	for _, operation := range provider {
		declared[operation.OperationID] = true
		if !left[operation.OperationID] {
			expected = append(expected, operation)
		}
	}
	var unknown []string
	for _, operationID := range omitted {
		if !declared[operationID] {
			unknown = append(unknown, operationID)
		}
	}
	return expected, unknown
}

func operationSemanticsEqual(generated, provider []clientcontract.GeneratedOperation) bool {
	if len(generated) != len(provider) {
		return false
	}
	for i := range provider {
		if generated[i].OperationID != provider[i].OperationID || generated[i].Stream != provider[i].Stream ||
			!reflect.DeepEqual(generated[i].Transports, provider[i].Transports) ||
			!reflect.DeepEqual(generated[i].Cache, provider[i].Cache) {
			return false
		}
	}
	return true
}

// manifestFindings maps manifest diagnostics to findings. A manifest that
// declares a cache policy its runtime requirement does not cover, or requires
// a capability the runtimes of this protocol version do not implement, gets
// its own code: regenerating does not fix it, upgrading the runtime does.
func manifestFindings(manifestRel, serviceID string, diags []diag.Diagnostic) []Finding {
	var findings []Finding
	for _, diagnostic := range diags {
		if diagnostic.Severity != diag.Error {
			continue
		}
		code := "clientgen.invalid-manifest"
		if diagnostic.Code == clientcontract.ErrorCodeUnsupportedRuntimeCapability {
			code = "clientgen.unsupported-runtime-capability"
		}
		findings = append(findings, Finding{Code: code, Path: manifestRel, ServiceID: serviceID, Message: diagnostic.String()})
	}
	return findings
}

func detectExtraGeneratedFiles(outputPrefix string, manifest *clientcontract.GeneratedClientManifestV1, reader artifactReader) []Finding {
	owned := map[string]bool{joinRel(outputPrefix, clientcontract.GeneratedManifestFile): true}
	for _, file := range manifest.Files {
		owned[joinRel(outputPrefix, file.Path)] = true
	}
	paths, err := reader.List(outputPrefix)
	if err != nil {
		return []Finding{{Code: "clientgen.inventory-read", Path: outputPrefix, Message: err.Error()}}
	}
	var findings []Finding
	for _, file := range paths {
		if owned[file] {
			continue
		}
		content, readErr := reader.Read(file)
		if readErr != nil {
			continue
		}
		if carriesGeneratedMarker(content) {
			findings = append(findings, Finding{Code: "clientgen.uninventoried-generated-file", Path: file,
				ServiceID: manifest.Service.ID, Message: "file claims first-party generation but is absent from client.putnami.json"})
		}
	}
	return findings
}

func carriesGeneratedMarker(content []byte) bool {
	return bytes.Contains(content, []byte("Code generated by go.putnami.dev/api. DO NOT EDIT.")) ||
		bytes.Contains(content, []byte("Auto-generated by @putnami/client")) ||
		bytes.Contains(content, []byte("Generated by @putnami/clientgen"))
}

func defaultOutput(value, fallback string) string {
	if blank(value) {
		return fallback
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
}

func safeWorkspacePath(value string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || filepath.IsAbs(filepath.FromSlash(value)) || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q is outside the workspace", value)
	}
	return clean, nil
}

func joinRel(parts ...string) string {
	joined := filepath.Join(parts...)
	return filepath.ToSlash(joined)
}

func canonicalizeReport(report *Report) {
	sort.Slice(report.Providers, func(i, j int) bool { return report.Providers[i].Project < report.Providers[j].Project })
	for i := range report.Providers {
		sort.Slice(report.Providers[i].Targets, func(a, b int) bool {
			return report.Providers[i].Targets[a].Language < report.Providers[i].Targets[b].Language
		})
	}
	sort.Slice(report.AppliedAdaptations, func(i, j int) bool {
		return adaptationSortKey(report.AppliedAdaptations[i]) < adaptationSortKey(report.AppliedAdaptations[j])
	})
	sort.Slice(report.AdaptationQueue, func(i, j int) bool {
		return adaptationSortKey(report.AdaptationQueue[i]) < adaptationSortKey(report.AdaptationQueue[j])
	})
	sort.Slice(report.ConsumerEdges, func(i, j int) bool {
		a, b := report.ConsumerEdges[i], report.ConsumerEdges[j]
		return a.ProviderProject+"\x00"+a.OperationID+"\x00"+string(a.Language)+"\x00"+a.GeneratedArtifact+"\x00"+a.ConsumerProject+"\x00"+a.BindingSymbol <
			b.ProviderProject+"\x00"+b.OperationID+"\x00"+string(b.Language)+"\x00"+b.GeneratedArtifact+"\x00"+b.ConsumerProject+"\x00"+b.BindingSymbol
	})
	sort.Slice(report.ExternalContracts, func(i, j int) bool {
		return report.ExternalContracts[i].Project+"\x00"+report.ExternalContracts[i].Adapter <
			report.ExternalContracts[j].Project+"\x00"+report.ExternalContracts[j].Adapter
	})
	sort.Slice(report.FrameworkTransports, func(i, j int) bool {
		return report.FrameworkTransports[i].Project+"\x00"+report.FrameworkTransports[i].Adapter <
			report.FrameworkTransports[j].Project+"\x00"+report.FrameworkTransports[j].Adapter
	})
	sort.Slice(report.Findings, func(i, j int) bool {
		a, b := report.Findings[i], report.Findings[j]
		return findingSortKey(a) < findingSortKey(b)
	})
	sort.Slice(report.PendingTransports, func(i, j int) bool {
		return findingSortKey(report.PendingTransports[i]) < findingSortKey(report.PendingTransports[j])
	})
}

func adaptationSortKey(value Adaptation) string {
	if value.Callsite == nil {
		return fmt.Sprintf("%s\x00%s", value.Path, value.ServiceID)
	}
	return fmt.Sprintf("%s\x00%09d\x00%09d\x00%s\x00%s", value.Path, value.Callsite.Line,
		value.Callsite.Column, value.Callsite.Transport, value.ServiceID)
}

func findingSortKey(value Finding) string {
	return fmt.Sprintf("%s\x00%s\x00%09d\x00%09d\x00%s\x00%s", value.Code, value.Path,
		value.Line, value.Column, value.ServiceID, value.Message)
}

// MarshalReport renders a deterministic machine-readable report.
func MarshalReport(report Report) ([]byte, error) {
	canonicalizeReport(&report)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
