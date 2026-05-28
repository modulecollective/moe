// Package moe exposes the hand-authored guidance shipped inside the moe
// binary: soul.md (workflow-agnostic philosophy) and the per-workflow
// stage fragments under workflows/<workflow>/<stage>.md. Keeping these as
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

//go:embed workflows
var workflowsFS embed.FS

//go:embed skills/moe-bureaucracy/SKILL.md
var moeBureaucracySkill string

//go:embed skills/moe-context/SKILL.md
var moeContextSkill string

//go:embed skills/moe-howto/SKILL.md
var moeHowtoSkill string

// Soul returns the embedded soul.md content. Never empty in a correctly
// built binary; an empty return means the embed directive is broken.
func Soul() string {
	return soulContent
}

// Stage returns the embedded workflows/<workflow>/<docID>.md fragment,
// or "" if no fragment exists for this (workflow, docID) pair. The empty
// return is the fallback path — buildSystemPrompt drops the stage
// section entirely when there's no fragment, so a workflow that hasn't
// authored one for a given stage just gets no stage lens in the prompt.
func Stage(workflow, docID string) string {
	b, err := fs.ReadFile(workflowsFS, path.Join("workflows", workflow, docID+".md"))
	if err != nil {
		return ""
	}
	return string(b)
}

// MoeBureaucracySkill returns the embedded SKILL.md template for the
// moe-bureaucracy skill. The body carries `{{.TwinFeedback}}`,
// `{{.LoreFeedback}}`, and `{{.Followups}}` placeholders that the
// session materialization step substitutes with per-run absolute
// paths before writing into the session worktree's .claude/skills/
// and .codex/skills/ trees. Never empty in a correctly built binary.
func MoeBureaucracySkill() string {
	return moeBureaucracySkill
}

// MoeContextSkill returns the embedded SKILL.md template for the
// moe-context skill — the sibling to MoeBureaucracySkill that teaches
// the agent how to *read* the bureaucracy as context (prior runs,
// journal trailers, past stage transcripts). The body carries
// `{{.Project}}`, `{{.Run}}`, `{{.BureaucracyRoot}}`, `{{.ClonePath}}`,
// and `{{.HasClone}}` placeholders that the session materialization
// step substitutes per run before writing into .claude/skills/ and
// .codex/skills/. Never empty in a correctly built binary.
func MoeContextSkill() string {
	return moeContextSkill
}

// MoeHowtoSkill returns the embedded SKILL.md for the moe-howto skill —
// the chat workflow's idea-capture / backlog-grooming guidance. Unlike
// its two siblings it carries no template placeholders (its body is
// project-agnostic command guidance), so the materialiser writes it
// verbatim. Never empty in a correctly built binary.
func MoeHowtoSkill() string {
	return moeHowtoSkill
}

// OneShot returns the embedded workflows/<workflow>/oneshot.md fragment
// — the addendum buildSystemPrompt appends when a stage is being driven
// headlessly with no operator on stdin. The fragment tells the agent it
// has one turn and must either ship the canvas or refuse silently. ""
// when the workflow has no oneshot.md, in which case headless callers
// just don't get the addendum (the rest of the prompt assembly still
// applies).
func OneShot(workflow string) string {
	b, err := fs.ReadFile(workflowsFS, path.Join("workflows", workflow, "oneshot.md"))
	if err != nil {
		return ""
	}
	return string(b)
}
