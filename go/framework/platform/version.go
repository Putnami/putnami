package platform

import (
	"encoding/json"
	"runtime/debug"

	"go.putnami.dev/errors"
	"go.putnami.dev/http"
	protocol "go.putnami.dev/protocol/platform"
)

// codeInvalidVersionFile is returned by VersionFromGenerated when the bytes
// are not the JSON object the build pipeline writes to .gen/version.json.
const codeInvalidVersionFile errors.Code = "platform.invalid_version_file"

// generatedVersion mirrors the JSON object Putnami's build pipeline writes to
// each project's .gen/version.json. It is a superset of the wire-facing
// VersionInfo: the build records branch / buildTime / isDirty (and a parsed
// suffix) that runtime/debug.ReadBuildInfo structurally cannot supply.
type generatedVersion struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Suffix    string `json:"suffix"`
	SHA       string `json:"sha"`
	Branch    string `json:"branch"`
	IsDirty   bool   `json:"isDirty"`
	BuildTime string `json:"buildTime"`
}

// VersionInfo is what /version returns. The wire shape is owned by the
// protocol package — aliasing here keeps the framework API ergonomic
// while ensuring the JSON payload stays in lockstep with the
// cross-language contract.
type VersionInfo = protocol.VersionInfo

// versionHandler returns the configured VersionInfo, augmented with
// anything that runtime/debug.ReadBuildInfo can fill in. Caller-supplied
// fields win — debug info is a best-effort fallback for fields the
// caller did not provide.
func (p *Plugin) versionHandler() http.Handler {
	info := resolveVersion(p.cfg.Version)
	return func(_ *http.Context) *http.Response {
		return http.JSON(info)
	}
}

func resolveVersion(v VersionInfo) VersionInfo {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	var derived VersionInfo
	derived.Name = bi.Main.Path
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		derived.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			derived.SHA = s.Value
		case "vcs.time":
			derived.BuildTime = s.Value
		}
	}
	return mergeBuildInfo(v, derived)
}

// mergeBuildInfo returns configured with any empty field filled from
// derived. Caller-supplied (configured) fields always win; derived
// values are a best-effort fallback. Branch is intentionally excluded
// because runtime/debug does not expose it.
func mergeBuildInfo(configured, derived VersionInfo) VersionInfo {
	if configured.Name == "" {
		configured.Name = derived.Name
	}
	if configured.Version == "" {
		configured.Version = derived.Version
	}
	if configured.SHA == "" {
		configured.SHA = derived.SHA
	}
	if configured.BuildTime == "" {
		configured.BuildTime = derived.BuildTime
	}
	return configured
}

// VersionFromGenerated parses the JSON the Putnami build pipeline writes to a
// project's .gen/version.json into a VersionInfo suitable for Config.Version,
// so /version reflects the branch, build time, and dirty state the framework
// already records — fields runtime/debug.ReadBuildInfo cannot supply.
//
// Embed the generated file at build time and wire it in:
//
//	//go:embed .gen/version.json
//	var versionJSON []byte
//
//	info, err := platform.VersionFromGenerated(versionJSON)
//	if err != nil {
//	    info = platform.VersionInfo{Name: "my-service"} // degrade gracefully
//	}
//	platform.NewPlugin(platform.Config{Version: info})
//
// Any field the generated file leaves empty still falls back to
// runtime/debug.ReadBuildInfo at serve time (Config.Version values win over
// derived ones), so a partially populated file degrades cleanly. The wire
// shape carries no boolean dirty field, so a dirty build is surfaced by
// appending a "+dirty" marker to the commit SHA (mirroring `git describe
// --dirty`); the generated version string, which already encodes dirtiness,
// travels through Version unchanged.
func VersionFromGenerated(data []byte) (VersionInfo, error) {
	var gen generatedVersion
	if err := json.Unmarshal(data, &gen); err != nil {
		return VersionInfo{}, errors.Wrapf(err, codeInvalidVersionFile,
			"version metadata is not valid .gen/version.json")
	}
	info := VersionInfo{
		Name:      gen.Name,
		Version:   gen.Version,
		SHA:       gen.SHA,
		Branch:    gen.Branch,
		BuildTime: gen.BuildTime,
	}
	if gen.IsDirty && info.SHA != "" {
		info.SHA += "+dirty"
	}
	return info, nil
}
