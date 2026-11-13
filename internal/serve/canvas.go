package serve

import (
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/md"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// canvasVM backs the canvas read-only page.
type canvasVM struct {
	Project string
	Slug    string
	Stage   string
	Body    template.HTML // rendered markdown (empty when no file)
	ModTime string        // human "Xm ago", empty when no file
	Missing bool          // true when the canvas file isn't on disk yet
	Path    string        // absolute path; surfaced in the empty-state message
}

// handleCanvas renders a single stage canvas at
// GET /run/{project}/{slug}/canvas/{stage}. The path comes from
// Options.ResolveCanvas, which closes over the bureaucracy root and
// validates project → run → workflow → stage (mirrors
// `moe sdlc cat`). A missing canvas file is a 200 with an empty
// state, not a 404 — a stale bookmark shouldn't punish the reader.
func (s *Server) handleCanvas(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	stage := r.PathValue("stage")

	if s.opts.ResolveCanvas == nil {
		http.Error(w, "canvas not configured (Options.ResolveCanvas is nil)", http.StatusInternalServerError)
		return
	}
	path, err := s.opts.ResolveCanvas(projectID, slug, stage)
	if err != nil {
		http.Error(w, "canvas: "+err.Error(), http.StatusNotFound)
		return
	}

	vm := canvasVM{
		Project: projectID,
		Slug:    slug,
		Stage:   stage,
		Path:    path,
	}
	body, mod, err := readCanvas(path, s.canvasFiles(projectID, slug, stage), s.canvasReferenceResolver(projectID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			vm.Missing = true
			s.render(w, r, "canvas.html", vm)
			return
		}
		s.logf("canvas read %s: %v", path, err)
		http.Error(w, "canvas read: "+err.Error(), http.StatusInternalServerError)
		return
	}
	vm.Body = body
	if !mod.IsZero() {
		vm.ModTime = dash.HumanAgo(time.Now(), mod)
	}
	s.render(w, r, "canvas.html", vm)
}

// canvasFiles is Options.CanvasFiles, nil-safe.
func (s *Server) canvasFiles(projectID, slug, stage string) []string {
	if s.opts.CanvasFiles == nil {
		return nil
	}
	return s.opts.CanvasFiles(projectID, slug, stage)
}

// readCanvas reads a stage's canvases and renders their markdown. path
// is the file-form canvas ResolveCanvas returned; its directory is the
// document dir, which is where a directory-form stage's content/ lives
// too, so the shared run resolver decides the layout. refs is a
// resolver from canvasReferenceResolver; a caller rendering several
// canvases into one page builds it once, since it caches run lookups.
//
// A file-form canvas renders exactly as it always has. A
// directory-form stage renders each file under its own label (Code,
// Test, …), declared files first in declaration order, then any extra
// Markdown file the agent added; a declared file not yet written shows
// a labelled missing state instead of vanishing. Nothing on disk at all
// returns an fs.ErrNotExist error so callers keep their empty state.
// modTime is the newest file's.
//
// A canvas is a run document, not part of the wiki/twin link graph, so
// it has no relative-link routes to resolve (nil route resolver: any
// relative link renders with its source target untouched).
func readCanvas(path string, declared []string, refs func(md.Reference) string) (template.HTML, time.Time, error) {
	docDir := filepath.Dir(path)
	canvases, err := run.DocCanvases(docDir)
	if err != nil {
		return "", time.Time{}, err
	}
	if len(canvases) == 0 {
		return "", time.Time{}, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	if len(canvases) == 1 && canvases[0].Name == "" {
		body, err := os.ReadFile(canvases[0].Path)
		if err != nil {
			return "", time.Time{}, err
		}
		var mod time.Time
		if st, err := os.Stat(canvases[0].Path); err == nil {
			mod = st.ModTime()
		}
		return template.HTML(md.RenderWithReferences(string(body), nil, refs)), mod, nil
	}
	byName := map[string]run.Canvas{}
	for _, c := range canvases {
		byName[c.Name] = c
	}
	names := append([]string(nil), declared...)
	for _, c := range canvases {
		if !slices.Contains(names, c.Name) {
			names = append(names, c.Name)
		}
	}
	var b strings.Builder
	var mod time.Time
	for _, name := range names {
		label := template.HTMLEscapeString(run.CanvasLabel(name))
		file := template.HTMLEscapeString("content/" + name)
		fmt.Fprintf(&b, "<section class=\"canvas-file\">\n<h2 class=\"canvas-file-label\">%s <span class=\"meta\">%s</span></h2>\n", label, file)
		c, ok := byName[name]
		if !ok {
			fmt.Fprintf(&b, "<p class=\"meta\">no %s yet</p>\n</section>\n", file)
			continue
		}
		body, err := os.ReadFile(c.Path)
		if err != nil {
			return "", time.Time{}, err
		}
		if st, err := os.Stat(c.Path); err == nil && st.ModTime().After(mod) {
			mod = st.ModTime()
		}
		b.WriteString(md.RenderWithReferences(string(body), nil, refs))
		b.WriteString("\n</section>\n")
	}
	return template.HTML(b.String()), mod, nil
}

func (s *Server) canvasReferenceResolver(currentProject string) func(md.Reference) string {
	commitBase := ""
	if projectMD, err := project.Load(s.opts.Root, currentProject); err == nil {
		commitBase = githubCommitBase(projectMD.Remote)
	}
	runs := make(map[string]string)
	return func(ref md.Reference) string {
		switch ref.Kind {
		case md.ReferenceCommit:
			if commitBase != "" {
				return commitBase + ref.Text
			}
		case md.ReferenceRun:
			projectID, slug := currentProject, ref.Text
			if qualifiedProject, qualifiedSlug, ok := strings.Cut(ref.Text, "/"); ok {
				projectID, slug = qualifiedProject, qualifiedSlug
			}
			if !slugPattern.MatchString(projectID) || !slugPattern.MatchString(slug) {
				return ""
			}
			id := projectID + "/" + slug
			if href, ok := runs[id]; ok {
				return href
			}
			runs[id] = ""
			if _, err := run.Load(s.opts.Root, projectID, slug); err == nil {
				runs[id] = "/run/" + id
			}
			return runs[id]
		}
		return ""
	}
}

func githubCommitBase(remote string) string {
	const (
		httpsPrefix = "https://github.com/"
		sshPrefix   = "git@github.com:"
	)
	var path string
	switch {
	case strings.HasPrefix(remote, httpsPrefix):
		path = strings.TrimPrefix(remote, httpsPrefix)
	case strings.HasPrefix(remote, sshPrefix):
		path = strings.TrimPrefix(remote, sshPrefix)
	default:
		return ""
	}
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || strings.Contains(repo, "/") {
		return ""
	}
	repo = strings.TrimSuffix(repo, ".git")
	if !githubPathPart(owner) || !githubPathPart(repo) {
		return ""
	}
	return "https://github.com/" + owner + "/" + repo + "/commit/"
}

func githubPathPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}
