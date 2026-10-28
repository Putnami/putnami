package useragent

import (
	"net/http"
	"testing"
)

func TestStringUsesSanitizedVersion(t *testing.T) {
	SetVersion("v1.2.3 beta")
	t.Cleanup(func() { SetVersion("dev") })

	if got, want := String(), "putnami-cli/v1.2.3-beta"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestSetDoesNotOverwriteExistingUserAgent(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "custom/1.0")

	Set(req)

	if got := req.Header.Get("User-Agent"); got != "custom/1.0" {
		t.Fatalf("User-Agent = %q, want custom/1.0", got)
	}
}
