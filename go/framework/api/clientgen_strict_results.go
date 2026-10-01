package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// Result emission for an operation that declares more than one success status.
//
// Such an operation answers "created or existing" (200/201), "done or
// accepted" (200/202) or "a body or nothing" (200/204): the status is part of
// the answer, so the emitted method returns it. The TypeScript emitter returns
// the same facts as a union discriminated by `status`; Go has no such union, so
// the method returns one struct per operation:
//
//   - Status is the declared success status the provider answered.
//   - Body is the answered body when every status that carries a body declares
//     the same schema — the common case, where the status is the only
//     difference. It is nil on a status that declares no body.
//   - Otherwise each body-carrying status gets its own field, Body<status>,
//     non-nil only when the provider answered that status.
//
// Every body is decoded under the schema of the status the provider answered,
// and a status the operation does not declare fails in the runtime, exactly as
// the TypeScript runtime refuses it. A pointer per body, not a value, keeps
// "this status carries no body" apart from a zero body.

// successResult is the result type emitted for one operation.
type successResult struct {
	typeName string
	fields   []successResultField
}

// successResultField is one body field of a result, and the statuses whose
// body it holds.
type successResultField struct {
	name     string
	statuses []int
	// decodeType is the type argument client.DecodeResponse decodes into.
	decodeType string
	// pointer is true when decodeType already is the field's pointer type: a
	// nullable schema, where a JSON null decodes to nil.
	pointer bool
}

// returnsSuccessResult reports an operation whose method returns a result
// type rather than a bare body.
func returnsSuccessResult(method MethodIR) bool {
	return method.Client != nil && method.Client.Stream == clientcontract.StreamUnary && len(method.Successes) > 1
}

// successResultName is the result type of the method named methodName.
func successResultName(methodName string) string {
	return methodName + "Result"
}

// declareSuccessResult declares the result type of a multi-success operation.
// Every success must be a JSON document or empty; anything else is refused
// with the operation and the status named.
func (generator *strictClientGen) declareSuccessResult(methodName string, method MethodIR) (*successResult, error) {
	successes := append([]SuccessIR(nil), method.Successes...)
	sort.SliceStable(successes, func(i, j int) bool { return successes[i].Status < successes[j].Status })
	statuses := make([]int, 0, len(successes))
	var bodies []SuccessIR
	for i, success := range successes {
		if i > 0 && success.Status == successes[i-1].Status {
			return nil, errors.Newf(CodeClientGenConfig, "api: operation %s declares success status %d twice", method.OperationID, success.Status)
		}
		statuses = append(statuses, success.Status)
		if len(success.Headers) > 0 {
			return nil, errors.Newf(CodeClientGenConfig,
				"api: operation %s declares typed response headers on success %d; the Go client carries no typed response headers",
				method.OperationID, success.Status)
		}
		switch len(success.Content) {
		case 0:
			continue
		case 1:
		default:
			return nil, errors.Newf(CodeClientGenConfig,
				"api: operation %s declares %d media types on success %d; explicit representation selection is required",
				method.OperationID, len(success.Content), success.Status)
		}
		if success.Content[0].MediaType != "application/json" || success.Content[0].Schema == nil {
			return nil, errors.Newf(CodeClientGenConfig,
				"api: operation %s success %d representation %s is unsupported by REST JSON emission",
				method.OperationID, success.Status, success.Content[0].MediaType)
		}
		bodies = append(bodies, success)
	}

	shared, err := sameBodySchema(bodies)
	if err != nil {
		return nil, errors.Newf(CodeClientGenConfig, "api: operation %s: %v", method.OperationID, err)
	}
	result := &successResult{typeName: successResultName(methodName)}
	switch {
	case len(bodies) == 0:
		// No status carries a body ("created or replaced", 201/204): the
		// status is the whole answer, so the result has no body field.
	case shared:
		field, err := generator.successResultField(method, "Body", methodName+"Output", bodies)
		if err != nil {
			return nil, err
		}
		result.fields = append(result.fields, field)
	default:
		for _, body := range bodies {
			status := strconv.Itoa(body.Status)
			field, err := generator.successResultField(method, "Body"+status, methodName+"Output"+status, []SuccessIR{body})
			if err != nil {
				return nil, err
			}
			result.fields = append(result.fields, field)
		}
	}

	var definition strings.Builder
	fmt.Fprintf(&definition, "// %s is the result of %s: the declared success status the\n", result.typeName, method.OperationID)
	if len(result.fields) == 0 {
		fmt.Fprintf(&definition, "// provider answered, %s. No status declares a body.\n", statusList(statuses))
	} else {
		fmt.Fprintf(&definition, "// provider answered, %s, and the body that status declares.\n", statusList(statuses))
	}
	fmt.Fprintf(&definition, "type %s struct {\n", result.typeName)
	definition.WriteString("\t// Status is the declared success status the provider answered.\n\tStatus int\n")
	for _, field := range result.fields {
		if len(field.statuses) == len(statuses) {
			fmt.Fprintf(&definition, "\t// %s is the answered body.", field.name)
		} else {
			fmt.Fprintf(&definition, "\t// %s is the body of a %s answer, nil on any other status.", field.name, statusList(field.statuses))
		}
		if field.pointer {
			definition.WriteString(" A null body is nil as well.")
		}
		fieldType := field.decodeType
		if !field.pointer {
			fieldType = "*" + fieldType
		}
		fmt.Fprintf(&definition, "\n\t%s %s\n", field.name, fieldType)
	}
	definition.WriteString("}\n")
	generator.addDef(result.typeName, definition.String())
	return result, nil
}

