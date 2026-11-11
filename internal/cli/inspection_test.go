package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/workspace"
)

func inspectionFile(t *testing.T, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func inspectionFixture(t *testing.T, named bool) (string, string, *run.Metadata) {
	t.Helper()
	root := t.TempDir()
	md := stageMetadata("demo", "inspect", "sdlc")
	if named {
		md.Workspace = "warm"
	}
	repo := sandbox.Path(root, md.Project, md.ID)
	if named {
		repo = workspace.Path(root, md.Project, md.Workspace)
	}
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gittest.InitAt(t, repo)
	gittest.Run(t, repo, "checkout", "-b", "trunk")
	gittest.WriteAndCommit(t, repo, "before.txt", "original\n", "base")
	gittest.Run(t, repo, "checkout", "-b", "moe/"+md.ID)
	inspectionFile(t, filepath.Join(root, "projects/demo/project.json"), `{"remote":"unused","default_branch":"trunk"}`, 0600)
	return root, repo, md
}

func TestInspectionDiffSnapshotsWholeRunWithoutMutations(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprint(named), func(t *testing.T) {
			root, repo, md := inspectionFixture(t, named)
			base := gittest.HeadSHA(t, repo)
			gittest.WriteAndCommit(t, repo, "one.txt", "first change\n", "one")
			gittest.Run(t, repo, "mv", "before.txt", "renamed.txt")
			inspectionFile(t, filepath.Join(repo, "binary"), "\x00\x01\x02", 0600)
			inspectionFile(t, filepath.Join(repo, "executable"), "#!/bin/sh\n", 0700)
			tip := gittest.Commit(t, repo, "two")
			gittest.Run(t, repo, "checkout", "trunk")
			gittest.WriteAndCommit(t, repo, "diverged.txt", "not in patch\n", "diverge")
			inspectionFile(t, filepath.Join(repo, "one.txt"), "dirty excluded\n", 0600)
			gittest.Run(t, repo, "add", "one.txt")
			beforeRefs := gittest.Output(t, repo, "show-ref")
			beforeIndex, err := os.ReadFile(filepath.Join(repo, ".git/index"))
			if err != nil {
				t.Fatal(err)
			}
			beforeStatus := gittest.Output(t, repo, "--no-optional-locks", "status", "--porcelain=v1")
			var out bytes.Buffer
			a, err := sourceDiffArtifact(root, md, &out)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"+first change", "rename from before.txt", "rename to renamed.txt", "Binary files", "new file mode 100755"} {
				if !strings.Contains(a.body, want) {
					t.Fatalf("patch missing %q:\n%s", want, a.body)
				}
			}
			if !strings.HasPrefix(a.body, "diff --git ") || strings.Contains(a.body, "dirty excluded") || strings.Contains(a.body, "diverged.txt") {
				t.Fatalf("wrong scope:\n%s", a.body)
			}
			for _, want := range []string{repo, "default branch: trunk", base, tip, "uncommitted files excluded"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("summary missing %q: %s", want, &out)
				}
			}
			afterIndex, err := os.ReadFile(filepath.Join(repo, ".git/index"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeIndex, afterIndex) || beforeRefs != gittest.Output(t, repo, "show-ref") || beforeStatus != gittest.Output(t, repo, "--no-optional-locks", "status", "--porcelain=v1") {
				t.Fatal("inspection mutated repository")
			}
		})
	}
}

func TestInspectionDiffEmptyMissingRefsAndWorkspace(t *testing.T) {
	root, repo, md := inspectionFixture(t, false)
	var out bytes.Buffer
	a, err := sourceDiffArtifact(root, md, &out)
	if err != nil || a.body != "" || !strings.Contains(out.String(), "no committed source changes") {
		t.Fatalf("empty: %+v %v %s", a, err, &out)
	}
	gittest.Run(t, repo, "update-ref", "-d", "refs/heads/moe/"+md.ID)
	if _, err := sourceDiffArtifact(root, md, io.Discard); err == nil {
		t.Fatal("missing run ref accepted")
	}
	md.ID = "absent"
	if _, err := sourceDiffArtifact(root, md, io.Discard); err == nil {
		t.Fatal("missing workspace accepted")
	}
	if _, err := os.Stat(sandbox.Path(root, md.Project, md.ID)); !os.IsNotExist(err) {
		t.Fatalf("workspace created: %v", err)
	}
}

