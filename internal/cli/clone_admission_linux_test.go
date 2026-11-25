//go:build linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

func TestCloneStageAdmissionExplainsRegistryRecovery(t *testing.T) {
	for _, kind := range []string{"unenrolled", "malformed", "foreign-view"} {
		t.Run(kind, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "finish", "sdlc", run.StatusInProgress)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")
			path := cleanTestClone(t, root, "tele", "finish")
			marker := filepath.Join(path, ".moe", "clone-identity.json")
			records := filepath.Join(root, ".moe", "clone-reclamation", "tele", "finish")
			switch kind {
			case "unenrolled":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(records); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				writeFile(t, marker, "{")
			case "foreign-view":
				b, err := os.ReadFile(marker)
				if err != nil {
					t.Fatal(err)
				}
				var id clonereclaim.Identity
				if err := json.Unmarshal(b, &id); err != nil {
					t.Fatal(err)
				}
				id.Namespace = "foreign-view"
				b, err = json.Marshal(id)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, marker, string(b))
			}
			const secret = "PRIVATE_CACHE_VALUE=must-stay-private\n"
			cache := filepath.Join(path, devEnvCacheRel)
			writeFile(t, cache, secret)
			local := filepath.Join(path, "local-work.txt")
			writeFile(t, local, "keep this work\n")
			before := map[string][]byte{}
			for _, p := range []string{marker, cache, local, filepath.Join(root, run.Dir("tele", "finish"), "run.json")} {
				b, err := os.ReadFile(p)
				if err != nil && !(p == marker && kind == "unenrolled" && os.IsNotExist(err)) {
					t.Fatal(err)
				}
				before[p] = b
			}
			h, err := runowner.Acquire(root, "tele", "finish")
			if err != nil {
				t.Fatal(err)
			}
			_, cause := clonereclaim.LoadRegistry(h)
			held := clonereclaim.CheckRunRecovery(h)
			h.Release()
			if _, ok := errors.AsType[*clonereclaim.HoldError](held); !ok || cause == nil {
				t.Fatalf("registry refusal lost hold classification: cause=%v held=%v", cause, held)
			}
			started := filepath.Join(t.TempDir(), "started")
			t.Setenv("MOE_TEST_ADMISSION_STARTED", started)
			fakeClaudeOnPath(t, "#!/bin/sh\ntouch \"$MOE_TEST_ADMISSION_STARTED\"\nexit 1\n")
			writeHookScript(t, root, "tele", "dev-env", "10-started.sh", "#!/bin/sh\ntouch \"$MOE_TEST_ADMISSION_STARTED\"\n")
			gittest.Run(t, root, "add", "projects/tele/hooks")
			gittest.Commit(t, root, "setup sentinel")
			head := gittest.HeadSHA(t, root)
			var out, errb bytes.Buffer
			if code := Run([]string{"--no-cooldown", "sdlc", "design", "--once", "tele/finish"}, &out, &errb); code != 1 {
				t.Fatalf("stage exit=%d: %s %s", code, &out, &errb)
			}
			for _, want := range []string{"cannot admit clone for tele/finish", "until host enrollment/recovery", cause.Error(), "moe clone recover tele/finish", "after stopping all old MoE binaries, shells, descendants and unmanaged users", "interactive host terminal", "inspect and validate", "recovery records location: " + records} {
				if !strings.Contains(errb.String(), want) {
					t.Errorf("missing %q in %s", want, &errb)
				}
			}
			for _, unwanted := range []string{"retained evidence", "Legacy clone", secret, "must-stay-private"} {
				if strings.Contains(out.String()+errb.String(), unwanted) {
					t.Errorf("diagnostic contains %q", unwanted)
				}
			}
			if _, err := os.Stat(started); !os.IsNotExist(err) {
				t.Fatalf("setup or agent executed: %v", err)
			}
			if gittest.HeadSHA(t, root) != head {
				t.Fatal("refused stage changed journal")
			}
			for p, want := range before {
				got, err := os.ReadFile(p)
				if kind == "unenrolled" && p == marker {
					if !os.IsNotExist(err) {
						t.Fatal("refusal enrolled legacy clone")
					}
				} else if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("refusal changed %s: %v", p, err)
				}
			}
			if kind == "unenrolled" {
				if _, err := os.Stat(records); !os.IsNotExist(err) {
					t.Fatalf("refusal created recovery records: %v", err)
				}
			}
		})
	}
}

func TestCloneAdmissionAcceptsCreatedRunIDs(t *testing.T) {
	for _, id := range []string{"valid--id", "valid-id-"} {
		for _, withClone := range []bool{false, true} {
			t.Run(id+map[bool]string{false: "/absent", true: "/clone"}[withClone], func(t *testing.T) {
				root := newTestBureaucracy(t)
				markBureaucracy(t, root)
				seedProjectWithSubmodule(t, root, "tele--")
				t.Setenv("MOE_HOME", root)
				var md *run.Metadata
				err := repolock.With(root, repolock.Options{Purpose: "create regression run"}, func() error {
					var err error
					md, err = run.New(root, "tele--", run.Options{ID: id, Workflow: "sdlc"})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				h, err := runowner.Acquire(root, md.Project, md.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := clonereclaim.CheckRunRecovery(h); err != nil {
					h.Release()
					t.Fatal(err)
				}
				var path string
				if withClone {
					path, err = attachRunWorkspace(h, md, "moe/"+md.ID)
					if err == nil {
						_, _, err = setupRunEnvironment(h, path, md, io.Discard, io.Discard)
					}
				}
				h.Release()
				if err != nil {
					t.Fatal(err)
				}
				if p, r, err := splitCloneRef(md.Project + "/" + id); err != nil || p != md.Project || r != id {
					t.Fatalf("exact filter: %q %q %v", p, r, err)
				}
				if withClone {
					report, code := gcReport(t, md.Project+"/"+id)
					if code != 0 || len(report.Candidates) != 1 || report.Candidates[0].Run != id || report.Candidates[0].Reason != "nonterminal" {
						t.Fatalf("inventory and exact filter: %d %+v", code, report)
					}
				}
				if code := Run([]string{"sdlc", "close", md.Project + "/" + id}, io.Discard, io.Discard); code != 0 {
					t.Fatalf("close exit %d", code)
				}
				if got := loadRun(t, root, md.Project, id); got.Status != run.StatusClosed {
					t.Fatalf("close status: %s", got.Status)
				}
				if !withClone {
					if _, err := os.Stat(filepath.Join(root, ".moe", "clones", md.Project, id)); !os.IsNotExist(err) {
						t.Fatalf("unexpected clone: %v", err)
					}
				}
			})
		}
	}
}
