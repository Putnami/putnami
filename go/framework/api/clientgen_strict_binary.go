package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// Raw octet emission for the strict Go client.
//
// What the emitted method looks like:
//
//   - the request payload is an io.Reader. Binary reads under its bound before
//     opening a socket; BinaryStream hands the reader to the transport once.
//   - the success payload is a named output struct carrying bytes or a reader, the
//     status the provider answered and the content type it labeled them with.
//   - nothing is base64-encoded and nothing is wrapped in JSON.

// binaryRequest returns the raw octet request representation of an operation,
// or nil when the request is a JSON document.
func binaryRequest(method MethodIR) *ContentIR {
	if method.Request == nil {
		return nil
	}
	for i := range method.Request.Content {
		if method.Request.Content[i].IsBinary() {
			return &method.Request.Content[i]
		}
	}
	return nil
}

// binaryResponse returns the raw octet success representation of an operation,
// or nil when every declared success is a JSON document or empty.
func binaryResponse(method MethodIR) *ContentIR {
	for i := range method.Successes {
		for j := range method.Successes[i].Content {
			if method.Successes[i].Content[j].IsBinary() {
				return &method.Successes[i].Content[j]
			}
		}
	}
	return nil
}

// operationCarriesBinary reports an operation with a raw octet payload on
// either side.
func operationCarriesBinary(method MethodIR) bool {
	return binaryRequest(method) != nil || binaryResponse(method) != nil
}

// refuseBinaryStream refuses raw octets on a streaming operation. A stream
// carries declared messages, and a message is a value, not a body: there is no
// framing that would say where one octet payload ends and the next begins.
func refuseBinaryStream(method MethodIR) error {
	if method.Client == nil || method.Client.Stream == clientcontract.StreamUnary {
		return nil
	}
	if !operationCarriesBinary(method) {
		return nil
	}
	return errors.Newf(CodeClientGenUnsupportedSemantic,
		"api: operation %s declares a raw octet payload on a %s stream; streams carry declared messages, not bodies",
		method.OperationID, method.Client.Stream)
}

