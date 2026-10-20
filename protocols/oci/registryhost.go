package oci

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Registry host rules. They reproduce how go-containerregistry's name package
// reads a repository and a registry under strict validation
// (name.NewRepository and name.NewRegistry with name.StrictValidation, then
// RegistryStr and Scheme), so a component that does not import that library
// derives the same registry host and scheme as one that does. One rule
// differs: the host is lowercased first, so hosts that differ only in case
// name one registry.
const (
	// dockerHubAlias is the name Docker Hub is written as in a repository.
	dockerHubAlias = "docker.io"
	// dockerHubRegistry is the registry host Docker Hub is served from.
	dockerHubRegistry = "index.docker.io"
	// repositoryPathCharacters are the characters of a repository path.
	repositoryPathCharacters = "abcdefghijklmnopqrstuvwxyz0123456789_-./"
	// minRepositoryPathBytes and maxRepositoryPathBytes bound a repository
	// path.
	minRepositoryPathBytes = 2
	maxRepositoryPathBytes = 255
)

// localRegistry matches a host under .localhost, with an optional port.
var localRegistry = regexp.MustCompile(`.*\.localhost(?::\d{1,5})?$`)

// privateNetworks are the RFC 1918 address blocks.
var privateNetworks = func() []*net.IPNet {
	blocks := make([]*net.IPNet, 0, 3)
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		_, block, _ := net.ParseCIDR(cidr)
		blocks = append(blocks, block)
	}
	return blocks
}()

// RegistryHost returns the registry host of a repository: the part before the
// first '/', lowercased by NormalizeRegistryHost, with docker.io read as
// index.docker.io. The repository must name its registry explicitly: the
// part before the first '/' holds a '.' or a ':'. The path after it is 2 to
// 255 characters from [a-z0-9_-./], and a Docker Hub path names its
// namespace (library/nginx, not nginx). No error repeats the repository,
// which may carry a credential.
func RegistryHost(repository string) (string, error) {
	host, path, found := strings.Cut(repository, "/")
	if !found || !strings.ContainsAny(host, ".:") {
		return "", errors.New("oci: a repository must start with its registry host, a name that holds a '.' or a ':', followed by '/'")
	}
	if !validRepositoryPath(path) {
		return "", errors.New("oci: a repository path must be 2 to 255 characters from [a-z0-9_-./]")
	}
	registry, err := NormalizeRegistryHost(host)
	if err != nil {
		return "", err
	}
	if registry == dockerHubRegistry && !strings.Contains(path, "/") {
		return "", errors.New("oci: a Docker Hub repository must name its namespace, such as library/")
	}
	return registry, nil
}

// NormalizeRegistryHost returns a registry host in the form RegistryHost
// returns it: lowercased, with docker.io read as index.docker.io. The host is
// a URI authority (RFC 3986) without userinfo: a name or an IP address, and
// an optional port. No error repeats the host, which may carry a credential.
func NormalizeRegistryHost(host string) (string, error) {
	host = lowerASCII(host)
	if host == "" {
		return "", errors.New("oci: a registry host is required")
	}
	if parsed, err := url.Parse("//" + host); err != nil || parsed.Host != host {
		return "", errors.New("oci: a registry host must be a URI authority without userinfo, path, query or fragment")
	}
	if host == dockerHubAlias {
		return dockerHubRegistry, nil
	}
	return host, nil
}

// RegistryScheme returns the scheme a registry host is reached with: http
// for a host on a loopback or private network, https for any other. After
// the host is lowercased, http applies to a host that:
//   - is an RFC 1918 IPv4 address (10/8, 172.16/12, 192.168/16), with or
//     without a port;
//   - starts with "localhost:";
//   - ends with ".localhost", with or without a port;
//   - holds "127.0.0.1" or "::1".
//
// These are go-containerregistry's rules, kept as they are: "localhost"
// without a port and "127.0.0.2" are reached with https.
func RegistryScheme(host string) string {
	host = lowerASCII(host)
	switch {
	case privateAddress(host),
		strings.HasPrefix(host, "localhost:"),
		localRegistry.MatchString(host),
		strings.Contains(host, "127.0.0.1"),
		strings.Contains(host, "::1"):
		return "http"
	}
	return "https"
}

// validRepositoryPath reports whether path is 2 to 255 bytes, each one of
// repositoryPathCharacters. Every such character is one byte, so the bound
// in bytes is the bound in characters.
func validRepositoryPath(path string) bool {
	if len(path) < minRepositoryPathBytes || len(path) > maxRepositoryPathBytes {
		return false
	}
	for index := 0; index < len(path); index++ {
		if !strings.ContainsRune(repositoryPathCharacters, rune(path[index])) {
			return false
		}
	}
	return true
}

// privateAddress reports whether the host, up to its first ':', is an IP
// address in an RFC 1918 block.
func privateAddress(host string) bool {
	address, _, _ := strings.Cut(host, ":")
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	for _, block := range privateNetworks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// lowerASCII lowercases the ASCII letters of value and leaves every other
// byte as it is, invalid UTF-8 included.
func lowerASCII(value string) string {
	lowered := []byte(value)
	for index, char := range lowered {
		if char >= 'A' && char <= 'Z' {
			lowered[index] = char + ('a' - 'A')
		}
	}
	return string(lowered)
}
