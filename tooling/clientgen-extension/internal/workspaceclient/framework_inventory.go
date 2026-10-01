package workspaceclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const frameworkInventoryFile = "clientgen.framework.json"

// frameworkTransportV2 is one entry of a project's clientgen.framework.json.
type frameworkTransportV2 struct {
	Runtime     string             `json:"runtime"`
	Status      string             `json:"status"`
	Adapter     string             `json:"adapter"`
	Callsites   []ExternalCallsite `json:"callsites"`
	Owner       string             `json:"owner"`
	Tests       []string           `json:"tests"`
	Reason      string             `json:"reason"`
	Operations  []string           `json:"operations,omitempty"`
	PendingWork string             `json:"pendingWork,omitempty"`
}

type frameworkInventoryV2 struct {
	ProtocolVersion int                    `json:"protocolVersion"`
	Transports      []frameworkTransportV2 `json:"transports"`
}

func loadAndValidateFrameworkInventory(workspaceRoot string) ([]FrameworkTransport, []Finding) {
	indexed, indexErr := indexedProjectPaths(workspaceRoot)
	if indexErr != nil {
		return nil, []Finding{{Code: "clientgen.invalid-framework-inventory", Path: frameworkInventoryFile, Message: indexErr.Error()}}
	}
	files, findings := readProjectInventories(workspaceRoot, indexed, frameworkInventoryFile,
		"clientgen.framework-inventory-read", "clientgen.invalid-framework-inventory")
	seen, claimed := map[string]bool{}, map[string]bool{}
	validated := []FrameworkTransport{}
	for _, file := range files {
		var inventory frameworkInventoryV2
		if err := decodeStrictInventory(file.data, &inventory); err != nil {
			findings = append(findings, Finding{Code: file.code, Path: file.path, Message: err.Error()})
			continue
		}
		if inventory.ProtocolVersion != inventoryProtocolVersion {
			findings = append(findings, file.finding("protocolVersion", fmt.Sprintf("must equal %d", inventoryProtocolVersion)))
		}
		if inventory.Transports == nil {
			findings = append(findings, file.finding("transports", "is required and must be an array"))
		}
		last := ""
		for index, entry := range inventory.Transports {
			transport := FrameworkTransport{Project: file.project, Runtime: entry.Runtime, Status: entry.Status,
				Adapter: entry.Adapter, Callsites: entry.Callsites, Owner: entry.Owner, Tests: entry.Tests,
				Reason: entry.Reason, Operations: entry.Operations, PendingWork: entry.PendingWork}
			before := len(findings)
			field := fmt.Sprintf("transports[%d]", index)
			findings = append(findings, validateFrameworkStatus(workspaceRoot, file, field, transport)...)
			findings = append(findings, validateInventoryFile(workspaceRoot, file, field+".adapter", transport.Adapter)...)
			if !pathOwnedByProject(transport.Adapter, transport.Project) {
				findings = append(findings, file.finding(field+".adapter", "must sit in the directory of the project that holds this file"))
			}
			entryKey := transport.Project + "\x00" + transport.Adapter
			if seen[entryKey] || index > 0 && entryKey <= last {
				findings = append(findings, file.finding(field+".adapter",
					"transports must be unique and sorted by adapter"))
			}
			seen[entryKey], last = true, entryKey
			if blank(transport.Owner) || blank(transport.Reason) {
				findings = append(findings, file.finding(field, "owner and reason are required"))
			}
			if len(transport.Callsites) == 0 {
				findings = append(findings, file.finding(field+".callsites", "at least one callsite is required"))
			}
			lastCallsite := ""
			for callsiteIndex, callsite := range transport.Callsites {
				callsiteField := fmt.Sprintf("%s.callsites[%d]", field, callsiteIndex)
				findings = append(findings, validateAuthorityCallsite(workspaceRoot, file, transport.Adapter, callsiteField, callsite)...)
				key := externalCallsiteKey(callsite)
				if callsiteIndex > 0 && key <= lastCallsite {
					findings = append(findings, file.finding(callsiteField, "callsites must be unique and sorted"))
				}
				if claimed[key] {
					findings = append(findings, file.finding(callsiteField, "callsite is already claimed"))
				}
				claimed[key], lastCallsite = true, key
			}
			if len(transport.Tests) == 0 {
				findings = append(findings, file.finding(field+".tests", "at least one contract test is required"))
			}
			lastTest := ""
			for testIndex, testPath := range transport.Tests {
				testField := fmt.Sprintf("%s.tests[%d]", field, testIndex)
				findings = append(findings, validateInventoryFile(workspaceRoot, file, testField, testPath)...)
				if testIndex > 0 && testPath <= lastTest {
					findings = append(findings, file.finding(testField, "test paths must be unique and sorted"))
				}
				lastTest = testPath
			}
			if len(findings) == before {
				validated = append(validated, transport)
			}
		}
	}
	sort.Slice(validated, func(i, j int) bool {
		return validated[i].Project+"\x00"+validated[i].Adapter < validated[j].Project+"\x00"+validated[j].Adapter
	})
	return validated, findings
}