// emitBinaryMethod writes one unary method whose request payload, response
// payload, or both are raw octets.
func (generator *strictClientGen) emitBinaryMethod(output *strings.Builder, methodName string, method MethodIR) error {
	transport, err := generator.selectUnaryTransport(method)
	if err != nil {
		return err
	}
	if transport.Protocol != clientcontract.TransportRESTJSON {
		// The transport a raw octet payload needs is the endpoint's own URL. A
		// Connect envelope carries one encoded message, so the bytes would have
		// to travel base64 inside it — the silent re-wrapping this declaration
		// refuses.
		return errors.Newf(CodeClientGenUnsupportedSemantic,
			"api: operation %s declares a raw octet payload and dispatches on %s; only rest-json carries octets unchanged",
			method.OperationID, transport.Protocol)
	}
	request := binaryRequest(method)
	response := binaryResponse(method)
	if ((request != nil && request.Streamed) || (response != nil && response.Streamed)) &&
		method.Client.Resilience != nil && method.Client.Resilience.Cache != nil {
		return errors.Newf(CodeClientGenUnsupportedSemantic, "api: operation %s cannot cache a streamed octet body", method.OperationID)
	}
	if request != nil && len(method.Request.Content) != 1 {
		return errors.Newf(CodeClientGenConfig,
			"api: operation %s mixes a raw octet request representation with %d others; explicit representation selection is required",
			method.OperationID, len(method.Request.Content)-1)
	}
	if len(method.Successes) == 0 {
		return errors.Newf(CodeClientGenConfig, "api: operation %s declares no success status", method.OperationID)
	}
	if response != nil && len(method.Successes) != 1 {
		// The octet payload is the answer, and the caller could not tell which
		// declared status carries it. The TypeScript emitter refuses the same
		// declaration. Octets in the request only leave the JSON answers to the
		// ordinary result below.
		return errors.Newf(CodeClientGenUnsupportedSemantic,
			"api: operation %s declares a raw octet payload beside %d declared success variants; the octets travel on exactly one success status",
			method.OperationID, len(method.Successes))
	}
	success := method.Successes[0]
	if len(success.Headers) > 0 && !returnsSuccessResult(method) {
		return errors.Newf(CodeClientGenConfig,
			"api: operation %s has typed response headers; result-union emission is required", method.OperationID)
	}
	if response != nil && len(success.Content) != 1 {
		return errors.Newf(CodeClientGenConfig,
			"api: operation %s mixes a raw octet success representation with %d others; explicit representation selection is required",
			method.OperationID, len(success.Content)-1)
	}

	groups, err := generator.binaryParameterGroups(method)
	if err != nil {
		return err
	}
	inputName := methodName + "Input"
	if err := generator.declareBinaryInput(inputName, methodName, method, groups, request); err != nil {
		return err
	}

	// The JSON success half of a binary-request operation keeps the ordinary
	// projection: a provider that answers a typed document to an octet upload
	// is one operation, not two.
	outputType := ""
	returnType := ""
	var result *successResult
	switch {
	case response != nil:
		outputType = methodName + "Output"
		returnType = "*" + outputType
		generator.declareBinaryOutput(outputType, method, success, *response)
	case returnsSuccessResult(method):
		result, err = generator.declareSuccessResult(methodName, method)
		if err != nil {
			return err
		}
		outputType = "*" + result.typeName
		returnType = outputType
	case len(success.Content) == 1:
		if success.Content[0].MediaType != "application/json" || success.Content[0].Schema == nil {
			return errors.Newf(CodeClientGenConfig,
				"api: operation %s success representation %s is unsupported beside a raw octet request",
				method.OperationID, success.Content[0].MediaType)
		}
		outputType, err = generator.schemaType(*success.Content[0].Schema, methodName+"Output")
		if err != nil {
			return errors.Newf(CodeClientGenConfig, "api: operation %s success body: %v", method.OperationID, err)
		}
		if generator.schemaNullable(*success.Content[0].Schema, map[string]bool{}) {
			outputType = "*" + outputType
		}
		returnType = outputType
		if !strings.HasPrefix(returnType, "*") {
			returnType = "*" + returnType
		}
	}

	operationVar, err := generator.emitBinaryOperationVar(output, methodName, method)
	if err != nil {
		return err
	}
	decoderName, err := generator.declareTypedErrors(methodName, method)
	if err != nil {
		return err
	}

	fail := "return err"
	if outputType != "" {
		fail = "return nil, err"
	}
	fmt.Fprintf(output, "// %s calls %s through its generated service binding. ", methodName, method.OperationID)
	generator.describeBinaryMethod(output, request, response)
	if outputType == "" {
		fmt.Fprintf(output, "func (c *%s) %s(ctx context.Context, in %s) error {\n", generator.clientName, methodName, inputName)
	} else {
		fmt.Fprintf(output, "func (c *%s) %s(ctx context.Context, in %s) (%s, error) {\n", generator.clientName, methodName, inputName, returnType)
	}
	fmt.Fprintf(output, "\tpath := %q\n", transport.Path)
	if err := generator.emitPathParams(output, groups["path"]); err != nil {
		return err
	}
	output.WriteString("\tquery := url.Values{}\n\theaders := make(http.Header)\n")
	for _, parameter := range groups["query"] {
		if err := generator.emitParameter(output, "query", "in.Query."+exportedFieldName(parameter.Name), parameter); err != nil {
			return err
		}
	}
	for _, parameter := range groups["header"] {
		if err := generator.emitParameter(output, "headers", "in.Header."+exportedFieldName(parameter.Name), parameter); err != nil {
			return err
		}
	}
	output.WriteString("\tvar body []byte\n\tvar err error\n")
	if request != nil {
		generator.imports["io"] = true
		if request.Streamed {
			output.WriteString("\t_ = err\n\theaders.Set(\"Content-Type\", in.ContentType)\n")
		} else {
			fmt.Fprintf(output, "\tbody, err = client.ReadBoundedBody(in.Body, %d)\n", request.MaxBytes)
			fmt.Fprintf(output, "\tif err != nil { %s }\n", fail)
			fmt.Fprintf(output, "\theaders.Set(\"Content-Type\", %q)\n", request.MediaType)
		}
	} else {
		output.WriteString("\t_ = err\n")
	}
	fmt.Fprintf(output, "\trequest := &client.Request{Method: %q, Path: path, QueryValues: query, Headers: headers, Body: body", method.HTTPMethod)
	if request != nil && request.Streamed {
		output.WriteString(", BodyStream: in.Body")
	}
	if generator.opts.Design != nil {
		fmt.Fprintf(output, ", FeatureTrace: %sDesign.Trace(%q)", generator.clientName, method.OperationID)
	}
	output.WriteString("}\n")
	if len(groups["path"]) > 0 {
		output.WriteString("\tcall := &client.OperationCall{Request: request, PathParams: params}\n")
	} else {
		output.WriteString("\tcall := &client.OperationCall{Request: request}\n")
	}
	if result != nil {
		generator.emitSuccessResultDispatch(output, result, operationVar, decoderName)
	} else {
		generator.emitBinaryDispatch(output, methodName, method, operationVar, decoderName, outputType, returnType, response)
	}
	output.WriteString("}\n\n")
	return nil
}

