package workspaceclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

const externalInventoryFile = "clientgen.external.json"

// inventoryProtocolVersion is the wire version of a project's inventory file.
// Its entries name no project: the directory that holds the file does.
const inventoryProtocolVersion = 2

// rootLayoutProtocolVersion is the older workspace-root layout, one file
// whose entries each name their project. clientgen reads none of its entries;
// it only lists the project file each entry moves to.
const rootLayoutProtocolVersion = 1

// externalContractV2 is one entry of a project's clientgen.external.json.
type externalContractV2 struct {
	Authority string             `json:"authority"`
	Adapter   string             `json:"adapter"`
	Callsites []ExternalCallsite `json:"callsites"`
	Owner     string             `json:"owner"`
	Tests     []string           `json:"tests"`
	Reason    string             `json:"reason"`
}

type externalInventoryV2 struct {
	ProtocolVersion int                  `json:"protocolVersion"`
	Contracts       []externalContractV2 `json:"contracts"`
}

// inventoryFile is one project's inventory file: the finding code its
// validation reports and the workspace-relative path findings point at.
type inventoryFile struct {
	code    string
	project string
	path    string
	data    []byte
}

func (file inventoryFile) finding(field, message string) Finding {
	return Finding{Code: file.code, Path: file.path, Message: field + ": " + message}
}

// projectInventoryPath is the workspace-relative path of a project's
// inventory file: the file sits in the project directory.
func projectInventoryPath(project, name string) string {
	if project == "." {
		return name
	}
	return project + "/" + name
}

// readProjectInventories reads the inventory file named name in every member
// project directory, in project order. A project without the file declares
// nothing. A protocolVersion 1 document is the older workspace-root layout:
// at the root it is a finding that lists the project file each entry moves
// to, and in a project directory it fails. A root file of any other version
// is read only when a member project lives at the root.
func readProjectInventories(workspace workspaceFiles, projects []string, name, readCode, invalidCode string) ([]inventoryFile, []Finding) {
	var files []inventoryFile
	var findings []Finding
	rootIsProject := false
	for _, project := range projects {
		rootIsProject = rootIsProject || project == "."
		rel := projectInventoryPath(project, name)
		data, err := workspace.Read(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			findings = append(findings, Finding{Code: readCode, Path: rel, Message: err.Error()})
			continue
		}
		switch {
		case !isRootLayout(data):
			files = append(files, inventoryFile{code: invalidCode, project: project, path: rel, data: data})
		case project == ".":
			findings = append(findings, Finding{Code: invalidCode, Path: rel, Message: rootLayoutMessage(name, data)})
		default:
			findings = append(findings, Finding{Code: invalidCode, Path: rel, Message: fmt.Sprintf(
				"protocolVersion %d is the workspace-root layout; a project's %s is protocolVersion %d: "+
					"set protocolVersion to %d and remove project from every entry, the directory names it",
				rootLayoutProtocolVersion, name, inventoryProtocolVersion, inventoryProtocolVersion)})
		}
	}
	if rootIsProject {
		return files, findings
	}
	data, err := workspace.Read(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		findings = append(findings, Finding{Code: readCode, Path: name, Message: err.Error()})
	case isRootLayout(data):
		findings = append(findings, Finding{Code: invalidCode, Path: name, Message: rootLayoutMessage(name, data)})
	default:
		findings = append(findings, Finding{Code: invalidCode, Path: name, Message: fmt.Sprintf(
			"the workspace root is not a Putnami project, so clientgen does not read %s there: "+
				"move each entry into <project>/%s, the file of the project that owns its adapter, then delete the root file",
			name, name)})
	}
	return files, findings
}

// isRootLayout reports whether data declares the older workspace-root
// protocolVersion. Anything else, unreadable JSON included, goes to the
// strict decoder, which names the problem.
func isRootLayout(data []byte) bool {
	var probe struct {
		ProtocolVersion json.Number `json:"protocolVersion"`
	}
	return json.Unmarshal(data, &probe) == nil && probe.ProtocolVersion.String() == strconv.Itoa(rootLayoutProtocolVersion)
}

// rootLayoutMessage tells the owner of an older workspace-root inventory
// where each entry goes: into the file of the project the entry names.
func rootLayoutMessage(name string, data []byte) string {
	var document struct {
		Contracts  []struct{ Project string } `json:"contracts"`
		Transports []struct{ Project string } `json:"transports"`
	}
	targets := map[string]bool{}
	if json.Unmarshal(data, &document) == nil {
		for _, entry := range append(document.Contracts, document.Transports...) {
			if clean, err := safeInventoryProject(entry.Project); err == nil && clean == entry.Project {
				targets[projectInventoryPath(entry.Project, name)] = true
			}
		}
	}
	destination := "<project>/" + name
	if len(targets) > 0 {
		paths := make([]string, 0, len(targets))
		for target := range targets {
			paths = append(paths, target)
		}
		sort.Strings(paths)
		destination = strings.Join(paths, ", ")
	}
	return fmt.Sprintf("protocolVersion %d is the older workspace-root layout, and clientgen reads each project's %s instead: "+
		"move each entry into the %s of the project it names (%s) as a protocolVersion %d file whose entries omit project, "+
		"then delete the root file", rootLayoutProtocolVersion, name, name, destination, inventoryProtocolVersion)
}

