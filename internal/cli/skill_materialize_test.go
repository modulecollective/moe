package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/run"
)

// TestMoeBureaucracySkillEmbedded pins the //go:embed wiring. A
// silently broken embed directive (typo'd path, renamed file) would
// otherwise degrade to an empty skill body — the skill would land on
// disk with `{{.TwinFeedback}}`-style placeholders unsubstituted, and
// the agent would silently lose its trace-recording guidance.
func TestMoeBureaucracySkillEmbedded(t *testing.T) {
	body := moe.MoeBureaucracySkill()
	if body == "" {
		t.Fatal("MoeBureaucracySkill() is empty; //go:embed skills/... likely broken")
	}
	for _, want := range []string{
		"name: moe-bureaucracy",
		"{{.TwinFeedback}}",
		"{{.LoreFeedback}}",
		"{{.Followups}}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("embedded skill missing %q in body", want)
		}
	}
}

// TestMaterializeMoeBureaucracySkillBothBackends pins the on-disk
// shape both claude (.claude/skills/) and codex (.codex/skills/)
// expect. Dropping one of the two directories silently disables the
// skill for that backend.
func TestMaterializeMoeBureaucracySkillBothBackends(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	if err := materializeMoeBureaucracySkill(root, md); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	wantTwin := filepath.Join(root, run.FeedbackPath(md.Project, md.ID, "twin"))
	wantLore := filepath.Join(root, run.FeedbackPath(md.Project, md.ID, "lore"))
	wantFollowups := filepath.Join(root, run.FollowupsPath(md.Project, md.ID))

	for _, dir := range []string{".claude", ".codex"} {
		path := filepath.Join(root, dir, "skills", "moe-bureaucracy", "SKILL.md")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		got := string(body)
		// Frontmatter survives substitution.
		if !strings.Contains(got, "name: moe-bureaucracy") {
			t.Errorf("%s: missing name: frontmatter:\n%s", dir, got)
		}
		// Placeholders substituted, not left as raw template text.
		for _, raw := range []string{"{{.TwinFeedback}}", "{{.LoreFeedback}}", "{{.Followups}}"} {
			if strings.Contains(got, raw) {
				t.Errorf("%s: placeholder %q left unsubstituted:\n%s", dir, raw, got)
			}
		}
		// Absolute per-run paths land in the body.
		for _, want := range []string{wantTwin, wantLore, wantFollowups} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing substituted path %q:\n%s", dir, want, got)
			}
		}
	}
}

// TestMaterializeMoeBureaucracySkillIsIdempotent pins the cheap
// rewrite-each-turn behaviour. The materialiser runs on every
// BuildSpec call (including session resume); a second call must
// produce the same on-disk content as the first.
func TestMaterializeMoeBureaucracySkillIsIdempotent(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}

	if err := materializeMoeBureaucracySkill(root, md); err != nil {
		t.Fatalf("materialize (first): %v", err)
	}
	first, err := os.ReadFile(filepath.Join(root, ".claude", "skills", "moe-bureaucracy", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := materializeMoeBureaucracySkill(root, md); err != nil {
		t.Fatalf("materialize (second): %v", err)
	}
	second, err := os.ReadFile(filepath.Join(root, ".claude", "skills", "moe-bureaucracy", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("materialize is not idempotent across two calls:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}
