package extensions

import (
	"fmt"
	"os"
	"path/filepath"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"

	proto "go.putnami.dev/protocol/extension"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ExtensionsValidate validates an extension manifest against the protocol.
// If no path is given, it looks for putnami.extension.json in the current directory.
func ExtensionsValidate(args []string) error {
	path, err := resolveExtensionManifestPath(args)
	if err != nil {
		return err
	}

	iox.Fprintf(os.Stdout, "  Validating %s\n\n", path)

	// Read and parse manifest via protocol strict parser.
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read extension manifest: %w", err)
	}

	manifest, diags := proto.ParseManifest(data)
	if diag.HasErrors(diags) {
		iox.Fprintf(os.Stdout, "  ✗ Schema validation failed\n")
		printDiagnostics(diags)
		return cmderr.Classify(fmt.Errorf("extension manifest is invalid"), cmderr.ErrInvalidConfig)
	}
	iox.Fprintf(os.Stdout, "  ✓ Manifest parsed successfully\n")

	// Run full protocol validation (structural, DAG, schema refs).
	vDiags := proto.FullValidateManifest(manifest)
	diags = append(diags, vDiags...)

	if diag.HasErrors(diags) {
		iox.Fprintf(os.Stdout, "  ✗ Validation errors:\n")
		printDiagnostics(diags)
		return cmderr.Classify(fmt.Errorf("extension manifest has validation errors"), cmderr.ErrInvalidConfig)
	}

	// Print non-error diagnostics (warnings).
	if len(diags) > 0 {
		for _, d := range diags {
			iox.Fprintf(os.Stdout, "  ! %s\n", d.Message)
		}
	}

	iox.Fprintf(os.Stdout, "  ✓ Structural validation passed\n")

	// Contract validation (requires CLI-level resolved extension description).
	proto.NormalizeManifest(manifest)
	extPath := filepath.Dir(path)
	desc := extension.Resolve(manifest, extPath)
	contractErrors := extension.ValidateContracts([]*extension.ExtensionDescription{desc})
	if len(contractErrors) > 0 {
		iox.Fprintf(os.Stdout, "  ✗ Contract validation errors:\n")
		for _, e := range contractErrors {
			iox.Fprintf(os.Stdout, "    - %s\n", e.Error())
		}
		iox.Fprintln(os.Stdout)
		return cmderr.Classify(fmt.Errorf("%d contract error(s)", len(contractErrors)), cmderr.ErrInvalidConfig)
	}
	iox.Fprintf(os.Stdout, "  ✓ Contracts valid\n")

	iox.Fprintf(os.Stdout, "\n  Extension is valid.\n")
	return nil
}

// printDiagnostics renders protocol diagnostics to stdout.
func printDiagnostics(diags []diag.Diagnostic) {
	for _, d := range diags {
		if d.Severity == diag.Error {
			if d.Field != "" {
				iox.Fprintf(os.Stdout, "    - [%s] %s: %s\n", d.Code, d.Field, d.Message)
			} else {
				iox.Fprintf(os.Stdout, "    - [%s] %s\n", d.Code, d.Message)
			}
		}
	}
	iox.Fprintln(os.Stdout)
}

// resolveExtensionManifestPath finds the putnami.extension.json path from args or cwd.
func resolveExtensionManifestPath(args []string) (string, error) {
	if len(args) > 0 {
		path := args[0]
		info, err := os.Stat(path)
		if err != nil {
			return "", cmderr.NotFoundf("path not found: %s", path)
		}
		if info.IsDir() {
			path = filepath.Join(path, "putnami.extension.json")
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("no putnami.extension.json found at %s", path)
		}
		return path, nil
	}

	// Look in current directory
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine working directory: %w", err)
	}
	path := filepath.Join(cwd, "putnami.extension.json")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no putnami.extension.json in current directory")
	}
	return path, nil
}
