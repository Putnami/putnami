package api

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// enumSchema declares a closed enum. Each value is a JSON literal, so a string
// member keeps its quotes.
func enumSchema(schemaType, format string, values ...string) clientcontract.Schema {
	schema := clientcontract.Schema{Type: schemaType, Format: format}
	for _, value := range values {
		schema.Enum = append(schema.Enum, json.RawMessage(value))
	}
	return schema
}

// enumParameterFixtureSpec is one operation with a string enum in every place a
// parameter travels: a path segment, a required and an optional query value, an
// array of query values, and a header. The integer enum and the plain string
// are the two forms that need no conversion.
func enumParameterFixtureSpec() SpecIR {
	label := enumSchema("string", "", `"bug"`, `"feature"`)
	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "tickets", Audience: "https://tickets.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas: map[string]clientcontract.Schema{},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "listTickets", OperationID: "listTickets", HTTPMethod: "GET", Path: "/queues/{queue}/tickets",
			Parameters: []ParameterIR{
				{Name: "queue", Location: "path", Required: true, Schema: enumSchema("string", "", `"support"`, `"billing"`)},
				{Name: "labels", Location: "query", Schema: clientcontract.Schema{Type: "array", Items: &label}},
				{Name: "order", Location: "query", Required: true, Schema: enumSchema("string", "", `"newest"`, `"oldest"`)},
				{Name: "priority", Location: "query", Schema: enumSchema("integer", "int32", `1`, `2`, `3`)},
				{Name: "search", Location: "query", Schema: clientcontract.Schema{Type: "string"}},
				{Name: "state", Location: "query", Schema: enumSchema("string", "", `"open"`, `"closed"`)},
				{Name: "X-View", Location: "header", Schema: enumSchema("string", "", `"compact"`, `"full"`)},
			},
			Successes: []SuccessIR{{Status: 204}},
			Client:    strictUnaryOperation("/queues/{queue}/tickets", []clientcontract.DeclaredError{}),
		}}}},
	}
}

// A string enum parameter has its own named Go type, and url.Values,
// http.Header and the path map take a plain string. The emitted call converts
// the value, or the client does not compile.
func TestGenerateStrictClient_ConvertsAStringEnumParameterToString(t *testing.T) {
	source, err := GenerateClientFromIR(enumParameterFixtureSpec(), ClientGenOptions{PackageName: "ticketsclient", ClientName: "TicketsClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", source, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, source)
	}
	for _, want := range []string{
		"type ListTicketsQueryState string",
		"State *ListTicketsQueryState `json:\"state,omitempty\"`",
		"Order ListTicketsQueryOrder `json:\"order\"`",
		"Labels *[]ListTicketsQueryLabelsItem `json:\"labels,omitempty\"`",
		`params["queue"] = string(in.Path.Queue)`,
		`query.Add("order", string(in.Query.Order))`,
		`query.Add("state", string(*in.Query.State))`,
		`query.Add("labels", string(value))`,
		`headers.Add("X-View", string(*in.Header.XView))`,
		// fmt.Sprint formats a named integer type as its number.
		`query.Add("priority", fmt.Sprint(*in.Query.Priority))`,
		// A plain string needs no conversion and keeps its emitted form.
		`query.Add("search", *in.Query.Search)`,
	} {
		if !containsNormalized(source, want) {
			t.Errorf("generated source missing %q:\n%s", want, source)
		}
	}
}

