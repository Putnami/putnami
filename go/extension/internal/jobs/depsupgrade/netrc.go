package depsupgrade

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.putnami.dev/go/extension/internal/workspacejob"
)

// A channel query to an https origin carries the credential the go command
// would send there: the netrc entry that names the longest prefix of the URL,
// when GOAUTH includes netrc.

// netrcEntry is one complete `machine` entry of a netrc file.
type netrcEntry struct {
	machine, login, password string
}

// netrcCredentials is what the job's netrc file holds.
type netrcCredentials struct {
	// enabled reports whether GOAUTH lets the go command read the netrc file.
	enabled bool
	// path is the netrc file the go command reads, or "" when no home names one.
	path string
	// unreadable is why path could not be read, or "" when it was read or does
	// not exist.
	unreadable string
	entries    []netrcEntry
}

// loadNetrc reads the netrc file the go command reads with env under goAuth:
// NETRC names the file, and otherwise it is .netrc in the home os.UserHomeDir
// resolves, or _netrc there on Windows when that file exists. A file that
// cannot be located or read holds no credential.
func loadNetrc(env *workspacejob.Env, goAuth string) netrcCredentials {
	return loadNetrcOn(env, goAuth, runtime.GOOS)
}

// loadNetrcOn is loadNetrc for the go command of goos.
func loadNetrcOn(env *workspacejob.Env, goAuth, goos string) netrcCredentials {
	credentials := netrcCredentials{enabled: goAuthUsesNetrc(goAuth)}
	if !credentials.enabled {
		return credentials
	}
	path, err := netrcPath(env, goos)
	credentials.path = path
	if err != nil {
		credentials.unreadable = pathlessError(err)
		return credentials
	}
	if credentials.path == "" {
		return credentials
	}
	data, err := os.ReadFile(credentials.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			credentials.unreadable = pathlessError(err)
		}
		return credentials
	}
	credentials.entries = parseNetrc(string(data))
	return credentials
}

// pathlessError is err without the operation and path a file system error
// carries, since the diagnostic names the path itself.
func pathlessError(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// goAuthUsesNetrc reports whether the go command reads the netrc file under
// goAuth, a semicolon-separated list of authentication commands whose default
// is "netrc".
func goAuthUsesNetrc(goAuth string) bool {
	if strings.TrimSpace(goAuth) == "" {
		return true
	}
	for command := range strings.SplitSeq(goAuth, ";") {
		if strings.TrimSpace(command) == "netrc" {
			return true
		}
	}
	return false
}

// netrcPath is the file the go command reads on goos: NETRC, else the home's
// .netrc, with the home's _netrc first on Windows. A Windows _netrc whose
// existence cannot be established is the file and the error, since the go
// command then reads no file at all.
func netrcPath(env *workspacejob.Env, goos string) (string, error) {
	if path := env.Get("NETRC"); path != "" {
		return path, nil
	}
	homeVariable := "HOME"
	if goos == "windows" {
		homeVariable = "USERPROFILE"
	}
	home := env.Get(homeVariable)
	if home == "" {
		return "", nil
	}
	if goos == "windows" {
		legacy := filepath.Join(home, "_netrc")
		_, err := os.Stat(legacy)
		if err == nil {
			return legacy, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return legacy, err
		}
	}
	return filepath.Join(home, ".netrc"), nil
}

// lookup is the login and password for location, an https URL without its
// scheme: the first entry whose machine, without an https:// scheme or a
// trailing slash, is location, else the first for the next shorter path
// prefix, down to the host with its port.
func (c netrcCredentials) lookup(location string) (login, password string, ok bool) {
	prefix := strings.TrimSuffix(location, "/")
	for {
		for _, entry := range c.entries {
			if strings.TrimSuffix(strings.TrimPrefix(entry.machine, "https://"), "/") == prefix {
				return entry.login, entry.password, true
			}
		}
		slash := strings.LastIndexByte(prefix, '/')
		if slash < 0 {
			return "", "", false
		}
		prefix = prefix[:slash]
	}
}

// missing is the reason the job sent no credential to host.
func (c netrcCredentials) missing(host string) string {
	switch {
	case !c.enabled:
		return "no credential for " + host + ": GOAUTH does not include netrc"
	case c.path == "":
		return "no credential for " + host + ": no home directory names a netrc file"
	case c.unreadable != "":
		return "no credential for " + host + ": cannot read " + c.path + ": " + c.unreadable
	default:
		return "no credential for " + host + " in " + c.path
	}
}

// parseNetrc keeps the complete machine entries of a netrc file, as the go
// command parses it: tokens pair up per line, a machine token starts a new
// entry, a macdef body runs to the next empty line, and nothing after the
// default token is read.
func parseNetrc(data string) []netrcEntry {
	var (
		entries []netrcEntry
		entry   netrcEntry
		inMacro bool
	)
	for line := range strings.SplitSeq(data, "\n") {
		if inMacro {
			if line == "" {
				inMacro = false
			}
			continue
		}
		fields := strings.Fields(line)
		i := 0
		for ; i < len(fields)-1; i += 2 {
			switch fields[i] {
			case "machine":
				entry = netrcEntry{machine: fields[i+1]}
			case "login":
				entry.login = fields[i+1]
			case "password":
				entry.password = fields[i+1]
			case "macdef":
				inMacro = true
			}
			if entry.machine != "" && entry.login != "" && entry.password != "" {
				entries = append(entries, entry)
				entry = netrcEntry{}
			}
		}
		if i < len(fields) && fields[i] == "default" {
			break
		}
	}
	return entries
}
