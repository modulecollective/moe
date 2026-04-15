package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/request"
)

// newTestBureaucracy initializes a throwaway git repo with scoped git config,
// so commits can happen without polluting ~/.gitconfig. Returns the root path.
func newTestBureaucracy(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\temail=t@example.com\n\tname=T\n[init]\n\tdefaultBranch=main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"commit", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return root
}

func writeStageDesign(t *testing.T, root, body string) {
	t.Helper()
	writeStageFile(t, root, "design.md", body)
}

func writeStageFile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "stages")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSoul(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "soul.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// signStage records a MoE-Stage-Signed: <name> trailer for requestID, the same
// way `moe sign` does. Used to move the request past the design stage in tests.
func signStage(t *testing.T, root, requestID, name string) {
	t.Helper()
	commitTrailer(t, root, "sign: "+name, "MoE-Request: "+requestID+"\nMoE-Stage-Signed: "+name, time.Time{})
}

// signStageAt is signStage with an explicit committer date. Banner detection
// compares git committer epochs (%ct, integer seconds), so tests that exercise
// "since last turn" need controlled ordering — two commits made in the same
// second look simultaneous to the comparison.
func signStageAt(t *testing.T, root, requestID, name string, when time.Time) {
	t.Helper()
	commitTrailer(t, root, "sign: "+name, "MoE-Request: "+requestID+"\nMoE-Stage-Signed: "+name, when)
}

// commitWorkTurnAt records a `work: update <docID>` commit with the trailers
// commitTurn writes in production, dated to when. Returns HEAD's SHA so the
// caller can assert it appears in the banner.
func commitWorkTurnAt(t *testing.T, root, requestID, docID string, when time.Time) string {
	t.Helper()
	commitTrailer(t, root, "work: update "+docID,
		"MoE-Request: "+requestID+"\nMoE-Document: "+docID, when)
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func commitTrailer(t *testing.T, root, subject, trailers string, when time.Time) {
	t.Helper()
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", subject+"\n\n"+trailers+"\n")
	cmd.Dir = root
	if !when.IsZero() {
		stamp := when.Format(time.RFC3339)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+stamp,
			"GIT_COMMITTER_DATE="+stamp,
		)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func TestBuildSystemPromptInjectsDesignFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	writeStageDesign(t, root, "# Stage: design\n\nresist over-specifying.\n")

	md := &request.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "resist over-specifying.") {
		t.Fatalf("prompt missing design fragment:\n%s", got)
	}
	if !strings.Contains(got, "\n---\n") {
		t.Fatalf("prompt missing fragment separator:\n%s", got)
	}
}

func TestBuildSystemPromptSwitchesToCodeFragmentAfterDesignSigned(t *testing.T) {
	root := newTestBureaucracy(t)
	writeStageDesign(t, root, "DESIGN-BODY")
	writeStageFile(t, root, "code.md", "CODE-BODY")
	signStage(t, root, "fix-it", "design")

	md := &request.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "CODE-BODY") {
		t.Fatalf("expected code fragment after design signed:\n%s", got)
	}
	if strings.Contains(got, "DESIGN-BODY") {
		t.Fatalf("design fragment should drop out once design is signed:\n%s", got)
	}
}

func TestBuildSystemPromptOmitsFragmentAfterCodeSigned(t *testing.T) {
	root := newTestBureaucracy(t)
	writeStageDesign(t, root, "design guidance body")
	writeStageFile(t, root, "code.md", "code guidance body")
	// Realistic terminal state: every stage signed in order. `moe sign`
	// refuses to sign `code` without `design` first (sign.go preconditions),
	// so exercising "code-signed-in-isolation" would test a journal state
	// the CLI cannot produce.
	signStage(t, root, "fix-it", "design")
	signStage(t, root, "fix-it", "code")

	md := &request.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "design guidance body") {
		t.Fatalf("design fragment should drop out once code is signed:\n%s", got)
	}
	if strings.Contains(got, "code guidance body") {
		t.Fatalf("code fragment should drop out once code is signed:\n%s", got)
	}
}

func TestBuildSystemPromptMissingFragmentIsNotAnError(t *testing.T) {
	root := newTestBureaucracy(t)
	// no soul.md, no stages/design.md written
	md := &request.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "Your canvas for this document") {
		t.Fatalf("core prompt missing:\n%s", got)
	}
	if strings.Contains(got, "\n---\n") {
		t.Fatalf("no fragment, no separator expected:\n%s", got)
	}
}

