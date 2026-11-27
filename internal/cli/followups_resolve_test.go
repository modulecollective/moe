package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// Real SDLC callers, including directory-form build, consume this fake's
// authored blocked canvas and mixed filings. Pulse gets an empty quiet gate.
// Feedback files are written once, so a repeat turn sees them checked.
const taggedFollowupAgent = `#!/bin/sh
set -eu
prompt=
next=0
for a in "$@"; do
 if [ "$next" = 1 ]; then prompt=$a; next=0; fi
 case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
if [ -z "$canvas" ]; then
 canvas=$(printf '%s' "$prompt" | awk '/Your canvases for this document are these files, all writable:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
 rundir=$(dirname "$(dirname "$(dirname "$(dirname "$canvas")")")")
else
 rundir=$(dirname "$(dirname "$(dirname "$canvas")")")
fi
case "$canvas" in
 */documents/pulse/content.md)
 ticks=$(printf '\140\140\140')
 printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok"}' "$ticks" > "$canvas"
 exit 0 ;;
esac
if [ "${MOE_TEST_TAGGED_MODE:-}" = no-output ]; then exit 0; fi
if [ "$(basename "$rundir")" != design-gate ]; then
 printf '\nPrerequisite design remains unresolved.\n' >> "$canvas"
 exit 0
fi
printf '%s' "$prompt" > "$MOE_TEST_TAGGED_PROMPT"
if [ "${MOE_TEST_TAGGED_MODE:-}" != unchanged ]; then
 printf '\nBlocked pending prerequisite evidence.\n' >> "$canvas"
fi
bt=$(printf '\140')
printf '%s\n' "- [ ] ${bt}required-fix${bt} (sdlc) — Required fix" '' '  Prerequisite for tele/design-gate. Verify the saved synthetic trace.' "- [ ] ${bt}needs-judgment${bt} — Needs judgment" '  The contract is undecided.' > "$rundir/followups.md"
mkdir -p "$rundir/feedback"
if [ ! -f "$rundir/feedback/lore.md" ]; then
 printf '%s\n' "- [ ] ${bt}portable-fact${bt} — Keep this fact" '' '  applies-when: a test needs a portable fact' '' '  The fact itself.' > "$rundir/feedback/lore.md"
fi
if [ ! -f "$rundir/feedback/twin.md" ]; then
 printf '%s\n' "- [ ] ${bt}twin-note${bt} — Keep this observation" > "$rundir/feedback/twin.md"
fi
case "${MOE_TEST_TAGGED_MODE:-}" in
 fail) exit 3 ;;
 interrupt) exit 130 ;;
 boundary)
 clone=$(printf '%s' "$prompt" | awk '/Your project.s source tree is exposed as an additional writable/ {getline; getline; gsub(/^ +| +$/, ""); print; exit}')
 printf 'boundary violation\n' >> "$clone/README.md" ;;
 harvest-fail) printf '%s\n' "- [ ] ${bt}missing/required-fix${bt} (sdlc) — Missing project" > "$rundir/followups.md" ;;
 hygiene|hygiene-clear)
 projectdir=$(dirname "$(dirname "$rundir")")
 kb="$projectdir/knowledge"
 if [ "${MOE_TEST_TAGGED_MODE:-}" = hygiene-clear ] && [ -f "$kb/index.md" ]; then
  printf '%s\n' '# Knowledge' '' '- [Contract](topics/contract.md)' > "$kb/index.md"
 else
  mkdir -p "$kb/topics"
  printf '%s\n' '# Knowledge' > "$kb/index.md"
  printf '%s\n' '# Contract' '' 'The bounded contract.' > "$kb/topics/contract.md"
 fi ;;
esac
`

func taggedTurnFixture(t *testing.T) (string, *run.Metadata) {
	t.Helper()
	root, md, _ := designFixture(t, "")
	t.Setenv("MOE_TEST_TAGGED_PROMPT", filepath.Join(t.TempDir(), "prompt"))
	t.Setenv("MOE_TEST_TAGGED_MODE", "")
	fakeClaudeOnPath(t, taggedFollowupAgent)
	return root, md
}

