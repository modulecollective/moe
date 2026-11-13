package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

func assertCommitPaths(t *testing.T, root, ref string, want ...string) {
	t.Helper()
	got := strings.Fields(gittest.Output(t, root, "diff-tree", "--no-commit-id", "--name-only", "-r", ref))
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s paths = %v, want %v", ref, got, want)
	}
}

func TestHarvestOpenCommitsExcludeScratch(t *testing.T) {
	for _, state := range []string{"unstaged", "staged", "staged-and-edited"} {
		t.Run(state, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
			writeFollowups(t, root, "tele", "ship-it", "# Followups\n")
			writeTwinFeedback(t, root, "tele", "ship-it", "# Twin\n")
			writeLoreFeedback(t, root, "tele", "ship-it", "# Lore\n")
			if err := os.MkdirAll(filepath.Join(root, "lore"), 0o755); err != nil {
				t.Fatal(err)
			}
			existingLore := "lore/existing.md"
			if err := os.WriteFile(filepath.Join(root, existingLore), []byte("Original lore\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gittest.Commit(t, root, "scratch baseline")
			before := gittest.Output(t, root, "rev-parse", "HEAD")
			bodies := map[string]string{
				run.FollowupsPath("tele", "ship-it"):        "- [ ] `cleanup-foo` — Clean up foo\n  Followup body.\n",
				run.FeedbackPath("tele", "ship-it", "twin"): "- [ ] `correct-twin` — Correct the twin\n  Twin body.\n",
				run.FeedbackPath("tele", "ship-it", "lore"): "- [ ] `portable-fact` — Portable fact\n  applies-when: testing staged scratch\n\n  Lore body.\n",
				existingLore: "Updated lore\n",
			}
			for rel, body := range bodies {
				if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				if state != "unstaged" {
					gittest.Run(t, root, "add", "--", rel)
				}
				if state == "staged-and-edited" {
					body += "\n  Working-tree note.\n"
					bodies[rel] = body
					if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.WriteFile(filepath.Join(root, "stray"), []byte("untracked"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := harvestRunInProcess(root, "sdlc", "tele", "ship-it", true); err != nil {
				t.Fatal(err)
			}
			commits := strings.Fields(gittest.Output(t, root, "rev-list", "--reverse", before+"..HEAD"))
			if len(commits) != 3 {
				t.Fatalf("harvest commits = %v, want two opens and one harvest", commits)
			}
			for i, id := range []string{"cleanup-foo", "correct-twin"} {
				assertCommitPaths(t, root, commits[i], filepath.Join(run.Dir("tele", id), "run.json"), run.ContentPath("tele", id, "idea"))
				msg := gittest.Output(t, root, "show", "-s", "--format=%B", commits[i])
				for _, want := range []string{"Open run tele/" + id, "MoE-Run: " + id, "MoE-From-Run: tele/ship-it"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("open missing %q: %s", want, msg)
					}
				}
			}
			var finalPaths []string
			for rel, body := range bodies {
				finalPaths = append(finalPaths, rel)
				want := strings.TrimSpace(strings.ReplaceAll(body, "- [ ]", "- [x]"))
				if got := gittest.Output(t, root, "show", "HEAD:"+rel); got != want {
					t.Fatalf("committed %s = %q, want %q", rel, got, want)
				}
			}
			finalPaths = append(finalPaths, "lore/portable-fact.md")
			assertCommitPaths(t, root, "HEAD", finalPaths...)
			if got := gittest.Output(t, root, "show", "HEAD:lore/portable-fact.md"); !strings.Contains(got, "Lore body.") {
				t.Fatalf("promoted lore missing body: %s", got)
			}
			if got := gittest.Output(t, root, "show", "-s", "--format=%s", "HEAD"); !strings.HasPrefix(got, "harvest: capture follow-ups") {
				t.Fatalf("final commit is not harvest: %s", got)
			}
			if got := runStatus(t, root, "tele", "ship-it"); got != run.StatusInProgress {
				t.Fatalf("harvest changed run status: %s", got)
			}
			if got := gittest.Output(t, root, "status", "--porcelain"); got != "?? stray" {
				t.Fatalf("harvest left unexpected changes: %s", got)
			}
			if b, err := os.ReadFile(filepath.Join(root, "stray")); err != nil || string(b) != "untracked" {
				t.Fatalf("stray changed: %q, %v", b, err)
			}
			if err := harvestRunInProcess(root, "sdlc", "tele", "ship-it", true); err != nil {
				t.Fatalf("repeat harvest: %v", err)
			}
			if got := gittest.Output(t, root, "rev-parse", "HEAD"); got != commits[2] {
				t.Fatal("repeat harvest created a commit or duplicate idea")
			}
		})
	}
}

func TestCloseOpenCommitsExcludeStagedScratchAndPulseStamp(t *testing.T) {
	for _, workflow := range []string{"sdlc", "pulse"} {
		t.Run(workflow, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "ship-it", workflow, run.StatusInProgress)
			writeFollowups(t, root, "tele", "ship-it", "# Followups\n")
			canvasRel := run.ContentPath("tele", "ship-it", pulseDoc)
			if workflow == "pulse" {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, canvasRel)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, canvasRel), []byte("# Survey\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gittest.Commit(t, root, "scratch baseline")
			before := gittest.Output(t, root, "rev-parse", "HEAD")
			writeFollowups(t, root, "tele", "ship-it", "- [ ] `cleanup-foo` — Clean up foo\n  Followup body.\n")
			followupsRel := run.FollowupsPath("tele", "ship-it")
			gittest.Run(t, root, "add", "--", followupsRel)
			var errb bytes.Buffer
			var err error
			if workflow == "pulse" {
				err = closePulseRunStamped(root, "tele", "ship-it", "test stamp", func(body []byte) []byte {
					return append(body, []byte("\nSurvey complete.\n")...)
				}, &errb)
			} else {
				reg, ok := lookupCloseRegistration(workflow)
				if !ok {
					t.Fatal("missing close registration")
				}
				err = closeRunInProcess(root, workflow, reg.subject, reg.cleanup, "tele", "ship-it", true, &errb)
			}
			if err != nil {
				t.Fatalf("close: %v; stderr=%s", err, errb.String())
			}
			if got := gittest.Output(t, root, "rev-list", "--count", before+"..HEAD"); got != "2" {
				t.Fatalf("close commits = %s, want one open and one close", got)
			}
			assertCommitPaths(t, root, "HEAD~1", filepath.Join(run.Dir("tele", "cleanup-foo"), "run.json"), run.ContentPath("tele", "cleanup-foo", "idea"))
			finalPaths := []string{filepath.Join(run.Dir("tele", "ship-it"), "run.json"), followupsRel}
			if workflow == "pulse" {
				finalPaths = append(finalPaths, canvasRel)
				if got := gittest.Output(t, root, "show", "HEAD:"+canvasRel); got != "# Survey\n\nSurvey complete." {
					t.Fatalf("committed stamp = %q", got)
				}
			}
			assertCommitPaths(t, root, "HEAD", finalPaths...)
			if got := gittest.Output(t, root, "show", "HEAD:"+followupsRel); got != "- [x] `cleanup-foo` — Clean up foo\n  Followup body." {
				t.Fatalf("committed scratch = %q", got)
			}
			if got := gittest.Output(t, root, "show", "HEAD:"+finalPaths[0]); !strings.Contains(got, `"status": "closed"`) {
				t.Fatalf("close commit missing terminal status: %s", got)
			}
			if got := gittest.Output(t, root, "status", "--porcelain"); got != "" {
				t.Fatalf("close left changes: %s", got)
			}
		})
	}
}
