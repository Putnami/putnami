package api

import (
	"fmt"
	"strings"

	"go.putnami.dev/errors"
)

// omitOperations returns spec without the operations named in names, and the
// operations it left out, in contract order. The input spec is not modified.
//
// It is the only place a Go target leaves an operation out, and the emitter,
// the ownership manifest and the design graph all call it, so the three can
// never disagree about which operations the target has.
//
// A name the contract does not declare fails. An entry that matches nothing
// would read as a decision while changing nothing, and a mistyped one would
// leave the operation it meant to name failing generation with no hint why.
func omitOperations(spec SpecIR, names []string) (SpecIR, []MethodIR, error) {
	if len(names) == 0 {
		return spec, nil, nil
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	found := make(map[string]bool, len(names))
	var omitted []MethodIR
	services := make([]ServiceIR, len(spec.Services))
	for i, service := range spec.Services {
		kept := make([]MethodIR, 0, len(service.Methods))
		for _, method := range service.Methods {
			if wanted[method.OperationID] {
				found[method.OperationID] = true
				omitted = append(omitted, method)
				continue
			}
			kept = append(kept, method)
		}
		service.Methods = kept
		services[i] = service
	}
	for _, name := range names {
		if !found[name] {
			return SpecIR{}, nil, errors.Newf(CodeClientGenConfig,
				"api: go.omitOperations names operation %q, which the contract does not declare", name)
		}
	}
	spec.Services = services
	return spec, omitted, nil
}

// omittedOperationIDs lists the operation ids of omitted methods, in contract
// order.
func omittedOperationIDs(omitted []MethodIR) []string {
	if len(omitted) == 0 {
		return nil
	}
	ids := make([]string, len(omitted))
	for i := range omitted {
		ids[i] = omitted[i].OperationID
	}
	return ids
}

// emitOmittedOperations continues a client's doc comment with every operation
// go.omitOperations left out, so the generated source names each one whichever
// emitter wrote it. It writes nothing when none was left out.
func emitOmittedOperations(source *strings.Builder, omitted []MethodIR) {
	if len(omitted) == 0 {
		return
	}
	source.WriteString("//\n// The client configuration leaves these operations out of the Go target\n")
	source.WriteString("// (go.omitOperations), so they have no method here:\n//\n")
	for _, method := range omitted {
		fmt.Fprintf(source, "//   - %s (%s %s)\n", method.OperationID, strings.ToUpper(method.HTTPMethod), method.Path)
	}
}
