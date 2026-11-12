// Command runtime-clients generates the Go clients the @putnami/cloud runtime
// compiles, and writes a go build overlay that places them. prepare-runtime
// runs it before it builds the runtime.
//
// Usage:
//
//	go run ./internal/cmd/runtime-clients --output <dir>
//
// Git holds no generated client, and the runtime builds before any
// client-generate job can run: the workspace probe needs this runtime first. So
// the runtime generates its clients itself. For each module a local replace
// directive of ./go.mod reaches, a committed clientgen.json declares an
// in-module Go client, generated from the module's committed
// schema/openapi.json. The command writes each client under <dir> and writes
// <dir>/overlay.json, which maps each client's place in the source tree to the
// generated file. The source tree is never written.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.putnami.dev/api"
)

// OverlayFile is the go build overlay the command writes under its output.
const OverlayFile = "overlay.json"

func main() {
	// The api module imports go.putnami.dev/app, so the Go extension's describe
	// step runs every main package of this module with PUTNAMI_DESCRIBE set.
	// This command is no application and has nothing to describe.
	if os.Getenv("PUTNAMI_DESCRIBE") != "" {
		return
	}
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("runtime-clients", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "", "directory that receives the clients and overlay.json")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *output == "" || flags.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "usage: runtime-clients --output <dir>")
		return 2
	}
	if err := Generate(".", *output); err != nil {
		_, _ = fmt.Fprintf(stderr, "runtime-clients: %v\n", err)
		return 1
	}
	return 0
}

// overlay is the go build -overlay document: each source path the build reads
// maps to the file that holds its content.
type overlay struct {
	Replace map[string]string `json:"Replace"`
}

// clientConfig is the part of a committed clientgen.json that names an
// in-module Go client.
type clientConfig struct {
	Targets []string `json:"targets"`
	Go      struct {
		Output         string   `json:"output"`
		ModulePath     string   `json:"modulePath"`
		PackageName    string   `json:"packageName"`
		ClientName     string   `json:"clientName"`
		OmitOperations []string `json:"omitOperations"`
	} `json:"go"`
}

// Generate writes the client of every locally replaced module of the module at
// moduleDir that declares one, under output, then writes output/overlay.json.
// A module whose contract declares no operation gets no client and no overlay
// entry.
func Generate(moduleDir, output string) error {
	moduleDir, err := filepath.Abs(moduleDir)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	replaced, err := localReplacements(filepath.Join(moduleDir, "go.mod"))
	if err != nil {
		return err
	}
	document := overlay{Replace: map[string]string{}}
	for index, dir := range replaced {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(moduleDir, dir)
		}
		config, found, err := readClientConfig(dir)
		if err != nil || !found {
			if err != nil {
				return err
			}
			continue
		}
		contractPath := filepath.Join(dir, "schema", "openapi.json")
		contract, err := os.ReadFile(contractPath) //nolint:gosec // G304: the module's committed contract
		if err != nil {
			return fmt.Errorf("%s declares a Go client in clientgen.json but its contract does not read: %w", dir, err)
		}
		generated := filepath.Join(output, strconv.Itoa(index))
		result, err := api.EmitGoClient(contract, generated, api.EmitGoClientOptions{
			PackageName:    config.Go.PackageName,
			ClientName:     config.Go.ClientName,
			OmitOperations: config.Go.OmitOperations,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", contractPath, err)
		}
		if !result.Written {
			continue
		}
		place := filepath.Join(dir, filepath.FromSlash(config.Go.Output), api.GoClientFile)
		document.Replace[place] = filepath.Join(generated, api.GoClientFile)
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(output, OverlayFile), append(data, '\n'), 0o600)
}

// readClientConfig reads dir/clientgen.json. found is false when the module
// commits none. A clientgen.json that declares anything but an in-module Go
// client is an error: the runtime compiles only Go packages of its replaced
// modules.
func readClientConfig(dir string) (clientConfig, bool, error) {
	path := filepath.Join(dir, "clientgen.json")
	data, err := os.ReadFile(path) //nolint:gosec // G304: a replaced module's committed file
	if errors.Is(err, os.ErrNotExist) {
		return clientConfig{}, false, nil
	}
	if err != nil {
		return clientConfig{}, false, err
	}
	var config clientConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return clientConfig{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if !slices.Equal(config.Targets, []string{"go"}) || config.Go.ModulePath != "" {
		return clientConfig{}, false, fmt.Errorf("%s declares no in-module Go client", path)
	}
	output := filepath.ToSlash(filepath.Clean(filepath.FromSlash(config.Go.Output)))
	if config.Go.Output == "" || filepath.IsAbs(config.Go.Output) || output == ".." || strings.HasPrefix(output, "../") {
		return clientConfig{}, false, fmt.Errorf("%s go.output %q must name a directory inside the module", path, config.Go.Output)
	}
	config.Go.Output = output
	return config, true, nil
}

// localReplacements returns the directory of each local replace directive of
// the go.mod at path, in file order. A local directory starts with ./, ../ or
// /, as the go command requires.
func localReplacements(path string) ([]string, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the module's own go.mod
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var dirs []string
	inBlock := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if comment := strings.Index(line, "//"); comment >= 0 {
			line = strings.TrimSpace(line[:comment])
		}
		switch {
		case line == "replace (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "replace "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "replace "))
		case !inBlock:
			continue
		}
		_, target, ok := strings.Cut(line, "=>")
		if !ok {
			continue
		}
		fields := strings.Fields(target)
		if len(fields) == 0 {
			continue
		}
		dir := fields[0]
		if strings.HasPrefix(dir, "./") || strings.HasPrefix(dir, "../") || filepath.IsAbs(dir) {
			dirs = append(dirs, filepath.FromSlash(dir))
		}
	}
	return dirs, scanner.Err()
}
