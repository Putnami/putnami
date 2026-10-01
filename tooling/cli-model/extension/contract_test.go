package extension

import (
	"testing"
)

func TestValidateContracts_ValidPipeline(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test/ext",
		Tasks: map[string]TaskDefinition{
			"compile": {
				Command: "tsc",
				Inputs: map[string]TaskInputPort{
					"sources": {From: "project", Files: []string{"src/**/*.ts"}},
				},
				Outputs: map[string]TaskOutputPort{
					"dist": {Kind: "directory", Path: "dist/"},
				},
			},
			"bundle": {
				Command: "esbuild",
				Inputs: map[string]TaskInputPort{
					"compiledFiles": {From: "task"},
				},
				Outputs: map[string]TaskOutputPort{
					"bundle": {Kind: "file", Path: "dist/bundle.js"},
				},
			},
		},
		Commands: map[string]string{"build": "Build project"},
		Jobs: map[string]*JobDefinition{
			"build": {
				Name: "build",
				PipelineSteps: []PipelineStep{
					{
						ID:   "compile",
						Task: "compile",
					},
					{
						ID:        "bundle",
						Task:      "bundle",
						DependsOn: []string{"compile"},
						With: map[string]InputBinding{
							"compiledFiles": {FromStep: "compile", Output: "dist"},
						},
					},
				},
			},
		},
	}

	errors := ValidateContracts([]*ExtensionDescription{ext})
	if len(errors) != 0 {
		t.Errorf("expected no errors for valid pipeline, got %d:", len(errors))
		for _, e := range errors {
			t.Errorf("  %s", e.Error())
		}
	}
}

func TestValidateContracts_UnknownTask(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test/ext",
		Tasks: map[string]TaskDefinition{
			"compile": {Command: "tsc"},
		},
		Commands: map[string]string{"build": "Build"},
		Jobs: map[string]*JobDefinition{
			"build": {
				Name: "build",
				PipelineSteps: []PipelineStep{
					{ID: "step1", Task: "nonexistent"},
				},
			},
		},
	}

	errors := ValidateContracts([]*ExtensionDescription{ext})
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
	if errors[0].Step != "step1" {
		t.Errorf("expected error on step1, got %q", errors[0].Step)
	}
}

func TestValidateContracts_UnknownInputPort(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test/ext",
		Tasks: map[string]TaskDefinition{
			"compile": {
				Command: "tsc",
				Inputs: map[string]TaskInputPort{
					"sources": {From: "project"},
				},
			},
		},
		Commands: map[string]string{"build": "Build"},
		Jobs: map[string]*JobDefinition{
			"build": {
				Name: "build",
				PipelineSteps: []PipelineStep{
					{
						ID:   "step1",
						Task: "compile",
						With: map[string]InputBinding{
							"wrongName": {Value: "test", HasValue: true},
						},
					},
				},
			},
		},
	}

	errors := ValidateContracts([]*ExtensionDescription{ext})
	if len(errors) != 1 {
		t.Fatalf("expected 1 error for unknown input port, got %d", len(errors))
	}
}

func TestValidateContracts_UnknownStepRef(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test/ext",
		Tasks: map[string]TaskDefinition{
			"bundle": {Command: "esbuild"},
		},
		Commands: map[string]string{"build": "Build"},
		Jobs: map[string]*JobDefinition{
			"build": {
				Name: "build",
				PipelineSteps: []PipelineStep{
					{
						ID:   "step1",
						Task: "bundle",
						With: map[string]InputBinding{
							"input": {FromStep: "nonexistent", Output: "data"},
						},
					},
				},
			},
		},
	}

	errors := ValidateContracts([]*ExtensionDescription{ext})
	if len(errors) != 1 {
		t.Fatalf("expected 1 error for unknown step ref, got %d", len(errors))
	}
}

func TestValidateContracts_UnknownOutputPort(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test/ext",
		Tasks: map[string]TaskDefinition{
			"compile": {
				Command: "tsc",
				Outputs: map[string]TaskOutputPort{
					"dist": {Kind: "directory"},
				},
			},
			"bundle": {
				Command: "esbuild",
				Inputs: map[string]TaskInputPort{
					"input": {From: "task"},
				},
			},
		},
		Commands: map[string]string{"build": "Build"},
		Jobs: map[string]*JobDefinition{
			"build": {
				Name: "build",
				PipelineSteps: []PipelineStep{
					{ID: "compile", Task: "compile"},
					{
						ID:        "bundle",
						Task:      "bundle",
						DependsOn: []string{"compile"},
						With: map[string]InputBinding{
							"input": {FromStep: "compile", Output: "wrongOutput"},
						},
					},
				},
			},
		},
	}

	errors := ValidateContracts([]*ExtensionDescription{ext})
	if len(errors) != 1 {
		t.Fatalf("expected 1 error for unknown output port, got %d", len(errors))
	}
}

func TestValidateContracts_UnknownDependsOn(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test/ext",
		Tasks: map[string]TaskDefinition{
			"bundle": {Command: "esbuild"},
		},
		Commands: map[string]string{"build": "Build"},
		Jobs: map[string]*JobDefinition{
			"build": {
				Name: "build",
				PipelineSteps: []PipelineStep{
					{
						ID:        "step1",
						Task:      "bundle",
						DependsOn: []string{"nonexistent"},
					},
				},
			},
		},
	}

	errors := ValidateContracts([]*ExtensionDescription{ext})
	if len(errors) != 1 {
		t.Fatalf("expected 1 error for unknown dependsOn, got %d", len(errors))
	}
}

func TestValidateContracts_ExternalRefsAllowed(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test/ext",
		Tasks: map[string]TaskDefinition{
			"compile": {Command: "tsc"},
		},
		Commands: map[string]string{"build": "Build"},
		Jobs: map[string]*JobDefinition{
			"build": {
				Name: "build",
				PipelineSteps: []PipelineStep{
					{
						ID:        "step1",
						Task:      "compile",
						DependsOn: []string{"^build"},
					},
				},
			},
		},
	}

	errors := ValidateContracts([]*ExtensionDescription{ext})
	if len(errors) != 0 {
		t.Errorf("external refs should not produce errors, got %d", len(errors))
	}
}

func TestValidateContracts_NoTaskInputs_SkipValidation(t *testing.T) {
	// When a task has no declared inputs, binding names are not validated
	ext := &ExtensionDescription{
		Name: "@test/ext",
		Tasks: map[string]TaskDefinition{
			"compile": {Command: "tsc"}, // no Inputs declared
		},
		Commands: map[string]string{"build": "Build"},
		Jobs: map[string]*JobDefinition{
			"build": {
				Name: "build",
				PipelineSteps: []PipelineStep{
					{
						ID:   "step1",
						Task: "compile",
						With: map[string]InputBinding{
							"anything": {Value: "ok", HasValue: true},
						},
					},
				},
			},
		},
	}

	errors := ValidateContracts([]*ExtensionDescription{ext})
	if len(errors) != 0 {
		t.Errorf("should not validate binding names when task has no declared inputs, got %d errors", len(errors))
	}
}
