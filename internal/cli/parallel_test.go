package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// These fixtures use TestMain's immutable Git defaults and explicit roots.
// They never bind MOE_HOME; dispatch and discovery tests keep their serial
// fixtures. Call t.Parallel before using them to catch accidental Setenv.
func parallelBureaucracy(t *testing.T) string {
	t.Helper()
	return gittest.InitSeededUsingEnv(t)
}

func parallelProjectFixture(t *testing.T) string {
	t.Helper()
	root := parallelBureaucracy(t)
	markBureaucracy(t, root)
	seedGitHubProject(t, root, "moe")
	return root
}

func parallelQuietFixture(t *testing.T) string {
	t.Helper()
	root := parallelProjectFixture(t)
	backdateHead(t, root, time.Hour)
	return root
}

// Serial URL-rewrite fixtures must restore the suite defaults before the
// parallel cohort resumes, regardless of test order.
func TestPrivateGitConfigDoesNotChangeSuiteDefaults(t *testing.T) {
	cfg := os.Getenv("GIT_CONFIG_GLOBAL")
	before, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("private rewrite", func(t *testing.T) {
		root := newTestBureaucracy(t)
		addInsteadOfRewrite(t, "https://parallel-fixture.invalid/", root)
		private, err := os.ReadFile(os.Getenv("GIT_CONFIG_GLOBAL"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(private, []byte("https://parallel-fixture.invalid/")) {
			t.Fatal("rewrite was not installed in the private config")
		}
	})
	if os.Getenv("GIT_CONFIG_GLOBAL") != cfg {
		t.Fatal("private fixture did not restore the suite config")
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("private rewrite changed suite config")
	}
}

func TestParallelGitFixturesShareOnlyImmutableDefaults(t *testing.T) {
	t.Parallel()
	cfg := os.Getenv("GIT_CONFIG_GLOBAL")
	before, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := filepath.Join(t.TempDir(), "expected.gitconfig")
	if err := gittest.WriteConfig(wantConfig); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(wantConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, want) {
		t.Fatal("suite config differs from isolated defaults")
	}
	first := parallelBureaucracy(t)
	second := parallelBureaucracy(t)
	gittest.WriteAndCommit(t, first, "first-only", "one\n", "first")
	if _, err := os.Stat(filepath.Join(second, "first-only")); !os.IsNotExist(err) {
		t.Fatalf("repos share contents: %v", err)
	}
	// Secondary repositories opened by production code inherit the same
	// identity and background-work suppression without using our fixture.
	secondary := t.TempDir()
	if err := git.Run(secondary, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(secondary, "commit", "--allow-empty", "-m", "secondary"); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{first, second, secondary} {
		if got := gittest.Output(t, root, "symbolic-ref", "--short", "HEAD"); got != "main" {
			t.Errorf("branch = %q, want main", got)
		}
		if got := gittest.Output(t, root, "log", "-1", "--format=%an <%ae>"); got != "test <test@example.com>" {
			t.Errorf("identity = %q", got)
		}
		for key, want := range map[string]string{
			"commit.gpgsign": "false", "gc.auto": "0", "gc.autoDetach": "false",
			"gc.writeCommitGraph": "false", "maintenance.auto": "false",
			"maintenance.strategy": "none", "fetch.writeCommitGraph": "false",
			"core.fsmonitor": "false", "core.commitGraph": "false",
		} {
			if got := gittest.Output(t, root, "config", "--get", key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || os.Getenv("GIT_CONFIG_GLOBAL") != cfg {
		t.Fatal("parallel fixtures changed shared Git defaults")
	}
}
