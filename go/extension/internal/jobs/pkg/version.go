package pkg

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
)

func resolveVersion(ctx *pctx.Context) (baseVersion, packageName string) {
	packageName = ctx.Project.Name
	if ctx.Workspace.Version != "" {
		baseVersion = ctx.Workspace.Version
	}
	projectRoot := ctx.Project.FullPath
	pkgJSON := filepath.Join(projectRoot, "package.json")
	if data, err := os.ReadFile(pkgJSON); err == nil {
		var pkg struct {
			Version string `json:"version"`
			Name    string `json:"name"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			if baseVersion == "" {
				baseVersion = pkg.Version
			}
			if pkg.Name != "" {
				packageName = pkg.Name
			}
		}
	}
	if baseVersion == "" {
		baseVersion = "0.0.0"
	}
	if idx := strings.IndexByte(baseVersion, '-'); idx >= 0 {
		baseVersion = baseVersion[:idx]
	}
	return
}

// resolveSuffix returns the version suffix to append after the base semver.
// The CLI orchestrator computes the suffix once at command start and writes
// it into ctx.Version.Suffix, so all jobs share the same value even if some
// build/codegen step has since dirtied the working tree. Falls back to a
// per-call git read only when the context is missing the field (e.g. direct
// extension invocation outside the CLI).
func resolveSuffix(ctx *pctx.Context) string {
	if ctx.Version != nil && ctx.Version.Suffix != "" {
		return ctx.Version.Suffix
	}
	return gitSuffix(ctx.WorkspaceRoot)
}

func gitSuffix(workspaceRoot string) string {
	shaCmd := exec.Command("git", "-C", workspaceRoot, "rev-parse", "--short", "HEAD")
	shaOut, err := shaCmd.Output()
	if err != nil {
		return "nogit"
	}
	sha := strings.TrimSpace(string(shaOut))
	if sha == "" {
		return "nogit"
	}

	// Detect dirty working tree and append a diff-based hash
	statusCmd := exec.Command("git", "-C", workspaceRoot, "status", "--porcelain")
	statusOut, err := statusCmd.Output()
	if err != nil || strings.TrimSpace(string(statusOut)) == "" {
		return sha
	}

	diffCmd := exec.Command("git", "-C", workspaceRoot, "diff", "HEAD", "--no-color")
	diffOut, _ := diffCmd.Output()
	h := sha256.Sum256(diffOut)
	dirtyHash := fmt.Sprintf("%x", h[:4])[:7]
	return sha + "-" + dirtyHash
}

var (
	artifactNameRe   = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	multiDashRe      = regexp.MustCompile(`-+`)
	startsAlphaNumRe = regexp.MustCompile(`^[A-Za-z0-9]`)
)

func toResolverArtifactName(value string) (string, error) {
	noScope := strings.TrimPrefix(value, "@")
	dashed := strings.NewReplacer("/", "-", "\\", "-").Replace(noScope)
	normalized := artifactNameRe.ReplaceAllString(dashed, "-")
	normalized = strings.Trim(normalized, "-")
	normalized = multiDashRe.ReplaceAllString(normalized, "-")
	if normalized == "" || !startsAlphaNumRe.MatchString(normalized) {
		return "", fmt.Errorf("invalid artifact name: %s", value)
	}
	return normalized, nil
}

func safeName(s string) string {
	return strings.NewReplacer(":", "-", "/", "-", "\\", "-").Replace(s)
}

func resolveString(flags map[string]string, flagKey string, params pctx.Params, paramKeys ...string) string {
	if v := cli.FlagString(flags, flagKey, ""); v != "" {
		return v
	}
	for _, k := range paramKeys {
		if v := params.String(k); v != "" {
			return v
		}
	}
	return params.String(flagKey)
}

func resolveBool(flags map[string]string, flagKey string, params pctx.Params, paramKeys ...string) bool {
	if _, ok := flags[flagKey]; ok {
		return cli.FlagBool(flags, flagKey, false)
	}
	keys := make([]string, 0, len(paramKeys)+1)
	keys = append(keys, paramKeys...)
	keys = append(keys, flagKey)
	return params.Bool(keys[0], false, keys[1:]...)
}
