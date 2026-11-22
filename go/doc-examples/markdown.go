package docexamples

import (
	"strings"
)

// NoCheck is the info-string word that opts a fenced block out of the check:
// "```go nocheck" or "```ts nocheck" marks a fragment that is not meant to
// compile, such as pseudo-code or a block with elisions.
const NoCheck = "nocheck"

// Block is one fenced code block of a markdown document.
type Block struct {
	// Lang is the first word of the info string, lowercased.
	Lang string
	// Attributes are the other words of the info string.
	Attributes []string
	// Line is the 1-based line of the opening fence.
	Line int
	// Code is the block's content, with the fence's indentation removed.
	Code string
}

// Checked reports whether the block carries no opt-out marker.
func (b Block) Checked() bool {
	for _, attribute := range b.Attributes {
		if attribute == NoCheck {
			return false
		}
	}
	return true
}

// Blocks returns the fenced code blocks of a markdown document, in order. A
// fence is three or more backticks or tildes; the block ends at a fence of the
// same character at least as long, or at the end of the document.
func Blocks(markdown string) []Block {
	lines := strings.Split(markdown, "\n")
	var blocks []Block
	for i := 0; i < len(lines); i++ {
		indent, fence, info, ok := openingFence(lines[i])
		if !ok {
			continue
		}
		block := Block{Line: i + 1}
		words := strings.Fields(info)
		if len(words) > 0 {
			block.Lang = strings.ToLower(words[0])
			block.Attributes = words[1:]
		}
		var body []string
		j := i + 1
		for ; j < len(lines); j++ {
			if closingFence(lines[j], fence) {
				break
			}
			body = append(body, dedent(lines[j], indent))
		}
		block.Code = strings.Join(body, "\n")
		blocks = append(blocks, block)
		i = j
	}
	return blocks
}

func openingFence(line string) (indent int, fence, info string, ok bool) {
	trimmed := strings.TrimLeft(line, " \t")
	indent = len(line) - len(trimmed)
	for _, char := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == char {
			n++
		}
		if n < 3 {
			continue
		}
		info = strings.TrimSpace(trimmed[n:])
		if char == '`' && strings.Contains(info, "`") {
			return 0, "", "", false
		}
		return indent, trimmed[:n], info, true
	}
	return 0, "", "", false
}

func closingFence(line, fence string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < len(fence) || trimmed[0] != fence[0] {
		return false
	}
	return strings.Trim(trimmed, string(fence[0])) == ""
}

func dedent(line string, indent int) string {
	n := 0
	for n < indent && n < len(line) && (line[n] == ' ' || line[n] == '\t') {
		n++
	}
	return line[n:]
}
