package cli

import (
	"bytes"
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/ui"
)

// Real definitions and journal timestamps exercise the gatherer's due filter,
// including time-only transitions through the HEAD-keyed journal/edit caches.
func TestRegisteredChoresGatherUIAndHTTP(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root := newMoeHome(t)
	trailerstest.SeedProject(t, root, "alpha")
	trailerstest.SeedProject(t, root, "beta")
	trailerstest.SeedRun(t, root, "alpha", "linked", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "alpha", "idea", "idea", run.StatusInProgress)
	now := time.Now().UTC().Truncate(time.Second)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	criterion := "landed <artifact> changes docs with a full unabridged criterion"
	prompt := "# Full authored prompt\n\nInspect everything.\x1b[2J\n" + strings.Repeat("More maintenance detail.\n", 60) + "Final prompt line.\n"
	defs := map[string]string{
		"judged":  `{"when":"` + criterion + `"}`,
		"due":     `{"cadence":"1d"}`,
		"idle":    `{"cadence":"1d","cooldown":"2h","trigger":"*.go"}`,
		"cooling": `{"cadence":"1d","cooldown":"12h"}`,
		"expired": `{"when":"check the landed work","cooldown":"2h"}`,
		"linked":  `{"when":"check linked work","cooldown":"12h"}`,
	}
	for name, def := range defs {
		write("projects/alpha/chores/"+name+"/chore.json", def)
		write("projects/alpha/chores/"+name+"/prompt.md", prompt)
	}
	write("projects/beta/chores/other/chore.json", `{"when":"other project"}`)
	gittest.Run(t, root, "add", "-A")
	stamp := now.Add(-48 * time.Hour).Format(time.RFC3339)
	gittest.RunWithEnv(t, root, []string{"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp}, "commit", "-m", "register chores")
	trailerstest.CommitTrailer(t, root, "open chore run", "MoE-Project: alpha\nMoE-Run: linked\nMoE-Chore: alpha/linked", now.Add(-8*time.Hour))
	for _, name := range []string{"idle", "cooling", "expired", "linked"} {
		trailerstest.CommitTrailer(t, root, "skip chore", "MoE-Chore-Skipped: alpha/"+name, now.Add(-6*time.Hour))
	}
	before := gittest.HeadSHA(t, root)
	// Any accidentally launched agent fails visibly without spending a session.
	bin := t.TempDir()
	marker := filepath.Join(bin, "called")
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 91\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	gather := func(at time.Time, filter DashFilter) DashSnapshot {
		t.Helper()
		s, err := GatherDashSnapshot(root, at, filter)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	snap := gather(now, DashFilter{})
	if len(snap.Chores) != 7 || snap.ActiveProjects != 1 {
		t.Fatalf("snapshot chores=%d active=%d", len(snap.Chores), snap.ActiveProjects)
	}
	wantStates := map[string]string{"judged": "judged — pulse decides", "due": "due — ", "idle": "not due", "cooling": "cooling down until ", "expired": "judged — pulse decides", "linked": "open run linked"}
	var choreOrder []string
	for _, r := range snap.Rows {
		if r.Bucket != dash.BucketChores {
			continue
		}
		choreOrder = append(choreOrder, r.Project+"/"+r.Run)
		if r.Control != "" {
			t.Fatalf("chore inherited run overlay: %+v", r)
		}
		if r.Project == "alpha" && !strings.HasPrefix(r.Note, wantStates[r.Run]) {
			t.Fatalf("state %s: %s", r.Run, r.Note)
		}
	}
	if len(choreOrder) != 7 || choreOrder[0] != "alpha/due" || !slices.IsSorted(choreOrder[1:]) {
		t.Fatalf("order=%v", choreOrder)
	}
	if dash.Live(snap.Rows) {
		t.Fatal("no open session, yet the board reads live")
	}
	for _, f := range []DashFilter{{ProjectFilter: "alpha"}, {WorkflowFilter: "sdlc"}} {
		s := gather(now, f)
		for _, r := range s.Rows {
			if f.ProjectFilter != "" && r.Project != "alpha" || f.WorkflowFilter != "" && r.Bucket == dash.BucketChores {
				t.Fatalf("filter %+v: %+v", f, r)
			}
		}
		if f.ProjectFilter != "" && len(s.Chores) != 6 || f.WorkflowFilter != "" && len(s.Chores) != 0 {
			t.Fatalf("filtered states %+v", f)
		}
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"dash", "--project", "alpha"}, &out, &errb); code != 0 {
		t.Fatalf("dash exit %d: %s", code, &errb)
	}
	for _, want := range []string{"CHORES (6)", "BACKLOG (1)", "judged — pulse decides", "open run linked"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dash missing %q:\n%s", want, &out)
		}
	}
	source := newUISource(root, DashFilter{ProjectFilter: "alpha"}, os.Stdout)
	at := now
	source.gather = func(_ time.Time, cached liveLookup) (DashSnapshot, error) {
		return gatherDashSnapshot(root, at, source.filter, cached)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := source.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	b := takeBoard(t, source)
	for _, it := range b.Items {
		if !strings.Contains(b.Lines[it.Line], it.Label()) {
			t.Fatalf("bad index: %+v", it)
		}
	}
	judged := uiItem(t, b, "alpha", "judged", ui.ItemChore)
	if d := strings.Join(judged.Detail, "\n"); !strings.Contains(d, criterion) || !strings.Contains(d, prompt) || !strings.Contains(d, "last completion: never") {
		t.Fatalf("incomplete detail: %s", d)
	}
	for _, it := range b.Items {
		// A chore is editable (its definition, in the text form) and takes
		// none of a run's capabilities.
		if it.Kind == ui.ItemChore && (it.Controls || !it.Editable || it.Notes || it.Terminal || it.Question != nil || it.Generation != "") {
			t.Fatalf("chore capabilities: %+v", it)
		}
		if it.Kind == ui.ItemChore {
			spec, err := chore.ParseSpecText(it.Body)
			if want, _ := chore.LoadSpec(root, it.Project, it.Run); err != nil || spec != want {
				t.Fatalf("chore body %q does not round-trip: %v", it.Body, err)
			}
		}
	}
	rows, _ := source.overlay(now)
	for _, r := range rows {
		if r.Bucket == dash.BucketChores && r.Control != "" {
			t.Fatalf("live overlay on registration: %+v", r)
		}
	}
	linked := uiItem(t, b, "alpha", "linked", ui.ItemChore)
	runItem := uiItem(t, b, "alpha", "linked", ui.ItemRun)
	if linked.Bucket == runItem.Bucket || !runItem.Controls || !runItem.Notes || !strings.Contains(strings.Join(linked.Detail, "\n"), "linked run: linked") {
		t.Fatalf("identity/capabilities: chore=%+v run=%+v", linked, runItem)
	}
	row, ok, err := GatherRunRow(root, "alpha", "linked", now)
	if err != nil || !ok || row.Bucket == dash.BucketChores {
		t.Fatalf("run lookup: %+v %v %v", row, ok, err)
	}
	cooling := uiItem(t, b, "alpha", "cooling", ui.ItemChore)
	if d := strings.Join(cooling.Detail, "\n"); !strings.Contains(d, "cooldown: 12h0m0s") || !strings.Contains(d, "cooldown eligible:") || !strings.Contains(d, "last completion: ") {
		t.Fatal(d)
	}
	for _, advance := range []struct {
		d     time.Duration
		state string
	}{{6 * time.Hour, "not due"}, {18 * time.Hour, "due — cadence"}} {
		at = now.Add(advance.d)
		if err := source.refresh(ctx); err != nil {
			t.Fatal(err)
		}
		b = takeBoard(t, source)
		it := uiItem(t, b, "alpha", "cooling", ui.ItemChore)
		if it.Bucket != cooling.Bucket || !strings.Contains(b.Lines[it.Line], advance.state) {
			t.Fatalf("time-only transition %s: %+v", advance.d, it)
		}
	}
	// Use the same callbacks as moe serve, including its real GatherChore and
	// GatherRunRow paths; no listener or heartbeat is started.
	s, err := serve.New(serveOptions(root, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string) string {
		t.Helper()
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	for _, path := range []string{"/", "/projects/alpha"} {
		body := request(path)
		for _, want := range []string{`href="/chore/alpha/judged"`, `href="/chore/alpha/cooling"`, `href="/chore/alpha/linked"`, `href="/run/alpha/linked"`, "judged — pulse decides", "cooling down until", "open run linked"} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q", path, want)
			}
		}
		if !(strings.Index(body, "<span>intents") < strings.Index(body, "<span>chores") && strings.Index(body, "<span>chores") < strings.Index(body, "<span>backlog")) {
			t.Fatalf("%s section order", path)
		}
		if !strings.Contains(body, html.EscapeString(criterion)) || strings.Contains(body, "landed <artifact>") {
			t.Fatalf("%s criterion escaping", path)
		}
		if path != "/" && strings.Contains(body, `href="/chore/beta/other"`) {
			t.Fatal("unfiltered hub")
		}
	}
	if body := request("/projects"); !strings.Contains(body, "6 chores") {
		t.Fatal("project count misses non-due chores")
	}
	for _, name := range []string{"judged", "cooling", "linked"} {
		body := request("/chore/alpha/" + name)
		// "open now" is offered exactly when no run is open; it is a form
		// post, so rendering it opens nothing (HEAD is checked below).
		if !strings.Contains(body, "Full authored prompt") || strings.Contains(body, "/open\"") != (name != "linked") {
			t.Fatalf("detail %s", name)
		}
		if name == "judged" && !strings.Contains(body, html.EscapeString(criterion)) {
			t.Fatal("judged criterion missing")
		}
		if name == "linked" && !strings.Contains(body, `href="/run/alpha/linked"`) {
			t.Fatal("linked navigation missing")
		}
	}
	request("/run/alpha/linked")
	if gittest.HeadSHA(t, root) != before {
		t.Fatal("read paths changed journal HEAD")
	}
	mds, err := run.Scan(root)
	if err != nil || len(mds) != 2 {
		t.Fatalf("read paths opened runs: %d %v", len(mds), err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("read paths invoked an agent")
	}
}

// Registrations precede completed rows, so a same-name lookup must skip them.
func TestGatherRunRowSkipsSameNameChore(t *testing.T) {
	root := newMoeHome(t)
	trailerstest.SeedRun(t, root, "alpha", "same", "sdlc", run.StatusMerged)
	gittest.WriteAndCommit(t, root, "projects/alpha/chores/same/chore.json", `{"when":"inspect changes"}`, "register same-name chore")
	row, found, err := GatherRunRow(root, "alpha", "same", time.Now())
	if err != nil || !found || row.Bucket != dash.BucketCompletedRuns {
		t.Fatalf("row=%+v found=%v err=%v", row, found, err)
	}
}
