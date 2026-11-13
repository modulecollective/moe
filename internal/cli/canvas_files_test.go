package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

const skeletonA = "# Alpha\n\n## Gate\n\n```json\n{\"status\":\"blocked\"}\n```\n\n## Body\n\n(agent fills: alpha)\n"

// dirFormWorkflow registers a throwaway workflow whose "assemble" stage
// is directory-form with two files. Nothing about it is named build, so
// a test that passes here proves the machinery keys on the declaration.
func dirFormWorkflow(t *testing.T) *Workflow {
	t.Helper()
	w := NewWorkflow("dirform-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")))
	w.RegisterStage("plan")
	w.RegisterStage("assemble", "plan")
	w.RegisterStage("judge", "assemble")
	w.RegisterStageFiles("assemble",
		CanvasFile{Name: "alpha.md", Skeleton: skeletonA, LegacyDoc: "old"},
		CanvasFile{Name: "beta.md", Skeleton: strings.Replace(skeletonA, "Alpha", "Beta", 1)},
	)
	RegisterWorkflow(w)
	t.Cleanup(func() { delete(workflows, w.Name) })
	return w
}

func writeAssemble(t *testing.T, root string, md *run.Metadata, name, body string) {
	t.Helper()
	writeFile(t, filepath.Join(root, run.ContentFilePath(md.Project, md.ID, "assemble", name)), body)
}

const stampedA = "# Alpha\n\n## Gate\n\n```json\n{\"status\":\"ready\"}\n```\n\n## Body\n\ndone\n"

// TestDirectoryFormGateNamesEachUnstampedFile drives every shape the
// design lists as blocking — each named in the refusal — and the one
// that advances, through CheckStageGate / StageGateProblems on a stage
// that is not build.
func TestDirectoryFormGateNamesEachUnstampedFile(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string // substrings, one per expected problem; empty = stamped
	}{
		{"both seeded", map[string]string{"alpha.md": skeletonA, "beta.md": skeletonA}, []string{"alpha.md: gate blocked", "beta.md: gate blocked"}},
		{"alpha ready, beta blocked", map[string]string{"alpha.md": stampedA, "beta.md": skeletonA}, []string{"beta.md: gate blocked"}},
		{"beta ready, alpha blocked", map[string]string{"alpha.md": skeletonA, "beta.md": stampedA}, []string{"alpha.md: gate blocked"}},
		{"ready over a placeholder", map[string]string{"alpha.md": stampedA, "beta.md": strings.Replace(skeletonA, `"blocked"`, `"ready"`, 1)}, []string{`beta.md: section "Body" unfilled`}},
		{"ready over an empty section", map[string]string{"alpha.md": stampedA, "beta.md": stampedA + "\n## Extra\n\n"}, []string{`beta.md: section "Extra" unfilled`}},
		{"declared file deleted", map[string]string{"alpha.md": stampedA}, []string{"beta.md: missing"}},
		{"extra file without a gate", map[string]string{"alpha.md": stampedA, "beta.md": stampedA, "notes.md": "# Notes\n\nscratch\n"}, []string{"notes.md: no gate"}},
		{"close is not ready", map[string]string{"alpha.md": strings.Replace(stampedA, `"ready"`, `"close"`, 1), "beta.md": stampedA}, []string{"alpha.md: gate close"}},
		{"both stamped", map[string]string{"alpha.md": stampedA, "beta.md": stampedA}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := dirFormWorkflow(t)
			root := t.TempDir()
			md := &run.Metadata{Project: "p", ID: "r", Workflow: w.Name}
			for name, body := range tc.files {
				writeAssemble(t, root, md, name, body)
			}
			problems, err := w.StageGateProblems(root, md, "assemble")
			if err != nil {
				t.Fatal(err)
			}
			ok, err := w.CheckStageGate(root, md, "assemble")
			if err != nil {
				t.Fatal(err)
			}
			if ok != (len(tc.want) == 0) {
				t.Fatalf("CheckStageGate = %v with problems %v", ok, problems)
			}
			if len(problems) != len(tc.want) {
				t.Fatalf("problems = %v, want %v", problems, tc.want)
			}
			for i, want := range tc.want {
				if !strings.Contains(problems[i], want) {
					t.Fatalf("problems[%d] = %q, want %q", i, problems[i], want)
				}
			}
		})
	}
}

// TestDirectoryFormGateRefusesAmbiguousLayout: content.md beside
// content/ parks the stage with the reason named, rather than reading
// either one.
func TestDirectoryFormGateRefusesAmbiguousLayout(t *testing.T) {
	w := dirFormWorkflow(t)
	root := t.TempDir()
	md := &run.Metadata{Project: "p", ID: "r", Workflow: w.Name}
	writeAssemble(t, root, md, "alpha.md", stampedA)
	writeAssemble(t, root, md, "beta.md", stampedA)
	writeFile(t, filepath.Join(root, run.ContentPath("p", "r", "assemble")), stampedA)
	problems, err := w.StageGateProblems(root, md, "assemble")
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "both content.md and content/") {
		t.Fatalf("problems = %v, %v", problems, err)
	}
	if !w.HasStageGate("assemble") || w.HasStageGate("plan") {
		t.Fatal("HasStageGate must follow the declaration")
	}
}

