package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/session"
)

func TestSessionListInventory(t *testing.T) {
	root := newSessionTestRoot(t)
	s, err := session.Open(root, "demo", "registered", "design")
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external")
	gittest.Run(t, root, "worktree", "add", "-b", "session/demo/external/design", external)
	stale := filepath.Join(t.TempDir(), "stale")
	gittest.Run(t, root, "worktree", "add", "-b", "session/demo/stale/design", stale)
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	// A missing ref does not erase a surviving worktree registration.
	registrationOnly := filepath.Join(t.TempDir(), "registration-only")
	gittest.Run(t, root, "worktree", "add", "-b", "session/demo/registration-only/design", registrationOnly)
	gittest.Run(t, root, "update-ref", "-d", "refs/heads/session/demo/registration-only/design")
	gittest.Run(t, root, "branch", "session/demo/missing-run/code")
	gittest.Run(t, root, "branch", "session/nonstandard")
	// Full ref names must prevent an ambiguous tag from changing the handle.
	gittest.Run(t, root, "tag", "session/demo/missing-run/code")
	gittest.Run(t, root, "tag", "session/tag-only")
	gittest.Run(t, root, "branch", "unrelated")
	gittest.Run(t, root, "worktree", "add", filepath.Join(t.TempDir(), "unrelated"), "unrelated")
	gittest.Run(t, root, "worktree", "add", "--detach", filepath.Join(t.TempDir(), "detached"))

	refs := gittest.Output(t, root, "for-each-ref", "--format=%(refname) %(objectname)")
	registrations := gittest.Output(t, root, "worktree", "list", "--porcelain")
	var out, errs bytes.Buffer
	withCwd(t, root, func() {
		if code := Run([]string{"session", "list"}, &out, &errs); code != 0 || errs.Len() != 0 {
			t.Fatalf("list exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
		}
	})
	want := "session/demo/external/design\t" + external + "\n" +
		"session/demo/missing-run/code\t(no registered worktree)\n" +
		"session/demo/registered/design\t" + s.WorktreePath + "\n" +
		"session/demo/registration-only/design\t" + registrationOnly + "\n" +
		"session/demo/stale/design\t" + stale + "\n" +
		"session/nonstandard\t(no registered worktree)\n"
	if out.String() != want {
		t.Fatalf("list = %q, want %q", out.String(), want)
	}
	if got := gittest.Output(t, root, "for-each-ref", "--format=%(refname) %(objectname)"); got != refs {
		t.Fatal("listing changed refs")
	}
	if got := gittest.Output(t, root, "worktree", "list", "--porcelain"); got != registrations {
		t.Fatal("listing changed registrations")
	}
	managed, err := session.List(root)
	if err != nil || len(managed) != 1 || managed[0].Branch != s.Branch || managed[0].WorktreePath != s.WorktreePath {
		t.Fatalf("managed List contract changed: %+v, %v", managed, err)
	}
}

func TestSessionListEnumerationFailure(t *testing.T) {
	for _, command := range []string{"for-each-ref", "worktree"} {
		t.Run(command, func(t *testing.T) {
			root := newSessionTestRoot(t)
			if _, err := session.Open(root, "demo", "registered", "design"); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "branch", "session/demo/orphan/design")
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			shimDir := t.TempDir()
			shim := "#!/bin/sh\nif [ \"$1\" = " + command + " ]; then\n echo enumeration-failure >&2\n exit 1\nfi\nexec '" + strings.ReplaceAll(realGit, "'", "'\\''") + "' \"$@\"\n"
			if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			var out, errs bytes.Buffer
			withCwd(t, root, func() {
				if code := Run([]string{"session", "list"}, &out, &errs); code != 1 || out.Len() != 0 || !strings.Contains(errs.String(), "enumeration-failure") || !strings.Contains(errs.String(), command) {
					t.Fatalf("list exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
				}
			})
		})
	}
}

func TestSessionListThenResolveOrphan(t *testing.T) {
	for _, landed := range []bool{true, false} {
		name := "unlanded"
		if landed {
			name = "landed"
		}
		t.Run(name, func(t *testing.T) {
			root := newSessionTestRoot(t)
			s, err := session.Open(root, "demo", "orphan", "design")
			if err != nil {
				t.Fatal(err)
			}
			gittest.WriteAndCommit(t, s.WorktreePath, "kept-work", "retain this commit\n", "session work")
			tip := gittest.Output(t, root, "rev-parse", s.Branch)
			gittest.Run(t, root, "worktree", "remove", "--force", s.WorktreePath)
			if landed {
				gittest.Run(t, root, "merge", "--ff-only", s.Branch)
			}
			var out, errs bytes.Buffer
			withCwd(t, root, func() {
				if code := Run([]string{"session", "list"}, &out, &errs); code != 0 || out.String() != s.Branch+"\t(no registered worktree)\n" {
					t.Fatalf("list exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
				}
				branch, _, _ := strings.Cut(out.String(), "\t")
				out.Reset()
				code := Run([]string{"session", "resolve", branch}, &out, &errs)
				if landed {
					if code != 0 || out.String() != "resolved "+branch+"\n" || errs.Len() != 0 {
						t.Fatalf("resolve exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
					}
				} else {
					if code != 1 || !strings.Contains(errs.String(), "restore a worktree before retrying") {
						t.Fatalf("resolve exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
					}
					if got := gittest.Output(t, root, "rev-parse", branch); got != tip {
						t.Fatal("refused resolve changed tip")
					}
				}
				out.Reset()
				errs.Reset()
				want := "no open sessions\n"
				if !landed {
					want = branch + "\t(no registered worktree)\n"
				}
				if code := Run([]string{"session", "list"}, &out, &errs); code != 0 || out.String() != want {
					t.Fatalf("post-resolve list exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
				}
			})
		})
	}
}