// harvestTurnEnd is the harvest a successful stage turn runs on exit.
func harvestTurnEnd(root string, md *run.Metadata) error {
	return harvestRunInProcess(root, md.Workflow, md.Project, md.ID)
}

// TestSDLCBlockedTurnHarvestsEverything: a successful sdlc turn, blocked or
// not, extracts every channel — tagged and untagged followups, lore and
// twin notes — and no harvest opens an editor.
func TestSDLCBlockedTurnHarvestsEverything(t *testing.T) {
	for _, stage := range []string{"design", "build", "review"} {
		t.Run(stage, func(t *testing.T) {
			root, md := taggedTurnFixture(t)
			// Any editor launch fails the test: no harvest pops one.
			t.Setenv("EDITOR", "sh -c 'echo editor-ran >&2; exit 9'")
			t.Setenv("VISUAL", "")
			if stage != "design" {
				writeStageCanvas(t, root, md, "design", readyDesign)
				if stage == "review" {
					writeStampedBuild(t, root, md)
				}
				gittest.Run(t, root, "add", "-A")
				gittest.Run(t, root, "commit", "-m", "seed prior canvases")
			}
			defer withRideMode(rideDynamic)()
			var out bytes.Buffer
			if code := openSdlcStage(stage, "tele", md.ID, true, &out, &out); code != 0 {
				t.Fatalf("%s exit %d: %s", stage, code, &out)
			}
			idea, err := run.Load(root, "tele", "required-fix")
			if err != nil {
				t.Fatalf("tagged idea absent after successful blocked %s turn: %v", stage, err)
			}
			if idea.Workflow != "idea" || idea.PromoteTo != "sdlc" {
				t.Fatalf("idea=%+v", idea)
			}
			b, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", idea.ID, "idea")))
			if err != nil || !strings.Contains(string(b), "Verify the saved synthetic trace.") {
				t.Fatalf("seed=%q, %v", b, err)
			}
			if strings.Contains(out.String(), "editor-ran") {
				t.Fatalf("a harvest opened an editor: %s", &out)
			}
			untagged, err := run.Load(root, "tele", "needs-judgment")
			if err != nil || untagged.PromoteTo != "" {
				t.Fatalf("untagged entry: %+v %v", untagged, err)
			}
			if p, id, err := run.FollowupDestination(root, "tele/"+md.ID+"/tele/needs-judgment"); err != nil || p != "tele" || id != "needs-judgment" {
				t.Fatalf("untagged receipt=%s/%s %v", p, id, err)
			}
			audit := readFollowups(t, root, "tele", md.ID)
			if !strings.Contains(audit, "- [x] `required-fix` (sdlc)") || !strings.Contains(audit, "- [x] `needs-judgment`") {
				t.Fatalf("audit=%s", audit)
			}
			for _, kind := range []string{"lore", "twin"} {
				b, err := os.ReadFile(filepath.Join(root, run.FeedbackPath("tele", md.ID, kind)))
				if err != nil || !strings.Contains(string(b), "[x]") {
					t.Fatalf("feedback %s=%q, %v", kind, b, err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "lore", "portable-fact.md")); err != nil {
				t.Fatalf("lore not promoted: %v", err)
			}
			twin, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", "twin-note", "idea")))
			if err != nil || !strings.Contains(string(twin), twinNoteProvenance("tele", md.ID)) {
				t.Fatalf("twin idea=%q %v", twin, err)
			}
			wf, _ := LookupWorkflow("sdlc")
			if ok, err := wf.CheckStageGate(root, md, stage); err != nil || ok {
				t.Fatalf("source gate=(%v,%v)", ok, err)
			}
			if runStatus(t, root, "tele", md.ID) != run.StatusInProgress {
				t.Fatal("source status changed")
			}
			p, id, err := run.FollowupDestination(root, "tele/"+md.ID+"/tele/required-fix")
			if err != nil || p != "tele" || id != idea.ID {
				t.Fatalf("receipt=%s/%s %v", p, id, err)
			}
			log := gitLog(t, root, "--format=%B", "--grep=^Open run tele/required-fix$")
			for _, want := range []string{"MoE-From-Run: tele/" + md.ID, "MoE-Followup-Key: tele/" + md.ID + "/tele/required-fix", "MoE-Consent: dynamic"} {
				if !strings.Contains(log, want) {
					t.Fatalf("missing %q: %s", want, log)
				}
			}
			openSHA := gitLog(t, root, "-1", "--format=%H", "--grep=^Open run tele/required-fix$")
			assertCommitPaths(t, root, strings.TrimSpace(openSHA), filepath.Join(run.Dir("tele", idea.ID), "run.json"), run.ContentPath("tele", idea.ID, "idea"))
			if paths := gitLog(t, root, "-1", "--format=", "--name-only"); !strings.Contains(paths, run.FollowupsPath("tele", md.ID)) {
				t.Fatalf("audit paths=%q", paths)
			}
			if msg := gitLog(t, root, "-1", "--format=%B"); !strings.Contains(msg, "MoE-Consent: dynamic") {
				t.Fatalf("audit consent=%s", msg)
			}
			prompt, err := os.ReadFile(os.Getenv("MOE_TEST_TAGGED_PROMPT"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"(sdlc)", "mechanical, bounded AND verifiable", "checked means extracted", "the pulse resumes this run",
				"belongs to another project", "should land whether or not this run does", "produces evidence"} {
				if !strings.Contains(string(prompt), want) {
					t.Errorf("rendered %s prompt missing %q", stage, want)
				}
			}
			for _, gone := range []string{"never resumes", "terminal or manual harvest"} {
				if strings.Contains(string(prompt), gone) {
					t.Errorf("rendered %s prompt still says %q", stage, gone)
				}
			}
			// The fake refiles the same original slug. Its receipt wins on resume.
			if code := openSdlcStage(stage, "tele", md.ID, true, &out, &out); code != 0 {
				t.Fatalf("repeat exit %d: %s", code, &out)
			}
			for _, slug := range []string{"required-fix", "needs-judgment"} {
				if n := strings.Count(gitLog(t, root, "--format=%s", "--grep=^Open run tele/"+slug), "Open run"); n != 1 {
					t.Fatalf("repeat opened %d %s ideas", n, slug)
				}
			}
			if st := gittest.Output(t, root, "status", "--porcelain"); st != "" {
				t.Fatalf("dirty retry: %s", st)
			}
		})
	}
}

func TestSDLCFailedTurnDoesNotExtract(t *testing.T) {
	for _, mode := range []string{"fail", "interrupt", "unchanged", "no-output", "boundary", "hygiene", "harvest-fail"} {
		t.Run(mode, func(t *testing.T) {
			root, md := taggedTurnFixture(t)
			t.Setenv("MOE_TEST_TAGGED_MODE", mode)
			var out bytes.Buffer
			code := openSdlcStage("design", "tele", md.ID, true, &out, &out)
			if code == 0 {
				t.Fatalf("%s succeeded: %s", mode, &out)
			}
			for _, slug := range []string{"required-fix", "needs-judgment", "twin-note"} {
				if _, err := run.Load(root, "tele", slug); err == nil {
					t.Fatalf("failed turn extracted %s", slug)
				}
			}
			if mode == "hygiene" && !strings.Contains(out.String(), "findings survived the fix turn") {
				t.Fatalf("did not drive the hygiene recovery: %s", &out)
			}
			if mode == "harvest-fail" {
				if !strings.Contains(out.String(), "harvest:") {
					t.Fatalf("no harvest error: %s", &out)
				}
				b, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", md.ID, "design")))
				if err != nil || !strings.Contains(string(b), "Blocked pending prerequisite") {
					t.Fatalf("committed evidence lost: %q %v", b, err)
				}
				if got := gitLog(t, root, "--format=%s", "--grep=^work: update design"); got == "" {
					t.Fatal("stage commit lost")
				}
				if _, err := os.Stat(filepath.Join(root, run.ContentDir("tele", md.ID, "build"))); !os.IsNotExist(err) {
					t.Fatalf("build started: %v", err)
				}
			}
		})
	}
}

func TestFollowupLiveOwners(t *testing.T) {
	for _, tag := range []string{" (sdlc)", ""} {
		for _, tc := range []struct {
			name                               string
			ids                                []string
			status, workflow, want, diagnostic string
		}{
			{"exact untagged idea", []string{"fix"}, run.StatusInProgress, "idea", "fix", ""},
			{"dated pushed", []string{"fix-2026-11-20"}, run.StatusPushed, "sdlc", "fix-2026-11-20", ""},
			{"similar prefix", []string{"fix-more"}, run.StatusInProgress, "sdlc", "fix", ""},
			{"ambiguous", []string{"fix", "fix-2026-11-20"}, run.StatusInProgress, "sdlc", "", "ambiguous live owners"},
			{"self", nil, "", "", "", "own source"},
		} {
			t.Run(tc.name+tag, func(t *testing.T) {
				root := seedCloseFixture(t, "tele", "source", "sdlc", run.StatusInProgress)
				md, _ := run.Load(root, "tele", "source")
				for _, id := range tc.ids {
					trailerstest.SeedRun(t, root, "tele", id, tc.workflow, tc.status)
				}
				slug := "fix"
				if tc.name == "self" {
					slug = "source"
				}
				writeFollowups(t, root, "tele", "source", fmt.Sprintf("- [ ] `%s`%s — A fix\n  Required evidence.\n", slug, tag))
				before := gitLog(t, root, "--format=%s", "--grep=^Open run")
				err := harvestTurnEnd(root, md)
				if tc.diagnostic != "" {
					if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
						t.Fatalf("error=%v", err)
					}
					if !strings.Contains(readFollowups(t, root, "tele", "source"), "[ ]") {
						t.Fatal("failed resolution marked extracted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(readFollowups(t, root, "tele", "source"), "[x] `"+tc.want+"`") {
					t.Fatal("wrong owner")
				}
				owner, err := run.Load(root, "tele", tc.want)
				if err != nil {
					t.Fatal(err)
				}
				if tc.name != "similar prefix" {
					if before != gitLog(t, root, "--format=%s", "--grep=^Open run") {
						t.Fatal("reuse created a run")
					}
					if owner.PromoteTo != "" {
						t.Fatal("reuse changed permission")
					}
				}
			})
		}
	}
}

func TestFollowupReceiptRetry(t *testing.T) {
	for _, state := range []string{"open", "promoted", "closed", "untagged", "untagged-entry"} {
		t.Run(state, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "source", "sdlc", run.StatusInProgress)
			md, _ := run.Load(root, "tele", "source")
			trailerstest.SeedRun(t, root, "tele", "fix", "idea", run.StatusClosed)
			original := "- [ ] `fix` (sdlc) — A fix\n  Original contract.\n"
			if state == "untagged-entry" {
				// Every-turn harvest gives an untagged entry the same receipt.
				original = "- [ ] `fix` — A fix\n  Original contract.\n"
			}
			writeFollowups(t, root, "tele", "source", original)
			gittest.Run(t, root, "add", run.FollowupsPath("tele", "source"))
			gittest.Run(t, root, "commit", "-m", "file tagged work")
			installCreateFailureHook(t, root, `case "$(head -n 1 "$1")" in 'harvest: capture follow-ups'*) echo reject-audit >&2; exit 1 ;; esac`)
			if err := harvestTurnEnd(root, md); err == nil || !strings.Contains(err.Error(), "reject-audit") {
				t.Fatalf("audit failure=%v", err)
			}
			p, id, err := run.FollowupDestination(root, "tele/source/tele/fix")
			if err != nil || p != "tele" || id != "fix-2" {
				t.Fatalf("open receipt=%s/%s %v", p, id, err)
			}
			// Simulate losing the uncommitted audit on crash, then change the idea's
			// lifecycle before retry. The journal alone must recover the -2 allocation.
			gittest.Run(t, root, "config", "--unset", "core.hooksPath")
			writeFollowups(t, root, "tele", "source", original)
			gittest.Run(t, root, "add", run.FollowupsPath("tele", "source"))
			if state == "promoted" {
				t.Setenv("MOE_HOME", root)
				var out bytes.Buffer
				if code := runNew("sdlc", []string{"--from-idea", "tele/fix-2", "--park"}, &out, &out); code != 0 {
					t.Fatalf("promotion=%d %s", code, &out)
				}
			} else if state == "closed" {
				t.Setenv("MOE_HOME", root)
				var out bytes.Buffer
				if code := Run([]string{"idea", "close", "tele/fix-2"}, &out, &out); code != 0 {
					t.Fatalf("close=%d %s", code, &out)
				}
			} else if state == "untagged" {
				t.Setenv("MOE_HOME", root)
				var out bytes.Buffer
				if code := Run([]string{"idea", "untag", "tele/fix-2"}, &out, &out); code != 0 {
					t.Fatalf("untag=%d %s", code, &out)
				}
			}
			// A fresh matching live owner must not redirect the receipt.
			trailerstest.SeedRun(t, root, "tele", "fix-2026-11-22", "sdlc", run.StatusInProgress)
			// Edits under the same slug do not request another extraction.
			writeFollowups(t, root, "tele", "source", strings.Replace(original, "Original contract.", "Edited prose.", 1))
			if err := harvestTurnEnd(root, md); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(readFollowups(t, root, "tele", "source"), "[x] `fix-2`") {
				t.Fatal("receipt not reused")
			}
			b, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", "fix-2", "idea")))
			if err != nil || !strings.Contains(string(b), "Original contract.") {
				t.Fatalf("seed overwritten=%q %v", b, err)
			}
			if _, err := run.Load(root, "tele", "fix-3"); err == nil {
				t.Fatal("retry created fix-3")
			}
			if state == "untagged" {
				idea, err := run.Load(root, "tele", "fix-2")
				if err != nil || idea.PromoteTo != "" {
					t.Fatalf("receipt restored a removed tag: %+v %v", idea, err)
				}
			}
			// Exercise receipt resolution in full/manual and terminal harvest,
			// rather than proving only their checked-line no-op.
			writeFollowups(t, root, "tele", "source", original)
			if err := harvestRunInProcess(root, "sdlc", "tele", "source"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(readFollowups(t, root, "tele", "source"), "[x] `fix-2`") {
				t.Fatal("manual harvest did not reuse the receipt")
			}
			writeFollowups(t, root, "tele", "source", original)
			t.Setenv("MOE_HOME", root)
			if code := Run([]string{"sdlc", "close", "tele/source"}, io.Discard, io.Discard); code != 0 {
				t.Fatalf("terminal harvest exit=%d", code)
			}
			if !strings.Contains(readFollowups(t, root, "tele", "source"), "[x] `fix-2`") {
				t.Fatal("terminal harvest did not reuse the receipt")
			}
			if st := gittest.Output(t, root, "status", "--porcelain"); st != "" {
				t.Fatalf("retry dirty: %s", st)
			}
		})
	}
}

func TestFollowupCrossProjectAndReceiptErrors(t *testing.T) {
	root := seedCloseFixture(t, "tele", "source", "sdlc", run.StatusInProgress)
	md, _ := run.Load(root, "tele", "source")
	trailerstest.SeedRun(t, root, "other", "existing", "idea", run.StatusClosed)
	writeFollowups(t, root, "tele", "source", "- [ ] `other/fix` (sdlc) — Cross-project fix\n  A bounded contract.\n")
	if err := harvestTurnEnd(root, md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFollowups(t, root, "tele", "source"), "[x] `other/fix`") {
		t.Fatal("prefix lost")
	}
	p, id, err := run.FollowupDestination(root, "tele/source/other/fix")
	if err != nil || p != "other" || id != "fix" {
		t.Fatalf("receipt=%s/%s %v", p, id, err)
	}
	// Existing source audit reset to exercise the same resolver in full harvest.
	writeFollowups(t, root, "tele", "source", "- [ ] `other/fix` (sdlc) — Again\n")
	gittest.Run(t, root, "rm", "-r", run.Dir("other", "fix"))
	gittest.Run(t, root, "commit", "-m", "remove recorded destination")
	if err := harvestRunInProcess(root, "sdlc", "tele", "source"); err == nil || !strings.Contains(err.Error(), "missing or unreadable") {
		t.Fatalf("missing target=%v", err)
	}
}

func TestFollowupReuseAuditFailure(t *testing.T) {
	root := seedCloseFixture(t, "tele", "source", "sdlc", run.StatusInProgress)
	md, _ := run.Load(root, "tele", "source")
	trailerstest.SeedRun(t, root, "tele", "fix", "idea", run.StatusInProgress)
	writeFollowups(t, root, "tele", "source", "- [ ] `fix` (sdlc) — Existing owner\n")
	installCreateFailureHook(t, root, "echo reject-audit >&2\nexit 1\n")
	if err := harvestTurnEnd(root, md); err == nil {
		t.Fatal("failed audit reported success")
	}
	if got := gitLog(t, root, "--format=%s", "--grep=^Open run"); strings.Contains(got, "fix-2") {
		t.Fatal("reuse opened duplicate")
	}
	gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	// No receipt exists for a reused owner. A failed audit must leave the
	// source unchecked so a retry re-judges live ownership.
	if !strings.Contains(readFollowups(t, root, "tele", "source"), "[ ] `fix`") {
		t.Fatal("failed link left a checked source")
	}
	if err := harvestTurnEnd(root, md); err != nil {
		t.Fatal(err)
	}
	if st := gittest.Output(t, root, "status", "--porcelain"); st != "" {
		t.Fatalf("retry dirty: %s", st)
	}
}

func TestFollowupHarvestDoesNotStageUnrelatedFiles(t *testing.T) {
	root := seedCloseFixture(t, "tele", "source", "sdlc", run.StatusInProgress)
	md, _ := run.Load(root, "tele", "source")
	writeFollowups(t, root, "tele", "source", "- [ ] `fix` (sdlc) — Fix\n")
	if err := os.WriteFile(filepath.Join(root, "unrelated"), []byte("keep out"), 0600); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "unrelated")
	if err := harvestTurnEnd(root, md); err == nil {
		t.Fatal("accepted unrelated staged change")
	}
	if got := gitLog(t, root, "-1", "--format=", "--name-only"); strings.Contains(got, "unrelated") {
		t.Fatal("staged unrelated change")
	}
}

func TestFollowupFailedLinkRejudgesOwner(t *testing.T) {
	for _, mode := range []string{"early", "manual", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "source", "sdlc", run.StatusInProgress)
			md, _ := run.Load(root, "tele", "source")
			t.Setenv("MOE_HOME", root)
			trailerstest.SeedRun(t, root, "tele", "fix", "idea", run.StatusInProgress)
			// An earlier untagged creation must survive a later failed link:
			// its receipt resolves it on retry rather than minting later-2.
			original := "- [ ] `later` — Needs judgment\n  An unresolved contract.\n- [ ] `fix` (sdlc) — Existing owner\n"
			writeFollowups(t, root, "tele", "source", original)
			installCreateFailureHook(t, root, `case "$(head -n 1 "$1")" in 'harvest: link follow-up'*) echo reject-link >&2; exit 1 ;; esac`)
			harvest := func() error {
				if mode == "early" {
					return harvestTurnEnd(root, md)
				}
				verb := "harvest"
				if mode == "terminal" {
					verb = "close"
				}
				var out bytes.Buffer
				if code := Run([]string{"sdlc", verb, "tele/source"}, &out, &out); code != 0 {
					return fmt.Errorf("exit %d: %s", code, &out)
				}
				return nil
			}
			if err := harvest(); err == nil || !strings.Contains(err.Error(), "reject-link") {
				t.Fatalf("audit failure=%v", err)
			}
			if !strings.Contains(readFollowups(t, root, "tele", "source"), "[ ] `fix`") || runStatus(t, root, "tele", "source") != run.StatusInProgress {
				t.Fatal("failed link became durable or closed its source")
			}
			gittest.Run(t, root, "config", "--unset", "core.hooksPath")
			if code := Run([]string{"idea", "close", "tele/fix"}, io.Discard, io.Discard); code != 0 {
				t.Fatalf("close old owner=%d", code)
			}
			if err := harvest(); err != nil {
				t.Fatal(err)
			}
			if _, err := run.Load(root, "tele", "fix-2"); err != nil {
				t.Fatalf("retry did not re-judge the now-terminal owner: %v", err)
			}
			if !strings.Contains(readFollowups(t, root, "tele", "source"), "[x] `fix-2`") {
				t.Fatal("retry did not link the new owner")
			}
			if _, err := run.Load(root, "tele", "later-2"); err == nil {
				t.Fatal("failed later link duplicated an earlier untagged creation")
			}
		})
	}
}

