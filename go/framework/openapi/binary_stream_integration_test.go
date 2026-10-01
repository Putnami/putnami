package openapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func TestTheEmittedGoClientStreamsCallerLabelledOctetsThroughTheRealProvider(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "streamed-binary-payloads", "the-emitted-client-uploads-and-downloads-caller-labeled-octets-through-a-real-provider")
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	provider := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "stream-blobs", Audience: "urn:stream-blobs"},
	}))
	var mu sync.Mutex
	var data []byte
	var mediaType string
	started := make(chan struct{})
	provider.Register(api.Endpoint("POST", "/stream-blob").Body(api.BinaryStream(4<<20)).
		ReturnsStatus(201, "Created", api.Type[blobStored]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			body, err := api.BinaryStreamBody(ctx)
			if err != nil {
				return phttp.InternalError("missing stream")
			}
			first := make([]byte, 4)
			if _, err := io.ReadFull(body, first); err != nil {
				return phttp.InternalError("missing prefix")
			}
			close(started)
			rest, err := io.ReadAll(body)
			if err != nil {
				return phttp.InternalError("upload failed")
			}
			mu.Lock()
			data = make([]byte, len(first)+len(rest))
			copy(data, first)
			copy(data[len(first):], rest)
			mediaType = ctx.Request.Header.Get("Content-Type")
			length := len(data)
			mu.Unlock()
			return phttp.JSONStatus(201, blobStored{Size: int64(length)})
		}))
	provider.Register(api.Endpoint("GET", "/stream-blob").Returns(api.BinaryStream(4 << 20)).
		Handle(func(*phttp.EndpointContext) *phttp.Response {
			mu.Lock()
			defer mu.Unlock()
			return api.BinaryStreamResponse(200, mediaType, bytes.NewReader(data))
		}))
	// This synchronization route proves the generated client sent the prefix
	// before asking its source for the suffix; a buffered upload deadlocks.
	httpServer.GET("/upload-started", func(ctx *phttp.Context) *phttp.Response {
		select {
		case <-started:
			return phttp.NoContent()
		case <-ctx.Context().Done():
			return phttp.InternalError("upload was buffered")
		}
	})
	openapiPlugin := NewPlugin(PluginOptions{Title: "Stream blobs", Version: "1.0.0"}).From(provider)
	clientsPlugin := api.Clients(api.ClientsOptions{
		Go: api.GoClientOptions{PackageName: "blobsclient", ClientName: "BlobsClient"},
	}).From(provider)
	application := app.New("stream-blobs")
	application.Use(provider).Use(openapiPlugin).Use(clientsPlugin)
	out := t.TempDir()
	if err := application.Describe(out, []string{"openapi", "clients"}); err != nil {
		t.Fatal(err)
	}
	document, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Paths map[string]map[string]struct {
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(document, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Paths["/stream-blob"]["post"].Responses["413"] == nil {
		t.Fatal("streamed upload omitted the OpenAPI 413 response")
	}
	ir, err := api.ReadOpenAPISpec(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range ir.Services {
		for _, method := range service.Methods {
			if len(method.Client.Transports) != 1 || method.Client.Transports[0].Protocol != clientcontract.TransportRESTJSON {
				t.Fatalf("streamed octets advertised a framed transport: %#v", method.Client.Transports)
			}
			found413 := false
			for _, declared := range method.Client.Errors {
				found413 = found413 || declared.Status == 413
			}
			if method.Request != nil && !found413 {
				t.Fatal("bounded stream omitted its 413 contract refusal")
			}
		}
	}
	source, err := os.ReadFile(filepath.Join(out, api.ClientStageDir, "go", "client.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpServer.Handler())
	defer server.Close()
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, string(source))
	if err := os.WriteFile(filepath.Join(moduleDir, "consumer_test.go"), []byte(streamedBlobConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "STREAM_BLOB_PROVIDER_URL="+server.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("emitted streamed client failed: %v\n%s\n%s", err, output, source)
	}
}

const streamedBlobConsumer = `package blobsclient

import (
  "bytes"
  "context"
  "io"
  "net/http"
  "os"
  "testing"
  "time"
  "go.putnami.dev/client"
)

type incrementalSource struct { prefix bool; ctx context.Context; url string; suffix io.Reader }
func (s *incrementalSource) Read(p []byte) (int, error) {
  if !s.prefix { s.prefix = true; return copy(p, []byte{0,255,128,10}), nil }
  if s.url != "" {
    request, err := http.NewRequestWithContext(s.ctx, "GET", s.url+"/upload-started", nil)
    if err != nil { return 0, err }
    response, err := http.DefaultClient.Do(request)
    if err != nil { return 0, err }
    response.Body.Close()
    s.url = ""
  }
  return s.suffix.Read(p)
}

func TestGeneratedStreamedUploadAndDownload(t *testing.T) {
  ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
  defer cancel()
  url := os.Getenv("STREAM_BLOB_PROVIDER_URL")
  transport, err := client.NewServiceClientBinding(client.ServiceBinding{URL:url, ClientID:"consumer", AllowInsecure:true}, serviceDescriptor)
  if err != nil { t.Fatal(err) }
  blobs := NewBlobsClient(transport)
  const label = "application/vnd.putnami.migration-bundle.v1.tar; version=1"
  suffix := bytes.Repeat([]byte{0xff, 0, 0x80, 0xfe}, 2048)
  reader := &incrementalSource{ctx:ctx, url:url, suffix:bytes.NewReader(suffix)}
  stored, err := blobs.CreateStreamBlob(ctx, CreateStreamBlobInput{ContentType:label, Body:reader})
  if err != nil { t.Fatal(err) }
  if stored.Size != int64(4+len(suffix)) { t.Fatalf("stored size = %d", stored.Size) }
  result, err := blobs.ListStreamBlob(ctx, ListStreamBlobInput{})
  if err != nil { t.Fatal(err) }
  defer result.Body.Close()
  data, err := io.ReadAll(result.Body)
  if err != nil { t.Fatal(err) }
  if result.Status != 200 || result.ContentType != label || !bytes.Equal(data, append([]byte{0,255,128,10}, suffix...)) {
    t.Fatalf("download status=%d type=%q bytes=%d", result.Status, result.ContentType, len(data))
  }
}
`
