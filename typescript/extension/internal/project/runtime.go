package project

import (
	"fmt"

	"go.putnami.dev/typescript/extension/internal/errs"
)

// Workload runtimes a project declares as options["@putnami/typescript"].runtime.
const (
	// RuntimeBun runs the workload on Bun. It is the runtime of a project that
	// declares none.
	RuntimeBun = "bun"
	// RuntimeNode runs the workload on Node: the build targets Node, serve and
	// run start node, and the image ships a Node base.
	RuntimeNode = "node"
)

// ResolveRuntime returns the runtime a project declares: bun when value is
// empty, value when it is bun or node, and a typescript.invalid_runtime error
// naming both values otherwise.
func ResolveRuntime(value string) (string, error) {
	switch value {
	case "":
		return RuntimeBun, nil
	case RuntimeBun, RuntimeNode:
		return value, nil
	}
	return "", errs.New(errs.CodeInvalidRuntime, fmt.Sprintf(
		"options[\"@putnami/typescript\"].runtime is %q: declare %q or %q in the project's putnami.json", value, RuntimeBun, RuntimeNode)).
		WithCategory(errs.CategoryUser)
}
