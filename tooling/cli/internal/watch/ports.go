package watch

import (
	"fmt"
	"net"
	"strconv"
)

// DefaultPort is the default port for serve mode.
const DefaultPort = 3000

// IsPortAvailable checks if a TCP port is available for binding.
func IsPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// ParsePort parses a port string, returning the default if empty or invalid.
func ParsePort(s string, defaultPort int) int {
	if s == "" {
		return defaultPort
	}
	port, err := strconv.Atoi(s)
	if err != nil || port < 1 || port > 65535 {
		return defaultPort
	}
	return port
}
