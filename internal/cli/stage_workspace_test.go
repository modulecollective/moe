package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/workspace"
)

// stageWorkspaceFixture opens tele/fix-it (no workspace unless newArgs
// carries one) against a project with a real submodule, so attach,
// release and the work probe all run against real clones.
func stageWorkspaceFixture(t *testing.T, newArgs ...string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	var out, errb bytes.Buffer
	if code := runNew("sdlc", append(newArgs, "tele/fix-it"), &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	return root
}

// attachFixtureTree builds the run's current working tree the way a
// design turn leaves it: attached, on the run branch, no commits.
func attachFixtureTree(t *testing.T, root string) string {
	t.Helper()
	md := loadRun(t, root, "tele", "fix-it")
	path, err := attachTestRunWorkspace(t, root, md, branchPrefix+md.ID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func runJSONBytes(t *testing.T, root string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, run.Dir("tele", "fix-it"), "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestStageWorkspaceSameBindingIsNoop: the operator's reported command —
// retyping the binding a run already has — proceeds with no commit.
func TestStageWorkspaceSameBindingIsNoop(t *testing.T) {
	for _, sv := range sdlcStageVerbs {
		t.Run(sv.name, func(t *testing.T) {
			root := stageWorkspaceFixture(t, "--workspace", "tele/dev")
			attachFixtureTree(t, root) // run holds the claim, as after design
			stages := stubOpenSdlcStage(t, nil)
			before := gittest.HeadSHA(t, root)

			var out, errb bytes.Buffer
			if code := sv.run([]string{"--workspace", "tele/dev", "--once", "tele/fix-it"}, &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			if got := gittest.HeadSHA(t, root); got != before {
				t.Fatalf("HEAD moved on a no-op rebind")
			}
			if strings.Contains(out.String(), "bound run") {
				t.Fatalf("no-op announced a bind: %q", out.String())
			}
			if len(*stages) != 1 {
				t.Fatalf("stage opens = %d, want 1", len(*stages))
			}
		})
	}
}

// TestStageWorkspaceBindsUnboundRun: an unbound run with no tree gains
// the bare name in run.json in one trailered commit, before the
// cascade dispatches.
func TestStageWorkspaceBindsUnboundRun(t *testing.T) {
	for _, sv := range sdlcStageVerbs {
		t.Run(sv.name, func(t *testing.T) {
			root := stageWorkspaceFixture(t)
			stubOpenSdlcStage(t, nil)
			var bound string
			prev := openSdlcStage
			openSdlcStage = func(stage, projectID, runID string, _ bool, _, _ io.Writer) int {
				bound = loadRun(t, root, projectID, runID).Workspace
				return 0
			}
			t.Cleanup(func() { openSdlcStage = prev })
			before := gittest.HeadSHA(t, root)

			var out, errb bytes.Buffer
			if code := sv.run([]string{"--workspace", "tele/dev", "--once", "tele/fix-it"}, &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			if bound != "dev" {
				t.Fatalf("workspace at dispatch = %q, want dev", bound)
			}
			if !strings.Contains(out.String(), "bound run to workspace tele/dev") {
				t.Fatalf("stdout=%q", out.String())
			}
			if n := strings.TrimSpace(gittest.Output(t, root, "rev-list", "--count", before+"..HEAD")); n != "1" {
				t.Fatalf("commits = %s, want 1", n)
			}
			log := gitLogFormat(t, root, 1, "HEAD", "%B")
			for _, want := range []string{
				"Bind workspace for tele/fix-it to dev",
				"MoE-Run: fix-it",
				"MoE-Project: tele",
				"MoE-Workflow: sdlc",
				"MoE-Document: " + sv.name,
			} {
				if !strings.Contains(log, want) {
					t.Fatalf("commit missing %q: %q", want, log)
				}
			}
			if files := gittest.Output(t, root, "show", "--name-only", "--format=", "HEAD"); strings.TrimSpace(files) != filepath.Join(run.Dir("tele", "fix-it"), "run.json") {
				t.Fatalf("commit touched %q", files)
			}
		})
	}
}

// TestStageWorkspaceReleasesCleanClone: a post-design per-run clone with
// no commits and only untracked scratch is dropped once the run binds.
func TestStageWorkspaceReleasesCleanClone(t *testing.T) {
	root := stageWorkspaceFixture(t)
	attachFixtureTree(t, root)
	stubOpenSdlcStage(t, nil)

	var out, errb bytes.Buffer
	if code := runBuild([]string{"--workspace", "tele/dev", "--once", "tele/fix-it"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := loadRun(t, root, "tele", "fix-it").Workspace; got != "dev" {
		t.Fatalf("workspace = %q", got)
	}
	if sandbox.Exists(root, "tele", "fix-it") {
		t.Fatalf("per-run clone survived the rebind")
	}
}

// TestStageWorkspaceReleasesHeldNamedWorkspace: moving between named
// workspaces drops the old claim when the old tree is clean.
func TestStageWorkspaceReleasesHeldNamedWorkspace(t *testing.T) {
	root := stageWorkspaceFixture(t, "--workspace", "tele/old")
	attachFixtureTree(t, root)
	stubOpenSdlcStage(t, nil)

	var out, errb bytes.Buffer
	if code := runBuild([]string{"--workspace", "tele/dev", "--once", "tele/fix-it"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := loadRun(t, root, "tele", "fix-it").Workspace; got != "dev" {
		t.Fatalf("workspace = %q", got)
	}
	if c, err := workspace.ReadClaim(root, "tele", "old"); err != nil || c != nil {
		t.Fatalf("old claim = %+v, %v; want released", c, err)
	}
	if !workspace.Exists(root, "tele", "old") {
		t.Fatalf("named workspace directory removed; it should outlive the run")
	}
}

// TestStageWorkspaceRefusesWhenTreeHasWork: commits on the run branch
// or a modified tracked file live only in the current tree, so the
// rebind refuses before writing anything.
func TestStageWorkspaceRefusesWhenTreeHasWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		dirt func(t *testing.T, clone string)
		want string
	}{
		{"commit", func(t *testing.T, clone string) {
			writeFile(t, filepath.Join(clone, "new.txt"), "work\n")
			gittest.Run(t, clone, "add", "new.txt")
			gittest.Run(t, clone, "commit", "-m", "work")
		}, "unique_git_objects"},
		{"modified", func(t *testing.T, clone string) {
			writeFile(t, filepath.Join(clone, "README.md"), "edited\n")
		}, "local_modifications"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := stageWorkspaceFixture(t)
			clone := attachFixtureTree(t, root)
			tc.dirt(t, clone)
			stages := stubOpenSdlcStage(t, nil)
			before := gittest.HeadSHA(t, root)
			runJSON := runJSONBytes(t, root)

			var out, errb bytes.Buffer
			code := runBuild([]string{"--workspace", "tele/dev", "--once", "tele/fix-it"}, &out, &errb)
			if code != 1 {
				t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
			}
			for _, want := range []string{tc.want, "rebind held"} {
				if !strings.Contains(errb.String(), want) {
					t.Errorf("stderr missing %q: %q", want, errb.String())
				}
			}
			if gittest.HeadSHA(t, root) != before || !bytes.Equal(runJSONBytes(t, root), runJSON) {
				t.Fatalf("refusal wrote state")
			}
			if st := gittest.Output(t, root, "status", "--porcelain"); st != "" {
				t.Fatalf("bureaucracy dirty after refusal: %q", st)
			}
			if !sandbox.Exists(root, "tele", "fix-it") {
				t.Fatalf("clone removed on refusal")
			}
			if len(*stages) != 0 {
				t.Fatalf("stage opened after refusal")
			}
		})
	}
}

// TestStageWorkspaceRefusesClaimedOrMalformed: the shared check refuses
// a workspace another run holds (exit 1) and a malformed value (exit
// 2) before anything is written or opened.
func TestStageWorkspaceRefusesClaimedOrMalformed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		code  int
		want  string
	}{
		{"claimed", "tele/dev", 1, "claimed by run tele/other"},
		{"invalid", "tele/Not Valid", 2, "must match"},
		{"bare", "dev", 2, "want <project>/<name> — try tele/dev"},
		{"other-project", "web/dev", 2, `names project "web", but the run is in "tele"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := stageWorkspaceFixture(t)
			if _, err := workspace.Acquire(root, "tele", "dev", "tele/other"); err != nil {
				t.Fatal(err)
			}
			stages := stubOpenSdlcStage(t, nil)
			before := gittest.HeadSHA(t, root)
			var out, errb bytes.Buffer
			code := runReview([]string{"--workspace", tc.value, "--once", "tele/fix-it"}, &out, &errb)
			if code != tc.code || !strings.Contains(errb.String(), tc.want) {
				t.Fatalf("exit=%d stderr=%q, want %d / %q", code, errb.String(), tc.code, tc.want)
			}
			if gittest.HeadSHA(t, root) != before || len(*stages) != 0 {
				t.Fatalf("refusal wrote or opened")
			}
		})
	}
}

// TestWorkspaceFlagShapeOnOpenVerbs: new and reopen take the same
// `<project>/<name>` shape through the same check.
func TestWorkspaceFlagShapeOnOpenVerbs(t *testing.T) {
	root := stageWorkspaceFixture(t)
	for _, verb := range []string{"new", "reopen"} {
		for _, tc := range []struct{ value, want string }{
			{"dev", "want <project>/<name> — try tele/dev"},
			{"web/dev", `names project "web", but the run is in "tele"`},
		} {
			var out, errb bytes.Buffer
			code := Run([]string{"sdlc", verb, "--workspace", tc.value, "tele/fix-it-2"}, &out, &errb)
			if code != 2 || !strings.Contains(errb.String(), tc.want) {
				t.Errorf("%s --workspace %s: exit=%d stderr=%q", verb, tc.value, code, errb.String())
			}
		}
	}
	if _, err := run.Load(root, "tele", "fix-it-2"); err == nil {
		t.Fatalf("a refused new still opened a run")
	}
}

// TestStageWorkspaceInteractiveOpensInNamedTree drives the real stage
// session (a fake claude records its argv): `sdlc design --workspace`
// on an unbound run attaches .moe/named/tele/dev and claims it.
func TestStageWorkspaceInteractiveOpensInNamedTree(t *testing.T) {
	root := stageWorkspaceFixture(t)
	argvFile := filepath.Join(t.TempDir(), "argv")
	fakeClaudeOnPath(t, `#!/bin/sh
printf '%s\n' "$@" > '`+argvFile+`'
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
if [ -n "$canvas" ]; then printf '# Design\n\n## Gate\n\n`+"```"+`json\n{"status":"ready"}\n`+"```"+`\n\n## Problem\n\ndone\n' > "$canvas"; fi
exit 0
`)
	var out, errb bytes.Buffer
	if code := runDesign([]string{"--workspace", "tele/dev", "--once", "tele/fix-it"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	got, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake claude never ran: %v; stderr=%q", err, errb.String())
	}
	// The session runs from the bureaucracy worktree; the working tree
	// reaches the agent as its writable add-dir.
	if want := workspace.Path(root, "tele", "dev"); !strings.Contains(string(got), want+"\n") {
		t.Fatalf("session argv lacks %s:\n%s", want, got)
	}
	if strings.Contains(string(got), sandbox.Path(root, "tele", "fix-it")) {
		t.Fatalf("session still pointed at the per-run clone:\n%s", got)
	}
	if c, err := workspace.ReadClaim(root, "tele", "dev"); err != nil || c == nil || c.Run != "tele/fix-it" {
		t.Fatalf("claim = %+v, %v", c, err)
	}
}
