package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/config"
	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
)

func TestServicesPluginLoadsRealTypedConfig(t *testing.T) {
	t.Setenv("CONFIG_DATA", `{"clients":{"clientId":"consumer.workload","services":{"inventory":{"url":"https://inventory.example","headers":{"X-Putnami-Observed-Revision":"revision-one"},"credentials":{"key":{"source":"static","value":"super-secret"},"service":{"source":"gcp-id-token","audience":"https://inventory-abc.a.run.app"}}}}}}`)
	plugin := Services()
	container := inject.NewContainer("test", nil)
	for _, registration := range plugin.Provides() {
		if err := container.Register(registration); err != nil {
			t.Fatal(err)
		}
	}
	value, err := container.Get(inject.TokenOf[*ServiceBindings]())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := value.(*ServiceBindings).For("inventory")
	if err != nil {
		t.Fatal(err)
	}
	if binding.URL != "https://inventory.example" || binding.ClientID != "consumer.workload" || binding.Credentials["key"].Value != "super-secret" {
		t.Fatalf("loaded binding = %#v", binding)
	}
	if binding.Headers["X-Putnami-Observed-Revision"] != "revision-one" {
		t.Fatalf("loaded static headers = %#v", binding.Headers)
	}
	if service := binding.Credentials["service"]; service.Source != CredentialSourceGCPIDToken || service.Audience != "https://inventory-abc.a.run.app" {
		t.Fatalf("loaded gcp-id-token binding = %#v", service)
	}
	if got := plugin.ConfigDefinitions(); len(got) != 1 || got[0].Path != "clients" {
		t.Fatalf("config definitions = %#v", got)
	}
}

// A binding's URL, credential source and audience are deployment values, not
// secrets: the services map must not be sensitive as a whole, or every consumer
// of a generated client gets required secrets and an unpublished binding.
// Only the credential secret material is sensitive.
func TestServicesConfigMarksOnlyCredentialSecretMaterialSensitive(t *testing.T) {
	if got := config.SensitiveFields(ServicesConfigDefinition()); len(got) != 0 {
		t.Fatalf("clients block sensitive fields = %v, want none at block level", got)
	}
	if got := config.SensitiveFields(config.Config[ServiceBinding]("binding")); len(got) != 0 {
		t.Fatalf("service binding sensitive fields = %v, want none", got)
	}
	got := config.SensitiveFields(config.Config[CredentialBinding]("credential"))
	want := []string{"clientSecret", "parameters", "assertion", "value"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("credential binding sensitive fields = %v, want %v", got, want)
	}
}

func TestServicesProgrammaticOverrideIsDefensivelyCopied(t *testing.T) {
	options := ServicesOptions{
		ClientID: "consumer",
		Services: map[string]ServiceBinding{
			"svc": {URL: "http://127.0.0.1:8080", Headers: map[string]string{"X-Revision": "original"}, Credentials: map[string]CredentialBinding{"key": {Source: CredentialSourceStatic, Value: "first"}}},
		},
	}
	plugin := Services(options)
	options.Services["svc"].Headers["X-Revision"] = "changed"
	options.Services["svc"] = ServiceBinding{URL: "https://mutated.invalid"}
	container := inject.NewContainer("test", nil)
	for _, registration := range plugin.Provides() {
		if err := container.Register(registration); err != nil {
			t.Fatal(err)
		}
	}
	value, err := container.Get(inject.TokenOf[*ServiceBindings]())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := value.(*ServiceBindings).For("svc")
	if err != nil {
		t.Fatal(err)
	}
	if binding.URL != "http://127.0.0.1:8080" || binding.Credentials["key"].Value != "first" {
		t.Fatalf("binding changed through caller alias: %#v", binding)
	}
	if binding.Headers["X-Revision"] != "original" {
		t.Fatal("Services retained a mutable header map alias")
	}
	binding.Credentials["key"] = CredentialBinding{Value: "second"}
	again, _ := value.(*ServiceBindings).For("svc")
	if again.Credentials["key"].Value != "first" {
		t.Fatal("For returned a mutable credential map alias")
	}
}

func TestServiceBindingURLValidationFailsClosed(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "typed-service-binding", "a-binding-url-that-is-not-a-safe-absolute-endpoint-fails-closed")
	tests := []struct {
		url      string
		insecure bool
		valid    bool
	}{
		{"https://service.example/base", false, true},
		{"http://localhost:8080", false, true},
		{"http://127.0.0.1:8080", false, true},
		{"http://service.example", false, false},
		{"http://service.example", true, true},
		{"https://user:pass@service.example", false, false},
		{"https://service.example?token=secret", false, false},
		{"https://service.example/#fragment", false, false},
		{"//service.example", false, false},
		{"mailto:service@example.com", false, false},
	}
	for _, test := range tests {
		t.Run(test.url, func(t *testing.T) {
			_, err := parseBoundURL(test.url, test.insecure)
			if (err == nil) != test.valid {
				t.Fatalf("parseBoundURL(%q) error = %v, valid=%v", test.url, err, test.valid)
			}
		})
	}
	if _, err := (&ServiceBindings{}).For("missing"); err == nil || !strings.Contains(err.Error(), "no binding") {
		t.Fatalf("missing binding error = %v", err)
	}
	if _, err := (*ServiceBindings)(nil).For("missing"); err == nil {
		t.Fatal("nil registry must fail")
	}
}

func TestSafeCredentialHTTPClientNeverFollowsRedirects(t *testing.T) {
	base := &http.Client{Timeout: time.Second}
	client := safeCredentialHTTPClient(base)
	if client == base || client.Timeout != time.Second {
		t.Fatal("credential client must clone caller transport settings")
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://other.example", nil)
	if err := client.CheckRedirect(request, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect decision = %v", err)
	}
}