// binaryParameterGroups applies the same parameter rules as the JSON emitter.
// A raw octet payload changes the body, never the parameters.
func (generator *strictClientGen) binaryParameterGroups(method MethodIR) (map[string][]ParameterIR, error) {
	groups := map[string][]ParameterIR{"path": {}, "query": {}, "header": {}}
	for _, parameter := range method.Parameters {
		if err := generator.refuseOpaqueParameter(method, parameter); err != nil {
			return nil, err
		}
		if parameter.Schema.Type == "object" || parameter.Schema.Ref != "" || len(parameter.Schema.OneOf) > 0 {
			return nil, errors.Newf(CodeClientGenConfig,
				"api: operation %s parameter %s has unsupported object serialization", method.OperationID, parameter.Name)
		}
		if generator.schemaNullable(parameter.Schema, map[string]bool{}) {
			return nil, errors.Newf(CodeClientGenConfig,
				"api: operation %s parameter %s is nullable and has no lossless default wire serialization", method.OperationID, parameter.Name)
		}
		groups[parameter.Location] = append(groups[parameter.Location], parameter)
	}
	return groups, nil
}

func (generator *strictClientGen) declareBinaryInput(
	inputName, methodName string,
	method MethodIR,
	groups map[string][]ParameterIR,
	request *ContentIR,
) error {
	var input strings.Builder
	fmt.Fprintf(&input, "// %s is the complete typed input for %s.\ntype %s struct {\n", inputName, method.OperationID, inputName)
	for _, location := range []string{"path", "query", "header"} {
		parameters := groups[location]
		if len(parameters) == 0 {
			continue
		}
		groupName := methodName + exportedFieldName(location)
		if err := generator.declareParameterGroup(groupName, parameters); err != nil {
			return errors.Newf(CodeClientGenConfig, "api: operation %s: %v", method.OperationID, err)
		}
		fmt.Fprintf(&input, "\t%s %s `json:%q`\n", exportedFieldName(location), groupName, location)
	}
	if request != nil {
		if request.Streamed {
			input.WriteString("\t// ContentType is the concrete media type of Body, including any parameters.\n\tContentType string\n")
			input.WriteString("\t// Body is sent incrementally once, without buffering or replay.\n")
		} else {
			fmt.Fprintf(&input, "\t// Body is the %s request payload. It is read under the declared\n", request.MediaType)
			fmt.Fprintf(&input, "\t// %d-byte bound: a reader that carries more is refused before it is read\n", request.MaxBytes)
			input.WriteString("\t// to the end, and before any socket is opened. A nil reader sends no bytes.\n")
		}
		input.WriteString("\tBody io.Reader `json:\"-\"`\n")
	}
	input.WriteString("}\n")
	generator.addDef(inputName, input.String())
	return nil
}

func (generator *strictClientGen) declareBinaryOutput(outputType string, method MethodIR, success SuccessIR, response ContentIR) {
	var definition strings.Builder
	fmt.Fprintf(&definition, "// %s is the raw octet success payload of %s.\n", outputType, method.OperationID)
	fmt.Fprintf(&definition, "type %s struct {\n", outputType)
	fmt.Fprintf(&definition, "\t// Status is the declared success status, %d.\n\tStatus int\n", success.Status)
	fmt.Fprintf(&definition, "\t// ContentType is the media type the provider labeled the payload with,\n")
	fmt.Fprintf(&definition, "\t// checked against the declared %s before the bytes are handed over.\n\tContentType string\n", response.MediaType)
	if response.Streamed {
		generator.imports["io"] = true
		definition.WriteString("\t// Body is the unbuffered payload. The caller must close it.\n\tBody io.ReadCloser\n")
	} else {
		fmt.Fprintf(&definition, "\t// Body is the payload, verbatim. It is read under the declared %d-byte\n", response.MaxBytes)
		definition.WriteString("\t// bound: a larger response fails instead of being buffered.\n\tBody []byte\n")
	}
	definition.WriteString("}\n")
	generator.addDef(outputType, definition.String())
}

