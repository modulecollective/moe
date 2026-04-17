package managed

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSessionPrimaryFirstThenSubmodulesThenBureaucracy(t *testing.T) {
	s, err := BuildSession(Params{
		AgentID:          "agt_x",
		EnvironmentID:    "env_x",
		ProjectRepo:      "https://github.com/acme/telomere",
		ProjectBranch:    "moe/fix-it",
		ProjectToken:     "p",
		BureaucracyRepo:  "https://github.com/acme/bureaucracy",
		BureaucracySHA:   "deadbeef",
		BureaucracyToken: "b",
		Submodules: []Submodule{
			{Path: "vendor/core", URL: "https://github.com/acme/core", SHA: "c0ffee", Token: "p"},
			{Path: "vendor/util", URL: "https://github.com/acme/util", SHA: "f00d", Token: "p"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Resources) != 4 {
		t.Fatalf("want 4 resources, got %d", len(s.Resources))
	}

	// Order: primary, submodules (in input order), bureaucracy last.
	wantMounts := []string{
		"/workspace/repo",
		"/workspace/repo/vendor/core",
		"/workspace/repo/vendor/util",
		"/workspace/bureaucracy",
	}
	for i, want := range wantMounts {
		if s.Resources[i].MountPath != want {
			t.Errorf("resource[%d] mount_path = %q, want %q", i, s.Resources[i].MountPath, want)
		}
	}

	// Primary uses branch checkout so the agent's pushes land somewhere
	// it can push; submodules use commit checkout so they're pinned;
	// bureaucracy uses commit checkout so it's a snapshot.
	if got := s.Resources[0].Checkout.Type; got != "branch" {
		t.Errorf("primary checkout type = %q, want branch", got)
	}
	if got := s.Resources[0].Checkout.Name; got != "moe/fix-it" {
		t.Errorf("primary branch = %q, want moe/fix-it", got)
	}
	for i := 1; i <= 2; i++ {
		if got := s.Resources[i].Checkout.Type; got != "commit" {
			t.Errorf("submodule[%d] checkout type = %q, want commit", i, got)
		}
	}
	if got := s.Resources[3].Checkout.Type; got != "commit" {
		t.Errorf("bureaucracy checkout type = %q, want commit", got)
	}
	if got := s.Resources[3].Checkout.SHA; got != "deadbeef" {
		t.Errorf("bureaucracy sha = %q, want deadbeef", got)
	}
}

func TestBuildSessionRejectsMissingProject(t *testing.T) {
	if _, err := BuildSession(Params{ProjectBranch: "x"}); err == nil {
		t.Fatal("expected error without ProjectRepo")
	}
	if _, err := BuildSession(Params{ProjectRepo: "x"}); err == nil {
		t.Fatal("expected error without ProjectBranch")
	}
}

func TestBuildSessionOmitsBureaucracyWhenUnset(t *testing.T) {
	s, err := BuildSession(Params{
		ProjectRepo:   "https://github.com/acme/x",
		ProjectBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Resources) != 1 {
		t.Fatalf("want 1 resource, got %d", len(s.Resources))
	}
}

func TestParseGitmodulesMissingFileIsEmpty(t *testing.T) {
	entries, err := parseGitmodules(filepath.Join(t.TempDir(), ".gitmodules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("want 0 entries for missing file, got %d", len(entries))
	}
}

func TestParseGitmodulesReadsPathAndURL(t *testing.T) {
	body := `[submodule "vendor/core"]
	path = vendor/core
	url = https://github.com/acme/core.git
[submodule "vendor/util"]
	path = vendor/util
	url = git@github.com:acme/util.git
	branch = main
`
	dir := t.TempDir()
	p := filepath.Join(dir, ".gitmodules")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := parseGitmodules(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].path != "vendor/core" || entries[0].url != "https://github.com/acme/core.git" {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if entries[1].path != "vendor/util" || entries[1].url != "git@github.com:acme/util.git" {
		t.Errorf("entry[1] = %+v", entries[1])
	}
}

func TestExpandSubmodulesReadsPinnedSHA(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Build a parent repo with one submodule pinned at a known SHA.
	// Use a local path as the submodule source so we don't need network.
	root := t.TempDir()
	sub := filepath.Join(root, "sub-src")
	parent := filepath.Join(root, "parent")

	seed := func(dir string, files map[string]string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	gitIn := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Keep tests hermetic: scoped git config for this subprocess.
		cfg := filepath.Join(t.TempDir(), "gitconfig")
		if err := os.WriteFile(cfg, []byte(
			"[user]\n\temail=t@example.com\n\tname=T\n[init]\n\tdefaultBranch=main\n[protocol \"file\"]\n\tallow=always\n",
		), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+cfg,
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	seed(sub, map[string]string{"hello.txt": "hi"})
	gitIn(sub, "init", "-b", "main")
	gitIn(sub, "add", ".")
	gitIn(sub, "commit", "-m", "seed")
	out, err := exec.Command("git", "-C", sub, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	wantSHA := strings.TrimSpace(string(out))

	seed(parent, map[string]string{"top.txt": "top"})
	gitIn(parent, "init", "-b", "main")
	gitIn(parent, "add", ".")
	gitIn(parent, "commit", "-m", "seed")
	gitIn(parent, "-c", "protocol.file.allow=always", "submodule", "add", sub, "vendor/core")
	gitIn(parent, "commit", "-m", "add submodule")

	got, err := ExpandSubmodules(parent, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 submodule, got %d", len(got))
	}
	if got[0].Path != "vendor/core" {
		t.Errorf("path = %q, want vendor/core", got[0].Path)
	}
	if got[0].SHA != wantSHA {
		t.Errorf("sha = %q, want %q", got[0].SHA, wantSHA)
	}
	if got[0].Token != "tok" {
		t.Errorf("token = %q, want tok", got[0].Token)
	}
}
