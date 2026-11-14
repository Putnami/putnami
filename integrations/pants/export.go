package pants

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/stacks"
)

// listArgs are the arguments of the command that lists the address of every
// target, generated targets included.
var listArgs = []string{"list", "::"}

// peekBatch is the largest number of addresses one pants peek reads.
var peekBatch = 1000

// exportName names the export the sync reads when it runs pants.
const exportName = "pants peek of pants list ::"

// peekTarget is what the export keeps of one target of a pants peek.
type peekTarget struct {
	Address      string   `json:"address"`
	TargetType   string   `json:"target_type"`
	Dependencies []string `json:"dependencies"`
	Sources      []string `json:"sources,omitempty"`
}

// exportGraph writes the export of the repository at root without asking
// Pants for the whole repository at once: pants list :: names every target,
// then pants peek reads them peekBatch addresses at a time, each output read
// as a stream, of which the export keeps the address, the type, the
// dependencies and the sources. Every call runs at the root, under the lock
// of the stack's root tasks, since Pants serves one command at a time per
// workspace. It answers the export file and the function that removes it;
// the error output of pants goes to stderr.
func exportGraph(ctx context.Context, root string, stderr io.Writer) (string, func(), error) {
	lock, err := stacks.LockRoot(root, profileID, wsproto.ProbeRootPath)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = lock.Release() }()
	dir, err := os.MkdirTemp("", "putnami-pants-graph-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	file, err := writeExport(ctx, root, dir, stderr)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return file, cleanup, nil
}

func writeExport(ctx context.Context, root, dir string, stderr io.Writer) (string, error) {
	output := filepath.Join(dir, "pants.out")
	if err := runPants(ctx, root, output, stderr, listArgs); err != nil {
		return "", fmt.Errorf("pants %s: %w", strings.Join(listArgs, " "), err)
	}
	addresses, err := readAddresses(output)
	if err != nil {
		return "", err
	}
	targets := make([]peekTarget, 0, len(addresses))
	seen := make(map[string]bool, len(addresses))
	for start := 0; start < len(addresses); start += peekBatch {
		batch := addresses[start:min(start+peekBatch, len(addresses))]
		if err := runPants(ctx, root, output, stderr, append([]string{"peek"}, batch...)); err != nil {
			return "", fmt.Errorf("pants peek on targets %d to %d of %d: %w", start+1, start+len(batch), len(addresses), err)
		}
		err := readPeek(output, func(t peekTarget) {
			if !seen[t.Address] {
				seen[t.Address] = true
				targets = append(targets, t)
			}
		})
		if err != nil {
			return "", fmt.Errorf("pants peek on targets %d to %d of %d: %w", start+1, start+len(batch), len(addresses), err)
		}
	}
	file := filepath.Join(dir, "pants-graph.json")
	data, err := json.Marshal(targets)
	if err != nil {
		return "", err
	}
	return file, os.WriteFile(file, data, 0o600)
}

// runPants runs pants with args at root and writes its standard output to
// the file output, which it replaces.
func runPants(ctx context.Context, root, output string, stderr io.Writer, args []string) error {
	out, err := os.Create(output)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "pants", args...)
	cmd.Dir = root
	cmd.Stdout = out
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if err := out.Close(); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

// readAddresses reads the addresses pants list printed, one per line.
func readAddresses(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		if address := strings.TrimSpace(scanner.Text()); address != "" {
			out = append(out, address)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("pants %s printed no address list: %w", strings.Join(listArgs, " "), err)
	}
	return out, nil
}

// readPeek reads the JSON array of targets pants peek printed, one target at
// a time, and hands each to add.
func readPeek(file string, add func(peekTarget)) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(bufio.NewReader(f))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return errors.New("pants peek printed no JSON array of targets")
	}
	for dec.More() {
		var t peekTarget
		if err := dec.Decode(&t); err != nil {
			return fmt.Errorf("pants peek printed a target that does not parse: %w", err)
		}
		add(t)
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("pants peek printed a truncated array: %w", err)
	}
	return nil
}
