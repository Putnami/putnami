package compose

import (
	"encoding/json"
	"testing"

	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/jobs"
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
