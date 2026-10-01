package watch

import (
	"net"
	"testing"
)

func TestIsPortAvailable(t *testing.T) {
	// Bind a port to make it unavailable
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if IsPortAvailable(port) {
		t.Errorf("port %d should not be available", port)
	}
}

func TestParsePort(t *testing.T) {
	tests := []struct {
		input string
		def   int
		want  int
	}{
		{"", 3000, 3000},
		{"8080", 3000, 8080},
		{"invalid", 3000, 3000},
		{"-1", 3000, 3000},
		{"70000", 3000, 3000},
		{"443", 8080, 443},
	}

	for _, tt := range tests {
		got := ParsePort(tt.input, tt.def)
		if got != tt.want {
			t.Errorf("ParsePort(%q, %d) = %d, want %d", tt.input, tt.def, got, tt.want)
		}
	}
}
