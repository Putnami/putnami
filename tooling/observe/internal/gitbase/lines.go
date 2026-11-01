package gitbase

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// MaxAddedLines bounds the added lines AddedLines keeps, all files together.
const MaxAddedLines = 1_000_000

// AddedLines returns, per path at head, the numbers of the lines the diff from
// base to head adds or modifies, in increasing order, renames detected. A
// binary file adds no line.
func AddedLines(ctx context.Context, dir, base, head string) (map[string][]int, error) {
	out, err := git{ctx: ctx, dir: dir}.raw("-c", "core.quotePath=false", "diff", "--no-ext-diff", "--no-textconv",
		"--no-color", "--unified=0", "-M", base, head, "--")
	if err != nil {
		return nil, err
	}
	return parseAddedLines(out)
}

// parseAddedLines reads a zero-context unified diff: each file's +++ line
// names its path at head, and each hunk header @@ -a,b +c,d @@ adds the lines
// c to c+d-1.
func parseAddedLines(out []byte) (map[string][]int, error) {
	lines := map[string][]int{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	current, total := "", 0
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			current = ""
		case strings.HasPrefix(line, "+++ "):
			name := strings.TrimPrefix(line, "+++ ")
			if name == "/dev/null" {
				current = ""
				continue
			}
			current = strings.TrimPrefix(unquote(name), "b/")
		case strings.HasPrefix(line, "@@ ") && current != "":
			start, count, err := hunkTarget(line)
			if err != nil {
				return nil, err
			}
			if total+count > MaxAddedLines {
				return lines, nil
			}
			total += count
			for n := start; n < start+count; n++ {
				lines[current] = append(lines[current], n)
			}
		}
	}
	return lines, scanner.Err()
}

// hunkTarget reads the +c,d range of a hunk header; d defaults to 1.
func hunkTarget(header string) (int, int, error) {
	fields := strings.Fields(header)
	if len(fields) < 3 || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, fmt.Errorf("unexpected hunk header %q", header)
	}
	startText, countText, hasCount := strings.Cut(strings.TrimPrefix(fields[2], "+"), ",")
	start, err := strconv.Atoi(startText)
	if err != nil {
		return 0, 0, fmt.Errorf("unexpected hunk header %q", header)
	}
	count := 1
	if hasCount {
		if count, err = strconv.Atoi(countText); err != nil || count < 0 {
			return 0, 0, fmt.Errorf("unexpected hunk header %q", header)
		}
	}
	return start, count, nil
}

// unquote reads a path git quoted because it holds a quote, a backslash or a
// control character.
func unquote(name string) string {
	if len(name) >= 2 && name[0] == '"' && name[len(name)-1] == '"' {
		if unquoted, err := strconv.Unquote(name); err == nil {
			return unquoted
		}
	}
	return name
}

// Show returns the content of path at revision. found is false when the
// revision holds no such file.
func Show(ctx context.Context, dir, revision, path string) (data []byte, found bool, err error) {
	g := git{ctx: ctx, dir: dir}
	if _, err := g.raw("cat-file", "-e", revision+":"+path); err != nil {
		return nil, false, nil
	}
	data, err = g.raw("cat-file", "blob", revision+":"+path)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}
