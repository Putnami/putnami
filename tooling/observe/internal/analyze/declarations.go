package analyze

import (
	"os"
	"path/filepath"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/gitbase"
)

// Diagnostic codes of the declarations.
const (
	CodeDeclarationUnread     = "observe.declaration_unread"
	CodeDeclarationsTruncated = "observe.declarations_truncated"
)

// WorkspaceFile is the file that makes a repository a Putnami workspace. Only
// such a repository declares: its declarations are read.
const WorkspaceFile = "putnami.workspace.json"

// declarations lists the declaration files the change touches, each with its
// document at the base and at the head, when the repository is a Putnami
// workspace. A document over evidence.MaxDeclarationBytes, or one that cannot
// be read, is left out and its entry kept.
func (r *Result) declarations(in Input) {
	if _, err := os.Stat(filepath.Join(in.Root, WorkspaceFile)); err != nil {
		return
	}
	left := 0
	for _, file := range in.ChangedFiles {
		renamed := file.Status == gitbase.StatusRenamed
		if !evidence.IsDeclaration(file.Path) && (!renamed || !evidence.IsDeclaration(file.PreviousPath)) {
			continue
		}
		if len(r.Declarations) == evidence.MaxDeclarations {
			left++
			continue
		}
		declaration := evidence.Declaration{Path: file.Path, Status: file.Status}
		basePath := file.Path
		if renamed {
			declaration.PreviousPath, basePath = file.PreviousPath, file.PreviousPath
		}
		if file.Status != gitbase.StatusAdded {
			declaration.Before = r.declarationDocument(in, Base, basePath)
		}
		if file.Status != gitbase.StatusDeleted {
			declaration.After = r.declarationDocument(in, Head, file.Path)
		}
		r.Declarations = append(r.Declarations, declaration)
	}
	if left > 0 {
		r.warn(CodeDeclarationsTruncated, "declarations", "the change touches more than %d declaration files; %d are left out", evidence.MaxDeclarations, left)
	}
}

// declarationDocument reads a declaration at one side of the change, nil when
// that side cannot be read or the document is over its bound.
func (r *Result) declarationDocument(in Input, side Revision, file string) *string {
	var data []byte
	var err error
	found := true
	switch {
	case in.Show != nil:
		data, found, err = in.Show(side, file)
	case side == Head:
		data, err = os.ReadFile(filepath.Join(in.Root, filepath.FromSlash(file)))
	default:
		found = false
	}
	switch {
	case err != nil:
		r.warn(CodeDeclarationUnread, file, "the declaration %s cannot be read: %v", file, err)
		return nil
	case !found:
		return nil
	case len(data) > evidence.MaxDeclarationBytes:
		r.warn(CodeDeclarationUnread, file, "the declaration %s is over %d bytes, so its document is left out", file, evidence.MaxDeclarationBytes)
		return nil
	}
	document := string(data)
	return &document
}
