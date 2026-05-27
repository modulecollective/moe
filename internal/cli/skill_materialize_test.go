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

// TestMoeContextSkillEmbedded pins the //go:embed wiring for the
// sibling moe-context skill. Same rationale as the bureaucracy
// embedded test: a silently broken embed directive would leave the
// rendered skill body empty or full of raw `{{...}}` placeholders.
func TestMoeContextSkillEmbedded(t *testing.T) {
	body := moe.MoeContextSkill()
	if body == "" {
		t.Fatal("MoeContextSkill() is empty; //go:embed skills/... likely broken")
	}
	for _, want := range []string{
		"name: moe-context",
		"{{.Project}}",
		"{{.Run}}",
		"{{.BureaucracyRoot}}",
		"{{.ClonePath}}",
		"{{if .HasClone}}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("embedded skill missing %q in body", want)
		}
	}
}

// TestMaterializeMoeContextSkillBothBackends pins the on-disk shape
// both claude (.claude/skills/) and codex (.codex/skills/) expect for
// the moe-context skill, in the sandbox-bearing case (clonePath
// non-empty). Mirrors the bureaucracy backends test.
func TestMaterializeMoeContextSkillBothBackends(t *testing.T) {
	root := t.TempDir()
	clone := "/tmp/clone-fixture/moe-tele"
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	if err := materializeMoeContextSkill(root, md, clone); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	for _, dir := range []string{".claude", ".codex"} {
		path := filepath.Join(root, dir, "skills", "moe-context", "SKILL.md")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		got := string(body)
		if !strings.Contains(got, "name: moe-context") {
			t.Errorf("%s: missing name: frontmatter:\n%s", dir, got)
		}
		// Placeholders substituted, not left as raw template text.
		for _, raw := range []string{
			"{{.Project}}", "{{.Run}}", "{{.BureaucracyRoot}}",
			"{{.ClonePath}}", "{{.HasClone}}", "{{if",
		} {
			if strings.Contains(got, raw) {
				t.Errorf("%s: placeholder %q left unsubstituted:\n%s", dir, raw, got)
			}
		}
		// Per-run substitutions land in the body verbatim.
		for _, want := range []string{md.Project, md.ID, root, clone} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing substituted value %q:\n%s", dir, want, got)
			}
		}
	}
}

// TestMaterializeMoeContextSkillDocumentOnly pins the no-clone branch
// — the conditional clone-path bullet must omit (no clone path
// rendered) and instead surface the "document-only" prose. Without
// this, a regression in the template could leak an empty clone path
// like “  “ into the rendered body.
func TestMaterializeMoeContextSkillDocumentOnly(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "design-only", Project: "tele", Workflow: "sdlc"}
	if err := materializeMoeContextSkill(root, md, ""); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, ".claude", "skills", "moe-context", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "No project source clone") {
		t.Errorf("document-only branch not rendered:\n%s", got)
	}
	// The bureaucracy root must still substitute even without a clone.
	if !strings.Contains(got, root) {
		t.Errorf("bureaucracy root missing in document-only render:\n%s", got)
	}
}

// TestMaterializeMoeContextSkillIsIdempotent pins the rewrite-each-turn
// behaviour for the context skill — same shape as the bureaucracy
// idempotency test.
func TestMaterializeMoeContextSkillIsIdempotent(t *testing.T) {
	root := t.TempDir()
	clone := "/tmp/clone-fixture/moe-tele"
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}

	if err := materializeMoeContextSkill(root, md, clone); err != nil {
		t.Fatalf("materialize (first): %v", err)
	}
	first, err := os.ReadFile(filepath.Join(root, ".claude", "skills", "moe-context", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := materializeMoeContextSkill(root, md, clone); err != nil {
		t.Fatalf("materialize (second): %v", err)
	}
	second, err := os.ReadFile(filepath.Join(root, ".claude", "skills", "moe-context", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("materialize is not idempotent across two calls:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}