func (generator *strictClientGen) describeBinaryMethod(output *strings.Builder, request, response *ContentIR) {
	switch {
	case request != nil && response != nil:
		fmt.Fprintf(output, "It sends %s and receives %s, unchanged.\n", request.MediaType, response.MediaType)
	case request != nil:
		fmt.Fprintf(output, "It sends %s unchanged.\n", request.MediaType)
	default:
		fmt.Fprintf(output, "It receives %s unchanged.\n", response.MediaType)
	}
}

func (generator *strictClientGen) emitBinaryOperationVar(output *strings.Builder, methodName string, method MethodIR) (string, error) {
	operationJSON, err := json.Marshal(method.Client)
	if err != nil {
		return "", errors.New(CodeClientGenConfig, "api: encode generated operation contract")
	}
	successes := make([]operationSuccessJSON, len(method.Successes))
	for i := range method.Successes {
		successes[i] = operationSuccessJSON{Status: method.Successes[i].Status}
		for _, content := range method.Successes[i].Content {
			successes[i].Content = append(successes[i].Content, operationContent(content))
		}
	}
	successJSON, err := json.Marshal(successes)
	if err != nil {
		return "", errors.New(CodeClientGenConfig, "api: encode generated success contract")
	}
	operationVar := lowerFirst(methodName) + "Operation"
	if method.Request == nil {
		fmt.Fprintf(output, "var %s = client.MustOperation(%q, %q, %q)\n\n", operationVar, method.OperationID, string(operationJSON), string(successJSON))
		return operationVar, nil
	}
	requestJSON, err := json.Marshal(method.Request)
	if err != nil {
		return "", errors.New(CodeClientGenConfig, "api: encode generated request contract")
	}
	fmt.Fprintf(output, "var %s = client.MustOperation(%q, %q, %q, %q)\n\n", operationVar, method.OperationID, string(operationJSON), string(successJSON), string(requestJSON))
	return operationVar, nil
}

func (generator *strictClientGen) emitBinaryDispatch(
	output *strings.Builder,
	methodName string,
	method MethodIR,
	operationVar, decoderName, outputType, returnType string,
	response *ContentIR,
) {
	_ = methodName
	_ = method
	switch {
	case response != nil:
		call := "CallOperationBinary"
		if response.Streamed {
			call = "CallOperationBinaryStream"
		}
		fmt.Fprintf(output, "\tpayload, err := client.%s(ctx, c.transport, call, %s)\n", call, operationVar)
		if decoderName != "" {
			fmt.Fprintf(output, "\tif err != nil { return nil, %s(err) }\n", decoderName)
		} else {
			output.WriteString("\tif err != nil { return nil, err }\n")
		}
		fmt.Fprintf(output, "\treturn &%s{Status: payload.Status, ContentType: payload.ContentType, Body: payload.Body}, nil\n", outputType)
	case outputType == "":
		fmt.Fprintf(output, "\terr = client.CallOperationVoid(ctx, c.transport, call, %s)\n", operationVar)
		if decoderName != "" {
			fmt.Fprintf(output, "\tif err != nil { return %s(err) }\n\treturn nil\n", decoderName)
		} else {
			output.WriteString("\treturn err\n")
		}
	default:
		fmt.Fprintf(output, "\tresult, err := client.CallOperation[%s](ctx, c.transport, call, %s)\n", outputType, operationVar)
		if decoderName != "" {
			fmt.Fprintf(output, "\tif err != nil { return nil, %s(err) }\n", decoderName)
		} else {
			output.WriteString("\tif err != nil { return nil, err }\n")
		}
		if returnType == outputType {
			output.WriteString("\treturn result, nil\n")
		} else {
			output.WriteString("\treturn &result, nil\n")
		}
	}
}
