// Package command collects and submits the public agent-readiness payload.
package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"go.putnami.dev/intelligence/agent-readiness/payload"
	"go.putnami.dev/intelligence/agent-readiness/schema"
	protocolcli "go.putnami.dev/protocol/cli"
)

const (
	Command        = "agent-readiness"
	InspectCommand = "putnami agent-readiness --print-payload"
	DefaultTimeout = 120 * time.Second
	maxTimeout     = time.Hour
)

type Options struct {
	PrintPayload bool
	Timeout      time.Duration
}

// ParseOptions validates the wire params before any repository read or send.
func ParseOptions(params map[string]json.RawMessage) (Options, error) {
	options := Options{Timeout: DefaultTimeout}
	if raw, ok := params["print-payload"]; ok {
		if err := json.Unmarshal(raw, &options.PrintPayload); err != nil {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return Options{}, protocolcli.Usagef("--print-payload must be a boolean")
			}
			switch value {
			case "true", "1", "yes", "on":
				options.PrintPayload = true
			case "false", "0", "no", "off":
				options.PrintPayload = false
			default:
				return Options{}, protocolcli.Usagef("--print-payload must be a boolean")
			}
		}
	}
	if raw, ok := params["timeout"]; ok {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return Options{}, protocolcli.Usagef("--timeout must be a whole number of seconds between 1 and 3600")
		}
		var seconds float64
		switch v := value.(type) {
		case string:
			parsed, err := strconv.Atoi(v)
			if err != nil {
				return Options{}, protocolcli.Usagef("--timeout must be a whole number of seconds between 1 and 3600")
			}
			seconds = float64(parsed)
		case float64:
			seconds = v
		default:
			return Options{}, protocolcli.Usagef("--timeout must be a whole number of seconds between 1 and 3600")
		}
		if seconds < 1 || seconds > 3600 || math.Trunc(seconds) != seconds {
			return Options{}, protocolcli.Usagef("--timeout must be a whole number of seconds between 1 and 3600")
		}
		options.Timeout = time.Duration(seconds) * time.Second
	}
	return options, nil
}

type Config struct {
	Dir              string
	CollectorVersion string
	APIURL           string
	Client           *http.Client
	Stdout           io.Writer
	Stderr           io.Writer
	Mode             protocolcli.OutputMode
}

type Result struct {
	Report        string  `json:"report"`
	Token         string  `json:"token"`
	Level         string  `json:"level"`
	ShareBlocked  float64 `json:"shareBlocked"`
	MethodVersion string  `json:"methodVersion"`
	SentBytes     int     `json:"sentBytes"`
	Commits       int     `json:"commits"`
	Areas         int     `json:"areas"`
	ElapsedMs     int64   `json:"elapsedMs"`
}

// Run prints a payload without sending, or sends precisely the bytes it would print.
func Run(parent context.Context, options Options, config Config) (Result, error) {
	if config.Stdout == nil {
		config.Stdout = io.Discard
	}
	if config.Stderr == nil {
		config.Stderr = io.Discard
	}
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	root, err := repositoryRoot(ctx, config.Dir)
	if err != nil {
		return Result{}, timedOut(err, options)
	}
	if !options.PrintPayload && !config.Mode.IsStructured() {
		fmt.Fprintf(config.Stderr, "Reading the git history of %s…\n", root)
	}
	built, err := BuildPayload(ctx, root, config.CollectorVersion, options.Timeout)
	if err != nil {
		return Result{}, timedOut(err, options)
	}
	if options.PrintPayload {
		_, err = fmt.Fprintln(config.Stdout, string(built.Bytes))
		return Result{}, err
	}
	if !config.Mode.IsStructured() {
		fmt.Fprintln(config.Stdout, readLine(built))
	}
	submission, err := Submit(ctx, config.APIURL, config.Client, built.Bytes)
	if err != nil {
		return Result{}, sendFailure(ctx, err, config.APIURL)
	}
	result := Result{
		Report: submission.Url, Token: submission.Token, Level: string(submission.Level),
		ShareBlocked: submission.ShareBlocked, MethodVersion: submission.MethodVersion,
		SentBytes: len(built.Bytes), Commits: built.Commits, Areas: built.Areas,
		ElapsedMs: built.Elapsed.Milliseconds(),
	}
	if !config.Mode.IsStructured() {
		fmt.Fprintln(config.Stdout, verdictLine(result.Level, result.ShareBlocked))
		fmt.Fprintln(config.Stdout, sentLine(result.SentBytes))
		fmt.Fprintln(config.Stdout, "Report: "+result.Report)
	}
	return result, nil
}

var collect = payload.Collect

type Built struct {
	Bytes   []byte
	Commits int
	Areas   int
	Elapsed time.Duration
}

func BuildPayload(ctx context.Context, root, collectorVersion string, timeout time.Duration) (Built, error) {
	collected, err := collect(ctx, payload.Options{Dir: root, CollectorVersion: collectorVersion, Timeout: timeout})
	if err != nil {
		if errors.Is(err, payload.ErrPrivacy) {
			return Built{}, fmt.Errorf("agent-readiness payload refused, it could expose private data: %w", err)
		}
		return Built{}, err
	}
	if err := ctx.Err(); err != nil {
		return Built{}, err
	}
	encoded, err := json.Marshal(collected.Payload)
	if err != nil {
		return Built{}, fmt.Errorf("encode agent-readiness payload: %w", err)
	}
	if err := validatePayload(encoded); err != nil {
		return Built{}, fmt.Errorf("agent-readiness payload does not match schema v1, nothing was printed or sent: %w", err)
	}
	return Built{Bytes: encoded, Commits: collected.Payload.Inventory.CommitsTotal, Areas: len(collected.Payload.Areas), Elapsed: collected.Elapsed}, nil
}

var payloadSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	if err := compiler.AddResource(schema.PayloadFile, bytes.NewReader(schema.PayloadV1)); err != nil {
		return nil, err
	}
	return compiler.Compile(schema.PayloadFile)
})

func validatePayload(encoded []byte) error {
	compiled, err := payloadSchema()
	if err != nil {
		return err
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return err
	}
	return compiled.Validate(document)
}

func repositoryRoot(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("read current directory: %w", err)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("agent-readiness needs git on PATH to read repository history")
	}
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", protocolcli.Usagef("%s is not inside a git repository. Run putnami agent-readiness from a repository's directory.", dir)
	}
	root := strings.TrimSpace(string(out))
	if _, err := git(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", protocolcli.Usagef("%s has no commit yet: there is no history to read.", root)
	}
	return root, nil
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	return cmd.Output()
}

func timedOut(err error, options Options) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("agent-readiness did not finish within %s; raise --timeout", options.Timeout)
	}
	return err
}