func TestFollowupFailedLinkPreservesSourceIndex(t *testing.T) {
	root := seedCloseFixture(t, "tele", "source", "sdlc", run.StatusInProgress)
	md, _ := run.Load(root, "tele", "source")
	trailerstest.SeedRun(t, root, "tele", "fix", "idea", run.StatusInProgress)
	writeFollowups(t, root, "tele", "source", "# Followups\n")
	gittest.Commit(t, root, "baseline scratch")
	staged := "- [ ] `fix` (sdlc) — Staged title\n"
	working := staged + "  Working context.\n"
	writeFollowups(t, root, "tele", "source", staged)
	rel := run.FollowupsPath("tele", "source")
	gittest.Run(t, root, "add", rel)
	writeFollowups(t, root, "tele", "source", working)
	installCreateFailureHook(t, root, "echo reject-link >&2\nexit 1\n")
	if err := harvestTurnEnd(root, md); err == nil {
		t.Fatal("failed audit reported success")
	}
	if got := gittest.Output(t, root, "show", ":"+rel); got != strings.TrimSpace(staged) {
		t.Fatalf("staged bytes changed: %q", got)
	}
	if got := readFollowups(t, root, "tele", "source"); got != working {
		t.Fatalf("working bytes changed: %q", got)
	}
}

