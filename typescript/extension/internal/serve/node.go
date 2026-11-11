package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/errs"
)

// NodeBuild is a Node bundle a node serve or run starts.
type NodeBuild struct {
	// Entry is the absolute path of the module node starts.
	Entry string
	// Inputs are the absolute paths of the files the bundle was built from,
	// which a watching serve reads.
	Inputs []string
}

// NodeBundler writes the Node bundle a node serve or run starts, and returns
// it, or the errors that refused the bundle.
type NodeBundler func() (built NodeBuild, bundleErrors []string, err error)

// NodeServe is a serve of a project on the node runtime.
type NodeServe struct {
	// NodeBin is the node program.
	NodeBin string
	// ProjectPath is the project directory: node's working directory.
	ProjectPath string
	// Port is the resolved port, which node reads as PORT.
	Port int
	// Params are the serve parameters.
	Params Params
	// Bundle writes the bundle, once at start and again after each change.
	Bundle NodeBundler
}

// watchInterval is how often a watching node serve reads the bundle's inputs.
var watchInterval = 500 * time.Millisecond

// LookNode returns the node program on PATH, or a typescript.node_not_found
// error naming node.
func LookNode() (string, error) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		return "", errs.New(errs.CodeNodeNotFound,
			"node is not on PATH: a project on the node runtime serves and runs with node; install Node.js and put node on PATH").
			WithCategory(errs.CategoryUser)
	}
	return nodeBin, nil
}

// NodeEnv is the environment node starts with: the serve environment, PORT,
// and PUTNAMI_PROJECT_ROOT.
func NodeEnv(base []string, projectPath string, port int) []string {
	env := base
	if port > 0 {
		env = append(env, "PORT="+strconv.Itoa(port))
	}
	resolved, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		resolved = projectPath
	}
	return append(env, "PUTNAMI_PROJECT_ROOT="+resolved)
}

// nodeServeArgs are the node arguments of a serve of entry.
func nodeServeArgs(entry string, params Params, isProd bool) []string {
	args := []string{"--enable-source-maps"}
	if !isProd {
		switch {
		case params.InspectBrk:
			args = append(args, "--inspect-brk")
		case params.InspectWait:
			args = append(args, "--inspect-wait")
		case params.Inspect:
			args = append(args, "--inspect")
		}
	}
	return append(args, entry)
}

// RunNodeServe bundles the project, starts node on the bundle and forwards its
// logs as JSONL events. With watch on, outside production, it bundles again
// when a file the bundle was built from changes and restarts node; a bundle
// that fails is reported and the running node keeps serving the previous one.
// It returns as RunBunServe does.
func RunNodeServe(emit *jsonl.Emitter, s NodeServe) (bool, error) {
	isProd := os.Getenv("NODE_ENV") == "production" || os.Getenv("ENV") == "production"
	built, bundleErrors, err := s.Bundle()
	if err != nil {
		return false, err
	}
	if len(bundleErrors) > 0 {
		emitBundleErrors(emit, bundleErrors)
		return false, nil
	}
	env := NodeEnv(buildServeEnv(s.Params, isProd), s.ProjectPath, s.Port)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if !s.Params.Watch || isProd {
		return superviseProcess(emit, serveCommand(s.NodeBin, nodeServeArgs(built.Entry, s.Params, isProd), s.ProjectPath, env), sigCh)
	}
	return watchNode(emit, s, built, env, sigCh), nil
}

