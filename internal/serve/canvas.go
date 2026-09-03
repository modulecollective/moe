package serve

import (
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"os"
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
	body, err := readCanvas(path, s.canvasReferenceResolver(projectID))
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
	if st, err := os.Stat(path); err == nil {
		vm.ModTime = dash.HumanAgo(time.Now(), st.ModTime())
	}
	s.render(w, r, "canvas.html", vm)
}

// readCanvas reads a canvas file and renders its markdown. refs is a
// resolver from canvasReferenceResolver; a caller rendering several
// canvases into one page builds it once, since it caches run lookups.
//
// A canvas is a run document, not part of the wiki/twin link graph, so
// it has no relative-link routes to resolve (nil route resolver: any
// relative link renders with its source target untouched).
func readCanvas(path string, refs func(md.Reference) string) (template.HTML, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return template.HTML(md.RenderWithReferences(string(body), nil, refs)), nil
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
