package tscscript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DefaultConfig is the config tsc -p compiles for a project with no build
// script: tsconfig.lib.json, the config @nx/js:tsc compiles, when the
// project holds one, else tsconfig.json. projectDir is the project directory.
func DefaultConfig(projectDir string) string {
	if info, err := os.Stat(filepath.Join(projectDir, "tsconfig.lib.json")); err == nil && !info.IsDir() {
		return "tsconfig.lib.json"
	}
	return "tsconfig.json"
}

// OwnOutDir returns the outDir a tsconfig file sets itself, resolved against
// the file's directory, and whether it sets one. It reads no config the file
// extends, so a caller that needs the outDir for certain treats false as
// unknown.
func OwnOutDir(configFile string) (string, bool) {
	data, err := os.ReadFile(configFile) //nolint:gosec // a tsconfig the caller names
	if err != nil {
		return "", false
	}
	var cfg struct {
		CompilerOptions struct {
			OutDir string `json:"outDir"`
		} `json:"compilerOptions"`
	}
	if json.Unmarshal(StripJSONC(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})), &cfg) != nil || cfg.CompilerOptions.OutDir == "" {
		return "", false
	}
	outDir := strings.ReplaceAll(cfg.CompilerOptions.OutDir, "${configDir}", filepath.ToSlash(filepath.Dir(configFile)))
	if filepath.IsAbs(outDir) {
		return filepath.Clean(outDir), true
	}
	return filepath.Join(filepath.Dir(configFile), filepath.FromSlash(outDir)), true
}

// StripJSONC removes the comments and the trailing commas tsc accepts in a
// tsconfig, so the rest decodes as JSON.
func StripJSONC(data []byte) []byte {
	var out bytes.Buffer
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out.WriteByte(c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out.WriteByte('\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && (data[i] != '*' || data[i+1] != '/') {
				i++
			}
			i++
		case c == ',':
			j := i + 1
			for j < len(data) && strings.ContainsRune(" \t\r\n", rune(data[j])) {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return out.Bytes()
}
