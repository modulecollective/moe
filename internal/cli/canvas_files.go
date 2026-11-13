package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/run"
)

// CanvasFile is one declared canvas of a directory-form stage: a file
// under documents/<stage>/content/, its seed skeleton, and optionally
// the legacy stage whose content.md seeds it on first entry.
//
// Declaring files is what makes a stage directory-form. Everything
// downstream — seeding, the per-entry gate reset, the derived gate,
// the turn-output check, prompts and readers — keys on the declaration,
// never on a stage name.
type CanvasFile struct {
	Name     string
	Skeleton string
	// LegacyDoc names a file-form document of the same run whose
	// content.md seeds this file when the file is missing — how a run
	// started under an older ladder carries its evidence into the
	// directory-form stage that replaced it. The legacy record is read,
	// never moved.
	LegacyDoc string
}

// blockedGateFence is the gate fence every reset writes and every
// skeleton seeds.
const blockedGateFence = "```json\n{\"status\":\"blocked\"}\n```\n"

// seedCanvasFiles writes each declared file that is missing under
// workRoot, from the run's legacy canvas when one is named and present,
// else from the skeleton. Existing files are never overwritten.
func seedCanvasFiles(workRoot string, md *run.Metadata, docID string, files []CanvasFile) error {
	dir := filepath.Join(workRoot, run.ContentDir(md.Project, md.ID, docID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("seed canvases: %w", err)
	}
	for _, f := range files {
		path := filepath.Join(dir, f.Name)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("seed canvases: %w", err)
		}
		body := []byte(f.Skeleton)
		if f.LegacyDoc != "" {
			legacy, err := os.ReadFile(filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, f.LegacyDoc)))
			if err == nil && len(bytes.TrimSpace(legacy)) > 0 {
				body = legacy
			}
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return fmt.Errorf("seed canvases: %w", err)
		}
	}
	return nil
}

// resetCanvasGates blocks a file-form canvas or every direct Markdown
// file in a directory-form document, leaving the rest of each file alone.
// It runs at every admitted stage entry, so a resumed stage has to
// restamp each file against the current revision before it can
// advance again. A `ship` declaration rides the fence and drops with
// it; the agent restates it when it restamps.
func resetCanvasGates(workRoot string, md *run.Metadata, docID string) error {
	canvases, err := run.DocCanvases(filepath.Join(workRoot, run.DocDir(md.Project, md.ID, docID)))
	if err != nil {
		return err
	}
	for _, c := range canvases {
		body, err := os.ReadFile(c.Path)
		if err != nil {
			return err
		}
		reset := resetGate(string(body))
		if reset == string(body) {
			continue
		}
		if err := os.WriteFile(c.Path, []byte(reset), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// resetGate returns body with its gate set to blocked. The first
// `## Gate` section's first ```json fence is replaced in place. A body
// with no parseable gate — no section, or a section whose fence is
// missing or mangled — gets a fresh Gate section directly under its
// `# ` title (or at the top when it has none). First-heading-wins in
// parseTestCanvasSections makes the inserted section the one every
// gate reader binds.
func resetGate(body string) string {
	if _, ok := stageGateJSON(body); ok {
		lines := strings.SplitAfter(body, "\n")
		inGate, inFence := false, false
		var out strings.Builder
		replaced := false
		for _, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if replaced {
				out.WriteString(ln)
				continue
			}
			if strings.HasPrefix(ln, "## ") {
				inGate = strings.TrimSpace(strings.TrimPrefix(ln, "## ")) == "Gate"
			}
			switch {
			case inGate && !inFence && trimmed == "```json":
				inFence = true
				out.WriteString(blockedGateFence)
			case inFence && trimmed == "```":
				inFence = false
				replaced = true
			case inFence:
				// Old fence payload: dropped.
			default:
				out.WriteString(ln)
			}
		}
		return out.String()
	}
	section := "## Gate\n\n" + blockedGateFence + "\n"
	lines := strings.SplitAfter(body, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, "# ") {
			head := strings.Join(lines[:i+1], "")
			if !strings.HasSuffix(head, "\n") {
				head += "\n"
			}
			return head + "\n" + section + strings.TrimLeft(strings.Join(lines[i+1:], ""), "\n")
		}
	}
	return section + body
}

// snapshotCanvases records the bytes of a file-form canvas or every
// direct Markdown file in a directory-form document. Taken after seeding
// and the gate reset, it is the baseline the turn-output check compares
// against, so harness preparation can never pass for agent work.
func snapshotCanvases(workRoot string, md *run.Metadata, docID string) (map[string]string, error) {
	canvases, err := run.DocCanvases(filepath.Join(workRoot, run.DocDir(md.Project, md.ID, docID)))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(canvases))
	for _, c := range canvases {
		body, err := os.ReadFile(c.Path)
		if err != nil {
			return nil, err
		}
		out[c.Name] = string(body)
	}
	return out, nil
}

