package serve

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/agentdefault"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// These inputs were already rejected before canonical selection. Exercise
// the HTTP decoding and journal boundary, not just the validators in isolation.
func TestFormBoundariesRejectHostileValuesWithoutWriting(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "my-idea", "idea")
	gittest.Commit(t, root, "seed idea")
	if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{Root: root})
	for _, tc := range []struct {
		path, field, valid, metadata string
	}{
		{"/agent", "agent", "codex", agentdefault.FileName},
		{"/projects/alpha/mode", "mode", "safe", "projects/alpha/project.json"},
		{"/projects/alpha/ship", "ship", "merge", "projects/alpha/project.json"},
		{"/run/alpha/my-idea/tag", "workflow", "sdlc", "projects/alpha/runs/my-idea/run.json"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			path := filepath.Join(root, tc.metadata)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			head := gittest.HeadSHA(t, root)
			for _, input := range []string{
				"--help", "-c core.sshCommand=INERT_PROBE", "$(INERT_PROBE)",
				"; INERT_PROBE", tc.valid + "\nINERT_PROBE",
				tc.valid + "; INERT_PROBE", tc.valid + "$(INERT_PROBE)",
				strings.ToUpper(tc.valid),
			} {
				form := url.Values{tc.field: {input}, "revision": {"1"}}.Encode()
				if rec := postForm(t, s, tc.path, form); rec.Code != http.StatusBadRequest {
					t.Fatalf("%q: status=%d, body=%s", input, rec.Code, rec.Body.String())
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(before) {
					t.Fatalf("%q: metadata changed or unreadable: %v", input, err)
				}
				if gittest.HeadSHA(t, root) != head {
					t.Fatalf("%q: rejection committed", input)
				}
				if status := gittest.Output(t, root, "status", "--porcelain"); status != "" {
					t.Fatalf("%q: rejection left changes: %s", input, status)
				}
			}
		})
	}
}

func TestProjectFormSelectionsCommitCanonicalValues(t *testing.T) {
	s := modeServer(t)
	root := s.opts.Root
	for _, tc := range []struct {
		field  string
		values []string
	}{
		{"mode", []string{"paused", "safe", "auto"}},
		{"ship", []string{"merge", "pr"}},
	} {
		for _, value := range tc.values {
			t.Run(tc.field+"/"+value, func(t *testing.T) {
				// These routes have always trimmed surrounding whitespace.
				form := url.Values{tc.field: {" \t" + value + "\n"}}.Encode()
				path := "/projects/alpha/" + tc.field
				if rec := postForm(t, s, path, form); rec.Code != http.StatusSeeOther {
					t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
				}
				md, err := project.Load(root, "alpha")
				if err != nil {
					t.Fatal(err)
				}
				stored := md.Mode
				if tc.field == "ship" {
					stored = md.Ship
				}
				want := value
				if value == "auto" || value == "pr" {
					want = ""
				}
				if stored != want {
					t.Fatalf("stored=%q, want %q", stored, want)
				}
				if got := gittest.Output(t, root, "log", "-1", "--format=%s"); got != "Set project alpha "+tc.field+": "+value {
					t.Fatalf("commit subject=%q", got)
				}
				head := gittest.HeadSHA(t, root)
				if rec := postForm(t, s, path, form); rec.Code != http.StatusSeeOther || gittest.HeadSHA(t, root) != head {
					t.Fatalf("duplicate changed HEAD or failed: %d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestAgentFormSelectionsUseRegistryAndReset(t *testing.T) {
	backend, err := agent.Get("codex")
	if err != nil {
		t.Fatal(err)
	}
	agent.Register("registered-test-backend", backend)
	t.Cleanup(func() { agent.Unregister("registered-test-backend") })
	root := gittest.InitSeeded(t)
	s := newTestServer(t, Options{Root: root})
	revision := 0
	for _, name := range append(agent.Names(), "reset") {
		form := url.Values{"agent": {name}, "revision": {fmt.Sprint(revision)}}.Encode()
		if rec := postForm(t, s, "/agent", form); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		revision++
		state, err := agentdefault.Read(root)
		want, label := name, name
		if name == "reset" {
			want, label = "", "stylesheet"
		}
		if err != nil || state.Agent != want || state.Revision != revision {
			t.Fatalf("%s: state=%+v, err=%v", name, state, err)
		}
		if got := gittest.Output(t, root, "log", "-1", "--format=%s"); got != "Set temporary default agent: "+label {
			t.Fatalf("commit subject=%q", got)
		}
		head := gittest.HeadSHA(t, root)
		if rec := postForm(t, s, "/agent", form); rec.Code != http.StatusSeeOther || gittest.HeadSHA(t, root) != head {
			t.Fatalf("duplicate changed HEAD or failed: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestTagWorkflowSelectsConfiguredNames(t *testing.T) {
	s := newTestServer(t, Options{Root: t.TempDir(), TagWorkflows: []string{"custom", "sdlc"}})
	for input, want := range map[string]string{"": "custom", "custom": "custom", "sdlc": "sdlc"} {
		if got, ok := s.tagWorkflow(input); !ok || got != want {
			t.Fatalf("tagWorkflow(%q)=%q, %v; want %q, true", input, got, ok, want)
		}
	}
	if got, ok := s.tagWorkflow("unknown"); ok || got != "" {
		t.Fatalf("unknown workflow=%q, %v", got, ok)
	}
	s.opts.TagWorkflows = nil
	if got, ok := s.tagWorkflow(""); ok || got != "" {
		t.Fatalf("unconfigured default=%q, %v", got, ok)
	}
}

func TestIdeaTagFormSelectionsCommitCanonicalValues(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "my-idea", "idea")
	gittest.Commit(t, root, "seed idea")
	s := newTestServer(t, Options{Root: root})
	for _, form := range []url.Values{
		{}, // absent workflow selects the configured default
		{"workflow": {" \tsdlc\n"}},
		{"workflow": {"sdlc"}, "design_only": {"1"}},
	} {
		if rec := postForm(t, s, "/run/alpha/my-idea/tag", form.Encode()); rec.Code != http.StatusSeeOther {
			t.Fatalf("%v: %d %s", form, rec.Code, rec.Body.String())
		}
		md, err := run.Load(root, "alpha", "my-idea")
		only := form.Get("design_only") == "1"
		if err != nil || md.PromoteTo != "sdlc" || md.DesignOnly != only {
			t.Fatalf("%v: metadata=%+v, err=%v", form, md, err)
		}
		want := "Tag idea alpha/my-idea (sdlc)"
		if only {
			want = "Tag idea alpha/my-idea (sdlc, design only)"
		}
		if got := gittest.Output(t, root, "log", "-1", "--format=%s"); got != want {
			t.Fatalf("commit subject=%q, want %q", got, want)
		}
		head := gittest.HeadSHA(t, root)
		if rec := postForm(t, s, "/run/alpha/my-idea/tag", form.Encode()); rec.Code != http.StatusSeeOther || gittest.HeadSHA(t, root) != head {
			t.Fatalf("duplicate changed HEAD or failed: %d %s", rec.Code, rec.Body.String())
		}
	}
}
