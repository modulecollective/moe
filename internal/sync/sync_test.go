package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseGitmodulesIncludesBranch(t *testing.T) {
	dir := t.TempDir()
	content := `[submodule "foo"]
	path = projects/foo/src
	url = https://example.com/foo.git
	branch = trunk
[submodule "bar"]
	path = projects/bar/src
	url = https://example.com/bar.git
`
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ParseGitmodules(filepath.Join(dir, ".gitmodules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(got), got)
	}
	if got[0].Branch != "trunk" {
		t.Fatalf("foo branch: want trunk, got %q", got[0].Branch)
	}
	if got[1].Branch != "" {
		t.Fatalf("bar branch: want empty (so resolver falls back to main), got %q", got[1].Branch)
	}
}

func TestParseGitmodulesMissingFileIsNil(t *testing.T) {
	got, err := ParseGitmodules(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing .gitmodules should return (nil, nil), got err=%v", err)
	}
	if got != nil {
		t.Fatalf("missing .gitmodules should return nil entries, got %v", got)
	}
}

func TestProjectIDForSubmodulePath(t *testing.T) {
	cases := map[string]string{
		"projects/moe/src":     "moe",
		"projects/foo-bar/src": "foo-bar",
		"projects/moe":         "", // not the canonical shape
		"vendor/thing":         "",
		"":                     "",
	}
	for in, want := range cases {
		if got := ProjectIDForSubmodulePath(in); got != want {
			t.Errorf("ProjectIDForSubmodulePath(%q) = %q, want %q", in, got, want)
		}
	}
}
