package runtimeinfo

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

func TestHandleRuntimeInfo(t *testing.T) {
	var out bytes.Buffer
	handled, err := Handle([]string{Verb, Command}, &out, "@putnami/test", "1.2.3")
	if err != nil || !handled {
		t.Fatalf("Handle() = (%v, %v), want handled success", handled, err)
	}
	var info runtimeproto.Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Extension != "@putnami/test" || info.Version != "1.2.3" {
		t.Fatalf("identity = %q@%q", info.Extension, info.Version)
	}
	if info.Platform != runtime.GOOS+"/"+runtime.GOARCH ||
		info.CLIContract != protocolcli.CurrentContract ||
		info.RuntimeProtocol != runtimeproto.MaxKnownProtocolVersion ||
		info.RuntimeABI != runtimeproto.RuntimeABIVersion {
		t.Fatalf("protocol descriptor = %+v", info)
	}
}

func TestHandleIgnoresNormalCommands(t *testing.T) {
	handled, err := Handle([]string{"build"}, &bytes.Buffer{}, "@putnami/test", "")
	if err != nil || handled {
		t.Fatalf("Handle() = (%v, %v), want ignored", handled, err)
	}
}
