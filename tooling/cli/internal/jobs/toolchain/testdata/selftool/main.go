// Command selftool is the toolchain or dispatcher a probe test places. It
// reads its configuration from the JSON file beside its own executable, the
// file the kernel runs:
//
//   - exec: it is a passthrough dispatcher. It execs that target, or the one
//     a .dispatch-target file of the working directory names, with its own
//     argv and environment unchanged.
//   - otherwise it is a toolchain: `version` prints the version a .version
//     file of the working directory names, or version; `root` prints root,
//     or, with argv0Root, the directory above the directory of the real path
//     of argv[0], as an answer read from argv[0] does.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type config struct {
	Exec      string `json:"exec,omitempty"`
	Version   string `json:"version,omitempty"`
	Root      string `json:"root,omitempty"`
	Argv0Root bool   `json:"argv0Root,omitempty"`
}

func main() {
	self, err := os.Executable()
	if err != nil {
		fail(err)
	}
	data, err := os.ReadFile(self + ".json")
	if err != nil {
		fail(err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		fail(err)
	}
	if cfg.Exec != "" {
		target := cfg.Exec
		if named, err := os.ReadFile(".dispatch-target"); err == nil {
			target = strings.TrimSpace(string(named))
		}
		fail(syscall.Exec(target, os.Args, os.Environ()))
	}
	switch strings.Join(os.Args[1:], " ") {
	case "version":
		version := cfg.Version
		if named, err := os.ReadFile(".version"); err == nil {
			version = strings.TrimSpace(string(named))
		}
		fmt.Println(version)
	case "root":
		if !cfg.Argv0Root {
			fmt.Println(cfg.Root)
			return
		}
		real, err := filepath.EvalSymlinks(os.Args[0])
		if err != nil {
			fail(err)
		}
		abs, err := filepath.Abs(real)
		if err != nil {
			fail(err)
		}
		fmt.Println(filepath.Dir(filepath.Dir(abs)))
	default:
		fail(fmt.Errorf("unknown arguments %q", os.Args[1:]))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
