package abstract

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

type stubSummarizer struct {
	got map[string]string
	out string
	err error
}

func (s *stubSummarizer) Summarize(_ context.Context, _ *run.Metadata, docs map[string]string) (string, error) {
	s.got = docs
	return s.out, s.err
}

func writeContent(t *testing.T, root, projectID, runID, docID, body string) {
	t.Helper()
	dir := filepath.Join(root, run.DocDir(projectID, runID, docID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateSetsAbstractFromSummarizer is the happy path: read every
// document, pass them to the summarizer, write the trimmed output to
// md.Abstract.
func TestUpdateSetsAbstractFromSummarizer(t *testing.T) {
	root := t.TempDir()
	writeContent(t, root, "tele", "kb-dns", "research", "- https://example.org — DNS primer\n")
	writeContent(t, root, "tele", "kb-dns", "summarize", "# DNS\n\nDNS resolves names to addresses.\n")

	md := &run.Metadata{
		ID: "kb-dns", Project: "tele", Title: "DNS", Workflow: "kb",
		Documents: map[string]*run.Document{
			"research":  {Session: "s1"},
			"summarize": {Session: "s2"},
		},
	}
	s := &stubSummarizer{out: "  DNS maps human-readable names to IP addresses. The article covers caching and failure modes.  "}
	if err := Update(context.Background(), root, md, s); err != nil {
		t.Fatalf("Update: %v", err)
	}
	want := "DNS maps human-readable names to IP addresses. The article covers caching and failure modes."
	if md.Abstract != want {
		t.Errorf("abstract = %q, want %q", md.Abstract, want)
	}
	if _, ok := s.got["research"]; !ok {
		t.Errorf("summarizer did not receive research doc: keys=%v", keys(s.got))
	}
	if _, ok := s.got["summarize"]; !ok {
		t.Errorf("summarizer did not receive summarize doc: keys=%v", keys(s.got))
	}
}

// TestUpdateSkipsMissingDocFiles documents the contract that a
// Documents entry without a content.md on disk is fine — the entry
// exists the moment EnsureDocument runs, which is before the first
// edit lands.
func TestUpdateSkipsMissingDocFiles(t *testing.T) {
	root := t.TempDir()
	writeContent(t, root, "tele", "kb-dns", "research", "sources here")

	md := &run.Metadata{
		ID: "kb-dns", Project: "tele", Title: "DNS", Workflow: "kb",
		Documents: map[string]*run.Document{
			"research":  {Session: "s1"},
			"summarize": {Session: "s2"}, // no file on disk
		},
	}
	s := &stubSummarizer{out: "Short abstract."}
	if err := Update(context.Background(), root, md, s); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, ok := s.got["summarize"]; ok {
		t.Errorf("summarize should have been skipped; got %v", keys(s.got))
	}
	if md.Abstract != "Short abstract." {
		t.Errorf("abstract = %q", md.Abstract)
	}
}

// TestUpdateLeavesAbstractOnSummarizerError is the non-fatal contract:
// a failed call never clobbers the prior abstract.
func TestUpdateLeavesAbstractOnSummarizerError(t *testing.T) {
	root := t.TempDir()
	writeContent(t, root, "tele", "kb-dns", "research", "sources")
	md := &run.Metadata{
		ID: "kb-dns", Project: "tele", Workflow: "kb",
		Abstract: "prior abstract",
		Documents: map[string]*run.Document{
			"research": {Session: "s1"},
		},
	}
	s := &stubSummarizer{err: errors.New("boom")}
	err := Update(context.Background(), root, md, s)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if md.Abstract != "prior abstract" {
		t.Errorf("abstract was clobbered on error: %q", md.Abstract)
	}
}

// TestUpdateRejectsEmptyOutput guards against a zero-length response
// silently wiping the abstract.
func TestUpdateRejectsEmptyOutput(t *testing.T) {
	root := t.TempDir()
	writeContent(t, root, "tele", "kb-dns", "research", "sources")
	md := &run.Metadata{
		ID: "kb-dns", Project: "tele", Workflow: "kb",
		Abstract: "prior",
		Documents: map[string]*run.Document{
			"research": {Session: "s1"},
		},
	}
	s := &stubSummarizer{out: "   \n  "}
	err := Update(context.Background(), root, md, s)
	if err == nil {
		t.Fatal("expected error for whitespace-only output")
	}
	if md.Abstract != "prior" {
		t.Errorf("abstract was clobbered: %q", md.Abstract)
	}
}

// TestUpdateNoDocumentsIsNoOp covers the pre-first-turn case: a run
// whose documents all still lack content.md should be left alone,
// not error.
func TestUpdateNoDocumentsIsNoOp(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{
		ID: "kb-dns", Project: "tele", Workflow: "kb",
		Documents: map[string]*run.Document{
			"research": {Session: "s1"},
		},
	}
	s := &stubSummarizer{out: "should not be called"}
	if err := Update(context.Background(), root, md, s); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if s.got != nil {
		t.Errorf("summarizer should not have been called; got docs %v", keys(s.got))
	}
	if md.Abstract != "" {
		t.Errorf("abstract should remain empty, got %q", md.Abstract)
	}
}

func TestBuildUserPromptIncludesAllDocs(t *testing.T) {
	md := &run.Metadata{ID: "r", Project: "p", Title: "t"}
	docs := map[string]string{
		"research":  "r-body",
		"summarize": "s-body",
	}
	got := buildUserPrompt(md, docs)
	for _, want := range []string{"p/r", "research", "r-body", "summarize", "s-body"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
