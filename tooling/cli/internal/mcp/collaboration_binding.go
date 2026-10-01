package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// The collaboration routing policy: resolving the collaboration contracts a
// workspace binds (go.putnami.dev/protocol/collaboration) against the
// extensions an entry path discovered, and the rules both entry paths share —
// which requests reach a provider, how a provider's answer is checked, and
// how a transport failure becomes an outcome. CallProviderOperation is the one
// caller; the files beside this one hold no subprocess.

// collabDocument is the options.collaboration block of one workspace document.
type collabDocument struct {
	// Present reports whether the workspace document declares the block at
	// all. A workspace without it has no collaboration surface: no contract
	// command, no contract tool, no reserved name.
	Present bool
	// Name is the workspace's own name, which memory identities default to.
	Name string
	// Declared names every contract the block binds, valid or not.
	Declared map[string]bool
	// Bindings holds the valid bindings.
	Bindings collab.Bindings
	// Issues are the reasons a declared binding is refused, per contract. The
	// empty key holds problems of the block as a whole, which refuse every
	// contract.
	Issues map[string][]collab.CapabilityIssue
	// Unknown reports members of the block that name no contract — a typo
	// that leaves the intended contract unbound. They are reported beside
	// every contract and refuse none.
	Unknown []collab.CapabilityIssue
}

// Bound reports whether the block declares a binding for contract, valid or not.
func (d collabDocument) bound(contract string) bool { return d.Declared[contract] }

// readCollabDocument reads the collaboration block from the workspace document
// (putnami.workspace.json) under workspaceRoot, and from nowhere else: the
// user's global configuration and project configurations never bind a
// provider, so one file answers which backend serves a contract.
//
// A workspace document that does not exist or does not parse as JSON has no
// block. A block that repeats a member at any level is refused whole, because
// encoding/json would silently keep one spelling.
func readCollabDocument(workspaceRoot string) (collabDocument, error) {
	document := collabDocument{Declared: map[string]bool{}, Bindings: collab.Bindings{}, Issues: map[string][]collab.CapabilityIssue{}}
	if workspaceRoot == "" {
		return document, nil
	}
	data, err := os.ReadFile(wsproto.ResolveFile(workspaceRoot, wsproto.WorkspaceConfigFilename))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return document, nil
		}
		return document, fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	var head struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(data, &head)
	document.Name = head.Name

	options, count, err := objectMember(data, "options")
	if err != nil || count == 0 {
		return document, nil
	}
	if count > 1 {
		document.Present = true
		document.addIssue("", collab.ReasonBindingAmbiguous, "the workspace document declares options more than once")
		return document, nil
	}
	block, count, err := objectMember(options, collab.BindingOptionNamespace)
	if err != nil || count == 0 {
		return document, nil
	}
	document.Present = true
	if count > 1 {
		document.addIssue("", collab.ReasonBindingAmbiguous, "the workspace document declares options.collaboration more than once")
		return document, nil
	}
	for _, contract := range declaredContracts(block) {
		document.Declared[contract] = true
	}
	bindings, diags := collab.ParseBindings(block)
	if bindings == nil {
		// The block as a whole is refused — ambiguous, oversized or not an
		// object — so no contract it names can be honored.
		for _, d := range diag.Errors(diags) {
			document.addIssue("", reasonFor(d.Code), "options.collaboration"+fieldSuffix(d.Field)+": "+d.Message)
		}
		return document, nil
	}
	document.Bindings = bindings
	for _, d := range diag.Errors(diags) {
		message := "options.collaboration" + fieldSuffix(d.Field) + ": " + d.Message
		if d.Code == collab.ErrorCodeUnknownContract {
			document.Unknown = append(document.Unknown, collab.CapabilityIssue{Reason: collab.ReasonBindingInvalid, Message: message})
			continue
		}
		document.addIssue(contractOf(d.Field), reasonFor(d.Code), message)
	}
	return document, nil
}

func fieldSuffix(field string) string {
	if field == "" {
		return ""
	}
	return "." + field
}

func (d *collabDocument) addIssue(contract, reason, message string) {
	d.Issues[contract] = append(d.Issues[contract], collab.CapabilityIssue{Reason: reason, Message: message})
}

// IssuesFor returns the problems that keep contract's binding from being
// honored: the block-wide ones and the contract's own.
func (d collabDocument) issuesFor(contract string) []collab.CapabilityIssue {
	issues := append([]collab.CapabilityIssue(nil), d.Issues[""]...)
	return append(issues, d.Issues[contract]...)
}

func reasonFor(code string) string {
	switch code {
	case collab.ErrorCodeDuplicateMember:
		return collab.ReasonBindingAmbiguous
	case collab.ErrorCodeUnsupportedVersion:
		return collab.ReasonBindingVersion
	default:
		return collab.ReasonBindingInvalid
	}
}

// contractOf is the leading segment of a binding diagnostic's field, when it
// names a contract.
func contractOf(field string) string {
	for i, r := range field {
		if r == '.' || r == '[' {
			field = field[:i]
			break
		}
	}
	if collab.IsContract(field) {
		return field
	}
	return ""
}

// declaredContracts lists the contract names a block's object declares, in
// canonical order, whether or not their bindings are valid.
func declaredContracts(block []byte) []string {
	var members map[string]json.RawMessage
	if json.Unmarshal(block, &members) != nil {
		return nil
	}
	var names []string
	for name := range members {
		if collab.IsContract(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// objectMember returns the raw value of one member of a JSON object and how
// many times the object declares it.
func objectMember(data []byte, name string) ([]byte, int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, 0, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, 0, errors.New("not an object")
	}
	var value []byte
	count := 0
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, 0, err
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, 0, err
		}
		if key, _ := keyToken.(string); key == name {
			count++
			value = raw
		}
	}
	if _, err := decoder.Token(); err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	return value, count, nil
}