func TestInspectionPinsRefsBeforeComparing(t *testing.T) {
	root, repo, md := inspectionFixture(t, false)
	base := gittest.HeadSHA(t, repo)
	tip := gittest.WriteAndCommit(t, repo, "change", "captured change\n", "change")
	old := git.Hook
	t.Cleanup(func() { git.Hook = old })
	moved := false
	git.Hook = func(dir string, args []string, _ time.Duration, err error) {
		if dir != repo || err != nil {
			t.Fatalf("unexpected Git read: %s %v %v", dir, args, err)
		}
		if args[0] == "rev-parse" && strings.Contains(args[2], "moe/") {
			// Model another process moving both refs after resolution. Subsequent
			// reads must still compare the captured objects.
			gittest.Run(t, repo, "update-ref", "refs/heads/trunk", tip)
			gittest.Run(t, repo, "update-ref", "refs/heads/moe/"+md.ID, base)
			moved = true
		}
		if args[0] == "merge-base" && !reflect.DeepEqual(args[1:], []string{base, tip}) {
			t.Fatalf("merge-base uses live refs: %v", args)
		}
		if args[0] == "diff" && (args[len(args)-3] != base || args[len(args)-2] != tip) {
			t.Fatalf("diff uses live refs: %v", args)
		}
	}
	a, err := sourceDiffArtifact(root, md, io.Discard)
	if err != nil || !moved || !strings.Contains(a.body, "+captured change") {
		t.Fatalf("pinned patch: moved=%v err=%v body=%s", moved, err, a.body)
	}
}

func TestInspectionViewerConfigurationAndLiteralFile(t *testing.T) {
	t.Setenv("MOE_MARKDOWN_PAGER", " \t")
	t.Setenv("MOE_DIFF_VIEWER", "hunk patch")
	t.Setenv("PAGER", "must-not-run")
	t.Setenv("GIT_PAGER", "must-not-run")
	bin := t.TempDir()
	inspectionFile(t, filepath.Join(bin, "less"), "#!/bin/sh\nexit 0\n", 0700)
	t.Setenv("PATH", bin)
	v, ok := selectInspectionViewer(inspectCanvas)
	if !ok || v.command != "" || v.less != filepath.Join(bin, "less") {
		t.Fatalf("canvas default: %+v %v", v, ok)
	}
	d, ok := selectInspectionViewer(inspectDiff)
	if !ok || d.command != "hunk patch" {
		t.Fatalf("diff override: %+v %v", d, ok)
	}
	t.Setenv("PATH", t.TempDir())
	if _, ok := selectInspectionViewer(inspectCanvas); ok {
		t.Fatal("missing less offered")
	}
	if _, ok := selectInspectionViewer(inspectDiff); !ok {
		t.Fatal("custom viewer depends on less")
	}
	// Execute the exact argv through env/sh: a recording wrapper sees one
	// literal final argument, and no shell source contains the artifact path.
	record := filepath.Join(t.TempDir(), "record")
	viewer := filepath.Join(t.TempDir(), "viewer with spaces")
	inspectionFile(t, viewer, "#!/bin/sh\nprintf '%s\\n' \"$#\" \"$1\" \"$2\" \"$PATH\" \"$LESSSECURE\" \"$LESSOPEN\" \"$LESSCLOSE\" \"$LESS\" > \"$RECORD\"\n", 0700)
	t.Setenv("RECORD", record)
	file := filepath.Join(t.TempDir(), `file ' $HOME; $(touch injected).md`)
	v = inspectionViewer{variable: "MOE_MARKDOWN_PAGER", command: "'" + viewer + "' 'flag with spaces'"}
	t.Setenv("LESS", "-R")
	t.Setenv("LESSOPEN", "bad")
	t.Setenv("LESSCLOSE", "bad")
	argv := v.argv(file)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = t.TempDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	want := "2\nflag with spaces\n" + file + "\n" + os.Getenv("PATH") + "\n1\n\n\n\n"
	if string(got) != want {
		t.Fatalf("argv/env: %q want %q", got, want)
	}
}

