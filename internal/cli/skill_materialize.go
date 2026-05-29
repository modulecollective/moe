package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/run"
)

// skillMaterializeDirs lists the skill discovery roots written under
// workRoot. Only codex reads from here: it runs cwd=workRoot and walks
// up to the session worktree's .git anchor to find
// `.codex/skills/<skill-name>/SKILL.md`. Claude is served entirely by
// the sessionCwd write in writeSkill — it runs cwd=sessionCwd and
// discovers `.claude/skills/` there directly — so no claude copy is
// written under workRoot. (Kept as a slice so a third backend is a
// one-line add.)
var skillMaterializeDirs = []string{".codex"}

// materializeMoeBureaucracySkill writes the moe-bureaucracy SKILL.md
// into the session worktree's .codex/skills/ tree (codex discovery
// walks up from cwd=workRoot to the worktree's .git anchor) and under
// sessionCwd/.claude/skills/ (claude runs cwd=sessionCwd and discovers
// the skill there directly).
//
// Materialized fresh on every BuildSpec call; the paths are
// session-stable for the run but cheap to rewrite, and a refresh costs
// less than reasoning about staleness across resumes. Lives inside the
// session worktree (workRoot) or under .moe/sessions/ (sessionCwd) so
// teardown is free: session.Close removes the worktree, taking those
// materialized skills with it; the sessionCwd dir is operator-local
// scratch under .moe/. Never staged or committed.
func materializeMoeBureaucracySkill(workRoot, sessionCwd string, md *run.Metadata) error {
	tmpl, err := template.New("moe-bureaucracy-skill").Parse(moe.MoeBureaucracySkill())
	if err != nil {
		return fmt.Errorf("skill: parse moe-bureaucracy template: %w", err)
	}
	data := struct {
		TwinFeedback string
		LoreFeedback string
		Followups    string
	}{
		TwinFeedback: filepath.Join(workRoot, run.FeedbackPath(md.Project, md.ID, "twin")),
		LoreFeedback: filepath.Join(workRoot, run.FeedbackPath(md.Project, md.ID, "lore")),
		Followups:    filepath.Join(workRoot, run.FollowupsPath(md.Project, md.ID)),
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("skill: render moe-bureaucracy template: %w", err)
	}
	return writeSkill(workRoot, sessionCwd, "moe-bureaucracy", buf.Bytes())
}

// materializeMoeContextSkill writes the moe-context SKILL.md into the
// session worktree's .codex/skills/ tree plus the sessionCwd
// .claude/skills/ tree, with the run's project, run id,
// bureaucracy root, and (if present) sandbox clone path pre-substituted.
// Sibling to materializeMoeBureaucracySkill: the bureaucracy skill
// teaches the agent how to *write* traces back into the bureaucracy;
// this one teaches the agent how to *read* prior runs, the journal,
// and past stage transcripts as context.
//
// clonePath is empty for document-only stages; the skill template uses
// HasClone to decide whether to render the project-source-clone bullet.
//
// Materialized fresh on every BuildSpec call — same lifecycle as
// materializeMoeBureaucracySkill. Never staged or committed.
func materializeMoeContextSkill(workRoot, sessionCwd string, md *run.Metadata, clonePath string) error {
	tmpl, err := template.New("moe-context-skill").Parse(moe.MoeContextSkill())
	if err != nil {
		return fmt.Errorf("skill: parse moe-context template: %w", err)
	}
	data := struct {
		Project         string
		Run             string
		BureaucracyRoot string
		ClonePath       string
		HasClone        bool
	}{
		Project:         md.Project,
		Run:             md.ID,
		BureaucracyRoot: workRoot,
		ClonePath:       clonePath,
		HasClone:        clonePath != "",
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("skill: render moe-context template: %w", err)
	}
	return writeSkill(workRoot, sessionCwd, "moe-context", buf.Bytes())
}

// materializeMoeHowtoSkill writes the chat workflow's moe-howto skill
// (idea capture + backlog grooming) into the same .codex/skills/ and
// sessionCwd .claude/skills/ trees as its two siblings. Unlike them it
// carries no per-run template — the body is project-agnostic command
// guidance — so it plants the embedded body verbatim. Chat is the only
// caller; a workflow gate in BuildSpec keeps it off every other stage,
// so a coding or reflect agent never sees the grooming verbs.
func materializeMoeHowtoSkill(workRoot, sessionCwd string) error {
	return writeSkill(workRoot, sessionCwd, "moe-howto", []byte(moe.MoeHowtoSkill()))
}

// writeSkill plants the rendered SKILL.md body under each backend's
// discovery root. workRoot/.codex/skills/ covers codex (its anchor-walk
// from cwd=workRoot); sessionCwd/.claude/skills/, when sessionCwd is
// non-empty, covers claude (its cwd-walkup from sessionCwd). Codex never
// sees sessionCwd — its cwd stays at workRoot.
func writeSkill(workRoot, sessionCwd, skillName string, body []byte) error {
	for _, dir := range skillMaterializeDirs {
		skillDir := filepath.Join(workRoot, dir, "skills", skillName)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return fmt.Errorf("skill: mkdir %s: %w", skillDir, err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), body, 0o644); err != nil {
			return fmt.Errorf("skill: write %s/SKILL.md: %w", skillDir, err)
		}
	}
	if sessionCwd != "" {
		skillDir := filepath.Join(sessionCwd, ".claude", "skills", skillName)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return fmt.Errorf("skill: mkdir %s: %w", skillDir, err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), body, 0o644); err != nil {
			return fmt.Errorf("skill: write %s/SKILL.md: %w", skillDir, err)
		}
	}
	return nil
}
