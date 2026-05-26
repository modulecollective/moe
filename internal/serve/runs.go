package serve

import (
	"net/http"
	"sort"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/workspace"
)

// agentOptions is the hardcoded set offered in the new-run form's
// agent dropdown. Two registered agents today; if a third ever
// appears, surface it here rather than pulling from internal/agent
// (which has no exported enumeration). The empty value means "use
// the run's default" — resolved at stage time.
var agentOptions = []string{"", "claude", "codex"}

// workspaceOption is one entry in the new-run form's workspace
// dropdown. Pre-joined as "project/name" so the template doesn't
// need to compose strings.
type workspaceOption struct {
	Project string
	Name    string
	Label   string // "project/name"
}

// newRunVM backs the new-run form. Projects and workspaces are
// gathered from disk at request time; the agent list is static.
type newRunVM struct {
	Projects    []string          // project IDs
	Workspaces  []workspaceOption // every named workspace this host has on disk, across all projects
	Agents      []string          // includes "" for "use default"
	ErrorBanner string            // populated on a POST validation failure (slice #4)
}

func (s *Server) handleNewRunForm(w http.ResponseWriter, r *http.Request) {
	vm, err := s.gatherNewRunVM()
	if err != nil {
		s.logf("new-run form gather: %v", err)
		http.Error(w, "new-run form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "new.html", vm)
}

func (s *Server) gatherNewRunVM() (newRunVM, error) {
	mds, warns, err := project.List(s.opts.Root)
	if err != nil {
		return newRunVM{}, err
	}
	for _, w := range warns {
		s.logf("project list: skipping %s: %v", w.ID, w.Err)
	}
	projectIDs := make([]string, 0, len(mds))
	for _, md := range mds {
		projectIDs = append(projectIDs, md.ID)
	}
	sort.Strings(projectIDs)

	infos, err := workspace.List(s.opts.Root, "")
	if err != nil {
		return newRunVM{}, err
	}
	wsOpts := make([]workspaceOption, 0, len(infos))
	for _, info := range infos {
		wsOpts = append(wsOpts, workspaceOption{
			Project: info.Project,
			Name:    info.Name,
			Label:   info.Project + "/" + info.Name,
		})
	}

	return newRunVM{
		Projects:   projectIDs,
		Workspaces: wsOpts,
		Agents:     agentOptions,
	}, nil
}
