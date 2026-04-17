package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/request"
)

// seedRequest writes a minimal request.json + project.json pair under
// root so moe dash's scan finds it. The opening commit is what newTestBureaucracy
// plus commitTrailer supply — tests add work/sign trailers on top.
func seedRequest(t *testing.T, root, projectID, reqID, status string) *request.Metadata {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "requests", projectID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "requests", projectID, "project.json"),
		[]byte(`{"id":"`+projectID+`"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	md := &request.Metadata{
		ID:        reqID,
		Project:   projectID,
		Title:     "T",
		Status:    status,
		Created:   "2026-04-01",
		Documents: map[string]*request.Document{},
	}
	if err := request.Save(root, md); err != nil {
		t.Fatal(err)
	}
	// Commit it so git log --grep=MoE-Request finds the request at all.
	reqJSONRel := filepath.Join(request.RunDir(projectID, reqID), "request.json")
	projectJSONRel := filepath.Join("requests", projectID, "project.json")
	addCmd := exec.Command("git", "-C", root, "add", reqJSONRel, projectJSONRel)
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commitTrailer(t, root, "Open request "+projectID+"/"+reqID+": T",
		"MoE-Request: "+reqID+"\nMoE-Project: "+projectID, time.Time{})
	return md
}

func writeContent(t *testing.T, root, projectID, reqID, docID, body string) {
	t.Helper()
	path := filepath.Join(root, request.ContentPath(projectID, reqID, docID))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// markBureaucracy writes the root-marker file so bureaucracy.Find picks
// up the test repo. newTestBureaucracy just initializes a git repo; the
// marker lives on top of it.
func markBureaucracy(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "bureaucracy.conf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDashEmptyBureaucracy(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"Ministry of Everything",
		"NEEDS ATTENTION (0)",
		"ACTIVE (0)",
		"RECENT (last 7 days) (0)",
		"0 project(s) registered · 0 active",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestDashReadyToSignLandsInNeedsAttention(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRequest(t, root, "tele", "fix-it", "in_progress")
	writeContent(t, root, "tele", "fix-it", "design", "# spec\nready.\n")
	// Represent the work turn that produced the content.
	commitWorkTurnAt(t, root, "fix-it", "design",
		time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "NEEDS ATTENTION (1)") {
		t.Fatalf("expected one needs-attention row, got:\n%s", got)
	}
	if !strings.Contains(got, "fix-it") || !strings.Contains(got, "tele") {
		t.Fatalf("row missing project/request:\n%s", got)
	}
	if !strings.Contains(got, "ready to sign design") {
		t.Fatalf("expected readiness note, got:\n%s", got)
	}
}

func TestDashPrereqResignedKeepsInActive(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRequest(t, root, "tele", "fix-it", "in_progress")
	writeContent(t, root, "tele", "fix-it", "code", "// implementation\n")

	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	signStageAt(t, root, "fix-it", "design", t0)
	commitWorkTurnAt(t, root, "fix-it", "code", t0.Add(time.Hour))
	// Design re-signed after the code work turn → readiness should
	// reject this request; it belongs in ACTIVE.
	signStageAt(t, root, "fix-it", "design", t0.Add(2*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "NEEDS ATTENTION (0)") {
		t.Fatalf("expected empty needs-attention, got:\n%s", got)
	}
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected one active row, got:\n%s", got)
	}
	if !strings.Contains(got, "stage code") {
		t.Fatalf("expected active-stage note, got:\n%s", got)
	}
}

func TestDashEmptyContentStaysInActive(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRequest(t, root, "tele", "fix-it", "in_progress")
	// Empty content.md — a fresh document dir, no work yet.
	writeContent(t, root, "tele", "fix-it", "design", "")

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "NEEDS ATTENTION (0)") {
		t.Fatalf("empty content should not be ready to sign:\n%s", got)
	}
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected ACTIVE row, got:\n%s", got)
	}
}

func TestDashApprovedLandsInRecent(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRequest(t, root, "tele", "fix-it", "approved")
	// An approved request needs a recent commit so LastActivity doesn't
	// return the opening commit's time (which would still be recent in
	// a freshly-made fixture, so this is belt-and-suspenders).
	signStageAt(t, root, "fix-it", "code",
		time.Now().UTC().Add(-2*24*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "RECENT (last 7 days) (1)") {
		t.Fatalf("expected recent row, got:\n%s", got)
	}
	if !strings.Contains(got, "approved") {
		t.Fatalf("expected 'approved' in note, got:\n%s", got)
	}
}

func TestDashDormantHiddenWithoutAll(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// An in_progress request whose last activity is 60 days ago.
	seedRequest(t, root, "tele", "old-one", "in_progress")
	commitTrailer(t, root, "work: update spec",
		"MoE-Request: old-one\nMoE-Document: spec",
		time.Now().UTC().Add(-60*24*time.Hour))

	// Default: hidden.
	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "ACTIVE (0)") {
		t.Fatalf("dormant request should be hidden, got:\n%s", out.String())
	}

	// --all: shown.
	out.Reset()
	errb.Reset()
	code = Run([]string{"dash", "--all"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "ACTIVE (1)") {
		t.Fatalf("--all should reveal dormant request, got:\n%s", out.String())
	}
}

func TestDashSortsNewestFirstWithinBucket(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRequest(t, root, "tele", "older", "in_progress")
	commitTrailer(t, root, "work: update spec",
		"MoE-Request: older\nMoE-Document: spec",
		time.Now().UTC().Add(-3*24*time.Hour))

	seedRequest(t, root, "tele", "newer", "in_progress")
	commitTrailer(t, root, "work: update spec",
		"MoE-Request: newer\nMoE-Document: spec",
		time.Now().UTC().Add(-1*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	newerIdx := strings.Index(got, "newer")
	olderIdx := strings.Index(got, "older")
	if newerIdx < 0 || olderIdx < 0 {
		t.Fatalf("missing one of the rows: newer=%d older=%d in:\n%s", newerIdx, olderIdx, got)
	}
	if newerIdx > olderIdx {
		t.Fatalf("expected newer before older:\n%s", got)
	}
}

func TestDashProjectCountReflectsProjectJSON(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	for _, p := range []string{"alpha", "beta", "gamma"} {
		if err := os.MkdirAll(filepath.Join(root, "requests", p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(root, "requests", p, "project.json"),
			[]byte(`{"id":"`+p+`"}`),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "3 project(s) registered") {
		t.Fatalf("expected 3 projects in footer, got:\n%s", out.String())
	}
}
