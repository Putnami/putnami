// Package imageload loads the image a stack extension packages into the local
// Docker daemon, for package --docker-load. Only the extensions that package
// an image import it, so the others keep no dependency on the OCI libraries.
package imageload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
)

// callTimeout bounds each docker call, and drainDelay how long its output may
// drain once the call ends, even when a process it started holds the output.
const (
	callTimeout = 5 * time.Minute
	drainDelay  = 5 * time.Second
)

// Load loads the image of the OCI layout into the local Docker daemon as
// tags[0], then tags it with each other tag. Each docker call runs at most five
// minutes, and its output goes to the job's log through emit, standard output
// as info and standard error as warn, or to stderr when emit is nil.
func Load(layoutDir string, tags []string, emit *jsonl.Emitter) error {
	if len(tags) == 0 {
		return errors.New("no tag to load the image as")
	}
	archive, err := os.CreateTemp("", "putnami-image-*.tar")
	if err != nil {
		return err
	}
	_ = archive.Close()
	defer func() { _ = os.Remove(archive.Name()) }()
	if err := oci.WriteDockerTarball(layoutDir, tags[0], archive.Name()); err != nil {
		return fmt.Errorf("write the docker tarball: %w", err)
	}
	if err := docker(emit, "load", "-i", archive.Name()); err != nil {
		return fmt.Errorf("docker load %s: %w", tags[0], err)
	}
	for _, alias := range tags[1:] {
		if err := docker(emit, "tag", tags[0], alias); err != nil {
			return fmt.Errorf("docker tag %s: %w", alias, err)
		}
	}
	return nil
}

// docker runs docker with args, its output sent as Load describes. The output
// goes through writers, not pipes the caller reads, so drainDelay bounds the
// copy once docker exits.
func docker(emit *jsonl.Emitter, args ...string) error {
	timed, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(timed, "docker", args...) //nolint:gosec // fixed verbs with the task's own tags
	cmd.WaitDelay = drainDelay
	if emit == nil {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		return cmd.Run()
	}
	stdout, stderr := &lineLog{emit: emit, level: "info"}, &lineLog{emit: emit, level: "warn"}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	stdout.flush()
	stderr.flush()
	return err
}

// lineLog writes each complete line it receives to the job's log at level.
type lineLog struct {
	emit    *jsonl.Emitter
	level   string
	mu      sync.Mutex
	pending bytes.Buffer
}

var _ io.Writer = (*lineLog)(nil)

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending.Write(p)
	for {
		line, err := l.pending.ReadString('\n')
		if err != nil {
			l.pending.WriteString(line)
			return len(p), nil
		}
		if text := line[:len(line)-1]; text != "" {
			l.emit.Log(l.level, text)
		}
	}
}

// flush writes the last line, one with no newline.
func (l *lineLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending.Len() > 0 {
		l.emit.Log(l.level, l.pending.String())
		l.pending.Reset()
	}
}
