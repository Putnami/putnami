// Package grpcservices defines putnami.grpc-services.v1, the language-neutral
// inventory of the gRPC and Connect services an application serves.
//
// A framework writes the inventory at build time from the services it actually
// registered, so a consumer can read which services a committed tree serves
// without running the workload or parsing its source. The inventory is
// independent of the HTTP route inventory: a Connect service joins a route of
// putnami.http-routes.v1 by its basePath, and a native gRPC service has no HTTP
// route at all.
package grpcservices

const (
	// Protocol identifies the v1 wire contract. A consumer must fail closed on
	// any other identifier.
	Protocol = "putnami.grpc-services.v1"

	// SchemaURL is the canonical JSON Schema reference stamped on emitted
	// inventories.
	SchemaURL = "https://putnami.dev/schemas/putnami-grpc-services-v1.json"
)

// Transport names how a service is reached.
type Transport string

const (
	// TransportGRPC is a native gRPC server over HTTP/2. Its service has no
	// HTTP path in this inventory, so basePath is forbidden.
	TransportGRPC Transport = "grpc"
	// TransportConnect is a Connect service mounted on an HTTP server under
	// basePath.
	TransportConnect Transport = "connect"
)

// SourceKind classifies the framework surface that registered a service. It
// describes provenance and fixes the transport: grpc-server is always grpc,
// connect-gateway and api-bridge are always connect.
type SourceKind string

// SourceKind values identify the registering framework surface.
const (
	// SourceGRPCServer is a service registered on a native gRPC server.
	SourceGRPCServer SourceKind = "grpc-server"
	// SourceConnectGateway is an opaque Connect handler mounted under one HTTP
	// prefix route equal to basePath.
	SourceConnectGateway SourceKind = "connect-gateway"
	// SourceAPIBridge is a Connect service whose every listed method is one
	// exact POST route at basePath followed by the method name.
	SourceAPIBridge SourceKind = "api-bridge"
)

// Method is one RPC of a service.
type Method struct {
	// Name is the RPC name as the service registers it, an identifier matching
	// [A-Za-z_][A-Za-z0-9_]*.
	Name string `json:"name"`
	// ClientStreaming reports that the caller sends a stream of messages.
	ClientStreaming bool `json:"clientStreaming"`
	// ServerStreaming reports that the service answers with a stream of
	// messages.
	ServerStreaming bool `json:"serverStreaming"`
}

// Provenance points back to the framework surface that registered a service.
type Provenance struct {
	// Project is the application that serves the service: the name of its root
	// module, the value the HTTP route inventory carries for the same
	// application.
	Project string `json:"project"`
	// Package is the framework package that registered the service.
	Package string `json:"package"`
	// SourceKind classifies the registering surface.
	SourceKind SourceKind `json:"sourceKind"`
}

// Service is one served service.
type Service struct {
	// Name is the fully qualified service name: one or more identifiers
	// matching [A-Za-z_][A-Za-z0-9_]* joined by '.'. A service declared in a
	// protobuf file without a package has a single-segment name.
	Name string `json:"name"`
	// Transport is how the service is reached.
	Transport Transport `json:"transport"`
	// BasePath is the Connect URL prefix of the service: a '/'-terminated
	// absolute path that ends in "/<name>/". It is required for connect and
	// forbidden for grpc.
	BasePath string `json:"basePath,omitempty"`
	// MethodsKnown reports whether Methods lists every RPC the service serves.
	// When false the framework cannot observe the methods and Methods is empty.
	MethodsKnown bool `json:"methodsKnown"`
	// Methods lists the served RPCs, sorted by name. It is always present and
	// empty when MethodsKnown is false.
	Methods []Method `json:"methods"`
	// Provenance points back to the registering framework surface.
	Provenance Provenance `json:"provenance"`
}

// Manifest is the canonical putnami.grpc-services.v1 artifact. Field order is
// normative: serializers emit this order with two-space indentation and a
// trailing newline.
type Manifest struct {
	// Schema is the canonical JSON Schema reference (SchemaURL).
	Schema string `json:"$schema"`
	// Protocol is the v1 wire contract identifier (Protocol).
	Protocol string `json:"protocol"`
	// Services is the complete inventory of the application, sorted by name,
	// transport and basePath. An empty list states that the application
	// composes a gRPC surface that serves no service.
	Services []Service `json:"services"`
	// Digest is "sha256:" followed by the lowercase SHA-256 hex of the
	// canonical {protocol, services} projection.
	Digest string `json:"digest"`
}
