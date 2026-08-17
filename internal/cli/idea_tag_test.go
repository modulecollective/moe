package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// tagFixture captures one open idea in projectID and returns the
// bureaucracy root. The tag verbs are pure metadata edits, so the
// fixture needs nothing beyond a live idea to point at.
func tagFixture(t *testing.T, projectID, slug string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, projectID)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", projectID + "/" + slug}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	return root
}

func runJSON(t *testing.T, root, projectID, slug string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "projects", projectID, "runs", slug, "run.json"))
	if err != nil {
		t.Fatalf("run.json unreadable: %v", err)
	}
	return string(body)
}

// TestIdeaTagStampsPromoteToAndCommits is the happy path and the whole
// point of the verb: after `moe idea tag`, the idea carries the same
// promote_to a filer's `(sdlc)` followup tag would have written, so the
// pulse survey reads operator-tagged and machine-tagged ideas
// identically. Workflow defaults to sdlc.
func TestIdeaTagStampsPromoteToAndCommits(t *testing.T) {
	root := tagFixture(t, "moe", "ready-to-ride")

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "tag", "moe/ready-to-ride"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "tagged idea moe/ready-to-ride → sdlc") {
		t.Fatalf("missing tag confirmation: %q", out.String())
	}
	if md := runJSON(t, root, "moe", "ready-to-ride"); !strings.Contains(md, `"promote_to": "sdlc"`) {
		t.Fatalf("promote_to not stamped:\n%s", md)
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Tag idea moe/ready-to-ride (sdlc)") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: ready-to-ride",
		"MoE-Project: moe",
		"MoE-Workflow: idea",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}

	entries, err := git.Status(root)
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tree should be clean after tag, got:\n%v", entries)
	}
}