func loadAndValidateExternalInventory(view workspaceView, providers []provider) ([]ExternalContract, []Finding) {
	projectPaths, indexErr := view.members()
	if indexErr != nil {
		return nil, []Finding{{Code: "clientgen.invalid-external-inventory", Path: externalInventoryFile, Message: indexErr.Error()}}
	}
	files, findings := readProjectInventories(view.workspaceFiles, projectPaths, externalInventoryFile,
		"clientgen.external-inventory-read", "clientgen.invalid-external-inventory")

	providerByProject := make(map[string]provider, len(providers))
	for _, item := range providers {
		providerByProject[item.rel] = item
	}
	seen := map[string]bool{}
	claimedCallsites := map[string]bool{}
	validSeen := map[string]bool{}
	validated := []ExternalContract{}
	for _, file := range files {
		var inventory externalInventoryV2
		if decodeErr := decodeStrictInventory(file.data, &inventory); decodeErr != nil {
			findings = append(findings, Finding{Code: file.code, Path: file.path, Message: decodeErr.Error()})
			continue
		}
		if inventory.ProtocolVersion != inventoryProtocolVersion {
			findings = append(findings, Finding{Code: file.code, Path: file.path,
				Message: fmt.Sprintf("protocolVersion %d is unsupported; expected %d", inventory.ProtocolVersion, inventoryProtocolVersion)})
		}
		if inventory.Contracts == nil {
			findings = append(findings, Finding{Code: file.code, Path: file.path,
				Message: "contracts is required and must be an array"})
		}
		lastContract := ""
		for i, entry := range inventory.Contracts {
			contract := ExternalContract{Project: file.project, Authority: entry.Authority, Adapter: entry.Adapter,
				Callsites: entry.Callsites, Owner: entry.Owner, Tests: entry.Tests, Reason: entry.Reason}
			findingCount := len(findings)
			field := fmt.Sprintf("contracts[%d]", i)
			if generatedBindingRuntimeIdentity(view.workspaceFiles, contract.Project, "@putnami/client") ||
				generatedBindingRuntimeIdentity(view.workspaceFiles, contract.Project, "go.putnami.dev/client") {
				findings = append(findings, Finding{Code: "clientgen.framework-external-bypass", Path: file.path,
					Message: "generated binding runtimes must claim their callsites in " +
						projectInventoryPath(contract.Project, frameworkInventoryFile)})
			}

			for name, value := range map[string]string{
				"authority": contract.Authority, "owner": contract.Owner, "reason": contract.Reason,
			} {
				if blank(value) {
					findings = append(findings, file.finding(field+"."+name, name+" is required"))
				}
			}
			findings = append(findings, validateInventoryFile(view.workspaceFiles, file, field+".adapter", contract.Adapter)...)
			if contract.Adapter != "" && !pathOwnedByProject(contract.Adapter, contract.Project) {
				findings = append(findings, file.finding(field+".adapter", "adapter must sit in the directory of the project that holds this file"))
			}
			contractKey := contract.Project + "\x00" + contract.Adapter
			if seen[contractKey] {
				findings = append(findings, file.finding(field+".adapter", "project and adapter pair appears more than once"))
			}
			if i > 0 && contractKey <= lastContract {
				findings = append(findings, file.finding(field+".adapter", "contracts must be unique and sorted by adapter"))
			}
			seen[contractKey] = true
			lastContract = contractKey
			if len(contract.Callsites) == 0 {
				findings = append(findings, file.finding(field+".callsites", "at least one transport callsite is required"))
			}
			lastCallsite := ""
			for callsiteIndex, callsite := range contract.Callsites {
				callsiteField := fmt.Sprintf("%s.callsites[%d]", field, callsiteIndex)
				findings = append(findings, validateAuthorityCallsite(view.workspaceFiles, file, contract.Adapter, callsiteField, callsite)...)
				callsiteKey := externalCallsiteKey(callsite)
				if callsiteIndex > 0 && callsiteKey <= lastCallsite {
					findings = append(findings, file.finding(callsiteField,
						"callsites must be unique and sorted by path, transport, symbol and fingerprint"))
				}
				if claimedCallsites[callsiteKey] {
					findings = append(findings, file.finding(callsiteField,
						"callsite is already claimed by another external authority"))
				}
				claimedCallsites[callsiteKey] = true
				lastCallsite = callsiteKey
			}
			if len(contract.Tests) == 0 {
				findings = append(findings, file.finding(field+".tests", "at least one contract test is required"))
			}
			lastTest := ""
			for testIndex, testPath := range contract.Tests {
				findings = append(findings, validateInventoryFile(view.workspaceFiles, file,
					fmt.Sprintf("%s.tests[%d]", field, testIndex), testPath)...)
				if testIndex > 0 && testPath <= lastTest {
					findings = append(findings, file.finding(fmt.Sprintf("%s.tests[%d]", field, testIndex),
						"test paths must be unique and sorted"))
				}
				lastTest = testPath
			}

			if serviceID, named := firstPartyAuthority(contract.Authority, providers); named {
				findings = append(findings, Finding{Code: "clientgen.first-party-external-bypass", Path: file.path,
					ServiceID: serviceID,
					Message: fmt.Sprintf("%s.authority %q names a first-party provider; call it through its generated binding",
						field, contract.Authority)})
			}

			// project is the CONSUMER of the external contract. A first-party
			// provider consumes external contracts like any other project (OTLP
			// export, object storage), so its own client contract is irrelevant
			// here: the bypass check above judges the authority, and the source
			// scan still refuses any claimed callsite that names a first-party
			// service.
			item, exists := providerByProject[contract.Project]
			switch {
			case !exists, item.classification == ClassificationFirstParty:
				// An external SDK or source adapter may have no OpenAPI document. The
				// exact adapter source and executable tests are its authority boundary.
			case item.classification != ClassificationThirdParty:
				findings = append(findings, Finding{Code: "clientgen.stale-external-contract", Path: file.path,
					Message: fmt.Sprintf("project %q is not an unmarked configured contract", contract.Project)})
			}
			if len(findings) == findingCount {
				validated = append(validated, contract)
				validSeen[contract.Project] = true
			}
		}
	}
	for _, item := range providers {
		if item.classification == ClassificationThirdParty && !validSeen[item.rel] {
			findings = append(findings, Finding{Code: "clientgen.unclassified-external-contract", Path: item.rel,
				Message: "unmarked configured contract requires an entry in " + projectInventoryPath(item.rel, externalInventoryFile) +
					" naming its authority, adapter, owner, tests and reason"})
		}
	}
	return validated, findings
}