// superviseProcess starts cmd, forwards its output, and supervises it as a
// serve: a termination signal reaches the process alone.
func superviseProcess(emit *jsonl.Emitter, cmd *exec.Cmd, sigCh <-chan os.Signal) (bool, error) {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return false, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("starting %s: %w", filepath.Base(cmd.Path), err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go jsonl.ForwardPipe(emit, stdoutPipe, "info", &wg)
	go jsonl.ForwardPipe(emit, stderrPipe, "warn", &wg)

	doneCh := make(chan error, 1)
	go func() {
		wg.Wait()
		doneCh <- cmd.Wait()
	}()

	waitErr, stopped := superviseServe(cmd.Process, sigCh, doneCh)
	return classifyServeExit(waitErr, stopped)
}

// watchNode serves built with node and restarts it after each change of the
// bundle's inputs that bundles. A node that exits on its own is started again
// after the next change that bundles. It returns true once a termination
// signal stopped the serve.
func watchNode(emit *jsonl.Emitter, s NodeServe, built NodeBuild, env []string, sigCh <-chan os.Signal) bool {
	args := nodeServeArgs(built.Entry, s.Params, false)
	inputs := built.Inputs
	snapshot := WatchSnapshot(inputs)
	server, err := startNode(emit, s.NodeBin, args, s.ProjectPath, env)
	if err != nil {
		emit.Diagnostic("error", "starting node: "+err.Error(), "", 0)
		return false
	}
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	for {
		var done <-chan error
		if server != nil {
			done = server.done
		}
		select {
		case <-sigCh:
			if server != nil {
				_ = server.stop(terminateGrace)
			}
			return true
		case waitErr := <-done:
			server = nil
			emit.Log("warn", fmt.Sprintf("node exited (%s); it restarts after the next change", exitDescription(waitErr)))
		case <-ticker.C:
			current := WatchSnapshot(inputs)
			if current == snapshot {
				continue
			}
			snapshot = current
			rebuilt, bundleErrors, err := s.Bundle()
			if err != nil {
				bundleErrors = append(bundleErrors, err.Error())
			}
			if len(bundleErrors) > 0 {
				emitBundleErrors(emit, bundleErrors)
				emit.Log("warn", "the change does not bundle; node keeps serving the previous build")
				continue
			}
			// The snapshot read before the bundle stays, so an edit saved while
			// bun ran bundles again at the next tick.
			if !slices.Equal(inputs, rebuilt.Inputs) {
				snapshot = WatchSnapshot(rebuilt.Inputs)
			}
			inputs = rebuilt.Inputs
			if server != nil {
				_ = server.stop(terminateGrace)
			}
			emit.Log("info", "restarting node after a change")
			if server, err = startNode(emit, s.NodeBin, nodeServeArgs(rebuilt.Entry, s.Params, false), s.ProjectPath, env); err != nil {
				emit.Diagnostic("error", "starting node: "+err.Error(), "", 0)
				return false
			}
		}
	}
}

// nodeOutputDelay bounds how long, once node exited, a watching serve waits
// for the rest of its output: a process node started that keeps node's
// stdout or stderr open does not hold a restart or a stop, and loses what it
// writes after that. It stays below terminateGrace.
var nodeOutputDelay = 2 * time.Second

// nodeProcess is one run of node in a watching serve. Like a Bun serve, it
// stays in the extension's process group, which the job runner signals and a
// composition records for orphan recovery, so no node outlives the job; a
// restart stops node alone.
type nodeProcess struct {
	proc *os.Process
	// done delivers node's wait error once node exited and its output was
	// forwarded, or nodeOutputDelay after node exited.
	done chan error
}

// startNode starts node with args in dir and forwards its output to emit.
func startNode(emit *jsonl.Emitter, nodeBin string, args []string, dir string, env []string) (*nodeProcess, error) {
	cmd := serveCommand(nodeBin, args, dir, env)
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	cmd.WaitDelay = nodeOutputDelay
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go forwardNodeOutput(emit, stdoutReader, "info", &wg)
	go forwardNodeOutput(emit, stderrReader, "warn", &wg)

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil
		}
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		wg.Wait()
		done <- err
	}()
	return &nodeProcess{proc: cmd.Process, done: done}, nil
}

// forwardNodeOutput forwards one output stream of node as JSONL events, and
// discards what is left of it once forwarding stops, such as after a line
// longer than the forwarder reads, so node's writes never block on it.
func forwardNodeOutput(emit *jsonl.Emitter, stream *io.PipeReader, fallbackLevel string, wg *sync.WaitGroup) {
	defer wg.Done()
	var forwarded sync.WaitGroup
	forwarded.Add(1)
	jsonl.ForwardPipe(emit, stream, fallbackLevel, &forwarded)
	_, _ = io.Copy(io.Discard, stream)
}

// stop sends node SIGTERM, SIGKILL when it still runs grace later, and returns
// its wait error.
func (p *nodeProcess) stop(grace time.Duration) error {
	return terminate(p.proc, p.done, grace, signalProcess)
}

// exitDescription words a wait error for a log line.
func exitDescription(waitErr error) string {
	if waitErr == nil {
		return "exit code 0"
	}
	return waitErr.Error()
}

// emitBundleErrors reports each error of a refused bundle.
func emitBundleErrors(emit *jsonl.Emitter, bundleErrors []string) {
	for _, message := range bundleErrors {
		emit.DiagnosticWithCode("error", message, "", 0, 0, errs.CodeCompileError.String())
	}
}

// WatchSnapshot digests the files a watching node serve reacts to: each of
// files with its size and modification time, or its absence, outside
// node_modules. Files the workload writes at run time, such as a database
// file, logs or uploads, are never inputs of the bundle and move nothing.
func WatchSnapshot(files []string) string {
	entries := make([]string, 0, len(files))
	for _, file := range files {
		if slices.Contains(strings.Split(filepath.ToSlash(file), "/"), "node_modules") {
			continue
		}
		info, err := os.Stat(file)
		if err != nil {
			entries = append(entries, file+":absent")
			continue
		}
		entries = append(entries, fmt.Sprintf("%s:%d:%d", file, info.Size(), info.ModTime().UnixNano()))
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