func TestResetGate(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "ready fence replaced, body kept",
			in:   "# T\n\n## Gate\n\n```json\n{\"status\":\"ready\",\"ship\":\"none\"}\n```\n\nAllowed values: x\n\n## Body\n\nkept\n",
			want: "# T\n\n## Gate\n\n```json\n{\"status\":\"blocked\"}\n```\n\nAllowed values: x\n\n## Body\n\nkept\n",
		},
		{
			name: "already blocked is a no-op",
			in:   skeletonA,
			want: skeletonA,
		},
		{
			name: "no gate gets one under the title",
			in:   "# Code\n\nlegacy canvas body\n\n## Test plan\n\n- run it\n",
			want: "# Code\n\n## Gate\n\n```json\n{\"status\":\"blocked\"}\n```\n\nlegacy canvas body\n\n## Test plan\n\n- run it\n",
		},
		{
			name: "mangled fence gets a fresh section that binds first",
			in:   "# Code\n\n## Gate\n\nready\n",
			want: "# Code\n\n## Gate\n\n```json\n{\"status\":\"blocked\"}\n```\n\n## Gate\n\nready\n",
		},
		{
			name: "no title puts the gate on top",
			in:   "just notes\n",
			want: "## Gate\n\n```json\n{\"status\":\"blocked\"}\n```\n\njust notes\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resetGate(tc.in)
			if got != tc.want {
				t.Fatalf("resetGate =\n%q\nwant\n%q", got, tc.want)
			}
			if status, ok := stageGateStatus(got); !ok || status != "blocked" {
				t.Fatalf("reset gate reads (%q, %v)", status, ok)
			}
		})
	}
}

// TestSeedAndResetCanvasFiles: seeding writes only missing declared
// files — from the run's legacy canvas when present, else the skeleton —
// never overwrites, leaves the legacy record in place, and the reset
// then blocks every direct Markdown file, inserting a gate the legacy
// canvas never had.
func TestSeedAndResetCanvasFiles(t *testing.T) {
	w := dirFormWorkflow(t)
	root := t.TempDir()
	md := &run.Metadata{Project: "p", ID: "r", Workflow: w.Name}
	legacy := "# Old\n\nwhat the retired stage wrote\n"
	writeFile(t, filepath.Join(root, run.ContentPath("p", "r", "old")), legacy)
	writeAssemble(t, root, md, "notes.md", stampedA)

	if err := seedCanvasFiles(root, md, "assemble", w.StageFiles("assemble")); err != nil {
		t.Fatal(err)
	}
	alpha, _ := os.ReadFile(filepath.Join(root, run.ContentFilePath("p", "r", "assemble", "alpha.md")))
	if string(alpha) != legacy {
		t.Fatalf("alpha.md = %q, want the legacy canvas", alpha)
	}
	if kept, _ := os.ReadFile(filepath.Join(root, run.ContentPath("p", "r", "old"))); string(kept) != legacy {
		t.Fatal("seeding must leave the legacy record intact")
	}

	writeAssemble(t, root, md, "beta.md", stampedA)
	if err := seedCanvasFiles(root, md, "assemble", w.StageFiles("assemble")); err != nil {
		t.Fatal(err)
	}
	if beta, _ := os.ReadFile(filepath.Join(root, run.ContentFilePath("p", "r", "assemble", "beta.md"))); string(beta) != stampedA {
		t.Fatal("an existing file must never be overwritten by a seed")
	}

	if err := resetCanvasGates(root, md, "assemble"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha.md", "beta.md", "notes.md"} {
		body, _ := os.ReadFile(filepath.Join(root, run.ContentFilePath("p", "r", "assemble", name)))
		if status, ok := stageGateStatus(string(body)); !ok || status != "blocked" {
			t.Fatalf("%s gate after reset = (%q, %v)", name, status, ok)
		}
	}
	alpha, _ = os.ReadFile(filepath.Join(root, run.ContentFilePath("p", "r", "assemble", "alpha.md")))
	if !strings.Contains(string(alpha), "what the retired stage wrote") {
		t.Fatalf("reset dropped the body: %q", alpha)
	}
}

// TestCheckCanvasOutput: the turn-output check compares against the
// post-preparation baseline. Seeds and resets alone fail; any authored
// edit — a progress note in one file — saves the turn; no canvas at all
// fails.
func TestCheckCanvasOutput(t *testing.T) {
	w := dirFormWorkflow(t)
	root := t.TempDir()
	md := &run.Metadata{Project: "p", ID: "r", Workflow: w.Name}
	if err := seedCanvasFiles(root, md, "assemble", w.StageFiles("assemble")); err != nil {
		t.Fatal(err)
	}
	if err := resetCanvasGates(root, md, "assemble"); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshotCanvases(root, md, "assemble")
	if err != nil {
		t.Fatal(err)
	}

	var refusal *canvasRefusalError
	err = checkCanvasOutput(root, md, "assemble", baseline)
	if !errors.As(err, &refusal) || refusal.Reason != "unchanged" {
		t.Fatalf("untouched seeds: err = %v, want unchanged refusal", err)
	}

	alphaPath := filepath.Join(root, run.ContentFilePath("p", "r", "assemble", "alpha.md"))
	body, _ := os.ReadFile(alphaPath)
	writeFile(t, alphaPath, string(body)+"\nProgress: half the parser is in.\n")
	if err := checkCanvasOutput(root, md, "assemble", baseline); err != nil {
		t.Fatalf("a progress note must save the turn: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(root, run.ContentDir("p", "r", "assemble"))); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, run.ContentDir("p", "r", "assemble")), 0o755); err != nil {
		t.Fatal(err)
	}
	err = checkCanvasOutput(root, md, "assemble", baseline)
	if !errors.As(err, &refusal) || refusal.Reason != "missing" {
		t.Fatalf("empty content dir: err = %v, want missing refusal", err)
	}
}
