package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// buildFakeClaude puts a fake claude on PATH whose build turn is chosen
// by mode, read from $MOE_TEST_BUILD_MODE at run time so one install
// serves several turns:
//
//	stamp     overwrite both build canvases with stamped bodies
//	progress  append a progress note to code.md only
//	nothing   touch no canvas (the refusal case)
//
// File-form stages (design) get a marker appended to their canvas.
func buildFakeClaude(t *testing.T) {
	t.Helper()
	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
if [ -n "$canvas" ]; then printf 'design body\n' >> "$canvas"; exit 0; fi
case "$MOE_TEST_BUILD_MODE" in
stamp)
`+fakeBuildCanvasesSnippet("stamped by fake build")+`  ;;
progress)
  code=$(printf '%s' "$prompt" | awk '/Your canvases for this document are these files, all writable:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
  printf '\nProgress: parser half done.\n' >> "$code"
  ;;
esac
exit 0
`)
}

func buildFixture(t *testing.T, runID string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	buildFakeClaude(t)
	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/" + runID}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	if code := openSdlcDesign("tele", runID, true, "", &out, &errb); code != 0 {
		t.Fatalf("design exit=%d stderr=%q", code, errb.String())
	}
	return root
}

func buildTurn(t *testing.T, mode, runID string) (int, string) {
	t.Helper()
	t.Setenv("MOE_TEST_BUILD_MODE", mode)
	var out, errb bytes.Buffer
	code := openSdlcBuild("tele", runID, true, "", &out, &errb)
	return code, errb.String()
}

func readBuildFile(t *testing.T, root, runID, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, run.ContentFilePath("tele", runID, "build", name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// TestBuildTurnRefusesWhenNoCanvasChanged: a build turn that only got
// the harness's seeds and reset — no authored edit — is refused at the
// turn-output check, lands no work commit, and leaves a diagnostic.
func TestBuildTurnRefusesWhenNoCanvasChanged(t *testing.T) {
	root := buildFixture(t, "noop-build")
	code, stderr := buildTurn(t, "nothing", "noop-build")
	if code == 0 {
		t.Fatalf("a no-op build turn must fail; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "unchanged since dispatch") {
		t.Fatalf("expected the unchanged refusal, got %q", stderr)
	}
	if log := gitLog(t, root, "--format=%s", "--grep=^work: update build"); strings.TrimSpace(log) != "" {
		t.Fatalf("no build work turn may land: %s", log)
	}
	if log := gitLog(t, root, "--format=%s", "--grep=^diagnostic: tele/noop-build build canvas unchanged"); strings.TrimSpace(log) == "" {
		t.Fatalf("expected a diagnostic commit for the refusal")
	}
}

// TestBuildTurnPersistsBothCanvasesAndResetsOnResume walks the shape a
// build actually takes: a stamped turn, then a resume that only writes
// progress to code.md. Both turns commit under the build identity with
// no content.md; the resume's entry reset reblocks both files — test.md
// included, though the agent never touched it — so the stage parks
// until both are restamped.
func TestBuildTurnPersistsBothCanvasesAndResetsOnResume(t *testing.T) {
	root := buildFixture(t, "resume-build")
	if code, stderr := buildTurn(t, "stamp", "resume-build"); code != 0 {
		t.Fatalf("stamp turn exit=%d stderr=%q", code, stderr)
	}
	md, err := run.Load(root, "tele", "resume-build")
	if err != nil {
		t.Fatal(err)
	}
	wf, _ := LookupWorkflow("sdlc")
	if ok, err := wf.CheckStageGate(root, md, "build"); err != nil || !ok {
		problems, _ := wf.StageGateProblems(root, md, "build")
		t.Fatalf("stamped build gate = (%v, %v): %v", ok, err, problems)
	}
	if _, err := os.Stat(filepath.Join(root, run.ContentPath("tele", "resume-build", "build"))); !os.IsNotExist(err) {
		t.Fatalf("build must not grow a content.md: %v", err)
	}

	if code, stderr := buildTurn(t, "progress", "resume-build"); code != 0 {
		t.Fatalf("progress turn exit=%d stderr=%q", code, stderr)
	}
	codeMD := readBuildFile(t, root, "resume-build", "code.md")
	if !strings.Contains(codeMD, "Progress: parser half done.") {
		t.Fatalf("progress note did not persist: %q", codeMD)
	}
	for _, name := range []string{"code.md", "test.md"} {
		if status, ok := stageGateStatus(readBuildFile(t, root, "resume-build", name)); !ok || status != "blocked" {
			t.Fatalf("%s after resume = (%q, %v), want blocked by the entry reset", name, status, ok)
		}
	}
	if ok, _ := wf.CheckStageGate(root, md, "build"); ok {
		t.Fatal("a resumed build must not advance until restamped")
	}
	log := gitLog(t, root, "--format=%s%n%b", "--grep=^work: update build")
	if strings.Count(log, "work: update build") != 2 || !strings.Contains(log, "MoE-Document: build") {
		t.Fatalf("expected two build work turns under the build identity:\n%s", log)
	}
	if strings.Contains(gitLog(t, root, "--format=%b"), "MoE-Document: code") {
		t.Fatal("no turn may be recorded under the retired code identity")
	}
	// The committed tree carries exactly the two content files plus the
	// harness's transcript/prompt — nothing generated became a canvas.
	canvases, err := run.Canvases(root, "tele", "resume-build", "build")
	if err != nil || len(canvases) != 2 {
		t.Fatalf("canvases = %+v, %v", canvases, err)
	}
}

// TestBuildFirstEntrySeedsFromLegacyCanvases: an active run that started
// under the code/test ladder carries its code and test canvases into
// build's first turn, gains the gate the legacy code canvas never had,
// and keeps the legacy records intact.
func TestBuildFirstEntrySeedsFromLegacyCanvases(t *testing.T) {
	root := buildFixture(t, "legacy-build")
	legacyCode := "# Code\n\nlegacy implementation notes\n"
	legacyTest := "# Test\n\n## Gate\n\n```json\n{\"status\":\"ready\"}\n```\n\n## What was verified\n\nlegacy evidence\n"
	for doc, body := range map[string]string{"code": legacyCode, "test": legacyTest} {
		writeFile(t, filepath.Join(root, run.ContentPath("tele", "legacy-build", doc)), body)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "legacy canvases")

	if code, stderr := buildTurn(t, "progress", "legacy-build"); code != 0 {
		t.Fatalf("build exit=%d stderr=%q", code, stderr)
	}
	codeMD := readBuildFile(t, root, "legacy-build", "code.md")
	testMD := readBuildFile(t, root, "legacy-build", "test.md")
	if !strings.Contains(codeMD, "legacy implementation notes") || !strings.Contains(testMD, "legacy evidence") {
		t.Fatalf("legacy canvases not carried:\ncode.md=%q\ntest.md=%q", codeMD, testMD)
	}
	for name, body := range map[string]string{"code.md": codeMD, "test.md": testMD} {
		if status, ok := stageGateStatus(body); !ok || status != "blocked" {
			t.Fatalf("%s gate = (%q, %v), want blocked", name, status, ok)
		}
	}
	for doc, body := range map[string]string{"code": legacyCode, "test": legacyTest} {
		got, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", "legacy-build", doc)))
		if err != nil || string(got) != body {
			t.Fatalf("legacy %s record changed: %q, %v", doc, got, err)
		}
	}
	// Historical records stay readable through cat while sdlc is live.
	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "cat", "tele/legacy-build", "code"}, &out, &errb); code != 0 || out.String() != legacyCode {
		t.Fatalf("cat legacy code = %d %q %q", code, out.String(), errb.String())
	}
}

// TestCatRendersBuildCanvasesInOrderWithLabels: `moe sdlc cat … build`
// renders code.md then test.md under file labels, and refuses an
// ambiguous layout rather than choosing.
func TestCatRendersBuildCanvasesInOrderWithLabels(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	md, err := run.New(root, "tele", run.Options{ID: "cat-build", Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	writeBuildCanvas(t, root, md, "test.md", "# Test\n")
	writeBuildCanvas(t, root, md, "code.md", "# Code\n")
	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "cat", "tele/cat-build", "build"}, &out, &errb); code != 0 {
		t.Fatalf("cat exit=%d stderr=%q", code, errb.String())
	}
	want := "==> content/code.md <==\n\n# Code\n\n==> content/test.md <==\n\n# Test\n"
	if out.String() != want {
		t.Fatalf("cat =\n%q\nwant\n%q", out.String(), want)
	}
	writeFile(t, filepath.Join(root, run.ContentPath("tele", "cat-build", "build")), "stray\n")
	out.Reset()
	errb.Reset()
	if code := Run([]string{"sdlc", "cat", "tele/cat-build", "build"}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "ambiguous canvas layout") {
		t.Fatalf("ambiguous layout: exit=%d stderr=%q", code, errb.String())
	}
}
