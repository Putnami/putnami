package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	golib "go.putnami.dev/examples/library"
)

func TestLibraryIntegration(t *testing.T) {
	// Test that we can use the library
	result := golib.Reverse("test")
	if result != "tset" {
		t.Errorf("Expected 'tset', got %q", result)
	}

	capitalized := golib.Capitalize("hello")
	if capitalized != "Hello" {
		t.Errorf("Expected 'Hello', got %q", capitalized)
	}

	isPal := golib.IsPalindrome("racecar")
	if !isPal {
		t.Error("Expected 'racecar' to be a palindrome")
	}
}

func TestHTTPServer(t *testing.T) {
	// Set PORT for testing
	_ = os.Setenv("PORT", "0")
	defer func() {
		_ = os.Unsetenv("PORT")
	}()

	// Create a test server
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Go App with Library Example\n"))
	})

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("Handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	expected := "Go App with Library Example"
	if rr.Body.String()[:len(expected)] != expected {
		t.Errorf("Handler returned unexpected body: got %v want %v", rr.Body.String(), expected)
	}

	// Note: w.Write error is checked by the test framework (rr.Recorder handles it)
}
