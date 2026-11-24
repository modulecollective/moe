package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func rootProjectFixture(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	return root
}

func TestCaptureCreatesTheRootProjectLazily(t *testing.T) {
	root := rootProjectFixture(t)
	for _, args := range [][]string{
		{"intent", "new", project.RootID + "/ship-less"},
		{"idea", "new", project.RootID + "/disk-ran-out"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 0 {
			t.Fatalf("%v: exit=%d stderr=%q", args, code, errb.String())
		}
	}
	log, err := git.Output(root, "log", "--format=%s", "--", filepath.Join("projects", project.RootID, "project.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(log); got != "Create root project "+project.RootID {
		t.Fatalf("root project commits = %q", got)
	}
}

func TestRootProjectRefusesSourceWork(t *testing.T) {
	root := rootProjectFixture(t)
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "new", "--park", project.RootID + "/x"}, &out, &errb)
	if code == 0 || !strings.Contains(errb.String(), "has no submodule on disk") {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, "projects", project.RootID, "runs", "x")); !os.IsNotExist(err) {
		t.Fatalf("refused run left a directory: %v", err)
	}
}

func TestBureaucracyIntentsReachEveryProjectsPrompt(t *testing.T) {
	root := rootProjectFixture(t)
	for _, args := range [][]string{
		{"intent", "new", project.RootID + "/ship-less"},
		{"intent", "new", "tele/faster-everything"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 0 {
			t.Fatalf("%v: exit=%d stderr=%q", args, code, errb.String())
		}
	}
	got := intentsReferenceSection(root, "tele")
	top, own := strings.Index(got, "## Bureaucracy intents"), strings.Index(got, "## Project intents")
	if top < 0 || own < top || !strings.Contains(got, "`ship-less`") || !strings.Contains(got, "`faster-everything`") {
		t.Fatalf("section:\n%s", got)
	}
	if n := strings.Count(got, "the pulse keeps each one's"); n != 1 {
		t.Fatalf("keeper rule rendered %d times", n)
	}
	inRoot := intentsReferenceSection(root, project.RootID)
	if strings.Count(inRoot, "## Bureaucracy intents") != 1 || strings.Count(inRoot, "`ship-less`") != 1 || !strings.Contains(inRoot, "`faster-everything`") {
		t.Fatalf("root section repeats itself:\n%s", inRoot)
	}
}

func TestFollowupGrammarRoutesToTheRootProject(t *testing.T) {
	m := followupOpenRE.FindStringSubmatch("- [ ] `_cabinet/disk-ran-out` — Recover the box")
	if m == nil || m[2] != "_cabinet/disk-ran-out" {
		t.Fatalf("match = %q", m)
	}
	if followupOpenRE.MatchString("- [ ] `_other/x` — Not a project") {
		t.Fatal("only the root project may carry an underscore")
	}
}
