package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// streamEntrypoint names the go.putnami.dev/client function a generated stream
// method calls. It is resolved from the provider's declared transport order,
// never from a consumer flag: an application states what it wants to do, and
// the declaration states how the provider carries it.
type streamEntrypoint string

const (
	// entrypointSSE is client.OpenServerStream: a one-directional HTTP stream.
	entrypointSSE streamEntrypoint = "sse"
	// entrypointWebSocket is client.OpenServerStreamWS, client.OpenClientStream
	// or client.OpenBidiStream, depending on the declared stream mode.
	entrypointWebSocket streamEntrypoint = "websocket"
)

// selectStreamTransport returns the first declared transport this Go client
// runtime can carry for the operation's stream mode, in declared order.
//
// It resolves the request the generated method builds — the path a stream is
// opened on — and, for a duplex stream, the runtime entrypoint. A server stream
// has one entrypoint whatever its declared order is: client.OpenServerStream
// dispatches over the whole declared list at runtime, because a fallback is a
// second opening of the same method, never a second generated method.
func selectStreamTransport(method MethodIR) (*clientcontract.Transport, streamEntrypoint, error) {
	mode := method.Client.Stream
	for i := range method.Client.Transports {
		candidate := &method.Client.Transports[i]
		if candidate.Encoding != clientcontract.EncodingJSON {
			continue
		}
		switch candidate.Protocol {
		case clientcontract.TransportSSE:
			if mode == clientcontract.StreamServer {
				return candidate, entrypointSSE, nil
			}
		case clientcontract.TransportWebSocket:
			if candidate.WebSocket == nil || candidate.WebSocket.Subprotocol != clientcontract.WebSocketSubprotocolV1 {
				return nil, "", errors.Newf(CodeClientGenUnsupportedSemantic,
					"api: %s stream operation %s declares a websocket transport without the %s subprotocol",
					mode, method.OperationID, clientcontract.WebSocketSubprotocolV1)
			}
			// Resume continues a position the caller already consumed up to.
			// Only a server stream can do that: continuing a duplex
			// conversation would replay the caller's own messages.
			if candidate.WebSocket.Resume && mode != clientcontract.StreamServer {
				return nil, "", errors.Newf(CodeClientGenUnsupportedSemantic,
					"api: %s stream operation %s declares websocket resume, which only a server stream can honor",
					mode, method.OperationID)
			}
			return candidate, entrypointWebSocket, nil
		}
	}
	return nil, "", errors.Newf(CodeClientGenConfig,
		"api: %s stream operation %s declares no transport this client can carry", mode, method.OperationID)
}

// emitStreamMethod routes a declared stream to the emitter of its mode. It is
// the single place a stream shape is turned into an entrypoint, so a shape this
// generator does not implement fails here by name instead of emitting a method
// whose first frame the runtime refuses.
func (generator *strictClientGen) emitStreamMethod(output *strings.Builder, methodName string, method MethodIR) error {
	if transport := clientcontract.OperationProviderWire(method.Client); transport != nil {
		return generator.emitProviderWireMethod(output, methodName, method, *transport)
	}
	switch method.Client.Stream {
	case clientcontract.StreamServer:
		return generator.emitServerStreamMethod(output, methodName, method)
	case clientcontract.StreamClient, clientcontract.StreamBidirectional:
		return generator.emitDuplexStreamMethod(output, methodName, method)
	default:
		return errors.Newf(CodeClientGenUnsupportedSemantic,
			"api: operation %s declares stream shape %q, which this generator does not emit",
			method.OperationID, method.Client.Stream)
	}
}

