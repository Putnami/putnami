//go:build unix

package devworkload

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// The members of a fixture composition are this test binary re-executed in
// fixture mode: each binds the port it is given, announces it with the
// runtime protocol's typed ready event and answers HTTP. One task of the
// fixture extension serves every project, so the workload reads what it is
// from fixture.json in its working directory, the project directory.
const fixtureModeEnv = "PUTNAMI_DEV_WORKLOAD_FIXTURE"

// fixtureStartsEnv, when set, names a directory outside the workspace where
// each fixture member records its start, in <pid>.json: what it is, and the
// environment its serve step received. Outside the workspace, the record
// changes no file a watched serve run or a tree binding reads. runEnv drops
// every PUTNAMI_ variable of the test's environment, so the name has no
// PUTNAMI_ prefix.
const fixtureStartsEnv = "DEV_WORKLOAD_FIXTURE_STARTS"

// fixtureStart is a member's start record.
type fixtureStart struct {
	Member     string `json:"member"`
	PID        int    `json:"pid"`
	Port       string `json:"port"`
	ConfigData string `json:"configData"`
}

// Fixture modes.
const (
	// fixtureServe binds, announces a server claim and serves.
	fixtureServe = "serve"
	// fixtureStarted serves, then reports completed startup with a workload
	// claim, as an application framework does once every start hook returned.
	fixtureStarted = "started"
	// fixtureExitEarly logs, then exits 3 before it announces anything.
	fixtureExitEarly = "exit-early"
)

// fixtureConfig is a project's fixture.json.
type fixtureConfig struct {
	Mode   string `json:"mode"`
	Member string `json:"member"`
}

// fixtureEvent is one runtime protocol v2 line a fixture member prints.
type fixtureEvent struct {
	V       int    `json:"v"`
	Type    string `json:"type"`
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

func TestMain(m *testing.M) {
	if os.Getenv(fixtureModeEnv) != "" {
		os.Exit(runFixtureWorkload())
	}
	// scratch.New, not os.MkdirTemp: a killed binary cannot run the removal
	// below, and the next run reclaims what it left.
	dir, err := scratch.New("putnami-dev-workload-test-")
	if err == nil {
		_ = os.Setenv("PUTNAMI_STORE_DIR", filepath.Join(dir.Path(), "store"))
		_ = os.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(dir.Path(), "artifacts"))
	}
	code := m.Run()
	fixtureproc.Remove()
	removeDevRuntime()
	if dir != nil {
		_ = dir.Remove()
	}
	os.Exit(code)
}

func emit(event fixtureEvent) {
	data, _ := json.Marshal(event)
	fmt.Println(string(data))
}

func emitLog(level, message string) {
	emit(fixtureEvent{V: 2, Type: "log", Level: level, Message: message})
}

// runFixtureWorkload is one member's serve step. It writes nothing in the
// workspace, so the worktree a local verdict binds to stays the committed one.
func runFixtureWorkload() int {
	var config fixtureConfig
	data, err := os.ReadFile("fixture.json")
	if err == nil {
		err = json.Unmarshal(data, &config)
	}
	if err != nil {
		emitLog("error", "fixture.json: "+err.Error())
		return 2
	}
	switch config.Mode {
	case fixtureServe, fixtureStarted, fixtureExitEarly:
	default:
		emitLog("error", fmt.Sprintf("fixture.json: unknown mode %q", config.Mode))
		return 2
	}
	emitLog("info", "fixture "+config.Member+" starting in "+config.Mode)
	if config.Mode == fixtureExitEarly {
		emitLog("error", "fixture "+config.Member+" cannot start")
		return 3
	}
	if dir := os.Getenv(fixtureStartsEnv); dir != "" {
		record, _ := json.Marshal(fixtureStart{
			Member: config.Member, PID: os.Getpid(), Port: os.Getenv("PORT"), ConfigData: os.Getenv("CONFIG_DATA"),
		})
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())+".json"), record, 0o600); err != nil {
			emitLog("error", "record the start: "+err.Error())
			return 1
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
	if err != nil {
		emitLog("error", err.Error())
		return 1
	}
	port := listener.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, config.Member)
	})
	mux.HandleFunc("/pid", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strconv.Itoa(os.Getpid()))
	})
	mux.HandleFunc("/items", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	emit(fixtureEvent{V: 2, Type: "ready", Data: map[string]any{
		"target":    "server",
		"endpoints": []map[string]any{{"scheme": "http", "host": "127.0.0.1", "port": port}},
	}})
	if config.Mode == fixtureStarted {
		emit(fixtureEvent{V: 2, Type: "ready", Data: map[string]any{"target": "workload"}})
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	for {
		select {
		case <-stop:
			_ = server.Close()
			return 0
		case <-time.After(50 * time.Millisecond):
			if os.Getppid() == 1 {
				return 0
			}
		}
	}
}
