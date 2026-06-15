package serve

import (
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/md"
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
	body, err := os.ReadFile(path)
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
	// A canvas is a run document, not part of the wiki/twin link graph,
	// so it has no relative-link routes to resolve (nil resolver: any
	// relative link renders with its source target untouched).
	vm.Body = template.HTML(md.Render(string(body), nil))
	if st, err := os.Stat(path); err == nil {
		vm.ModTime = dash.HumanAgo(time.Now(), st.ModTime())
	}
	s.render(w, r, "canvas.html", vm)
}
