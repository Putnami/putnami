package distributioncli

import (
	"net/http"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// PUTNAMI_CLOUD_TOKEN is never the registry bearer of a loopback
// http://<ip>:<port> endpoint. A loopback registry resolves like every other
// host: the machine token is never handed to it, nothing is cached, and
// with no stored credential, no linked workspace and no session the mint fails
// before any request leaves the process.
func TestMintUserRegistryTokenNeverHandsTheMachineTokenToALoopbackRegistry(t *testing.T) {
	const machineToken = "local_broker_capability_0123456789abcdef"
	for _, endpoint := range []RegistryEndpoint{
		{Registry: RegistryNPM, URL: "http://127.0.0.1:19281/npm", Host: "127.0.0.1:19281"},
		{Registry: RegistryOCI, URL: "http://[::1]:19281/oci", Host: "[::1]:19281"},
		{Registry: RegistryPut, URL: "http://127.0.0.1:19281/put", Host: "127.0.0.1:19281"},
	} {
		t.Run(string(endpoint.Registry), func(t *testing.T) {
			env := map[string]string{"PUTNAMI_HOME": t.TempDir(), "PUTNAMI_CLOUD_TOKEN": machineToken}
			requests := 0
			ioctx := clicore.IO{Client: &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
				requests++
				t.Errorf("unexpected request %s %s", request.Method, request.URL)
				return nil, http.ErrUseLastResponse
			})}}

			token, err := mintUserRegistryToken(nil, t.TempDir(), env, ioctx, endpoint)
			if err == nil || token != "" {
				t.Fatalf("loopback registry token = %q, err = %v; want an error and no token", token, err)
			}
			if strings.Contains(err.Error(), machineToken) {
				t.Fatalf("error leaks the machine token: %v", err)
			}
			if requests != 0 {
				t.Fatalf("mint sent %d requests, want 0", requests)
			}
			state, err := readRegistriesState(env)
			if err != nil {
				t.Fatal(err)
			}
			if state != nil && len(state.Access) != 0 {
				t.Fatalf("mint cached a registry credential: %+v", state.Access)
			}
		})
	}
}
