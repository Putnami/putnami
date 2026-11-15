package main

import (
	"fmt"
	"os"

	"go.putnami.dev/cloud/extension/internal/cloudcli"
	"go.putnami.dev/cloud/extension/internal/collabcli"
	deliverycli "go.putnami.dev/cloud/extension/internal/deliverycli"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__runner-next" {
		os.Exit(deliverycli.RunnerNext(os.Args[2:], os.Stdin, os.Stdout))
	}
	if len(os.Args) == 2 && os.Args[1] == "provider" {
		os.Exit(collabcli.Serve(collabcli.New(), os.Stdin, os.Stdout, os.Stderr))
	}
	if handled, err := cloudcli.HandleWorkspaceProbe(os.Args[1:], os.Stdin, os.Stdout); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "putnami-cloud: workspace-probe: %v\n", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(cloudcli.RunMain(os.Args[1:], cloudcli.IO{}))
}
