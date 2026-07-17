package serve

import (
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/md"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/wiki"
)

// slugRe bounds the {name}/{topic}/{doc}/{project} path segments to a
// safe leaf: word chars and hyphens, no dots or slashes. We append the
// ".md" ourselves, so a legitimate doc name never carries one — and
// rejecting dots/slashes closes path traversal (no "..", no escaping the
// content directory).
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// twinEngineFiles are the non-doc files the wiki engine maintains under
// digital-twin/; they're filtered out of the browsable twin doc list.
// checkpoint.json isn't .md so it's already excluded, but log.md and
// history-summary.md are.
var twinEngineFiles = map[string]bool{
	"log.md":             true,
	"history-summary.md": true,
}

// crumb is one breadcrumb in a doc page's trail.
type crumb struct {
	Label string
	Href  string // empty for the current (non-link) crumb
}

// docVM backs the generic rendered-doc page (lore entry, knowledge
// topic, twin doc). One template, three callers.
type docVM struct {
	Title       string
	Crumbs      []crumb
	AppliesWhen string // lore only; empty elsewhere
	Meta        fileMeta
	Body        template.HTML
}

// --- lore -------------------------------------------------------------

type loreIndexVM struct {
	Entries []loreItemVM
}

type loreItemVM struct {
	Name        string // slug for the link
	Title       string
	AppliesWhen string
}

// handleLoreIndex lists lore/*.md. Cheap: a frontmatter read per file,
// no git calls (provenance lives on the entry page).
func (s *Server) handleLoreIndex(w http.ResponseWriter, r *http.Request) {
	dir := wiki.LoreDir(s.opts.Root)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, "lore read: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var vm loreIndexVM
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		front, _ := splitFrontmatter(readFileString(filepath.Join(dir, name)))
		slug := strings.TrimSuffix(name, ".md")
		title := front["title"]
		if title == "" {
			title = slug
		}
		vm.Entries = append(vm.Entries, loreItemVM{
			Name:        slug,
			Title:       title,
			AppliesWhen: front["applies-when"],
		})
	}
	sort.Slice(vm.Entries, func(i, j int) bool { return vm.Entries[i].Name < vm.Entries[j].Name })
	s.render(w, r, "lore_index.html", vm)
}

// handleLoreEntry renders a single lore/*.md entry.
func (s *Server) handleLoreEntry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !slugRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	rel := filepath.Join(wiki.LoreDirRel, name+".md")
	src, ok := s.readDoc(w, r, rel)
	if !ok {
		return
	}
	front, body := splitFrontmatter(src)
	title := front["title"]
	if title == "" {
		title = name
	}
	// Lore is flat: a relative "other.md" link resolves to its entry.
	resolve := func(target string) string {
		if base, ok := relMDLeaf(target); ok {
			return "/lore/" + base
		}
		return ""
	}
	vm := docVM{
		Title:       title,
		AppliesWhen: front["applies-when"],
		Crumbs: []crumb{
			{Label: "dashboard", Href: "/"},
			{Label: "lore", Href: "/lore"},
			{Label: name},
		},
		Meta: s.gatherFileMeta(time.Now().UTC(), rel),
		Body: template.HTML(md.Render(body, resolve)),
	}
	s.render(w, r, "doc.html", vm)
}

// --- projects ---------------------------------------------------------

type projectsIndexVM struct {
	Projects []projectItemVM
}

type projectItemVM struct {
	ID       string
	Runs     int
	Chores   int
	Topics   int
	TwinDocs int
}

// handleProjectsIndex lists registered projects with cheap counts. No
// per-file git calls: run/chore counts come from the single GatherDash
// the dash already uses; knowledge/twin counts are directory reads.
func (s *Server) handleProjectsIndex(w http.ResponseWriter, r *http.Request) {
	mds, _, err := project.List(s.opts.Root)
	if err != nil {
		http.Error(w, "projects: "+err.Error(), http.StatusInternalServerError)
		return
	}
	runCounts, choreCounts := s.runChoreCounts()
	var vm projectsIndexVM
	for _, p := range mds {
		vm.Projects = append(vm.Projects, projectItemVM{
			ID:       p.ID,
			Runs:     runCounts[p.ID],
			Chores:   choreCounts[p.ID],
			Topics:   countMarkdown(s.knowledgeTopicsDir(p.ID), nil),
			TwinDocs: countMarkdown(wiki.TwinDir(s.opts.Root, p.ID), twinEngineFiles),
		})
	}
	s.render(w, r, "projects_index.html", vm)
}

type hubVM struct {
	// bannerArtVM carries the project-scoped histogram + factory art the
	// "bannerart" partial draws under the header, same as the home dash.
	bannerArtVM
	Project      string
	Active       []dashRowVM
	Backlog      []dashRowVM
	Completed    []dashRowVM
	Chores       []dashRowVM
	HasKnowledge bool
	TopicCount   int
	Twin         []twinDocVM
}

