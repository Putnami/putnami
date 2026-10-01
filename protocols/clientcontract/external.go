package clientcontract

import (
	"bytes"
	"encoding/json"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ExternalContractKey is the operation-scope OpenAPI extension that keeps one
// operation of a first-party document out of the first-party contract. Its
// value is a JSON string naming the external authority that owns the
// operation's wire contract, for example "OCI Distribution Specification v1.1".
//
// A marked operation stays in the OpenAPI document and carries no
// x-putnami-client. Every first-party reader skips it: it contributes no
// operation to the contract, no protobuf method and no generated client method.
// Its callers use the standard's own adapters, which each consumer project
// lists in its clientgen.external.json.
const ExternalContractKey = "x-putnami-external-contract"

// ExternalContractAuthority classifies one operation of a first-party document
// by its two operation-scope extensions. client is the raw value of
// x-putnami-client and external the raw value of x-putnami-external-contract;
// an absent extension is nil or empty.
//
// It returns the authority and no diagnostic for an operation an external
// authority owns, and "" and no diagnostic for an operation that declares no
// external contract: the caller then reads it as a first-party operation, which
// still requires x-putnami-client. It returns error diagnostics when the
// external declaration contradicts itself: both extensions on one operation
// (client_contract.duplicate), a value that is not a JSON string
// (client_contract.parse_error), or a blank authority (client_contract.required).
func ExternalContractAuthority(client, external json.RawMessage) (string, []diag.Diagnostic) {
	if len(external) == 0 {
		return "", nil
	}
	if len(client) > 0 {
		return "", []diag.Diagnostic{diag.Errorf(ErrorCodeDuplicate, ExternalContractKey,
			"an operation declares both %s and %s; a first-party operation carries only %s, and an operation an external authority owns carries only %s",
			ExtensionKey, ExternalContractKey, ExtensionKey, ExternalContractKey)}
	}
	trimmed := bytes.TrimSpace(external)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, ExternalContractKey,
			"%s must be a JSON string naming the external authority", ExternalContractKey)}
	}
	var authority string
	if err := json.Unmarshal(trimmed, &authority); err != nil {
		return "", []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, ExternalContractKey, "%v", err)}
	}
	if blank(authority) {
		return "", []diag.Diagnostic{diag.Errorf(ErrorCodeRequired, ExternalContractKey,
			"%s must name the external authority that owns the operation's wire contract", ExternalContractKey)}
	}
	return authority, nil
}
