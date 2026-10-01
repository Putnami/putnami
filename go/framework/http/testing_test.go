package http

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestTestServer_GET(t *testing.T) {
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/hello", func(_ *Context) *Response {
		return JSON(map[string]string{"msg": "hi"})
	})

	ts := server.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["msg"] != "hi" {
		t.Errorf("body = %v, want {msg: hi}", body)
	}
}

func TestTestServer_POST(t *testing.T) {
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.POST("/items", func(_ *Context) *Response {
		return JSONStatus(201, map[string]string{"created": "true"})
	})

	ts := server.TestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/items", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
}

func TestTestServer_NotFound(t *testing.T) {
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/exists", func(_ *Context) *Response {
		return JSON("ok")
	})

	ts := server.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTestServer_HeadFallsBackToGET(t *testing.T) {
	spectest.Proves(t, "go/http-services", "method-fallbacks", "head-falls-back-to-the-get-handler")
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/data", func(_ *Context) *Response {
		return JSON(map[string]string{"ok": "true"})
	})

	ts := server.TestServer()
	defer ts.Close()

	req, _ := http.NewRequest("HEAD", ts.URL+"/data", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("HEAD status = %d, want 200", resp.StatusCode)
	}
}

func TestTestServer_PathParams(t *testing.T) {
	server := NewServerPlugin(ServerConfig{Port: 0})
	server.GET("/users/{id}", func(ctx *Context) *Response {
		return JSON(map[string]string{"id": ctx.Param("id")})
	})

	ts := server.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/users/42")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result["id"] != "42" {
		t.Errorf("id = %q, want 42", result["id"])
	}
}

func TestTestServer_DoesNotWrapMiddlewareTwice(t *testing.T) {
	spectest.Proves(t, "go/http-services", "middleware-order", "middleware-is-composed-once-not-per-request")
	server := NewServerPlugin(ServerConfig{Port: 0})
	var calls int
	server.Use(func(_ *Context, next func() *Response) *Response {
		calls++
		return next()
	})
	server.GET("/ok", func(_ *Context) *Response { return JSON("ok") })

	ts1 := server.TestServer()
	resp1, err := http.Get(ts1.URL + "/ok")
	if err != nil {
		ts1.Close()
		t.Fatal(err)
	}
	resp1.Body.Close() //nolint:errcheck // test cleanup
	ts1.Close()

	ts2 := server.TestServer()
	resp2, err := http.Get(ts2.URL + "/ok")
	if err != nil {
		ts2.Close()
		t.Fatal(err)
	}
	resp2.Body.Close() //nolint:errcheck // test cleanup
	ts2.Close()

	if calls != 2 {
		t.Fatalf("middleware calls = %d, want 2; TestServer must not wrap routes repeatedly", calls)
	}
}
