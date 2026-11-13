package run

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDocCanvasesFileForm(t *testing.T) {
	doc := t.TempDir()
	writeFile(t, filepath.Join(doc, "content.md"), "# Design\n")
	writeFile(t, filepath.Join(doc, "prompt-claude.md"), "prompt")
	got, err := DocCanvases(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "" || got[0].Path != filepath.Join(doc, "content.md") {
		t.Fatalf("file form = %+v", got)
	}
	body, err := RenderDocCanvases(doc)
	if err != nil || body != "# Design\n" {
		t.Fatalf("render = %q, %v; want verbatim file", body, err)
	}
}

func TestDocCanvasesDirectoryFormOrderedDirectMarkdownOnly(t *testing.T) {
	doc := t.TempDir()
	writeFile(t, filepath.Join(doc, "content", "test.md"), "# Test\n")
	writeFile(t, filepath.Join(doc, "content", "code.md"), "# Code")
	writeFile(t, filepath.Join(doc, "content", "notes.txt"), "not a canvas")
	writeFile(t, filepath.Join(doc, "content", "nested", "deep.md"), "not a canvas")
	writeFile(t, filepath.Join(doc, "prompt-codex.md"), "harness output")
	got, err := DocCanvases(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "code.md" || got[1].Name != "test.md" {
		t.Fatalf("directory form = %+v, want code.md then test.md", got)
	}
	body, err := RenderDocCanvases(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := "==> content/code.md <==\n\n# Code\n\n==> content/test.md <==\n\n# Test\n"
	if body != want {
		t.Fatalf("render =\n%s\nwant\n%s", body, want)
	}
	if strings.Contains(body, "harness output") || strings.Contains(body, "not a canvas") {
		t.Fatal("non-canvas file leaked into render")
	}
}

func TestDocCanvasesAmbiguousAndMissing(t *testing.T) {
	doc := t.TempDir()
	if got, err := DocCanvases(doc); err != nil || got != nil {
		t.Fatalf("missing = %+v, %v", got, err)
	}
	if _, err := RenderDocCanvases(doc); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing render err = %v, want ErrNotExist", err)
	}
	if err := os.MkdirAll(filepath.Join(doc, "content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderDocCanvases(doc); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty content dir render err = %v, want ErrNotExist", err)
	}
	writeFile(t, filepath.Join(doc, "content.md"), "x")
	if _, err := DocCanvases(doc); !errors.Is(err, ErrAmbiguousLayout) {
		t.Fatalf("both present err = %v, want ErrAmbiguousLayout", err)
	}
	if _, err := RenderDocCanvases(doc); !errors.Is(err, ErrAmbiguousLayout) {
		t.Fatalf("both present render err = %v, want ErrAmbiguousLayout", err)
	}
}

func TestCanvasLabel(t *testing.T) {
	if got := CanvasLabel("code.md"); got != "Code" {
		t.Fatalf("label = %q", got)
	}
	if got := CanvasLabel(""); got != "" {
		t.Fatalf("empty label = %q", got)
	}
}