// emitDuplexStreamMethod emits a client stream or a bidirectional stream.
//
// Both are WebSocket-only and both return the single declared result through
// messages.output: a client stream delivers it from Result, a bidirectional
// stream delivers it as the last value read by Recv before io.EOF. The two
// modes therefore share every step but the runtime entrypoint and the handle
// type they return.
func (generator *strictClientGen) emitDuplexStreamMethod(output *strings.Builder, methodName string, method MethodIR) error {
	transport, entrypoint, err := selectStreamTransport(method)
	if err != nil {
		return err
	}
	if entrypoint != entrypointWebSocket {
		return errors.Newf(CodeClientGenConfig,
			"api: %s stream operation %s resolved to a %s transport, which cannot carry client messages",
			method.Client.Stream, method.OperationID, entrypoint)
	}
	messages := method.Client.Messages
	if messages == nil || messages.Output == nil || messages.Input == nil {
		return errors.Newf(CodeClientGenConfig,
			"api: %s stream operation %s must declare both an input and an output message schema",
			method.Client.Stream, method.OperationID)
	}
	// The declared HTTP body of a duplex stream is its message schema, carried
	// in frames. A second body on the upgrade request has no wire to travel on.
	if method.Request != nil {
		return errors.Newf(CodeClientGenConfig,
			"api: %s stream operation %s has an unsupported HTTP request body",
			method.Client.Stream, method.OperationID)
	}

	groups, inputName, err := generator.declareStreamInput(methodName, method)
	if err != nil {
		return err
	}
	sendType, err := generator.streamMessageType(*messages.Input, methodName+"Send")
	if err != nil {
		return err
	}
	receiveType, err := generator.streamMessageType(*messages.Output, methodName+"Message")
	if err != nil {
		return err
	}
	operationVar, decoderName, err := generator.declareStreamOperation(output, methodName, method)
	if err != nil {
		return err
	}

	handle, opener := "client.RequestStream", "client.OpenClientStream"
	summary := "sends request messages and reads the single declared result of"
	if method.Client.Stream == clientcontract.StreamBidirectional {
		handle, opener = "client.BidiStream", "client.OpenBidiStream"
		summary = "opens the bidirectional conversation of"
	}
	fmt.Fprintf(output, "// %s %s %s through its generated service binding.\n", methodName, summary, method.OperationID)
	fmt.Fprintf(output, "func (c *%s) %s(ctx context.Context, in %s) (*%s[%s, %s], error) {\n",
		generator.clientName, methodName, inputName, handle, sendType, receiveType)
	if err := generator.emitStreamRequest(output, method, transport, groups); err != nil {
		return err
	}
	if decoderName == "" {
		fmt.Fprintf(output, "\treturn %s[%s, %s](ctx, c.transport, request, %s)\n",
			opener, sendType, receiveType, operationVar)
	} else {
		fmt.Fprintf(output, "\treturn %s[%s, %s](ctx, c.transport, request, %s, %s)\n",
			opener, sendType, receiveType, operationVar, decoderName)
	}
	output.WriteString("}\n\n")
	return nil
}

// streamMessageType names the Go type of one declared stream message. A
// nullable message schema is emitted as a pointer for the same reason a
// nullable body is: absent and zero are different values on the wire.
func (generator *strictClientGen) streamMessageType(schema clientcontract.Schema, hint string) (string, error) {
	name, err := generator.schemaType(schema, hint)
	if err != nil {
		return "", err
	}
	if generator.schemaNullable(schema, map[string]bool{}) {
		return "*" + name, nil
	}
	return name, nil
}

// declareStreamInput declares the typed input struct of a stream method and
// returns its parameters grouped by location. Streams carry no body, so the
// struct holds path, query and ordinary header parameters only.
func (generator *strictClientGen) declareStreamInput(methodName string, method MethodIR) (map[string][]ParameterIR, string, error) {
	groups := map[string][]ParameterIR{"path": {}, "query": {}, "header": {}}
	for _, parameter := range method.Parameters {
		if parameter.Schema.Type == "object" || parameter.Schema.Ref != "" || len(parameter.Schema.OneOf) > 0 ||
			generator.schemaNullable(parameter.Schema, map[string]bool{}) {
			return nil, "", errors.Newf(CodeClientGenConfig,
				"api: operation %s stream parameter %s has unsupported serialization", method.OperationID, parameter.Name)
		}
		groups[parameter.Location] = append(groups[parameter.Location], parameter)
	}
	inputName := methodName + "Input"
	var input strings.Builder
	fmt.Fprintf(&input, "// %s is the complete typed input for %s.\ntype %s struct {\n", inputName, method.OperationID, inputName)
	for _, location := range []string{"path", "query", "header"} {
		parameters := groups[location]
		if len(parameters) == 0 {
			continue
		}
		groupName := methodName + exportedFieldName(location)
		if err := generator.declareParameterGroup(groupName, parameters); err != nil {
			return nil, "", err
		}
		fmt.Fprintf(&input, "\t%s %s `json:%q`\n", exportedFieldName(location), groupName, location)
	}
	input.WriteString("}\n")
	generator.addDef(inputName, input.String())
	return groups, inputName, nil
}

