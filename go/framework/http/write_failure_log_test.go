package http

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.putnami.dev/logger/logtest"
	"go.putnami.dev/protocol/features/spectest"
)

// endlessBody is a download that never ends, so the server is still writing
// when the client leaves.
type endlessBody struct{}

func (endlessBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// failingBodyWriter is a ResponseWriter whose body writes fail.
type failingBodyWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w failingBodyWriter) Write([]byte) (int, error) { return 0, w.err }

// writeResponseRecords returns the "write response" records the server logged,
// decoded.
func writeResponseRecords(t *testing.T, rec *logtest.Recorder) []map[string]any {
	t.Helper()
	lines := rec.Records("http", "write response")
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

// assertNoErrorRecord fails when the recorder holds a record at ERROR.
func assertNoErrorRecord(t *testing.T, rec *logtest.Recorder) {
	t.Helper()
	for _, line := range rec.Lines() {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["severity"] == "ERROR" {
			t.Errorf("unexpected ERROR record: %s", line)
		}
	}
}

func TestClientResettingDownloadMidBodyLogsNoError(t *testing.T) {
	spectest.Proves(t, "go/http-services", "caller-withdrawal", "a-write-to-a-caller-that-left-logs-below-error")
	for protocol, major := range map[string]int{"HTTP/1.1": 1, "HTTP/2": 2} {
		t.Run(protocol, func(t *testing.T) {
			rec := logtest.NewRecorder(t)
			server := NewServerPlugin(ServerConfig{})
			server.log = rec.Named("http")
			server.GET("/download", func(*Context) *Response {
				return Stream(http.StatusOK, "application/octet-stream", endlessBody{})
			})

			ts := httptest.NewUnstartedServer(server.Handler())
			if major == 2 {
				ts.EnableHTTP2 = true
				ts.StartTLS()
			} else {
				ts.Start()
			}
			defer ts.Close()

			resp, err := ts.Client().Get(ts.URL + "/download")
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.CopyN(io.Discard, resp.Body, 64<<10)
			resp.Body.Close() // the client resets the download mid-body
			if readErr != nil {
				t.Fatal(readErr)
			}
			if resp.ProtoMajor != major {
				t.Fatalf("protocol = %s, want %s", resp.Proto, protocol)
			}

			deadline := time.Now().Add(10 * time.Second)
			for len(writeResponseRecords(t, rec)) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("the server logged no failed write.%s", rec.Dump())
				}
				time.Sleep(10 * time.Millisecond)
			}
			for _, record := range writeResponseRecords(t, rec) {
				if record["severity"] != "INFO" {
					t.Errorf("severity = %v, want INFO", record["severity"])
				}
			}
			assertNoErrorRecord(t, rec)
		})
	}
}

func TestFailedWriteClassifiedByRequestContext(t *testing.T) {
	spectest.Proves(t, "go/http-services", "caller-withdrawal", "a-failed-write-with-a-live-request-stays-at-error")
	writeErr := stderrors.New("write: no space left on device")
	for name, tc := range map[string]struct {
		path     string
		leave    bool
		severity string
	}{
		"live request":           {path: "/orders", severity: "ERROR"},
		"caller left":            {path: "/orders", leave: true, severity: "INFO"},
		"not found, live":        {path: "/missing", severity: "ERROR"},
		"not found, caller left": {path: "/missing", leave: true, severity: "INFO"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := logtest.NewRecorder(t)
			server := NewServerPlugin(ServerConfig{})
			server.log = rec.Named("http")
			server.GET("/orders", func(*Context) *Response {
				return Text("ok")
			})

			reqCtx, leave := context.WithCancel(context.Background())
			defer leave()
			if tc.leave {
				leave()
			}
			w := failingBodyWriter{ResponseRecorder: httptest.NewRecorder(), err: writeErr}
			server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(reqCtx))

			records := writeResponseRecords(t, rec)
			if len(records) != 1 {
				t.Fatalf("want 1 write response record, got %d.%s", len(records), rec.Dump())
			}
			if records[0]["severity"] != tc.severity {
				t.Errorf("severity = %v, want %s", records[0]["severity"], tc.severity)
			}
			if logged, _ := records[0]["error"].(map[string]any); logged["message"] != writeErr.Error() {
				t.Errorf("error = %v, want the write error %q", records[0]["error"], writeErr)
			}
		})
	}
}
