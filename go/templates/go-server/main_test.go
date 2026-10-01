package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	phttp "go.putnami.dev/http"
)

func TestRootHandler(t *testing.T) {
	handler := func(_ *phttp.Context) *phttp.Response {
		return phttp.JSON(map[string]string{"Hello": "World"})
	}

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	ctx := phttp.NewContext(rr, req)

	resp := handler(ctx)
	if err := resp.WriteTo(rr); err != nil {
		t.Fatalf("failed to write response: %v", err)
	}

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var result map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["Hello"] != "World" {
		t.Errorf("handler returned unexpected body: got %v want %v", result["Hello"], "World")
	}
}
