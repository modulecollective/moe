package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

// A prompt whose run finished elsewhere while it waited — an older moe, a
// second clone — launches nothing on any answer, inspection included, and
// says what the run is now.
func TestStalePromptLaunchesNothing(t *testing.T) {
	for _, answer := range []string{"y", "", "f", "v", "x", "a", "b", "s", "!!"} {
		t.Run("answer="+answer, func(t *testing.T) {
			enablePromptInspection(t)
			root, repo, md := inspectionFixture(t, false)
			gittest.WriteAndCommit(t, repo, "change", "change\n", "change")
			inspectionFile(t, filepath.Join(root, run.ContentPath(md.Project, md.ID, "code")), "code canvas\n", 0o600)
			finished := *md
			finished.Status = run.StatusMerged
			if err := run.Save(root, &finished); err != nil {
				t.Fatal(err)
			}
			launched := 0
			old := launchInspection
			launchInspection = func(_ context.Context, _ rexInspectionLocation, _ inspectionArtifact, _ inspectionViewer) error {
				launched++
				return nil
			}
			t.Cleanup(func() { launchInspection = old })
			dispatched := 0
			cmd := func(name string) *Command {
				return &Command{Name: name, Run: func([]string, io.Writer, io.Writer) int { dispatched++; return 0 }}
			}
			withStdinLine(t, answer+"\n")
			var out, errOut bytes.Buffer
			code := promptStageNextStage(cmd("test"), []*Command{cmd("code")}, cmd("close"), root, md, "hint", &out, &errOut)
			if code != 0 || dispatched != 0 || launched != 0 {
				t.Fatalf("code=%d dispatched=%d launched=%d out=%s err=%s", code, dispatched, launched, &out, &errOut)
			}
			if !strings.Contains(out.String(), "demo/inspect is now merged — nothing launched") {
				t.Fatalf("output = %q, want the current status", out.String())
			}
		})
	}
}

func TestInspectionWorkspaceErrorNamesPathAndCause(t *testing.T) {
	root := t.TempDir()
	md := stageMetadata("demo", "gone", "sdlc")
	_, err := sourceDiffArtifact(root, md, io.Discard)
	want := sandbox.Path(root, "demo", "gone")
	if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want the path %s and its not-exist cause", err, want)
	}
	if _, statErr := os.Stat(want); !os.IsNotExist(statErr) {
		t.Fatal("inspection created the workspace")
	}
}
