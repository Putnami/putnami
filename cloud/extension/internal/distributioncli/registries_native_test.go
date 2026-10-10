package distributioncli

import (
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestNativeRegistryBrokerCapabilityStaysLocalAndUncached(t *testing.T) {
	const capability = "local_broker_capability_0123456789abcdef"
	for _, raw := range []string{"http://127.0.0.1:19281/npm", "http://[::1]:19281/oci"} {
		token, err := mintUserRegistryToken(nil, "", map[string]string{"PUTNAMI_CLOUD_TOKEN": capability}, clicore.IO{}, RegistryEndpoint{Registry: RegistryOCI, URL: raw})
		if err != nil || token != capability {
			t.Fatalf("local broker token=%q err=%v", token, err)
		}
	}
	if _, err := mintUserRegistryToken(nil, "", map[string]string{"PUTNAMI_CLOUD_TOKEN": "bad token"}, clicore.IO{}, RegistryEndpoint{URL: "http://127.0.0.1:19281/put"}); err == nil {
		t.Fatal("malformed local capability accepted")
	}
	for _, raw := range []string{"https://oci.putnami.dev", "http://localhost:19281", "http://127.0.0.1", "http://127.0.0.1:19281?x=1", "http://user@127.0.0.1:19281", "http://192.0.2.1:19281", "https://127.0.0.1:19281"} {
		if privateRegistryBroker(raw) {
			t.Fatalf("non-private endpoint accepted: %s", raw)
		}
	}
}
