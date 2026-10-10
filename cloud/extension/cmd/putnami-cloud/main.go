package main

import (
	"fmt"
	"os"

	"go.putnami.dev/cloud/extension/internal/cloudcli"
	deliverycli "go.putnami.dev/cloud/extension/internal/deliverycli"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__runner-next" {
		os.Exit(deliverycli.RunnerNext(os.Args[2:], os.Stdin, os.Stdout))
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