// declareStreamOperation emits the embedded operation contract of a stream
// method and its declared-error decoder. The contract travels verbatim: the
// runtime validates the emitted bytes against the provider's document, so a
// client generated from a stale declaration fails before it opens a socket.
func (generator *strictClientGen) declareStreamOperation(output *strings.Builder, methodName string, method MethodIR) (string, string, error) {
	operationJSON, err := json.Marshal(method.Client)
	if err != nil {
		return "", "", errors.New(CodeClientGenConfig, "api: encode generated stream operation contract")
	}
	successes := make([]operationSuccessJSON, len(method.Successes))
	for i := range method.Successes {
		successes[i] = operationSuccessJSON{Status: method.Successes[i].Status}
		for _, content := range method.Successes[i].Content {
			successes[i].Content = append(successes[i].Content, operationContentJSON(content))
		}
	}
	successJSON, err := json.Marshal(successes)
	if err != nil {
		return "", "", errors.New(CodeClientGenConfig, "api: encode generated stream success contract")
	}
	operationVar := lowerFirst(methodName) + "Operation"
	fmt.Fprintf(output, "var %s = client.MustOperation(%q, %q, %q)\n\n", operationVar, method.OperationID, string(operationJSON), string(successJSON))
	decoderName, err := generator.declareTypedErrors(methodName, method)
	if err != nil {
		return "", "", err
	}
	return operationVar, decoderName, nil
}

// emitStreamRequest writes the *client.Request a stream method opens with. It
// carries the path, the query and the ordinary headers only: identity,
// credentials and propagation context travel in the admission frame, and the
// runtime refuses any reserved header placed here.
func (generator *strictClientGen) emitStreamRequest(output *strings.Builder, method MethodIR,
	transport *clientcontract.Transport, groups map[string][]ParameterIR) error {
	fmt.Fprintf(output, "\tpath := %q\n", transport.Path)
	for _, parameter := range groups["path"] {
		generator.imports["strings"] = true
		field := "in.Path." + exportedFieldName(parameter.Name)
		expression, err := generator.parameterExpression(field, parameter.Schema)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "\tpath = strings.ReplaceAll(path, %q, url.PathEscape(%s))\n", "{"+parameter.Name+"}", expression)
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
	output.WriteString("\trequest := &client.Request{Method: http.MethodGet, Path: path, QueryValues: query, Headers: headers")
	if generator.opts.Design != nil {
		fmt.Fprintf(output, ", FeatureTrace: %sDesign.Trace(%q)", generator.clientName, method.OperationID)
	}
	output.WriteString("}\n")
	return nil
}

// emitProviderWireMethod emits the method of an operation whose WebSocket wire
// the provider owns. A byte stream returns a *client.ByteStream, which is an
// io.ReadWriteCloser; a typed wire returns a *client.FrameStream of the
// declared input and output messages. Either way the method states the shape
// and the runtime owns the socket: dial, credentials on the upgrade, budgets,
// heartbeat, close codes and telemetry. No frame vocabulary reaches the
// emitted code.
func (generator *strictClientGen) emitProviderWireMethod(output *strings.Builder, methodName string, method MethodIR,
	transport clientcontract.Transport) error {
	if method.Request != nil {
		return errors.Newf(CodeClientGenConfig,
			"api: stream operation %s has an unsupported HTTP request body", method.OperationID)
	}
	groups, inputName, err := generator.declareStreamInput(methodName, method)
	if err != nil {
		return err
	}
	var handle, opener, summary string
	if transport.ByteStream() {
		handle, opener, summary = "client.ByteStream", "client.OpenByteStream", "opens the byte stream of"
	} else {
		messages := method.Client.Messages
		if messages == nil || messages.Input == nil || messages.Output == nil {
			return errors.Newf(CodeClientGenConfig,
				"api: stream operation %s must declare both an input and an output message schema", method.OperationID)
		}
		sendType, err := generator.streamMessageType(*messages.Input, methodName+"Send")
		if err != nil {
			return err
		}
		receiveType, err := generator.streamMessageType(*messages.Output, methodName+"Message")
		if err != nil {
			return err
		}
		handle = fmt.Sprintf("client.FrameStream[%s, %s]", sendType, receiveType)
		opener = fmt.Sprintf("client.OpenFrameStream[%s, %s]", sendType, receiveType)
		summary = fmt.Sprintf("opens the %s frame stream of", transport.WebSocket.Subprotocol)
	}
	operationVar, decoderName, err := generator.declareStreamOperation(output, methodName, method)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "// %s %s %s through its generated service binding.\n", methodName, summary, method.OperationID)
	fmt.Fprintf(output, "func (c *%s) %s(ctx context.Context, in %s) (*%s, error) {\n",
		generator.clientName, methodName, inputName, handle)
	if err := generator.emitStreamRequest(output, method, &transport, groups); err != nil {
		return err
	}
	if decoderName == "" {
		fmt.Fprintf(output, "\treturn %s(ctx, c.transport, request, %s)\n", opener, operationVar)
	} else {
		fmt.Fprintf(output, "\treturn %s(ctx, c.transport, request, %s, %s)\n", opener, operationVar, decoderName)
	}
	output.WriteString("}\n\n")
	return nil
}
