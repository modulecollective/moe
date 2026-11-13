package run

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A document's authored canvas takes one of two mutually exclusive
// shapes under documents/<doc>/:
//
//   - file form: content.md, the single canvas every stage had before
//     directory-form stages existed;
//   - directory form: content/, whose direct *.md files are the
//     canvases, read in filename order.
//
// Everything else in the document directory — prompts, transcripts,
// nested directories, non-Markdown files — is harness output or
// supporting material, never a canvas. Readers go through Canvases so
// no caller grows its own idea of which files count.

// contentDirName is the directory-form canvas directory under a
// document directory.
const contentDirName = "content"

// ErrAmbiguousLayout reports a document carrying both content.md and
// content/. Readers refuse to choose or merge the two.
var ErrAmbiguousLayout = errors.New("ambiguous canvas layout: both content.md and content/ exist")

// ContentDir returns the directory-form canvas directory for a document,
// relative to the bureaucracy root.
func ContentDir(projectID, id, docID string) string {
	return filepath.Join(DocDir(projectID, id, docID), contentDirName)
}

// ContentFilePath returns one named canvas file inside a directory-form
// document, relative to the bureaucracy root.
func ContentFilePath(projectID, id, docID, name string) string {
	return filepath.Join(ContentDir(projectID, id, docID), name)
}

// Canvas is one authored canvas file. Name is the file name inside
// content/ for the directory form and "" for the file form; Path is
// absolute (or relative to the caller's base, whichever docDir was).
type Canvas struct {
	Name string
	Path string
}

// DocCanvases lists the canvases of the document directory docDir.
// File form yields one Canvas with an empty Name; directory form yields
// its direct *.md files sorted by name. A document with neither returns
// (nil, nil) — the missing state, which callers render however they
// render a missing canvas. Both present is ErrAmbiguousLayout.
func DocCanvases(docDir string) ([]Canvas, error) {
	file := filepath.Join(docDir, "content.md")
	dir := filepath.Join(docDir, contentDirName)
	fileInfo, fileErr := os.Stat(file)
	dirInfo, dirErr := os.Stat(dir)
	if fileErr != nil && !errors.Is(fileErr, fs.ErrNotExist) {
		return nil, fileErr
	}
	if dirErr != nil && !errors.Is(dirErr, fs.ErrNotExist) {
		return nil, dirErr
	}
	hasFile := fileErr == nil && !fileInfo.IsDir()
	hasDir := dirErr == nil && dirInfo.IsDir()
	switch {
	case hasFile && hasDir:
		return nil, fmt.Errorf("%s: %w", docDir, ErrAmbiguousLayout)
	case hasFile:
		return []Canvas{{Path: file}}, nil
	case !hasDir:
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Canvas
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, Canvas{Name: e.Name(), Path: filepath.Join(dir, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Canvases is DocCanvases for (project, run, doc) under root.
func Canvases(root, projectID, id, docID string) ([]Canvas, error) {
	return DocCanvases(filepath.Join(root, DocDir(projectID, id, docID)))
}

// CanvasLabel is the display label for a directory-form canvas file:
// its name without the .md suffix, first letter upper-cased
// ("code.md" → "Code"). File-form canvases have no label.
func CanvasLabel(name string) string {
	base := strings.TrimSuffix(name, ".md")
	if base == "" {
		return ""
	}
	return strings.ToUpper(base[:1]) + base[1:]
}

// RenderDocCanvases reads docDir's canvases into one display string. A
// file-form canvas is returned verbatim, so every existing reader sees
// byte-identical output. Directory-form canvases are concatenated in
// filename order, each under a `==> content/<name> <==` label line.
//
// The result is for people and prompts to read. It is never input to a
// gate: gates parse each file on its own (a concatenation would let one
// file's fence answer for another's).
//
// No canvas at all returns an fs.ErrNotExist-wrapping error so callers
// keep their existing missing-file branches.
func RenderDocCanvases(docDir string) (string, error) {
	canvases, err := DocCanvases(docDir)
	if err != nil {
		return "", err
	}
	if len(canvases) == 0 {
		return "", fmt.Errorf("%s: no canvas: %w", docDir, fs.ErrNotExist)
	}
	if len(canvases) == 1 && canvases[0].Name == "" {
		body, err := os.ReadFile(canvases[0].Path)
		return string(body), err
	}
	var b strings.Builder
	for i, c := range canvases {
		body, err := os.ReadFile(c.Path)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "==> %s/%s <==\n\n", contentDirName, c.Name)
		b.Write(body)
		if len(body) > 0 && body[len(body)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

// ReadCanvas renders (project, run, doc)'s canvases under root. See
// RenderDocCanvases.
func ReadCanvas(root, projectID, id, docID string) (string, error) {
	return RenderDocCanvases(filepath.Join(root, DocDir(projectID, id, docID)))
}

// CanvasRelPaths returns the root-relative paths of (project, run,
// doc)'s canvases — what a prompt names and what a diff command
// scopes to. Missing returns the file-form path so callers that name
// "where the canvas will be" keep working.
func CanvasRelPaths(root, projectID, id, docID string) ([]string, error) {
	canvases, err := Canvases(root, projectID, id, docID)
	if err != nil {
		return nil, err
	}
	if len(canvases) == 0 {
		return []string{ContentPath(projectID, id, docID)}, nil
	}
	out := make([]string, 0, len(canvases))
	for _, c := range canvases {
		rel, err := filepath.Rel(root, c.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, nil
}