type twinDocVM struct {
	Name string // slug for the link
	Meta fileMeta
}

// handleProjectHub aggregates everything under one project: runs and
// chores (filtered from GatherDash — no re-gather), a link to the
// knowledge page, and the twin docs with git provenance.
func (s *Server) handleProjectHub(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	if !slugRe.MatchString(projectID) {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(filepath.Join(s.opts.Root, project.Dir(projectID))); err != nil {
		http.NotFound(w, r)
		return
	}

	now := time.Now().UTC()
	vm := hubVM{Project: projectID}
	if s.opts.GatherDash != nil {
		rows, _, _, histogram, err := s.opts.GatherDash(projectID)
		if err != nil {
			http.Error(w, "hub gather: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// The snapshot is already scoped to projectID, so every row
		// belongs here — no re-filter — and the factory art + histogram
		// reflect this project alone.
		vm.bannerArtVM = newBannerArt(now, rows, histogram)
		for _, row := range rows {
			rvm := dashRowVM{Project: row.Project, Run: row.Run, Note: noteHTML(row.Project, row.Note), When: dash.HumanAgo(now, row.When), Member: row.Member}
			switch row.Bucket {
			case dash.BucketActiveRuns:
				vm.Active = append(vm.Active, rvm)
			case dash.BucketChores:
				vm.Chores = append(vm.Chores, rvm)
			case dash.BucketBacklog:
				vm.Backlog = append(vm.Backlog, rvm)
			case dash.BucketCompletedRuns:
				vm.Completed = append(vm.Completed, rvm)
			}
		}
	}

	vm.TopicCount = countMarkdown(s.knowledgeTopicsDir(projectID), nil)
	if _, err := os.Stat(filepath.Join(s.knowledgeDir(projectID), "index.md")); err == nil {
		vm.HasKnowledge = true
	}

	twinDir := wiki.TwinDir(s.opts.Root, projectID)
	for _, name := range listMarkdown(twinDir, twinEngineFiles) {
		slug := strings.TrimSuffix(name, ".md")
		rel := relTo(s.opts.Root, filepath.Join(twinDir, name))
		vm.Twin = append(vm.Twin, twinDocVM{Name: slug, Meta: s.gatherFileMeta(now, rel)})
	}
	s.render(w, r, "project_hub.html", vm)
}

// --- knowledge --------------------------------------------------------

type knowledgeVM struct {
	Project  string
	HasIndex bool
	Index    template.HTML
	Topics   []topicItemVM
}

type topicItemVM struct {
	Name string
}

// handleKnowledge renders the curated index.md as a header and lists
// every topics/*.md below it. The listing — not the curated index — is
// the source of truth for navigation: a stale index.md must never
// orphan a topic.
func (s *Server) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	if !slugRe.MatchString(projectID) {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(filepath.Join(s.opts.Root, project.Dir(projectID))); err != nil {
		http.NotFound(w, r)
		return
	}

	vm := knowledgeVM{Project: projectID}

	// Curated index.md (optional): rewrite its topics/*.md links to the
	// topic routes so the rendered front door is clickable.
	resolve := func(target string) string {
		if name, ok := topicLeaf(target); ok {
			return "/projects/" + projectID + "/knowledge/" + name
		}
		return ""
	}
	if src := readFileString(filepath.Join(s.knowledgeDir(projectID), "index.md")); src != "" {
		_, body := splitFrontmatter(src)
		vm.Index = template.HTML(md.Render(body, resolve))
		vm.HasIndex = true
	}

	// Names + links only: no per-topic git provenance. gatherFileMeta is an
	// N+1 of git subprocesses that does not belong on an index page; the
	// "updated · run" badge lives on each topic's detail page instead.
	for _, name := range listMarkdown(s.knowledgeTopicsDir(projectID), nil) {
		slug := strings.TrimSuffix(name, ".md")
		vm.Topics = append(vm.Topics, topicItemVM{Name: slug})
	}
	s.render(w, r, "knowledge_index.html", vm)
}

// handleKnowledgeTopic renders one topics/*.md.
func (s *Server) handleKnowledgeTopic(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	topic := r.PathValue("topic")
	if !slugRe.MatchString(projectID) || !slugRe.MatchString(topic) {
		http.NotFound(w, r)
		return
	}
	rel := relTo(s.opts.Root, filepath.Join(s.knowledgeTopicsDir(projectID), topic+".md"))
	src, ok := s.readDoc(w, r, rel)
	if !ok {
		return
	}
	resolve := func(target string) string {
		if target == "index.md" || target == "../index.md" {
			return "/projects/" + projectID + "/knowledge"
		}
		if base, ok := relMDLeaf(target); ok {
			return "/projects/" + projectID + "/knowledge/" + base
		}
		return ""
	}
	_, body := splitFrontmatter(src)
	vm := docVM{
		Title: topic,
		Crumbs: []crumb{
			{Label: "dashboard", Href: "/"},
			{Label: "projects", Href: "/projects"},
			{Label: projectID, Href: "/projects/" + projectID},
			{Label: "knowledge", Href: "/projects/" + projectID + "/knowledge"},
			{Label: topic},
		},
		Meta: s.gatherFileMeta(time.Now().UTC(), rel),
		Body: template.HTML(md.Render(body, resolve)),
	}
	s.render(w, r, "doc.html", vm)
}

// handleTwinDoc renders one digital-twin/*.md.
func (s *Server) handleTwinDoc(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	doc := r.PathValue("doc")
	if !slugRe.MatchString(projectID) || !slugRe.MatchString(doc) {
		http.NotFound(w, r)
		return
	}
	rel := relTo(s.opts.Root, filepath.Join(wiki.TwinDir(s.opts.Root, projectID), doc+".md"))
	src, ok := s.readDoc(w, r, rel)
	if !ok {
		return
	}
	resolve := func(target string) string {
		if base, ok := relMDLeaf(target); ok {
			return "/projects/" + projectID + "/twin/" + base
		}
		return ""
	}
	_, body := splitFrontmatter(src)
	vm := docVM{
		Title: doc,
		Crumbs: []crumb{
			{Label: "dashboard", Href: "/"},
			{Label: "projects", Href: "/projects"},
			{Label: projectID, Href: "/projects/" + projectID},
			{Label: doc},
		},
		Meta: s.gatherFileMeta(time.Now().UTC(), rel),
		Body: template.HTML(md.Render(body, resolve)),
	}
	s.render(w, r, "doc.html", vm)
}

// --- helpers ----------------------------------------------------------

// readDoc reads root-relative rel and writes the right status on
// failure: 404 for a missing file (a stale bookmark shouldn't 500), 500
// for anything else. ok=false means a response was already written.
func (s *Server) readDoc(w http.ResponseWriter, r *http.Request, rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(s.opts.Root, rel))
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return "", false
		}
		http.Error(w, "read: "+err.Error(), http.StatusInternalServerError)
		return "", false
	}
	return string(b), true
}