func TestInspectionExplicitShellFailures(t *testing.T) {
	for _, command := range []string{"'unclosed", "nonexistent-moe-inspection-viewer", "/bin/sh -c 'exit 7'"} {
		t.Run(command, func(t *testing.T) {
			v := inspectionViewer{variable: "MOE_DIFF_VIEWER", command: command}
			argv := v.argv(filepath.Join(t.TempDir(), "source.diff"))
			cmd := exec.Command(argv[0], argv[1:]...)
			if _, err := cmd.CombinedOutput(); err == nil {
				t.Fatal("explicit viewer failure accepted")
			}
		})
	}
}

func enablePromptInspection(t *testing.T) {
	t.Helper()
	old := inspectionTerminal
	inspectionTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { inspectionTerminal = old })
	t.Setenv("MOE_MARKDOWN_PAGER", "configured viewer")
	t.Setenv("MOE_DIFF_VIEWER", "hunk patch")
	bin := t.TempDir()
	inspectionFile(t, filepath.Join(bin, "rex"), "#!/bin/sh\nexit 99\n", 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func TestInspectionPromptSelectionsAndRepeatedViews(t *testing.T) {
	for _, mode := range []string{"normal", "push-review", "push-test", "push-code", "blocked", "skip"} {
		t.Run(mode, func(t *testing.T) {
			enablePromptInspection(t)
			root, repo, md := inspectionFixture(t, false)
			gittest.WriteAndCommit(t, repo, "change", "change\n", "change")
			stage := "code"
			if mode == "normal" {
				stage = "design"
			}
			if mode == "push-review" || mode == "blocked" {
				stage = "review"
			}
			if mode == "push-test" {
				stage = "test"
			}
			body := "body for " + stage
			inspectionFile(t, filepath.Join(root, run.ContentPath(md.Project, md.ID, stage)), body, 0600)
			var artifacts []inspectionArtifact
			old := launchInspection
			launchInspection = func(_ context.Context, _ rexInspectionLocation, a inspectionArtifact, _ inspectionViewer) error {
				artifacts = append(artifacts, a)
				return nil
			}
			t.Cleanup(func() { launchInspection = old })
			input := "v\nv\nf\nn\n"
			if mode == "normal" {
				input = "v\nv\nn\n"
			}
			if mode == "skip" {
				input = "s\nf\nn\n"
			}
			withStdinLine(t, input)
			var out, errOut bytes.Buffer
			dispatched := 0
			next := &Command{Name: "test", Run: func([]string, io.Writer, io.Writer) int { dispatched++; return 0 }}
			var code int
			switch {
			case mode == "blocked":
				code = promptKickback(sdlcGroup(t), nil, root, md, stage, body, &out, &errOut)
			case strings.HasPrefix(mode, "push-"):
				next.Name = "push"
				code = promptPushNextStage(next, nil, nil, root, md, "hint", &out, &errOut)
			default:
				if mode == "normal" {
					next.Name = "code"
				}
				code = promptStageNextStage(next, nil, nil, root, md, "hint", &out, &errOut)
			}
			if code != 0 || dispatched != 0 || errOut.Len() != 0 {
				t.Fatalf("code=%d dispatch=%d err=%s", code, dispatched, &errOut)
			}
			if mode == "skip" {
				if len(artifacts) != 1 || artifacts[0].kind != inspectDiff {
					t.Fatalf("skip: %+v", artifacts)
				}
				return
			}
			want := 3
			if mode == "normal" {
				want = 2
			}
			if len(artifacts) != want {
				t.Fatalf("artifacts=%+v output=%s", artifacts, &out)
			}
			if artifacts[0].body != "# demo/inspect — "+stage+"\n\n"+body || !reflect.DeepEqual(artifacts[0], artifacts[1]) {
				t.Fatalf("wrong snapshot: %+v", artifacts)
			}
			if strings.Count(out.String(), body) != 1 {
				t.Fatalf("canvas reprinted: %s", &out)
			}
			if mode != "normal" && !strings.HasPrefix(artifacts[2].body, "diff --git ") {
				t.Fatal("diff is not pure patch")
			}
		})
	}
}

func TestInspectionLeavesDecisionAndDefaultsIntact(t *testing.T) {
	for _, mode := range []string{"enter", "ship", "blocked", "failure", "empty-diff", "eof", "interrupt"} {
		t.Run(mode, func(t *testing.T) {
			enablePromptInspection(t)
			root, _, md := inspectionFixture(t, false)
			inspectionFile(t, filepath.Join(root, run.ContentPath(md.Project, md.ID, "code")), "canvas", 0600)
			calls, dispatches := 0, 0
			oldLaunch := launchInspection
			launchInspection = func(ctx context.Context, _ rexInspectionLocation, _ inspectionArtifact, _ inspectionViewer) error {
				calls++
				if mode == "failure" {
					return fmt.Errorf("MOE_MARKDOWN_PAGER viewer exited 1")
				}
				if mode == "interrupt" {
					if err := raiseSIGINT(); err != nil {
						return err
					}
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			t.Cleanup(func() { launchInspection = oldLaunch })
			input := "v\n\n"
			if mode == "ship" {
				input = "v\np\n"
			}
			if mode == "empty-diff" {
				input = "f\nn\n"
			}
			if mode == "eof" {
				input = "v"
			}
			withStdinLine(t, input)
			var out, errOut bytes.Buffer
			next := &Command{Name: "test", Run: func(args []string, _, _ io.Writer) int {
				dispatches++
				if mode == "ship" && !reflect.DeepEqual(args, []string{"--pr", "demo/inspect"}) {
					t.Fatalf("ship args %v", args)
				}
				return 0
			}}
			code := 0
			if mode == "ship" {
				next.Name = "push"
				code = promptPushNextStage(next, nil, nil, root, md, "hint", &out, &errOut)
			} else if mode == "blocked" {
				oldKickback := openKickbackSession
				openKickbackSession = func(_ *run.Metadata, stage, _, body string, _ bool, _, _ io.Writer) int {
					dispatches++
					if stage != "code" || body != "canvas" {
						t.Fatalf("kickback %s %s", stage, body)
					}
					return 0
				}
				t.Cleanup(func() { openKickbackSession = oldKickback })
				code = promptKickback(sdlcGroup(t), nil, root, md, "test", "canvas", &out, &errOut)
			} else {
				code = promptStageNextStage(next, nil, nil, root, md, "hint", &out, &errOut)
			}
			wantCalls, wantDispatches := 1, 1
			if mode == "eof" || mode == "interrupt" {
				wantDispatches = 0
			}
			if mode == "empty-diff" {
				wantCalls, wantDispatches = 0, 0
			}
			if code != 0 || calls != wantCalls || dispatches != wantDispatches {
				t.Fatalf("code=%d calls=%d dispatches=%d output=%s err=%s", code, calls, dispatches, &out, &errOut)
			}
			if mode == "failure" && !strings.Contains(errOut.String(), "MOE_MARKDOWN_PAGER") {
				t.Fatalf("missing inspection error: %s", &errOut)
			}
			if mode == "interrupt" && (strings.Count(out.String(), "run now?") != 1 || !strings.Contains(out.String(), "^C")) {
				t.Fatalf("interrupt re-prompted: %s", &out)
			}
		})
	}
}

func TestInspectionMissingCanvasAndEmptyDiffLaunchNothing(t *testing.T) {
	enablePromptInspection(t)
	root, _, md := inspectionFixture(t, false)
	p := newPromptInspection(root, md, "code", " \n", true, io.Discard)
	if _, ok := p.viewers[inspectCanvas]; ok {
		t.Fatal("empty canvas offered")
	}
	if _, ok := p.viewers[inspectDiff]; !ok {
		t.Fatal("existing source omitted")
	}
	md.ID = "absent"
	p = newPromptInspection(root, md, "code", "", true, io.Discard)
	if len(p.viewers) != 0 {
		t.Fatalf("missing artifacts offered: %v", p.viewers)
	}
}

func TestInspectionRealPresenterInterruptUnwindsPrompt(t *testing.T) {
	enablePromptInspection(t)
	r, log := fakeInspectionRex(t, "interrupt")
	t.Setenv("PATH", filepath.Dir(r.bin)+":"+os.Getenv("PATH"))
	root := t.TempDir()
	md := stageMetadata("demo", "inspect", "sdlc")
	inspectionFile(t, filepath.Join(root, run.ContentPath(md.Project, md.ID, "design")), "canvas", 0600)
	withStdinLine(t, "v\n\n")
	dispatched := false
	next := &Command{Name: "code", Run: func([]string, io.Writer, io.Writer) int { dispatched = true; return 0 }}
	var out, errOut bytes.Buffer
	if code := promptStageNextStage(next, nil, nil, root, md, "hint", &out, &errOut); code != 0 || dispatched {
		t.Fatalf("code=%d dispatched=%v", code, dispatched)
	}
	if strings.Count(out.String(), "run now?") != 1 || !strings.Contains(out.String(), "^C") {
		t.Fatalf("interrupt re-prompted: %s", &out)
	}
	closed := 0
	for _, record := range inspectionRexRecords(t, log) {
		if slices.Contains(record.Args, "close") {
			closed++
		}
		if record.File != "" {
			file := record.File
			t.Cleanup(func() { os.RemoveAll(filepath.Dir(file)) })
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("unknown reader input removed: %v", err)
			}
		}
	}
	if closed != 1 || !strings.Contains(errOut.String(), "snapshot retained") {
		t.Fatalf("close count=%d err=%s", closed, &errOut)
	}
}

func TestInspectionUnknownLifetimeDisablesFurtherViewers(t *testing.T) {
	enablePromptInspection(t)
	root := t.TempDir()
	md := stageMetadata("demo", "inspect", "sdlc")
	inspectionFile(t, filepath.Join(root, run.ContentPath(md.Project, md.ID, "design")), "canvas", 0600)
	calls := 0
	old := launchInspection
	launchInspection = func(context.Context, rexInspectionLocation, inspectionArtifact, inspectionViewer) error {
		calls++
		return &inspectionLifetimeUnknown{inspectionBlock, "/retained/snapshot"}
	}
	t.Cleanup(func() { launchInspection = old })
	withStdinLine(t, "v\nv\n")
	next := &Command{Name: "code", Run: func([]string, io.Writer, io.Writer) int { t.Fatal("inspection dispatched stage"); return 1 }}
	var out, errOut bytes.Buffer
	if code := promptStageNextStage(next, nil, nil, root, md, "hint", &out, &errOut); code != 0 || calls != 1 {
		t.Fatalf("code=%d calls=%d", code, calls)
	}
	if strings.Count(out.String(), "view canvas") != 1 || strings.Count(out.String(), "run now?") != 2 {
		t.Fatalf("unknown reader still offers inspection: %s", &out)
	}
}