// firstPartyAuthority reports whether an external authority names a marked
// first-party provider, by its service identity or its project path. Such an
// entry would exempt calls TO a provider that has a generated binding.
func firstPartyAuthority(authority string, providers []provider) (string, bool) {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		return "", false
	}
	for _, item := range providers {
		if item.classification != ClassificationFirstParty || item.document == nil {
			continue
		}
		if authority == item.document.Service.ID || authority == item.rel {
			return item.document.Service.ID, true
		}
	}
	return "", false
}

func validateAuthorityCallsite(files workspaceFiles, file inventoryFile, adapter, field string, callsite ExternalCallsite) []Finding {
	findings := validateInventoryFile(files, file, field+".path", callsite.Path)
	if callsite.Path != adapter {
		findings = append(findings, file.finding(field+".path", "callsite path must equal the declared adapter file"))
	}
	switch callsite.Transport {
	case "connect", "http", "putnami-client", "sse", "websocket":
	default:
		findings = append(findings, file.finding(field+".transport", "transport is unsupported"))
	}
	if blank(callsite.Symbol) || len(callsite.Symbol) > 256 {
		findings = append(findings, file.finding(field+".symbol", "symbol must contain 1 to 256 characters"))
	}
	if len(callsite.Fingerprint) != sha256.Size*2 {
		findings = append(findings, file.finding(field+".fingerprint", "fingerprint must be a lowercase SHA-256 hex value"))
	} else if _, err := hex.DecodeString(callsite.Fingerprint); err != nil || strings.ToLower(callsite.Fingerprint) != callsite.Fingerprint {
		findings = append(findings, file.finding(field+".fingerprint", "fingerprint must be a lowercase SHA-256 hex value"))
	}
	return findings
}

func externalCallsiteKey(callsite ExternalCallsite) string {
	return callsite.Path + "\x00" + callsite.Transport + "\x00" + callsite.Symbol + "\x00" + callsite.Fingerprint
}

func safeInventoryProject(value string) (string, error) {
	if value == "." {
		return value, nil
	}
	return safeWorkspacePath(value)
}

func validateInventoryFile(files workspaceFiles, file inventoryFile, field, value string) []Finding {
	if blank(value) {
		return []Finding{file.finding(field, "path is required")}
	}
	clean, err := safeWorkspacePath(value)
	if err != nil || clean != value {
		if err == nil {
			err = fmt.Errorf("path must be canonical")
		}
		return []Finding{file.finding(field, err.Error())}
	}
	if !files.isRegularFile(clean) {
		return []Finding{file.finding(field, "path must name an existing regular file")}
	}
	return nil
}

func decodeStrictInventory(data []byte, target any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, field string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s: object key is not a string", field)
			}
			if seen[key] {
				return fmt.Errorf("%s.%s: duplicate object key", field, key)
			}
			seen[key] = true
			if err := walkJSONValue(decoder, field+"."+key); err != nil {
				return err
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil {
			return closeErr
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("%s: malformed object", field)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkJSONValue(decoder, field+"["+strconv.Itoa(index)+"]"); err != nil {
				return err
			}
			index++
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil {
			return closeErr
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("%s: malformed array", field)
		}
	default:
		return fmt.Errorf("%s: unexpected JSON delimiter %q", field, delim)
	}
	return nil
}