func (s *Server) knowledgeDir(projectID string) string {
	return filepath.Join(s.opts.Root, "projects", projectID, "knowledge")
}

func (s *Server) knowledgeTopicsDir(projectID string) string {
	return filepath.Join(s.knowledgeDir(projectID), "topics")
}

// runChoreCounts buckets the single GatherDash result into per-project
// run and chore counts. Returns empty maps when GatherDash is unset.
func (s *Server) runChoreCounts() (runs, chores map[string]int) {
	runs, chores = map[string]int{}, map[string]int{}
	if s.opts.GatherDash == nil {
		return runs, chores
	}
	rows, _, _, _, err := s.opts.GatherDash("")
	if err != nil {
		return runs, chores
	}
	for _, row := range rows {
		if row.Bucket == dash.BucketChores {
			chores[row.Project]++
		} else {
			runs[row.Project]++
		}
	}
	return runs, chores
}

// splitFrontmatter separates a leading YAML frontmatter block from the
// body. The frontmatter is parsed only for the flat "key: value" pairs
// the corpus uses (lore's title / applies-when); no YAML parser. When
// there's no frontmatter the whole input is the body.
func splitFrontmatter(src string) (front map[string]string, body string) {
	front = map[string]string{}
	if !strings.HasPrefix(src, "---\n") {
		return front, src
	}
	rest := src[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return front, src // unterminated → treat as body, don't eat content
	}
	block := rest[:end]
	for _, line := range strings.Split(block, "\n") {
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		front[key] = val
	}
	// Body starts after the closing fence line.
	after := rest[end+len("\n---"):]
	after = strings.TrimPrefix(after, "\n")
	return front, strings.TrimPrefix(after, "\n")
}

// relMDLeaf reports whether target is a relative, single-segment ".md"
// link (e.g. "patterns.md") and returns the leaf without the suffix.
func relMDLeaf(target string) (string, bool) {
	if strings.Contains(target, "://") || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "#") {
		return "", false
	}
	if strings.Contains(target, "/") || !strings.HasSuffix(target, ".md") {
		return "", false
	}
	return strings.TrimSuffix(target, ".md"), true
}

// topicLeaf reports whether target is a "topics/<name>.md" relative link
// and returns <name>.
func topicLeaf(target string) (string, bool) {
	if !strings.HasPrefix(target, "topics/") || !strings.HasSuffix(target, ".md") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(target, "topics/"), ".md")
	if strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// relTo returns path relative to root (for `git log -- <rel>`); falls
// back to path on any error.
func relTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

func readFileString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// listMarkdown returns the sorted .md filenames in dir, minus skip and
// dot/underscore-prefixed files. Missing dir → nil.
func listMarkdown(dir string, skip map[string]bool) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || skip[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func countMarkdown(dir string, skip map[string]bool) int {
	return len(listMarkdown(dir, skip))
}
