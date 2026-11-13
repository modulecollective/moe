package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

func designFixture(t *testing.T, seed string) (string, *run.Metadata, string) {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	fakeClaudeOnPath(t, `#!/bin/sh
set -eu
prompt=
next=0
for a in "$@"; do
 if [ "$next" = 1 ]; then prompt=$a; next=0; fi
 case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
cp "$canvas" "$MOE_TEST_DESIGN_OBSERVED"
case "$MOE_TEST_DESIGN_MODE" in
 progress) printf '\nUnresolved: the service contract needs investigation.\n' >> "$canvas" ;;
 stamp) sed -i '0,/"status":"blocked"/s//"status":"ready"/' "$canvas"
 printf '\n## Design review\n\nVerified the service contract; ready for build.\n' >> "$canvas" ;;
 restamp) cp "$MOE_TEST_DESIGN_ORIGINAL" "$canvas" ;;
 close) sed -i '0,/"status":"blocked"/s//"status":"close"/' "$canvas"
 printf '\nThe requested behavior already landed; checked the implementation.\n' >> "$canvas" ;;
esac
`)
	observed := filepath.Join(t.TempDir(), "observed.md")
	t.Setenv("MOE_TEST_DESIGN_OBSERVED", observed)
	t.Setenv("MOE_TEST_DESIGN_MODE", "progress")
	if code := runNew("sdlc", []string{"tele/design-gate"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("new exit %d", code)
	}
	md, err := run.Load(root, "tele", "design-gate")
	if err != nil {
		t.Fatal(err)
	}
	if seed != "" {
		writeStageCanvas(t, root, md, "design", seed)
		gittest.Run(t, root, "add", "-A")
		gittest.Run(t, root, "commit", "-m", "seed design")
	}
	return root, md, observed
}

func designTurn(t *testing.T, mode string) (int, string) {
	t.Helper()
	t.Setenv("MOE_TEST_DESIGN_MODE", mode)
	var out, errs bytes.Buffer
	code := openSdlcDesign("tele", "design-gate", true, "", &out, &errs)
	return code, out.String() + errs.String()
}

func TestDesignEntrySeedsPreservesAndResets(t *testing.T) {
	for _, tc := range []struct{ name, seed string }{
		{"fresh", ""},
		{"promoted", "# Promoted brief\n\nKeep the requested behavior.\n"},
		{"ready baked", readyDesign},
		{"stale close", strings.Replace(readyDesign, `"ready"`, `"close"`, 1)},
		{"reopened", stripCloseGateSection(strings.Replace(readyDesign, `"ready"`, `"close"`, 1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, md, observed := designFixture(t, tc.seed)
			if code, out := designTurn(t, "progress"); code != 0 {
				t.Fatalf("progress exit %d: %s", code, out)
			}
			before, err := os.ReadFile(observed)
			if err != nil {
				t.Fatal(err)
			}
			want := designCanvasSkeleton
			if tc.seed != "" {
				want = resetGate(tc.seed)
			}
			if string(before) != want {
				t.Fatalf("agent saw %q, want %q", before, want)
			}
			body, err := os.ReadFile(filepath.Join(root, run.ContentPath(md.Project, md.ID, "design")))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "Unresolved: the service contract") {
				t.Fatalf("blocked reasoning not reachable: %s", body)
			}
			wf, _ := LookupWorkflow("sdlc")
			if ok, err := wf.CheckStageGate(root, md, "design"); err != nil || ok {
				t.Fatalf("blocked gate=(%v,%v)", ok, err)
			}
			var out bytes.Buffer
			if code := promptNextStageOverride(root, md, "design", "review", true, &out, io.Discard); code != 0 || !strings.Contains(out.String(), "parked at design") {
				t.Fatalf("recovery tail %d: %s", code, &out)
			}
			if log := gitLog(t, root, "--format=%s", "--grep=^work: update design"); !strings.Contains(log, "work: update design") {
				t.Fatal("authored blocked explanation did not land")
			}
			if _, err := os.Stat(filepath.Join(root, run.ContentDir(md.Project, md.ID, "design"))); !os.IsNotExist(err) {
				t.Fatalf("design became directory form: %v", err)
			}
			// A later entry must reset the stamped design again, without replacing prose.
			if code, out := designTurn(t, "stamp"); code != 0 {
				t.Fatalf("ready exit %d: %s", code, out)
			}
			if code, out := designTurn(t, "progress"); code != 0 {
				t.Fatalf("resume exit %d: %s", code, out)
			}
			before, err = os.ReadFile(observed)
			if err != nil {
				t.Fatal(err)
			}
			if status, ok := stageGateStatus(string(before)); !ok || status != "blocked" || !strings.Contains(string(before), "Verified the service contract") {
				t.Fatalf("resume did not reset/preserve: %s", before)
			}
		})
	}
}

func TestDesignPreparationDoesNotCountAsOutput(t *testing.T) {
	for _, seed := range []string{"", readyDesign} {
		t.Run(map[bool]string{true: "fresh seed", false: "reset ready"}[seed == ""], func(t *testing.T) {
			root, _, _ := designFixture(t, seed)
			if code, out := designTurn(t, "nothing"); code == 0 || !strings.Contains(out, "unchanged since dispatch") || !strings.Contains(out, "content.md") {
				t.Fatalf("no-op exit %d: %s", code, out)
			}
			if log := gitLog(t, root, "--format=%s", "--grep=^work: update design"); strings.TrimSpace(log) != "" {
				t.Fatalf("preparation landed as work: %s", log)
			}
			if log := gitLog(t, root, "--format=%s", "--grep=^diagnostic: tele/design-gate design canvas unchanged"); strings.TrimSpace(log) == "" {
				t.Fatal("missing refusal diagnostic")
			}
		})
	}
}

func TestDesignResetThenRestoreIsNotNewConclusion(t *testing.T) {
	root, _, _ := designFixture(t, readyDesign)
	original := filepath.Join(t.TempDir(), "original.md")
	writeFile(t, original, readyDesign)
	t.Setenv("MOE_TEST_DESIGN_ORIGINAL", original)
	if code, out := designTurn(t, "restamp"); code == 0 || !strings.Contains(out, "unchanged") {
		t.Fatalf("restoring old bytes counted as new conclusion: exit %d %s", code, out)
	}
	if log := gitLog(t, root, "--format=%s", "--grep=^work: update design"); strings.TrimSpace(log) != "" {
		t.Fatalf("restamp-only landed as work: %s", log)
	}
}

func TestDesignRecoveryEntryAlsoPreparesAndGates(t *testing.T) {
	root, md, observed := designFixture(t, readyDesign)
	var out, errs bytes.Buffer
	code := openRecoveryStageSession(md, "design", "review", true, "review needs a design decision", &out, &errs)
	if code == 0 || !strings.Contains(errs.String(), "design not ready — parked at design") {
		t.Fatalf("recovery exit %d: %s %s", code, &out, &errs)
	}
	before, err := os.ReadFile(observed)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != resetGate(readyDesign) {
		t.Fatalf("recovery agent saw stale verdict: %s", before)
	}
	if log := gitLog(t, root, "--format=%s", "--grep=^work: update design"); strings.TrimSpace(log) == "" {
		t.Fatal("blocked recovery reason did not land")
	}
}

// Drive the public verb and the real registered close, including clone
// cleanup, with the actual template still carrying unfinished sections.
func TestDesignPublicCascadeReallyCloses(t *testing.T) {
	root, md, _ := designFixture(t, "")
	t.Setenv("MOE_TEST_DESIGN_MODE", "close")
	var out, errs bytes.Buffer
	if code := Run([]string{"sdlc", "design", "--once", "tele/design-gate"}, &out, &errs); code != 0 {
		t.Fatalf("public design --once exit %d: %s %s", code, &out, &errs)
	}
	got, err := run.Load(root, md.Project, md.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != run.StatusClosed {
		t.Fatalf("run status %s, output=%s", got.Status, &out)
	}
	if strings.Contains(out.String(), "cascade: build") {
		t.Fatalf("closed design dispatched build: %s", &out)
	}
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath(md.Project, md.ID, "design")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "already landed") {
		t.Fatal("close reasoning was lost")
	}
	if strings.Count(gitLog(t, root, "--format=%s", "--grep=^Close sdlc run tele/design-gate"), "Close sdlc run tele/design-gate") != 1 {
		t.Fatal("real close did not land exactly once")
	}
}

func TestDesignReopenRetainsReasoningAndStartsBlocked(t *testing.T) {
	root, _, _ := designFixture(t, "")
	t.Setenv("MOE_TEST_DESIGN_MODE", "close")
	var out, errs bytes.Buffer
	if code := Run([]string{"sdlc", "design", "--once", "tele/design-gate"}, &out, &errs); code != 0 {
		t.Fatalf("close exit %d: %s", code, &errs)
	}
	out.Reset()
	errs.Reset()
	if code := Run([]string{"sdlc", "reopen", "tele/design-gate"}, &out, &errs); code != 0 {
		t.Fatalf("reopen exit %d: %s", code, &errs)
	}
	reopened := loadDatedRunJSON(t, root, "tele", "design-gate")
	t.Setenv("MOE_TEST_DESIGN_MODE", "progress")
	if code := openSdlcDesign("tele", reopened.ID, true, "", &out, &errs); code != 0 {
		t.Fatalf("reopened design exit %d: %s", code, &errs)
	}
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", reopened.ID, "design")))
	if err != nil {
		t.Fatal(err)
	}
	if status, ok := stageGateStatus(string(body)); !ok || status != "blocked" {
		t.Fatalf("reopened gate=(%q,%v)", status, ok)
	}
	if !strings.Contains(string(body), "already landed") || !strings.Contains(string(body), "Unresolved:") {
		t.Fatalf("reopen reasoning lost: %s", body)
	}
}
