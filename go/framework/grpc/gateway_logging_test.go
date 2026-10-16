package grpc

import (
	"encoding/json"
	"net/http"
	"testing"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger/logtest"
)

// A gateway-mounted Connect handler writes its answer to the response writer,
// and the request log records the status it wrote at the matching level.
func TestGatewayRequestLogRecordsTheStatusTheHandlerWrote(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		severity string
		outcome  string
	}{
		{name: "server error", status: http.StatusInternalServerError, severity: "ERROR", outcome: "failure"},
		{name: "success", status: http.StatusOK, severity: "INFO", outcome: "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := logtest.NewRecorder(t)
			gw := NewGatewayPlugin(GatewayConfig{})
			gw.MountHandler("/test.v1.Service/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{}`))
			}))
			server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
			server.Use(phttp.Logging(phttp.LoggerOptions{Logger: rec.Named("http")}))
			gw.RegisterOn(server)
			ts := server.TestServer()
			defer ts.Close()

			resp, err := http.Post(ts.URL+"/test.v1.Service/Call", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("wire status = %d, want %d", resp.StatusCode, tc.status)
			}

			var requestLines []map[string]any
			for _, line := range rec.Lines() {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if _, ok := record["http"]; ok {
					requestLines = append(requestLines, record)
				}
			}
			if len(requestLines) != 1 {
				t.Fatalf("want 1 request record, got %d.%s", len(requestLines), rec.Dump())
			}
			record := requestLines[0]
			if record["severity"] != tc.severity {
				t.Errorf("severity = %v, want %s", record["severity"], tc.severity)
			}
			group, _ := record["http"].(map[string]any)
			if group["status"] != float64(tc.status) {
				t.Errorf("logged status = %v, want %d", group["status"], tc.status)
			}
			if group["outcome"] != tc.outcome {
				t.Errorf("outcome = %v, want %s", group["outcome"], tc.outcome)
			}
		})
	}
}