// TestIdeaUntagClearsPromoteTo: untag is the per-idea pause. The key
// leaves run.json entirely (omitempty), which is what makes an untagged
// idea indistinguishable from one that was never tagged — the pulse's
// fence keys on nothing else.
func TestIdeaUntagClearsPromoteTo(t *testing.T) {
	root := tagFixture(t, "moe", "second-thoughts")
	if code := Run([]string{"idea", "tag", "moe/second-thoughts"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup tag failed")
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "untag", "moe/second-thoughts"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "untagged idea moe/second-thoughts") {
		t.Fatalf("missing untag confirmation: %q", out.String())
	}
	if md := runJSON(t, root, "moe", "second-thoughts"); strings.Contains(md, "promote_to") {
		t.Fatalf("promote_to should be gone:\n%s", md)
	}
	if head := gitLog(t, root, "-1", "--format=%s"); !strings.Contains(head, "Untag idea moe/second-thoughts") {
		t.Fatalf("commit subject wrong: %q", head)
	}
}

// TestIdeaTagOverwritesExistingTag: re-tagging is a legitimate operator
// correction — a filer's `(twin)` nomination that should have been an
// sdlc run — so the verb overwrites rather than refusing.
func TestIdeaTagOverwritesExistingTag(t *testing.T) {
	root := tagFixture(t, "moe", "wrong-destination")
	if code := Run([]string{"idea", "tag", "moe/wrong-destination", "twin"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup twin tag failed")
	}
	if md := runJSON(t, root, "moe", "wrong-destination"); !strings.Contains(md, `"promote_to": "twin"`) {
		t.Fatalf("setup did not stamp twin:\n%s", md)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "tag", "moe/wrong-destination", "sdlc"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if md := runJSON(t, root, "moe", "wrong-destination"); !strings.Contains(md, `"promote_to": "sdlc"`) {
		t.Fatalf("promote_to not overwritten:\n%s", md)
	}
}

// TestIdeaTagRepeatIsANoOp: tagging what's already tagged says so and
// exits clean, leaving no empty commit behind. The operator re-running
// their own one-liner (or double-tapping the dash chip) shouldn't read
// as a failure.
func TestIdeaTagRepeatIsANoOp(t *testing.T) {
	root := tagFixture(t, "moe", "already-licensed")
	if code := Run([]string{"idea", "tag", "moe/already-licensed"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup tag failed")
	}
	before := gitLog(t, root, "-1", "--format=%H")

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "tag", "moe/already-licensed", "sdlc"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "already tagged → sdlc") {
		t.Fatalf("expected already-tagged notice, got: %q", out.String())
	}
	if after := gitLog(t, root, "-1", "--format=%H"); after != before {
		t.Fatalf("no-op tag minted a commit: %q → %q", before, after)
	}
}

// TestIdeaTagRefusesUnusableWorkflows: the tag the operator types has to
// clear the same bar as one a filer writes in followups.md — registered,
// chainable, staged — with the harvest's wording, so both surfaces
// refuse the same tags for the same stated reason.
func TestIdeaTagRefusesUnusableWorkflows(t *testing.T) {
	for _, tc := range []struct{ workflow, want string }{
		{"nosuchflow", "is not registered"},
		{"chat", "is not a staged, chainable workflow"},
		{"idea", "is not a staged, chainable workflow"},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			root := tagFixture(t, "moe", "picky")
			var out, errb bytes.Buffer
			code := Run([]string{"idea", "tag", "moe/picky", tc.workflow}, &out, &errb)
			if code != 2 {
				t.Fatalf("expected exit=2, got %d; stderr=%q", code, errb.String())
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Fatalf("expected %q in error, got: %q", tc.want, errb.String())
			}
			if md := runJSON(t, root, "moe", "picky"); strings.Contains(md, "promote_to") {
				t.Fatalf("refused tag should leave run.json alone:\n%s", md)
			}
		})
	}
}

// TestIdeaTagRefusesNonIdeaRun: the verb is idea-only. An intent is the
// nearest miss — same capture shape, same slug namespace — and it is
// never promoted, so a tag on one would be a license to nothing.
func TestIdeaTagRefusesNonIdeaRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	if code := Run([]string{"intent", "new", "moe/heading-there"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup intent failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "tag", "moe/heading-there"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero tagging an intent; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "is a intent run, not an idea") {
		t.Fatalf("expected not-an-idea error, got: %q", errb.String())
	}
}

// TestIdeaTagRefusesTerminalIdea: a closed idea is settled. Tagging one
// would park a license nothing can act on — the pulse only surveys live
// ideas — so refuse rather than write dead metadata.
func TestIdeaTagRefusesTerminalIdea(t *testing.T) {
	root := tagFixture(t, "moe", "done-with-it")
	if code := Run([]string{"idea", "close", "moe/done-with-it"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup close failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "tag", "moe/done-with-it"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero tagging a closed idea; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not open — refusing to change its tag") {
		t.Fatalf("expected terminal-idea refusal, got: %q", errb.String())
	}
	if md := runJSON(t, root, "moe", "done-with-it"); strings.Contains(md, "promote_to") {
		t.Fatalf("refused tag should leave run.json alone:\n%s", md)
	}
}

// TestIdeaTagRefusesDirtyWorkingTree: a stray edit would ride along on
// the tag commit — the same gate every other idea verb applies.
func TestIdeaTagRefusesDirtyWorkingTree(t *testing.T) {
	root := tagFixture(t, "moe", "needs-clean-tree")
	dirtyTracked(t, root)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "tag", "moe/needs-clean-tree"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on dirty tree; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "uncommitted changes") {
		t.Fatalf("expected dirty-tree error, got: %q", errb.String())
	}
}

// TestIdeaTagUsageErrors: wrong arity exits 2 on both verbs.
func TestIdeaTagUsageErrors(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	for _, args := range [][]string{
		{"idea", "tag"},
		{"idea", "tag", "moe/x", "sdlc", "extra"},
		{"idea", "untag"},
		{"idea", "untag", "moe/x", "sdlc"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 2 {
			t.Fatalf("%v: expected exit=2, got %d; stderr=%q", args, code, errb.String())
		}
	}
}

// TestOperatorTaggedIdeaRidesLikeAHarvestedTag is the continuity claim
// the whole design rests on: an operator's `moe idea tag` writes the
// same disk state a filer's `(sdlc)` followup tag writes, so it drives
// the identical ride. Same fake-claude pulse turn as
// TestTaggedFollowupHarvestPromoteGroomAndKick, but the tag arrives by
// hand on an idea filed without one — the two stranded ideas that
// prompted this run, replayed. The destination deliberately fails its
// first turn, so the pulse invocation must report that stalled ride.
func TestOperatorTaggedIdeaRidesLikeAHarvestedTag(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Filed untagged — exactly what a forgetful filer leaves behind.
	if _, err := createIdea(root, "moe", "tagged-by-hand", "# Apply the mechanical fix\n", "", trailers.Block{}); err != nil {
		t.Fatalf("createIdea: %v", err)
	}
	if code := Run([]string{"idea", "tag", "moe/tagged-by-hand"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("idea tag failed")
	}

	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
ticks=$(printf '\140\140\140')
case "$canvas" in
  */documents/pulse/content.md)
    printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok","threads":[{"runs":[{"slug":"tagged-by-hand","title":"Apply the mechanical fix","why":"the operator tagged it"}]}]}' "$ticks" > "$canvas"
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
`)

	var out, errb bytes.Buffer
	defer withRideMode(rideDynamic)()
	if code := runPulseSurvey(root, "moe", "" /*emitRun*/, nil, &out, &errb); code != 1 {
		t.Fatalf("pulse exit=%d, want failed child exit 1; stderr=%q", code, errb.String())
	}

	idea, err := run.Load(root, "moe", "tagged-by-hand")
	if err != nil {
		t.Fatal(err)
	}
	if idea.Status != run.StatusPromoted {
		t.Fatalf("idea status=%q, want promoted — the hand-stamped tag should license the promote", idea.Status)
	}
	var promoted *run.Metadata
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range mds {
		if md.Project == "moe" && md.Workflow == "sdlc" {
			promoted = md
		}
	}
	if promoted == nil || promoted.SpawnedBy == "" {
		t.Fatalf("promoted run = %+v, want machine lineage", promoted)
	}
	if !strings.Contains(errb.String(), "pulse: kicking moe/"+promoted.ID+" (dynamic)") {
		t.Fatalf("pulse never reached self-kick for the hand-tagged idea; stderr=%q", errb.String())
	}
}
