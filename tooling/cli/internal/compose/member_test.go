package compose

import (
	"encoding/json"
	"slices"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

func TestReadyClaim_ThePortIsTheFirstHTTPEndpoint(t *testing.T) {
	endpoint := func(scheme string, port int) runtimeproto.ReadyEndpoint {
		return runtimeproto.ReadyEndpoint{Scheme: scheme, Host: "localhost", Port: port}
	}
	cases := map[string]struct {
		endpoints []runtimeproto.ReadyEndpoint
		want      int
	}{
		"no endpoint":        {nil, 0},
		"grpc sorts first":   {[]runtimeproto.ReadyEndpoint{endpoint(runtimeproto.ReadySchemeGRPC, 9090), endpoint(runtimeproto.ReadySchemeHTTP, 8080)}, 8080},
		"https":              {[]runtimeproto.ReadyEndpoint{endpoint(runtimeproto.ReadySchemeHTTPS, 8443)}, 8443},
		"no http endpoint":   {[]runtimeproto.ReadyEndpoint{endpoint(runtimeproto.ReadySchemeTCP, 7000)}, 7000},
		"first http of many": {[]runtimeproto.ReadyEndpoint{endpoint(runtimeproto.ReadySchemeHTTP, 8080), endpoint(runtimeproto.ReadySchemeHTTP, 8081)}, 8080},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(runtimeproto.ReadyData{Target: runtimeproto.ReadyTargetWorkload, Endpoints: tc.endpoints})
			if err != nil {
				t.Fatal(err)
			}
			event := jobs.RawJobEvent{Version: runtimeproto.ProtocolVersion2, Type: jobs.EventTypeReady}
			if err := json.Unmarshal(raw, &event.Data); err != nil {
				t.Fatal(err)
			}
			target, port := readyClaim(event)
			if target != runtimeproto.ReadyTargetWorkload || port != tc.want {
				t.Errorf("readyClaim = (%q, %d), want (%q, %d)", target, port, runtimeproto.ReadyTargetWorkload, tc.want)
			}
		})
	}
}

func TestMemberEnv_ProductionKeepsTheLocalConfigEnvironment(t *testing.T) {
	t.Setenv(envAppEnv, "")
	if env := memberEnv(true, nil); !slices.Equal(env, []string{envPort, envProductionNodeEnv, envLocalAppEnv}) {
		t.Errorf("production member env = %v, want production mode with APP_ENV=local", env)
	}
	// A watched member is not production: its serve step picks development, and
	// a TypeScript runtime then reads the local files without help.
	if env := memberEnv(false, nil); !slices.Equal(env, []string{envPort}) {
		t.Errorf("watched member env = %v, want PORT only", env)
	}
	// memberEnv leaves an APP_ENV the caller exported to the inherited
	// environment, as putnami serve does.
	t.Setenv(envAppEnv, "staging")
	if env := memberEnv(true, nil); slices.Contains(env, envLocalAppEnv) {
		t.Errorf("production member env = %v, overrode the exported APP_ENV", env)
	}
}
