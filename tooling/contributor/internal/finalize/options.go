package finalize

import (
	"errors"
	"fmt"
	"strings"
)

// ParseOptions reads the flags after `change finalize`. It is a copy of the
// core's parser for `proposals finalize`, with the same messages. Every
// error is a usage error.
func ParseOptions(args []string) (Options, error) {
	var options Options
	seen := map[string]bool{}
	for len(args) > 0 {
		raw := args[0]
		args = args[1:]
		name, value, hasValue := strings.Cut(raw, "=")
		switch name {
		case "--draft", "--partial-delivery":
			if hasValue {
				return options, fmt.Errorf("%s takes no value", name)
			}
			if seen[name] {
				return options, fmt.Errorf("%s given twice", name)
			}
			seen[name] = true
			if name == "--draft" {
				options.Draft = true
			} else {
				options.PartialDelivery = true
			}
			continue
		case "--base", "--branch", "--title", "--body-file", "--task-source", "--task-id", "--verification-file", "--workflow-file", "--gate", "--proof":
		default:
			return options, fmt.Errorf("unexpected finalize argument %q", raw)
		}
		if seen[name] {
			return options, fmt.Errorf("%s given twice", name)
		}
		seen[name] = true
		if !hasValue {
			if len(args) == 0 {
				return options, fmt.Errorf("%s needs a value", name)
			}
			value = args[0]
			args = args[1:]
		}
		if value == "" || strings.HasPrefix(value, "--") {
			return options, fmt.Errorf("%s needs a nonempty value", name)
		}
		if name == "--gate" && value != "ci" {
			return options, errors.New("--gate accepts only ci; local proof uses --verification-file")
		}
		switch name {
		case "--base":
			options.Base = value
		case "--branch":
			options.Branch = value
		case "--title":
			options.Title = value
		case "--body-file":
			options.BodyFile = value
		case "--task-source":
			options.TaskSource = value
		case "--task-id":
			options.TaskID = value
		case "--verification-file":
			options.VerificationFile = value
		case "--workflow-file":
			options.WorkflowFile = value
		case "--gate":
			options.Gate = value
		case "--proof":
			options.Proof = value
		}
	}
	return options, nil
}
