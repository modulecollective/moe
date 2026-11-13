package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// decisionSurface is one of the three chain prompts that read a decision:
// the ordinary next-stage gate, the push gate and the blocked-gate kickback.
type decisionSurface struct {
	name string
	// act is an answer that dispatches when the run is still in progress.
	act string
	// canvas is text from the canvas the prompt prints above its menu.
	canvas string
	run    func(t *testing.T, root string, md *run.Metadata, out, errOut io.Writer) int
}

// decisionSurfaces builds the prompts with every dispatch — next stage,
// back, scuttle, kickback — counted into dispatched and its argv kept in
// args.
func decisionSurfaces(dispatched *int, args *[]string) []decisionSurface {
	cmd := func(name string) *Command {
		return &Command{Name: name, Run: func(a []string, _, _ io.Writer) int {
			*dispatched++
			*args = append([]string{name}, a...)
			return 0
		}}
	}
	return []decisionSurface{
		{name: "next", act: "y", canvas: "build canvas", run: func(_ *testing.T, root string, md *run.Metadata, out, errOut io.Writer) int {
			return promptStageNextStage(cmd("review"), []*Command{cmd("build")}, cmd("close"), root, md, "hint", out, errOut)
		}},
		{name: "push", act: "m", canvas: "review canvas", run: func(_ *testing.T, root string, md *run.Metadata, out, errOut io.Writer) int {
			return promptPushNextStage(cmd("push"), []*Command{cmd("build")}, cmd("close"), root, md, "hint", out, errOut)
		}},
		{name: "blocked", act: "y", canvas: "never resets the counter", run: func(t *testing.T, root string, md *run.Metadata, out, errOut io.Writer) int {
			old := openKickbackSession
			openKickbackSession = func(_ *run.Metadata, document, _, _ string, _ bool, _, _ io.Writer) int {
				*dispatched++
				*args = []string{"kickback", document}
				return 0
			}
			t.Cleanup(func() { openKickbackSession = old })
			return promptKickback(sdlcGroup(t), cmd("close"), root, md, "review", blockedReviewCanvas, out, errOut)
		}},
	}
}

// decisionFixture saves an in-progress sdlc run with code and review
// canvases, so each prompt has a real run to reload and a canvas to print.
func decisionFixture(t *testing.T) (string, *run.Metadata) {
	t.Helper()
	root := t.TempDir()
	md := stageMetadata("demo", "decide", "sdlc")
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"build", "review"} {
		path := filepath.Join(root, run.ContentPath(md.Project, md.ID, stage))
		if stage == "build" {
			path = filepath.Join(root, run.ContentFilePath(md.Project, md.ID, stage, "code.md"))
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(stage+" canvas\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, md
}

// A prompt whose run finished elsewhere while it waited — an older moe, a
// second clone — launches nothing on any answer and says what the run is
// now. An unreadable run.json is treated the same way.
func TestStalePromptLaunchesNothing(t *testing.T) {
	var dispatched int
	var args []string
	states := []struct {
		name, want string
		finish     func(t *testing.T, root string, md *run.Metadata)
	}{
		{"merged", "demo/decide is now merged — nothing launched", func(t *testing.T, root string, md *run.Metadata) {
			finished := *md
			finished.Status = run.StatusMerged
			if err := run.Save(root, &finished); err != nil {
				t.Fatal(err)
			}
		}},
		{"closed", "demo/decide is now closed — nothing launched", func(t *testing.T, root string, md *run.Metadata) {
			finished := *md
			finished.Status = run.StatusClosed
			if err := run.Save(root, &finished); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable", "demo/decide: cannot reload run state", func(t *testing.T, root string, md *run.Metadata) {
			path := filepath.Join(root, run.Dir(md.Project, md.ID), "run.json")
			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, surface := range decisionSurfaces(&dispatched, &args) {
		for _, state := range states {
			for _, answer := range []string{"y", "", "m", "p", "d", "f", "v", "x", "a", "b", "s", "!!"} {
				t.Run(surface.name+"/"+state.name+"/answer="+answer, func(t *testing.T) {
					root, md := decisionFixture(t)
					state.finish(t, root, md)
					dispatched, args = 0, nil
					withStdinLine(t, answer+"\n")
					var out, errOut bytes.Buffer
					code := surface.run(t, root, md, &out, &errOut)
					if code != 0 || dispatched != 0 {
						t.Fatalf("code=%d dispatched=%d (%v) out=%s err=%s", code, dispatched, args, &out, &errOut)
					}
					if !strings.Contains(out.String(), state.want) {
						t.Fatalf("output = %q, want %q", out.String(), state.want)
					}
				})
			}
		}
	}
}

// The positive control: with the run still in progress, the same prompts
// dispatch the selected action and print no stale notice.
func TestFreshPromptDispatchesSelectedAction(t *testing.T) {
	var dispatched int
	var args []string
	for _, surface := range decisionSurfaces(&dispatched, &args) {
		t.Run(surface.name, func(t *testing.T) {
			root, md := decisionFixture(t)
			dispatched, args = 0, nil
			withStdinLine(t, surface.act+"\n")
			var out, errOut bytes.Buffer
			if code := surface.run(t, root, md, &out, &errOut); code != 0 || dispatched != 1 {
				t.Fatalf("code=%d dispatched=%d (%v) out=%s err=%s", code, dispatched, args, &out, &errOut)
			}
			if strings.Contains(out.String(), "nothing launched") {
				t.Fatalf("fresh run reported stale: %q", out.String())
			}
		})
	}
}
