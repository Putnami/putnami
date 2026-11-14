package collabcli

import (
	"bytes"
	"context"
	"io"

	collab "go.putnami.dev/protocol/collaboration"
)

// Serve answers one routed provider call read from stdin, writing the answer
// whole to stdout after every bearer the provider held has been redacted from
// it. It returns the process exit code.
func Serve(p *Provider, stdin io.Reader, stdout, stderr io.Writer) int {
	var answer bytes.Buffer
	if err := collab.Serve(context.Background(), stdin, &answer, p.Handlers()); err != nil {
		_, _ = io.WriteString(stderr, "putnami-cloud: the collaboration request could not be read\n")
		return 1
	}
	if _, err := stdout.Write(p.Redact(answer.Bytes())); err != nil {
		return 1
	}
	return 0
}
