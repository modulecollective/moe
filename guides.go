// Package moe exposes the hand-authored guidance shipped inside the moe
// binary: soul.md (workflow-agnostic philosophy) and the per-workflow
// stage fragments under stages/<workflow>/<stage>.md. Keeping these as
// embedded assets at the repo root means the Go code and the guidance
// it injects always ship together — renaming a stage in code without
// updating its fragment becomes a failing test, not a silent drift.
package moe

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed soul.md
var soulContent string

// all: prefix so stages/_shared/*.md — the cross-workflow guidance
// fragments — are included. Without it, //go:embed silently skips any
// file or directory whose name starts with "_" or ".".
//
//go:embed all:stages
var stagesFS embed.FS

// Soul returns the embedded soul.md content. Never empty in a correctly
// built binary; an empty return means the embed directive is broken.
func Soul() string {
	return soulContent
}

// Stage returns the embedded stages/<workflow>/<docID>.md fragment, or
// "" if no fragment exists for this (workflow, docID) pair. The empty
// return is the fallback path — buildSystemPrompt drops the stage
// section entirely when there's no fragment, so a workflow that hasn't
// authored one for a given stage just gets no stage lens in the prompt.
func Stage(workflow, docID string) string {
	b, err := fs.ReadFile(stagesFS, path.Join("stages", workflow, docID+".md"))
	if err != nil {
		return ""
	}
	return string(b)
}