// successResultField types one body field from the schema its statuses share.
func (generator *strictClientGen) successResultField(method MethodIR, name, hint string, bodies []SuccessIR) (successResultField, error) {
	field := successResultField{name: name}
	schema := *bodies[0].Content[0].Schema
	base, err := generator.schemaType(schema, hint)
	if err != nil {
		return field, errors.Newf(CodeClientGenConfig, "api: operation %s success %d body: %v", method.OperationID, bodies[0].Status, err)
	}
	field.decodeType = base
	if generator.schemaNullable(schema, map[string]bool{}) {
		field.decodeType = "*" + base
		field.pointer = true
	}
	for _, body := range bodies {
		field.statuses = append(field.statuses, body.Status)
	}
	return field, nil
}

// sameBodySchema reports whether every body-carrying success declares the
// same schema. Equality is on the canonical JSON of the declaration, so it is
// exact and independent of map order: two schemas that differ by one keyword
// get one field each.
func sameBodySchema(bodies []SuccessIR) (bool, error) {
	var first []byte
	for i, body := range bodies {
		encoded, err := json.Marshal(body.Content[0].Schema)
		if err != nil {
			return false, fmt.Errorf("encode success %d schema: %w", body.Status, err)
		}
		if i == 0 {
			first = encoded
			continue
		}
		if !bytes.Equal(first, encoded) {
			return false, nil
		}
	}
	return true, nil
}

// emitSuccessResultDispatch writes the call and the decode of a result: the
// runtime hands back a response whose status and body it already checked
// against the declaration, and each body is decoded under its own status.
func (generator *strictClientGen) emitSuccessResultDispatch(output *strings.Builder, result *successResult, operationVar, decoderName string) {
	fmt.Fprintf(output, "\tresponse, err := client.CallOperationResponse(ctx, c.transport, call, %s)\n", operationVar)
	if decoderName != "" {
		fmt.Fprintf(output, "\tif err != nil { return nil, %s(err) }\n", decoderName)
	} else {
		output.WriteString("\tif err != nil { return nil, err }\n")
	}
	fmt.Fprintf(output, "\tresult := &%s{Status: response.StatusCode}\n", result.typeName)
	if len(result.fields) > 0 {
		output.WriteString("\tswitch response.StatusCode {\n")
		for _, field := range result.fields {
			cases := make([]string, len(field.statuses))
			for i, status := range field.statuses {
				cases[i] = strconv.Itoa(status)
			}
			fmt.Fprintf(output, "\tcase %s:\n", strings.Join(cases, ", "))
			fmt.Fprintf(output, "\t\tdecoded, decodeErr := client.DecodeResponse[%s](c.transport, response, %s)\n", field.decodeType, operationVar)
			output.WriteString("\t\tif decodeErr != nil { return nil, decodeErr }\n")
			if field.pointer {
				fmt.Fprintf(output, "\t\tresult.%s = decoded\n", field.name)
			} else {
				fmt.Fprintf(output, "\t\tresult.%s = &decoded\n", field.name)
			}
		}
		output.WriteString("\t}\n")
	}
	output.WriteString("\treturn result, nil\n")
}

// statusList renders statuses as prose: "200", "200 or 201", "200, 202 or 204".
func statusList(statuses []int) string {
	parts := make([]string, len(statuses))
	for i, status := range statuses {
		parts[i] = strconv.Itoa(status)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
}
