package http

import (
	"context"
	"crypto/tls"
	"encoding/json"
	stderrors "errors"
	"io"
	"net"
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

// failingBodyWriter is a ResponseWriter whose body writes fail with err.
// onFailure runs as each write fails.
type failingBodyWriter struct {
	*httptest.ResponseRecorder
	err       error
	onFailure func()
}

func (w failingBodyWriter) Write([]byte) (int, error) {
	if w.onFailure != nil {
		w.onFailure()
	}
	return 0, w.err
}

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
	spectest.Proves(t, "go/http-services", "failed-writes", "a-write-to-a-caller-that-left-logs-below-error")
	for name, tc := range map[string]struct {
		major int
		// dropConnection closes the client's connection instead of the
		// response body, which resets only the HTTP/2 stream.
		dropConnection bool
	}{
		"HTTP/1.1":                 {major: 1},
		"HTTP/2 stream reset":      {major: 2},
		"HTTP/2 connection closed": {major: 2, dropConnection: true},
	} {
		t.Run(name, func(t *testing.T) {
			rec := logtest.NewRecorder(t)
			server := NewServerPlugin(ServerConfig{})
			server.log = rec.Named("http")
			server.GET("/download", func(*Context) *Response {
				return Stream(http.StatusOK, "application/octet-stream", endlessBody{})
			})

			handler, served := servedHandler(server.Handler())
			ts := httptest.NewUnstartedServer(handler)
			if tc.major == 2 {
				ts.EnableHTTP2 = true
				ts.StartTLS()
			} else {
				ts.Start()
			}
			defer ts.Close()

			client := ts.Client()
			conns := make(chan net.Conn, 1)
			if tc.dropConnection {
				transport := client.Transport.(*http.Transport).Clone()
				config := transport.TLSClientConfig.Clone()
				config.NextProtos = []string{"h2"}
				transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					conn, err := (&tls.Dialer{Config: config}).DialContext(ctx, network, addr)
					if err == nil {
						conns <- conn
					}
					return conn, err
				}
				client = &http.Client{Transport: transport}
			}

			resp, err := client.Get(ts.URL + "/download")
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.CopyN(io.Discard, resp.Body, 64<<10)
			if tc.dropConnection {
				(<-conns).Close()
			}
			resp.Body.Close() // the client leaves mid-body
			if readErr != nil {
				t.Fatal(readErr)
			}
			if resp.ProtoMajor != tc.major {
				t.Fatalf("protocol = %s, want HTTP/%d", resp.Proto, tc.major)
			}

			deadline := time.Now().Add(10 * time.Second)
			for len(writeResponseRecords(t, rec)) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("the server logged no failed write.%s", rec.Dump())
				}
				time.Sleep(10 * time.Millisecond)
			}
			waitServed(t, served, 1) // the handler wrote every record
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
	spectest.Proves(t, "go/http-services", "failed-writes", "a-failed-write-with-a-live-request-stays-at-error")
	writeErr := stderrors.New("write: no space left on device")
	// leave ends the request context: before the request, as the write fails,
	// or shortly after it failed, as HTTP/2 does when the connection closes.
	type leave int
	const (
		stays leave = iota
		before
		duringWrite
		afterWrite
	)
	for name, tc := range map[string]struct {
		path     string
		major    int
		leave    leave
		severity string
	}{
		"live request":                     {path: "/orders", major: 1, severity: "ERROR"},
		"live HTTP/2 request":              {path: "/orders", major: 2, severity: "ERROR"},
		"caller leaves during the write":   {path: "/orders", major: 1, leave: duringWrite, severity: "INFO"},
		"HTTP/2 caller left before":        {path: "/orders", major: 2, leave: before, severity: "INFO"},
		"HTTP/2 stream ends after failure": {path: "/orders", major: 2, leave: afterWrite, severity: "INFO"},
		"not found, live":                  {path: "/missing", major: 1, severity: "ERROR"},
		"not found, caller left":           {path: "/missing", major: 1, leave: before, severity: "INFO"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := logtest.NewRecorder(t)
			server := NewServerPlugin(ServerConfig{})
			server.log = rec.Named("http")
			server.GET("/orders", func(*Context) *Response {
				return Text("ok")
			})

			reqCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := failingBodyWriter{ResponseRecorder: httptest.NewRecorder(), err: writeErr}
			switch tc.leave {
			case before:
				cancel()
			case duringWrite:
				w.onFailure = cancel
			case afterWrite:
				w.onFailure = func() { time.AfterFunc(5*time.Millisecond, cancel) }
			}
			req := httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(reqCtx)
			req.ProtoMajor = tc.major
			server.Handler().ServeHTTP(w, req)

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
