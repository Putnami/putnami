package clientcontract

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ValidateOperationPaths checks explicit consumer routing for shared unary
// protocols. A mapping changes only a fixed REST path, never method, payload,
// security or transport. Paths are unescaped, absolute and same-authority;
// percent escapes and templates are refused rather than interpreted twice.
func ValidateOperationPaths(operations map[string]OperationV1, paths map[string]string) error {
	for _, id := range slices.Sorted(maps.Keys(paths)) {
		operation, ok := operations[id]
		if !ok {
			return fmt.Errorf("operation path names unknown operation %q", id)
		}
		if operation.Stream != StreamUnary || len(operation.Transports) != 1 ||
			operation.Transports[0].Protocol != TransportRESTJSON ||
			operation.Transports[0].Encoding != EncodingJSON || strings.ContainsAny(operation.Transports[0].Path, "{}") {
			return fmt.Errorf("operation path %q requires one fixed unary REST JSON transport", id)
		}
		if err := validateTransportPath(paths[id]); err != nil {
			return fmt.Errorf("operation path %q: %w", id, err)
		}
		if strings.ContainsAny(paths[id], "{}%") {
			return fmt.Errorf("operation path %q must be unescaped and have no template", id)
		}
	}
	return nil
}