func TestSDLCHygieneRecoveryExtractsOnlyAfterRescan(t *testing.T) {
	root, md := taggedTurnFixture(t)
	t.Setenv("MOE_TEST_TAGGED_MODE", "hygiene-clear")
	var out bytes.Buffer
	if code := openSdlcStage("design", "tele", md.ID, true, &out, &out); code != 0 {
		t.Fatalf("recovered turn %d: %s", code, &out)
	}
	if !strings.Contains(out.String(), "project-doc hygiene (headless fix turn)") {
		t.Fatalf("did not drive hygiene recovery: %s", &out)
	}
	if _, err := run.Load(root, "tele", "required-fix"); err != nil {
		t.Fatalf("recovered turn did not extract: %v", err)
	}
}

func TestBlockedSourceTaggedFollowupPulseHandoff(t *testing.T) {
	root, md := taggedTurnFixture(t)
	var out bytes.Buffer
	if code := openSdlcStage("design", "tele", md.ID, true, &out, &out); code != 0 {
		t.Fatalf("source turn %d: %s", code, &out)
	}
	// Assert before pulse: otherwise promotion fixtures could mask absent extraction.
	if _, err := run.Load(root, "tele", "required-fix"); err != nil {
		t.Fatalf("idea absent before pulse: %v", err)
	}
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	// Promotion is the pick's, so the survey names the idea.
	prevOpen := openPulse
	openPulse = func(projectID, runID string, _ bool, _ *pulseInterrupt, admission *pulseAdmission, _, _ io.Writer) surveyOutcome {
		writePulseGate(t, root, projectID, runID, `{"status":"ok","next":{"run":"tele/required-fix","why":"the prerequisite"}}`)
		return surveyOutcome{code: 0, agentStarted: true}
	}
	t.Cleanup(func() { openPulse = prevOpen })
	if code := runPulseSurvey(root, project.RootID, "", nil, &out, &out); code != 0 {
		t.Fatalf("quiet non-dynamic pulse %d: %s", code, &out)
	}
	idea, err := run.Load(root, "tele", "required-fix")
	if err != nil || idea.Status != run.StatusInProgress {
		t.Fatalf("non-dynamic pulse moved idea: %+v %v", idea, err)
	}
	defer withRideMode(rideDynamic)()
	// A park at a gate is a clean ride for the sweep's exit (no backoff).
	if code := runPulseSurvey(root, project.RootID, "", nil, &out, &out); code != 0 || !strings.Contains(out.String(), "design gate not satisfied; parked at design") || !strings.Contains(out.String(), "parked at its design gate") {
		t.Fatalf("quiet dynamic pulse should park at the prerequisite design gate, exit %d: %s", code, &out)
	}
	idea, err = run.Load(root, "tele", "required-fix")
	if err != nil || idea.Status != run.StatusPromoted {
		t.Fatalf("dynamic pulse left idea: %+v %v; %s", idea, err, &out)
	}
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var dest *run.Metadata
	for _, candidate := range mds {
		if candidate.Project == "tele" && candidate.Workflow == "sdlc" && candidate.ID != md.ID {
			if dest != nil {
				t.Fatal("multiple prerequisite runs")
			}
			dest = candidate
		}
	}
	if dest == nil {
		t.Fatalf("no promoted run: %s", &out)
	}
	b, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", dest.ID, "design")))
	if err != nil || !strings.Contains(string(b), "Prerequisite design remains unresolved.") {
		t.Fatalf("prerequisite design not driven: %q %v; %s", b, err, &out)
	}
	wf, _ := LookupWorkflow("sdlc")
	for _, source := range []*run.Metadata{md, dest} {
		if ok, err := wf.CheckStageGate(root, source, "design"); err != nil || ok {
			t.Fatalf("%s gate=(%v,%v)", source.ID, ok, err)
		}
		if _, err := os.Stat(filepath.Join(root, run.ContentDir("tele", source.ID, "build"))); !os.IsNotExist(err) {
			t.Fatalf("%s build started: %v", source.ID, err)
		}
	}
	if got := gitLog(t, root, "--format=%s", "--all-match", "--grep=^work: update design$", "--grep=^MoE-Run: design-gate$"); strings.Count(got, "work:") != 1 {
		t.Fatalf("source reran: %s", got)
	}
}