// validateFrameworkStatus binds each status to the identity it is allowed to
// claim. Every status requires the runtime to be the declared project's exact
// indexed identity, so an entry names one package and never a directory.
func validateFrameworkStatus(workspaceRoot string, file inventoryFile, field string, transport FrameworkTransport) []Finding {
	var findings []Finding
	switch transport.Status {
	case StatusFrameworkRuntime:
		if !generatedBindingRuntimeIdentity(workspaceRoot, transport.Project, transport.Runtime) {
			findings = append(findings, file.finding(field+".runtime",
				"must equal the indexed generated-binding runtime identity (@putnami/client or go.putnami.dev/client)"))
		}
	case StatusTransportPrimitive, StatusPendingProviderContract:
		if !indexedProjectIdentity(workspaceRoot, transport.Project, transport.Runtime) {
			findings = append(findings, file.finding(field+".runtime",
				"must equal the declared project's exact indexed package identity"))
		}
	default:
		findings = append(findings, file.finding(field+".status",
			"must be framework-runtime, transport-primitive or pending-provider-contract"))
	}
	pending := transport.Status == StatusPendingProviderContract
	if pending == blank(transport.PendingWork) {
		findings = append(findings, file.finding(field+".pendingWork",
			"is required for pending-provider-contract and forbidden for every other status"))
	}
	if pending == (len(transport.Operations) == 0) {
		findings = append(findings, file.finding(field+".operations",
			"is required for pending-provider-contract and forbidden for every other status"))
	}
	lastOperation := ""
	for index, operation := range transport.Operations {
		operationField := fmt.Sprintf("%s.operations[%d]", field, index)
		if blank(operation) || len(operation) > 256 {
			findings = append(findings, file.finding(operationField, "must contain 1 to 256 characters"))
		}
		if index > 0 && operation <= lastOperation {
			findings = append(findings, file.finding(operationField, "operations must be unique and sorted"))
		}
		lastOperation = operation
	}
	return findings
}

// sortExternalCallsites puts callsites in the canonical inventory order.
func sortExternalCallsites(callsites []ExternalCallsite) {
	sort.Slice(callsites, func(i, j int) bool {
		return externalCallsiteKey(callsites[i]) < externalCallsiteKey(callsites[j])
	})
}

// generatedBindingRuntimeIdentity reports whether the project is one of the two
// runtimes a generated binding executes on, named by its own manifest.
func generatedBindingRuntimeIdentity(workspaceRoot, project, runtimeName string) bool {
	return (runtimeName == "@putnami/client" || runtimeName == "go.putnami.dev/client") &&
		indexedProjectIdentity(workspaceRoot, project, runtimeName)
}

// indexedProjectIdentity reports whether runtimeName is the exact package name
// the project's own manifest declares.
func indexedProjectIdentity(workspaceRoot, project, runtimeName string) bool {
	if blank(runtimeName) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(project), "putnami.json")) //nolint:gosec // indexed project metadata
	if err != nil {
		return false
	}
	var metadata struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(data, &metadata) == nil && metadata.Name == runtimeName
}

func pathOwnedByProject(path, project string) bool {
	return project == "." || path == project || strings.HasPrefix(path, strings.TrimSuffix(project, "/")+"/")
}