// checkCanvasOutput refuses a turn whose canvases are
// byte-identical to the pre-dispatch baseline, or that left no canvas
// at all. Any authored edit to any file — a progress note is enough —
// saves the turn; readiness is the gate's question, not this one's.
func checkCanvasOutput(workRoot string, md *run.Metadata, docID string, baseline map[string]string) error {
	canvasRel := canvasDiffPath(md, docID)
	after, err := snapshotCanvases(workRoot, md, docID)
	if err != nil {
		return &canvasRefusalError{Path: filepath.Join(workRoot, canvasRel), Reason: "invalid", Detail: err.Error()}
	}
	if len(after) == 0 {
		return &canvasRefusalError{Path: filepath.Join(workRoot, canvasRel), Reason: "missing"}
	}
	if maps.Equal(after, baseline) {
		return &canvasRefusalError{Path: filepath.Join(workRoot, canvasRel), Reason: "unchanged"}
	}
	return nil
}

// canvasFileProblems derives a directory-form stage's gate: every
// declared file exists, and every direct Markdown file in content/ is
// stamped — its first `## Gate` parses to "ready" and no `## ` section
// is empty or still on its `(agent fills: …)` placeholder. It returns
// one human-readable problem per unstamped file (empty means the stage
// may advance). An ambiguous layout is a problem, not an error: it
// parks the stage with the reason named.
func canvasFileProblems(root string, md *run.Metadata, docID string, files []CanvasFile) ([]string, error) {
	docDir := filepath.Join(root, run.DocDir(md.Project, md.ID, docID))
	canvases, err := run.DocCanvases(docDir)
	if errors.Is(err, run.ErrAmbiguousLayout) {
		return []string{"both content.md and content/ exist; remove one"}, nil
	}
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	var problems []string
	for _, c := range canvases {
		if c.Name == "" {
			continue
		}
		present[c.Name] = true
		body, err := os.ReadFile(c.Path)
		if err != nil {
			return nil, err
		}
		if p := canvasFileProblem(string(body)); p != "" {
			problems = append(problems, c.Name+": "+p)
		}
	}
	var missing []string
	for _, f := range files {
		if !present[f.Name] {
			missing = append(missing, f.Name+": missing")
		}
	}
	return append(missing, problems...), nil
}

// canvasFileProblem returns why one canvas file is not stamped, or "".
func canvasFileProblem(body string) string {
	status, ok := stageGateStatus(body)
	switch {
	case !ok:
		return "no gate"
	case status != "ready":
		return "gate " + status
	}
	sections := parseTestCanvasSections(body)
	for _, title := range sectionOrder(body) {
		if !testSectionFilled(sections[title]) {
			return fmt.Sprintf("section %q unfilled", title)
		}
	}
	return ""
}

// sectionOrder lists a canvas's `## ` headings in order, first
// occurrence only — the same binding parseTestCanvasSections applies.
func sectionOrder(body string) []string {
	var out []string
	seen := map[string]bool{}
	for ln := range strings.SplitSeq(body, "\n") {
		if t := strings.TrimPrefix(ln, "## "); t != ln {
			t = strings.TrimSpace(t)
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// canvasFilesNominateClose reports whether any file of a
// directory-form document carries a `{"status":"close"}` gate. Each
// file is parsed on its own; the build prompt asks for the nomination
// in code.md, but whichever file carries it speaks for the stage.
func canvasFilesNominateClose(root string, md *run.Metadata, docID string) bool {
	canvases, err := run.Canvases(root, md.Project, md.ID, docID)
	if err != nil {
		return false
	}
	for _, c := range canvases {
		if c.Name == "" {
			continue
		}
		body, err := os.ReadFile(c.Path)
		if err != nil {
			continue
		}
		if status, ok := stageGateStatus(string(body)); ok && status == "close" {
			return true
		}
	}
	return false
}

// stageCanvasFiles returns docID's declared canvas files under the
// named workflow, or nil for a file-form stage or an unregistered
// workflow.
func stageCanvasFiles(workflow, docID string) []CanvasFile {
	wf, err := LookupWorkflow(workflow)
	if err != nil {
		return nil
	}
	return wf.StageFiles(docID)
}

// canvasDiffPath is the root-relative path that scopes a diff or a
// "where is this canvas" pointer to docID's canvases: content/ for a
// directory-form stage, content.md otherwise.
func canvasDiffPath(md *run.Metadata, docID string) string {
	if stageCanvasFiles(md.Workflow, docID) != nil {
		return run.ContentDir(md.Project, md.ID, docID)
	}
	return run.ContentPath(md.Project, md.ID, docID)
}

// stageNotReadyMessage is the refusal printed when a stage's gate keeps
// it from advancing: one line per unstamped file and why, then the verb
// that resumes it. "Gate not satisfied" alone stops being enough once a
// stage has two files.
func stageNotReadyMessage(workflow string, md *run.Metadata, stage string, problems []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s not ready — parked at %s:\n", stage, stage)
	for _, p := range problems {
		fmt.Fprintf(&b, "  %s\n", p)
	}
	fmt.Fprintf(&b, "next: moe %s %s %s/%s\n", workflow, stage, md.Project, md.ID)
	return b.String()
}
