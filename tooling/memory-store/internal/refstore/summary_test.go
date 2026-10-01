package refstore

import (
	"strings"
	"testing"
)

func TestAFailureMessageCarriesNoURLQueryOrFragment(t *testing.T) {
	stderr := []byte("fatal: unable to access 'https://host.example/acme/memory.git?token=s3cret&x=1': The requested URL returned error: 403\n")
	message := describe([]string{"push", "origin"}, Result{stderr: stderr, code: 128})
	if strings.Contains(message, "s3cret") || strings.Contains(message, "token=") ||
		!strings.Contains(message, "'https://host.example/acme/memory.git': The requested URL returned error: 403") {
		t.Fatalf("message %q", message)
	}
	fragment := summary([]byte("fatal: repository 'https://user:pw@host.example/acme/memory.git#key=s3cret' not found"))
	if strings.Contains(fragment, "s3cret") || strings.Contains(fragment, "pw@") || !strings.Contains(fragment, "https://host.example/acme/memory.git'") {
		t.Fatalf("summary %q", fragment)
	}
}
