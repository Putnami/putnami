package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// maxServeEnvBytes caps the --serve-env document. A supervisor's environment
// (a port, a few URLs, one CONFIG_DATA document) is far smaller; the cap only
// bounds what a run reads before it starts anything.
const maxServeEnvBytes = 1 << 20

// reservedServeEnvPrefix is the namespace of the variables the CLI itself
// hands a job: the host RPC channel, the parent session, the cache socket,
// the credential descriptor. A supervisor's document never sets one, so it
// can neither forge a channel nor rename the session that spawned the step.
const reservedServeEnvPrefix = "PUTNAMI_"

// readServeEnvStdin reads the --serve-env document from the process's stdin.
// A stdin that is a character device, such as a terminal, is refused
// as a usage error before any read: a supervisor pipes the document, and a
// read from a terminal would wait for input that no prompt asks for.
func readServeEnvStdin(stdin *os.File) ([]string, error) {
	if stdin == nil {
		return readServeEnv(nil)
	}
	if info, err := stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return nil, usageErrorf("--serve-env -: stdin is a character device, such as a terminal; pipe the JSON object of the serve steps' environment to stdin")
	}
	return readServeEnv(stdin)
}

// readServeEnv reads the --serve-env document from stdin: one JSON object
// whose members are the variables of the serve steps' process environment,
// each value a string. It returns them as KEY=value entries sorted by name.
//
// The document is refused, as a usage error, when it is larger than 1 MiB,
// when it is not exactly one JSON object, when a member is not a string or is
// repeated, and when a name is empty, holds "=" or a NUL byte, or lies in the
// CLI's reserved PUTNAMI_ namespace; a value with a NUL byte is refused too.
// No message quotes a value or the document's bytes, which may be secrets: a
// refusal names the variable, or the byte offset of a syntax error.
func readServeEnv(stdin io.Reader) ([]string, error) {
	if stdin == nil {
		return nil, usageErrorf("--serve-env -: stdin is not readable")
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxServeEnvBytes+1))
	if err != nil {
		return nil, usageErrorf("--serve-env -: read stdin: %v", err)
	}
	if len(data) > maxServeEnvBytes {
		return nil, usageErrorf("--serve-env -: the document on stdin exceeds %d bytes", maxServeEnvBytes)
	}
	values, err := decodeServeEnv(data)
	if err != nil {
		return nil, usageErrorf("--serve-env -: %v", err)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}
	return env, nil
}

// decodeServeEnv decodes the document token by token, so a repeated member
// and trailing content are refused instead of resolved silently.
func decodeServeEnv(data []byte) (map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	syntax := func(err error) error {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return fmt.Errorf("stdin is not one JSON object of string variables: invalid JSON at byte %d", syntaxErr.Offset)
		}
		return errors.New("stdin is not one JSON object of string variables")
	}
	open, err := decoder.Token()
	if err != nil {
		return nil, syntax(err)
	}
	if delim, ok := open.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("stdin is not one JSON object of string variables")
	}
	values := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, syntax(err)
		}
		name, _ := token.(string)
		if err := checkServeEnvName(name); err != nil {
			return nil, err
		}
		if _, repeated := values[name]; repeated {
			return nil, fmt.Errorf("the variable %s is set twice", name)
		}
		token, err = decoder.Token()
		if err != nil {
			return nil, syntax(err)
		}
		value, isString := token.(string)
		if !isString {
			return nil, fmt.Errorf("the value of %s is not a string", name)
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("the value of %s holds a NUL byte", name)
		}
		values[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, syntax(err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("stdin holds content after the JSON object")
	}
	return values, nil
}

// checkServeEnvName refuses a name a process environment cannot hold, or one
// in the CLI's reserved namespace.
func checkServeEnvName(name string) error {
	switch {
	case name == "":
		return errors.New("a variable has an empty name")
	case strings.ContainsAny(name, "=\x00"):
		return fmt.Errorf("the variable name %q holds = or a NUL byte", name)
	case strings.HasPrefix(strings.ToUpper(name), reservedServeEnvPrefix):
		return fmt.Errorf("the variable %s is in the PUTNAMI_ namespace, which the CLI reserves for the variables it hands a job", name)
	}
	return nil
}