// TestTerminalHarvestNeverOpensAnEditor: close extracts unchecked entries in
// all three files as written, with an editor that fails the test if run.
func TestTerminalHarvestNeverOpensAnEditor(t *testing.T) {
	root := seedCloseFixture(t, "tele", "source", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("EDITOR", "sh -c 'echo editor-ran >&2; exit 9'")
	t.Setenv("VISUAL", "")
	writeFollowups(t, root, "tele", "source", "- [ ] `later` — Needs judgment\n  An open contract.\n")
	writeLoreFeedback(t, root, "tele", "source", "- [ ] `closing-fact` — A fact\n\n  applies-when: always\n\n  The fact.\n")
	writeTwinFeedback(t, root, "tele", "source", "- [ ] `closing-note` — A twin note\n")
	var out bytes.Buffer
	if code := Run([]string{"sdlc", "close", "tele/source"}, &out, &out); code != 0 || strings.Contains(out.String(), "editor-ran") {
		t.Fatalf("close exit=%d: %s", code, &out)
	}
	for _, slug := range []string{"later", "closing-note"} {
		if _, err := run.Load(root, "tele", slug); err != nil {
			t.Fatalf("%s not extracted: %v", slug, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "lore", "closing-fact.md")); err != nil {
		t.Fatalf("lore not promoted: %v", err)
	}
}