// A WebSocket stream writes its path parameters straight into the URL, so it
// converts a string enum there too.
func TestGenerateStrictClient_WebSocketStreamConvertsAStringEnumParameterToString(t *testing.T) {
	message := strictSchemaObject(map[string]clientcontract.Schema{"value": {Type: "string"}}, "value")
	transport := wsTransport()
	transport.Path = "/queues/{queue}/tickets/exchange"
	method := wsStreamMethod(clientcontract.StreamBidirectional, []clientcontract.Transport{transport}, wsMessageShapes(&message, &message))
	method.Path = transport.Path
	method.Parameters = []ParameterIR{
		{Name: "queue", Location: "path", Required: true, Schema: enumSchema("string", "", `"support"`, `"billing"`)},
		{Name: "state", Location: "query", Schema: enumSchema("string", "", `"open"`, `"closed"`)},
	}
	source, err := GenerateClientFromIR(strictSpecWith(method, nil), ClientGenOptions{PackageName: "itemsclient", ClientName: "ItemsClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	for _, want := range []string{
		`path = strings.ReplaceAll(path, "{queue}", url.PathEscape(string(in.Path.Queue)))`,
		`query.Add("state", string(*in.Query.State))`,
	} {
		if !containsNormalized(source, want) {
			t.Errorf("generated stream missing %q:\n%s", want, source)
		}
	}
}

// generatedEnumParameterE2E runs inside the throwaway module, in the generated
// package: the emitted client calls a real provider through the real runtime,
// and the provider records what reached the wire.
const generatedEnumParameterE2E = `package ticketsclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"go.putnami.dev/client"
)

type observedRequest struct {
	path  string
	query url.Values
	view  []string
}

func TestTheEmittedClientSendsEachEnumParameterAsItsValue(t *testing.T) {
	observed := make(chan observedRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		observed <- observedRequest{path: request.URL.EscapedPath(), query: request.URL.Query(), view: request.Header.Values("X-View")}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{URL: server.URL, ClientID: "consumer", AllowInsecure: true}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	tickets := NewTicketsClient(transport)

	labels := []ListTicketsQueryLabelsItem{ListTicketsQueryLabelsItemBug, ListTicketsQueryLabelsItemFeature}
	priority := ListTicketsQueryPriority2
	search := "printer jam"
	state := ListTicketsQueryStateClosed
	view := ListTicketsHeaderXViewFull
	var in ListTicketsInput
	in.Path.Queue = ListTicketsPathQueueBilling
	in.Query.Labels = &labels
	in.Query.Order = ListTicketsQueryOrderOldest
	in.Query.Priority = &priority
	in.Query.Search = &search
	in.Query.State = &state
	in.Header.XView = &view
	if err := tickets.ListTickets(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got := <-observed
	want := url.Values{"labels": {"bug", "feature"}, "order": {"oldest"}, "priority": {"2"}, "search": {"printer jam"}, "state": {"closed"}}
	if got.path != "/queues/billing/tickets" || !reflect.DeepEqual(got.query, want) || !reflect.DeepEqual(got.view, []string{"full"}) {
		t.Fatalf("the provider saw path %q, query %v and X-View %q", got.path, got.query, got.view)
	}

	// An optional enum left unset stays off the wire.
	var bare ListTicketsInput
	bare.Path.Queue = ListTicketsPathQueueSupport
	bare.Query.Order = ListTicketsQueryOrderNewest
	if err := tickets.ListTickets(context.Background(), bare); err != nil {
		t.Fatal(err)
	}
	got = <-observed
	if got.path != "/queues/support/tickets" || !reflect.DeepEqual(got.query, url.Values{"order": {"newest"}}) || len(got.view) != 0 {
		t.Fatalf("the provider saw path %q, query %v and X-View %q", got.path, got.query, got.view)
	}
}
`

// TestTheEmittedGoClientSendsEnumParametersThroughTheRealRuntime compiles the
// emitted client against this repository's runtime and calls a real provider:
// each enum parameter reaches the wire as its declared value.
func TestTheEmittedGoClientSendsEnumParametersThroughTheRealRuntime(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "the-emitted-go-client-compiles-against-the-real-client-runtime")
	source, err := GenerateClientFromIR(enumParameterFixtureSpec(), ClientGenOptions{PackageName: "ticketsclient", ClientName: "TicketsClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, source)
	if err := os.WriteFile(filepath.Join(moduleDir, "enum_parameter_e2e_test.go"), []byte(generatedEnumParameterE2E), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted client did not send its enum parameters through the real runtime: %v\n%s\n--- source:\n%s", testErr, output, source)
	}
}
