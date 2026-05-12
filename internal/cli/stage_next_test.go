package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestPromptStageNextStagePrintsDesignCanvas: when the next stage is
// code and a design canvas exists on disk, its bytes appear above the
// [Y/n/o] prompt verbatim (no header, no decoration). follow no longer
// surfaces the design canvas once the design session closes, so this is
// the canvas's one chance to land in front of the operator at the
// design→code gate.
func TestPromptStageNextStagePrintsDesignCanvas(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "code",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	root := t.TempDir()
	canvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "design"))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "## Shape\n\nThread root through promptStageNextStage.\n"
	if err := os.WriteFile(canvas, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, root, md, "moe sdlc code tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, body) {
		t.Errorf("canvas body not printed verbatim:\n%s", got)
	}
	if i, j := strings.Index(got, body), strings.Index(got, "[Y/n/o]"); i < 0 || j < 0 || i >= j {
		t.Errorf("canvas should appear above the prompt label; canvas=%d prompt=%d", i, j)
	}
}

// TestPromptStageNextStageMissingDesignCanvasFallsThrough: a missing
// design canvas is silent — no header, no error, just the bare prompt.
// Robust against the sdlc resume path where the operator opens the
// chain prompt without a design session having committed.
func TestPromptStageNextStageMissingDesignCanvasFallsThrough(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "code",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, t.TempDir(), md, "moe sdlc code tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.HasPrefix(strings.TrimLeft(got, "\n"), "next: ") {
		t.Errorf("expected the bare prompt to be the only output; got:\n%s", got)
	}
}

// TestPromptStageNextStageWhitespaceDesignCanvasFallsThrough: a canvas
// with only whitespace is treated the same as missing — the agent
// didn't say anything worth surfacing, so don't decorate the prompt
// with blank lines.
func TestPromptStageNextStageWhitespaceDesignCanvasFallsThrough(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "code",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	root := t.TempDir()
	canvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "design"))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvas, []byte("\n\n   \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, root, md, "moe sdlc code tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if strings.HasPrefix(got, "\n") {
		t.Errorf("whitespace canvas should not pad the prompt with blank lines; got:\n%q", got)
	}
}

// TestPromptStageNextStageNonCodeStageSkipsCanvas: for stages other
// than code, no canvas is read or printed even if a same-named file
// happens to exist. Pins the next.Name == "code" trigger — generalising
// would mean threading justFinished, and that's deliberately out of
// scope for this change.
func TestPromptStageNextStageNonCodeStageSkipsCanvas(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "design",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	root := t.TempDir()
	canvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "design"))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "## Shape\n\nShould not appear above a non-code prompt.\n"
	if err := os.WriteFile(canvas, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, root, md, "moe sdlc design tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), body) {
		t.Errorf("canvas should not print for non-code stage; got:\n%s", stdout.String())
	}
}

// TestPromptStageNextStageOffersBackWhenJustFinished: passing a non-nil
// back command produces the [Y/n/o/b] label plus a legend that names
// the back target, and `b\n` on stdin dispatches back.Run with
// [project, run] (no --one-shot — back is an interactive correction).
func TestPromptStageNextStageOffersBackWhenJustFinished(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "code",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			rec.args = append([]string(nil), args...)
			return 0
		},
	}
	var backRan bool
	var backArgs []string
	back := &Command{
		Name: "design",
		Run: func(args []string, _, _ io.Writer) int {
			backRan = true
			backArgs = append([]string(nil), args...)
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "b\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, back, t.TempDir(), md, "moe sdlc code tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "[Y/n/o/b]") {
		t.Fatalf("expected [Y/n/o/b] label in prompt, got: %q", got)
	}
	if !strings.Contains(got, "Y=run · n=decline · o=run headless · b=back to design") {
		t.Fatalf("expected legend with back target in prompt, got: %q", got)
	}
	if rec.ran {
		t.Errorf("`b` must not dispatch next: rec.args=%v", rec.args)
	}
	if !backRan {
		t.Fatalf("expected back to be dispatched, but it was not")
	}
	if got, want := strings.Join(backArgs, " "), "tele fix-it"; got != want {
		t.Fatalf("back args = %q, want %q", got, want)
	}
}

// TestPromptStageNextStageNoBackWhenNil: a nil back collapses the
// label back to [Y/n/o] (no /b) and the legend omits the b row. Pins
// the empty-justFinished path — fresh-run callers must not see a
// back option that would dispatch nil.
func TestPromptStageNextStageNoBackWhenNil(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "code",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, t.TempDir(), md, "moe sdlc code tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "[Y/n/o]") {
		t.Fatalf("expected [Y/n/o] label without /b, got: %q", got)
	}
	if strings.Contains(got, "/b]") {
		t.Fatalf("expected no /b in label, got: %q", got)
	}
	if strings.Contains(got, "b=back") {
		t.Fatalf("expected legend without back entry, got: %q", got)
	}
}

// TestPromptStageNextStageBackWithoutOneShot: a non-sdlc workflow with
// a back target produces [Y/n/b] (no /o, but /b appended) and the
// legend reads "Y=run · n=decline · b=back to <stage>".
func TestPromptStageNextStageBackWithoutOneShot(t *testing.T) {
	next := &Command{
		Name: "ingest",
		Run:  func(_ []string, _, _ io.Writer) int { return 0 },
	}
	var backRan bool
	back := &Command{
		Name: "outline",
		Run: func(_ []string, _, _ io.Writer) int {
			backRan = true
			return 0
		},
	}
	md := &run.Metadata{ID: "dns-basics", Project: "tele", Workflow: "kb", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "b\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, back, t.TempDir(), md, "moe kb ingest tele dns-basics", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "[Y/n/b]") {
		t.Fatalf("expected [Y/n/b] label, got: %q", got)
	}
	if strings.Contains(got, "/o/") || strings.Contains(got, "o=run headless") {
		t.Fatalf("non-sdlc workflow must not offer one-shot, got: %q", got)
	}
	if !strings.Contains(got, "b=back to outline") {
		t.Fatalf("expected legend naming back target, got: %q", got)
	}
	if !backRan {
		t.Fatalf("expected back to dispatch on `b`")
	}
}
