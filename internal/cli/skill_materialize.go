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

// skillMaterializeDirs lists the per-backend skill discovery roots both
// claude and codex walk for. Discovery rules differ — codex stops at
// the nearest .git anchor and claude walks unanchored further up — but
// both look in `.<backend>/skills/<skill-name>/SKILL.md`. The session
// worktree is a git worktree, which gives codex its anchor; claude
// reaches the same file via its more permissive walk.
var skillMaterializeDirs = []string{".claude", ".codex"}

// materializeMoeBureaucracySkill writes the moe-bureaucracy SKILL.md
// into the session worktree's .claude/skills/ and .codex/skills/ trees
// with the run-specific twin/lore/followups paths pre-substituted.
//
// Materialized fresh on every BuildSpec call; the paths are
// session-stable for the run but cheap to rewrite, and a refresh costs
// less than reasoning about staleness across resumes. Lives inside the
// session worktree so teardown is free: session.Close removes the
// worktree, taking the materialized skill with it. Never staged or
// committed — commitTurn only stages explicit pathspecs (docDir,
// runJSON, followups, feedback), and the worktree-root .claude/.codex
// dirs aren't on that list.
func materializeMoeBureaucracySkill(workRoot string, md *run.Metadata) error {
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
	body := buf.Bytes()
	for _, dir := range skillMaterializeDirs {
		skillDir := filepath.Join(workRoot, dir, "skills", "moe-bureaucracy")
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return fmt.Errorf("skill: mkdir %s: %w", skillDir, err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), body, 0o644); err != nil {
			return fmt.Errorf("skill: write %s/SKILL.md: %w", skillDir, err)
		}
	}
	return nil
}
