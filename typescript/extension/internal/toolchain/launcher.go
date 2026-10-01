package toolchain

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Command returns the program and the arguments that start bin with args. A
// script whose shebang names node is a JavaScript launcher: the bun the task
// runs with starts it (see ResolveBun), and node from PATH never does. Every
// other file starts as itself.
func Command(bin string, args []string) (string, []string, error) {
	return commandWith(ResolveBun, bin, args)
}

// commandWith is Command with the bun resolveBun finds.
func commandWith(resolveBun func() (string, error), bin string, args []string) (string, []string, error) {
	if !isNodeScript(bin) {
		return bin, args, nil
	}
	bun, err := resolveBun()
	if err != nil {
		return "", nil, fmt.Errorf("%s is a JavaScript launcher: %w", bin, err)
	}
	return bun, append([]string{bin}, args...), nil
}

// shebangLimit bounds how much of a file isNodeScript reads.
const shebangLimit = 256

// isNodeScript reports whether file is a script whose shebang line names node,
// as "#!/usr/bin/env node" does. A file that cannot be read is not one.
func isNodeScript(file string) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	line, _ := bufio.NewReader(io.LimitReader(f, shebangLimit)).ReadString('\n')
	interpreter, ok := strings.CutPrefix(line, "#!")
	if !ok {
		return false
	}
	for _, field := range strings.Fields(interpreter) {
		if path.Base(field) == "node" {
			return true
		}
	}
	return false
}
