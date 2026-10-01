package openapi

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/api"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// connectEncodingOrderContract builds one real first-party provider whose
// bridge serves Connect over JSON and protobuf, registers one unary route with
// the declared client options, and returns the operation contract it publishes
// or the refusal the declaration earned.
func connectEncodingOrderContract(t *testing.T, options api.ClientOperationOptions, mountBridge bool) (*clientcontract.OperationV1, error) {
	t.Helper()
	apiPlugin := api.New(&fakeServer{}, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "users", Audience: "https://users.internal"},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/users/{id}").
		Params(api.Type[apiUserParams]()).
		Returns(api.Type[apiFirstPartyUser]()).
		Client(options).
		Document())
	openapiPlugin := NewPlugin(PluginOptions{Title: "Users", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	apiPlugin.PublishClientProtobuf(api.ClientProtobufProjection{
		Descriptor: &clientcontract.ProtobufDescriptor{
			Syntax:  "proto3",
			Package: "users.v1",
			Services: []clientcontract.ProtobufService{{Name: "ApiService", Methods: []clientcontract.ProtobufMethod{
				{Name: "GetUsers", Input: "GetUsersRequest", Output: "GetUsersReply"},
			}}},
			Messages: []clientcontract.ProtobufMessage{
				{Name: "GetUsersRequest", Fields: []clientcontract.ProtobufField{}},
				{Name: "GetUsersReply", Fields: []clientcontract.ProtobufField{}},
			},
			Enums: []clientcontract.ProtobufEnum{},
		},
		RouteMethods: map[string]string{"GET /users/{id}": "/users.v1.ApiService/GetUsers"},
	})
	if mountBridge {
		apiPlugin.PublishClientConnectTransport([]clientcontract.Encoding{clientcontract.EncodingJSON, clientcontract.EncodingProto})
	}
	// A projection refusal surfaces here: the document fails to serialize with
	// the route named rather than publishing a contract no client could satisfy.
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		return nil, err
	}
	if _, err := openapiPlugin.OpenAPISpecJSON(); err != nil {
		return nil, err
	}
	return openapiPlugin.Spec().Paths["/users/{id}"]["get"].ClientContract, nil
}

// publishedWires names each published transport as protocol/encoding, in
// published order.
func publishedWires(operation *clientcontract.OperationV1) []string {
	wires := make([]string, 0, len(operation.Transports))
	for _, transport := range operation.Transports {
		wires = append(wires, string(transport.Protocol)+"/"+string(transport.Encoding))
	}
	return wires
}

// The bridge serves JSON before protobuf, and a generated client dispatches the
// first declared entry it can carry, so without a declaration the second
// encoding never travels. The operation's own order is what decides it.
func TestOpenAPI_OperationDeclaresItsConnectEncodingOrder(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-connect-encoding-order",
		"a-declared-connect-encoding-order-is-the-published-order")

	connectOnly := []clientcontract.TransportProtocol{clientcontract.TransportConnect}
	cases := []struct {
		name    string
		options api.ClientOperationOptions
		want    []string
	}{
		{
			name: "the bridge order when the operation declares none",
			want: []string{"rest-json/json", "connect/json", "connect/proto"},
		},
		{
			name:    "protobuf declared first keeps REST in front",
			options: api.ClientOperationOptions{ConnectEncodings: []clientcontract.Encoding{clientcontract.EncodingProto, clientcontract.EncodingJSON}},
			want:    []string{"rest-json/json", "connect/proto", "connect/json"},
		},
		{
			name: "protobuf first on a Connect-only operation is the dispatched wire",
			options: api.ClientOperationOptions{
				Transports:       connectOnly,
				ConnectEncodings: []clientcontract.Encoding{clientcontract.EncodingProto, clientcontract.EncodingJSON},
			},
			want: []string{"connect/proto", "connect/json"},
		},
		{
			name: "a narrowed order publishes only the encoding it names",
			options: api.ClientOperationOptions{
				Transports:       []clientcontract.TransportProtocol{clientcontract.TransportConnect, clientcontract.TransportRESTJSON},
				ConnectEncodings: []clientcontract.Encoding{clientcontract.EncodingProto},
			},
			want: []string{"connect/proto", "rest-json/json"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			operation, err := connectEncodingOrderContract(t, testCase.options, true)
			if err != nil {
				t.Fatal(err)
			}
			got := publishedWires(operation)
			if strings.Join(got, ",") != strings.Join(testCase.want, ",") {
				t.Fatalf("published transports = %v, want %v", got, testCase.want)
			}
			for _, transport := range operation.Transports {
				if transport.Protocol == clientcontract.TransportConnect &&
					(transport.Path != "/users.v1.ApiService/GetUsers" || transport.ProtobufMethod != transport.Path) {
					t.Fatalf("connect transport = %#v, want the declared method identity", transport)
				}
			}
		})
	}
}

// An encoding order cannot add a wire: the encodings come from the mounted
// bridge, so naming one it does not serve — or naming one where no Connect
// transport is published at all — is a refusal with the route named.
func TestOpenAPI_RefusesAConnectEncodingTheBridgeDoesNotServe(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-connect-encoding-order",
		"a-connect-encoding-order-cannot-publish-an-encoding-the-bridge-does-not-serve")

	cases := []struct {
		name        string
		options     api.ClientOperationOptions
		mountBridge bool
		want        string
	}{
		{
			name:        "an encoding the bridge does not serve",
			options:     api.ClientOperationOptions{ConnectEncodings: []clientcontract.Encoding{"xml"}},
			mountBridge: true,
			want:        `declares Connect encoding "xml", which the mounted bridge does not serve`,
		},
		{
			name: "the same encoding twice",
			options: api.ClientOperationOptions{ConnectEncodings: []clientcontract.Encoding{
				clientcontract.EncodingJSON, clientcontract.EncodingJSON,
			}},
			mountBridge: true,
			want:        `declares Connect encoding "json" twice in its client encoding order`,
		},
		{
			name:    "an order without a mounted bridge",
			options: api.ClientOperationOptions{ConnectEncodings: []clientcontract.Encoding{clientcontract.EncodingJSON}},
			want:    "declares a Connect encoding order and publishes no Connect transport",
		},
		{
			name: "an order on an operation narrowed away from Connect",
			options: api.ClientOperationOptions{
				Transports:       []clientcontract.TransportProtocol{clientcontract.TransportRESTJSON},
				ConnectEncodings: []clientcontract.Encoding{clientcontract.EncodingProto},
			},
			mountBridge: true,
			want:        "declares a Connect encoding order and publishes no Connect transport",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := connectEncodingOrderContract(t, testCase.options, testCase.mountBridge)
			if err == nil {
				t.Fatal("an unserved Connect encoding order was published")
			}
			if !strings.Contains(err.Error(), testCase.want) || !strings.Contains(err.Error(), "GET /users/{id}") {
				t.Fatalf("refusal = %v, want %q with the route named", err, testCase.want)
			}
		})
	}
}
