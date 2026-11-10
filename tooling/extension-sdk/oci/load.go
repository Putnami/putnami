package oci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"go.putnami.dev/sdk/extension/jsonl"
)

// dockerLoadTimeout bounds each docker call of LoadDocker, and
// dockerLoadWaitDelay how long its output may drain after it ends.
const (
	dockerLoadTimeout   = 5 * time.Minute
	dockerLoadWaitDelay = 5 * time.Second
)

// LoadDocker loads the image of the OCI layout into the local Docker daemon as
// tags[0], then tags it with each other tag. Each docker call runs at most five
// minutes, and its output goes to the job's log through emit, or to stderr when
// emit is nil.
func LoadDocker(layoutDir string, tags []string, emit *jsonl.Emitter) error {
	if len(tags) == 0 {
		return errors.New("no tag to load the image as")
	}
	archive, err := os.CreateTemp("", "putnami-image-*.tar")
	if err != nil {
		return err
	}
	_ = archive.Close()
	defer func() { _ = os.Remove(archive.Name()) }()
	if err := WriteDockerTarball(layoutDir, tags[0], archive.Name()); err != nil {
		return fmt.Errorf("write the docker tarball: %w", err)
	}
	if err := dockerCall(emit, "load", "-i", archive.Name()); err != nil {
		return fmt.Errorf("docker load %s: %w", tags[0], err)
	}
	for _, alias := range tags[1:] {
		if err := dockerCall(emit, "tag", tags[0], alias); err != nil {
			return fmt.Errorf("docker tag %s: %w", alias, err)
		}
	}
	return nil
}

// dockerCall runs docker with args, its output sent as LoadDocker describes.
func dockerCall(emit *jsonl.Emitter, args ...string) error {
	timed, cancel := context.WithTimeout(context.Background(), dockerLoadTimeout)
	defer cancel()
	cmd := exec.CommandContext(timed, "docker", args...) //nolint:gosec // fixed verbs with the task's own tags
	cmd.WaitDelay = dockerLoadWaitDelay
	if emit == nil {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		return cmd.Run()
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go jsonl.ForwardPipe(emit, stdout, "info", &readers)
	go jsonl.ForwardPipe(emit, stderr, "warn", &readers)
	readers.Wait()
	return cmd.Wait()
}
