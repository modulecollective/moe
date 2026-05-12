package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/project"
)

// writeProjectMetadata lays a populated project.json at projects/<id>/
// without going through project.Register (which needs a real remote).
// `seedProject` in idea_test.go writes a minimal `{"id":"..."}` shape
// that's enough for the registered-project gate but doesn't carry the
// columns `moe project list` prints — this helper covers that gap.
func writeProjectMetadata(t *testing.T, root string, md *project.Metadata) {
	t.Helper()
	dir := filepath.Join(root, "projects", md.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRegistered(t *testing.T) {
	cmd, ok := commands["project"]
	if !ok {
		t.Fatal(`expected top-level command "project" to be registered`)
	}
	var out, errb bytes.Buffer
	if code := cmd.Run(nil, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"add", "list", "remove"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("project usage missing subcommand %q: %q", want, out.String())
		}
	}
}

func TestProjectListEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"project", "list"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "(no projects registered)") {
		t.Fatalf("expected empty marker, got: %q", out.String())
	}
}

func TestProjectListPrintsRows(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeProjectMetadata(t, root, &project.Metadata{
		ID:            "beta",
		Status:        "incubating",
		Submodule:     "projects/beta/src",
		Remote:        "https://example.com/beta.git",
		DefaultBranch: "main",
	})
	writeProjectMetadata(t, root, &project.Metadata{
		ID:            "alpha",
		Status:        "incubating",
		Submodule:     "projects/alpha/src",
		Remote:        "git@example.com:org/alpha.git",
		DefaultBranch: "trunk",
	})

	var out, errb bytes.Buffer
	code := Run([]string{"project", "list"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	want := "alpha\ttrunk\tgit@example.com:org/alpha.git\n" +
		"beta\tmain\thttps://example.com/beta.git\n"
	if out.String() != want {
		t.Fatalf("output mismatch:\nwant:\n%q\ngot:\n%q", want, out.String())
	}
}

func TestProjectListWarnsAndContinues(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeProjectMetadata(t, root, &project.Metadata{
		ID:            "good",
		Submodule:     "projects/good/src",
		Remote:        "https://example.com/good.git",
		DefaultBranch: "main",
	})
	badDir := filepath.Join(root, "projects", "broken")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "project.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"project", "list"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "good\tmain\thttps://example.com/good.git\n") {
		t.Fatalf("good project missing from stdout: %q", out.String())
	}
	if strings.Contains(out.String(), "broken") {
		t.Fatalf("broken project should not appear on stdout: %q", out.String())
	}
	if !strings.Contains(errb.String(), "project list: skipping broken") {
		t.Fatalf("expected skip warning on stderr, got: %q", errb.String())
	}
}

func TestProjectListRejectsArgs(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"project", "list", "stray-arg"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 on stray arg, got %d (stderr=%q)", code, errb.String())
	}
}
