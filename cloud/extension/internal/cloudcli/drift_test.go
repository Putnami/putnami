package cloudcli

import (
	"encoding/json"
	"os"
	"testing"
)

// TestManifestFlagTypesMatchParser guards the hand-maintained
// putnami.extension.json against the clicore boolean-flag registration in
// clicore_alias.go's init(): for every flag the manifest declares, the CLI's
// own flag parser must agree on whether it is a standalone boolean. This catches
// the drift where a flag is added or retyped in the manifest but the
// RegisterBooleanFlags call isn't updated (or vice versa) — which would make the
// fallback parser mis-handle `--flag <positional>` (treating the positional as
// the flag's value).
//
// It compares against the top-level `commands` map (the flag-bearing entries),
// not `commandGroups` (the flagless `cloud <verb>` aliases).
//
// parserExceptions lists the flags a command reads from argv itself, so the
// shared parser's reading never reaches them.
func TestManifestFlagTypesMatchParser(t *testing.T) {
	data, err := os.ReadFile("../../putnami.extension.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Commands map[string]struct {
			Flags map[string]struct {
				Type string `json:"type"`
			} `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	checked := 0
	for cmd, spec := range manifest.Commands {
		for flag, fs := range spec.Flags {
			if parserExceptions[cmd+"/"+flag] {
				continue
			}
			wantBoolean := fs.Type == "boolean"
			if got := isBooleanFlag(flag); got != wantBoolean {
				t.Errorf("flag --%s (manifest command %q) has type %q (boolean=%v) but the CLI parser registers boolean=%v; update RegisterBooleanFlags in clicore_alias.go init() or fix the manifest flag type",
					flag, cmd, fs.Type, wantBoolean, got)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no manifest flags checked — the manifest path or shape is wrong")
	}
	t.Logf("checked %d manifest flag declarations against the parser", checked)
}

// parserExceptions: `cloud channels status --wait <duration>` takes a value,
// while every other command's --wait is a switch. channelWait in
// internal/distributioncli reads both `--wait <duration>` and `--wait=<duration>`
// from argv before the shared parser sees them.
var parserExceptions = map[string]bool{
	"cloud-channels/wait": true,
}