func TestBuildSystemPromptInjectsSoul(t *testing.T) {
	root := newTestBureaucracy(t)
	writeSoul(t, root, "# Soul\n\ndo the thing that's asked.\n")

	md := &request.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "do the thing that's asked.") {
		t.Fatalf("prompt missing soul content:\n%s", got)
	}
}

func TestBuildSystemPromptOrdersSoulBeforeStageBeforeOperational(t *testing.T) {
	root := newTestBureaucracy(t)
	writeSoul(t, root, "SOUL-MARKER")
	writeStageDesign(t, root, "STAGE-MARKER")

	md := &request.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatal(err)
	}
	soulIdx := strings.Index(got, "SOUL-MARKER")
	stageIdx := strings.Index(got, "STAGE-MARKER")
	opIdx := strings.Index(got, "You are collaborating")
	if soulIdx < 0 || stageIdx < 0 || opIdx < 0 {
		t.Fatalf("missing section(s) soul=%d stage=%d op=%d in:\n%s", soulIdx, stageIdx, opIdx, got)
	}
	if !(soulIdx < stageIdx && stageIdx < opIdx) {
		t.Fatalf("expected soul < stage < operational, got soul=%d stage=%d op=%d", soulIdx, stageIdx, opIdx)
	}
}

func TestBannerFiresWhenPrereqResignedAfterWorkTurn(t *testing.T) {
	root := newTestBureaucracy(t)
	writeStageDesign(t, root, "DESIGN-BODY")
	writeStageFile(t, root, "code.md", "CODE-BODY")

	requestID := "fix-it"
	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	signStageAt(t, root, requestID, "design", t0)
	workSHA := commitWorkTurnAt(t, root, requestID, "code", t0.Add(10*time.Second))
	signStageAt(t, root, requestID, "design", t0.Add(20*time.Second))

	md := &request.Metadata{ID: requestID, Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `Since your last turn on "code"`) {
		t.Errorf("expected banner header, got:\n%s", got)
	}
	if !strings.Contains(got, workSHA) {
		t.Errorf("banner missing last-turn SHA %q:\n%s", workSHA, got)
	}
	relPath := request.ContentPath("tele", requestID, "design")
	if !strings.Contains(got, relPath) {
		t.Errorf("banner missing prereq content path %q:\n%s", relPath, got)
	}
	if !strings.Contains(got, "git -C "+root+" diff "+workSHA+"..HEAD -- "+relPath) {
		t.Errorf("banner missing usable diff command:\n%s", got)
	}
}

func TestBannerSilentBeforeFirstWorkTurn(t *testing.T) {
	root := newTestBureaucracy(t)
	writeStageDesign(t, root, "DESIGN-BODY")
	writeStageFile(t, root, "code.md", "CODE-BODY")

	requestID := "fix-it"
	signStageAt(t, root, requestID, "design", time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC))

	md := &request.Metadata{ID: requestID, Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Since your last turn") {
		t.Errorf("did not expect banner before first work turn:\n%s", got)
	}
}

func TestBannerSilentWhenPrereqResignedBeforeLastTurn(t *testing.T) {
	root := newTestBureaucracy(t)
	writeStageDesign(t, root, "DESIGN-BODY")
	writeStageFile(t, root, "code.md", "CODE-BODY")

	requestID := "fix-it"
	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	signStageAt(t, root, requestID, "design", t0)
	signStageAt(t, root, requestID, "design", t0.Add(10*time.Second)) // re-sign before any work
	commitWorkTurnAt(t, root, requestID, "code", t0.Add(20*time.Second))

	md := &request.Metadata{ID: requestID, Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Since your last turn") {
		t.Errorf("banner should not fire when prereq was re-signed before last turn:\n%s", got)
	}
}

func TestBannerSilentAtDesignStage(t *testing.T) {
	root := newTestBureaucracy(t)
	writeStageDesign(t, root, "DESIGN-BODY")

	requestID := "fix-it"
	// design unsigned → active stage is design, which has no prereqs. Even
	// with a prior work turn, there is nothing to surface.
	commitWorkTurnAt(t, root, requestID, "spec", time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC))

	md := &request.Metadata{ID: requestID, Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Since your last turn") {
		t.Errorf("banner should not fire at design stage (no prereqs):\n%s", got)
	}
}
