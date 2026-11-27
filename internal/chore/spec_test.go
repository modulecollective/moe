package chore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// specRepo is a bureaucracy with one project, committed, and the
// context-use-audit chore as it is registered today.
func specRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	writeChore(t, root, "projects/moe/project.json", `{"id":"moe"}`)
	writeChore(t, root, "projects/moe/chores/audit/chore.json", "{\n  \"cadence\": \"90d\",\n  \"cooldown\": \"90d\"\n}\n")
	writeChore(t, root, "projects/moe/chores/audit/prompt.md", "# audit\n\nlook around\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-q", "-m", "seed")
	return root
}

func head(t *testing.T, root string) string {
	t.Helper()
	h, err := git.HEAD(root)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func status(t *testing.T, root string) string {
	t.Helper()
	out, err := git.Output(root, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// lastCommitFiles is the subject and touched paths of HEAD.
func lastCommitFiles(t *testing.T, root string) (string, []string) {
	t.Helper()
	out, err := git.Output(root, "show", "--name-only", "--format=%s", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(strings.TrimSpace(out))
	// The subject is several words; paths have no spaces.
	var subject []string
	var files []string
	for _, l := range lines {
		if strings.Contains(l, "/") && strings.HasPrefix(l, "projects/") {
			files = append(files, l)
		} else {
			subject = append(subject, l)
		}
	}
	return strings.Join(subject, " "), files
}

func TestCreateCommitsOnlyTheChoreDir(t *testing.T) {
	root := specRepo(t)
	// Something else staged must stay out of the chore's commit.
	writeChore(t, root, "other.txt", "x")
	gittest.Run(t, root, "add", "other.txt")
	before := head(t, root)

	s := Spec{Trigger: "go.mod", Cooldown: "7d", Prompt: "# deps\n"}
	if err := Create(root, "moe", "deps", s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n, _ := git.Output(root, "rev-list", "--count", before+"..HEAD"); strings.TrimSpace(n) != "1" {
		t.Fatalf("commits = %q, want 1", n)
	}
	subject, files := lastCommitFiles(t, root)
	if subject != "Add chore moe/deps" {
		t.Errorf("subject %q", subject)
	}
	if strings.Join(files, " ") != "projects/moe/chores/deps/chore.json projects/moe/chores/deps/prompt.md" {
		t.Errorf("files %v", files)
	}
	if st := status(t, root); st != "A  other.txt\n" {
		t.Errorf("status after create = %q; the staged file should stay staged and nothing else dirty", st)
	}
	b, _ := os.ReadFile(filepath.Join(root, "projects/moe/chores/deps/chore.json"))
	if string(b) != "{\n  \"trigger\": \"go.mod\",\n  \"cooldown\": \"7d\"\n}\n" {
		t.Errorf("chore.json = %q", b)
	}
	defs, err := LoadAll(root)
	if err != nil {
		t.Fatal(err)
	}
	var got *Definition
	for i := range defs {
		if defs[i].Key() == "moe/deps" {
			got = &defs[i]
		}
	}
	if got == nil || got.Cooldown != 7*24*time.Hour || got.Workflow != DefaultWorkflow || got.Prompt != "# deps\n" {
		t.Fatalf("LoadAll got %+v", got)
	}
}

func TestWriterRefusalsWriteNothing(t *testing.T) {
	root := specRepo(t)
	before := head(t, root)
	cases := []struct {
		name string
		do   func() error
		is   error
	}{
		{"create existing", func() error { return Create(root, "moe", "audit", Spec{Cadence: "1d"}) }, ErrExists},
		{"update missing", func() error { return Update(root, "moe", "ghost", Spec{Cadence: "1d"}) }, ErrNotFound},
		{"remove missing", func() error { return Remove(root, "moe", "ghost") }, ErrNotFound},
		{"both families", func() error { return Create(root, "moe", "x", Spec{Cadence: "1d", When: "it rains"}) }, nil},
		{"no family", func() error { return Create(root, "moe", "x", Spec{Cooldown: "1d"}) }, nil},
		{"cadence < cooldown", func() error { return Create(root, "moe", "x", Spec{Cadence: "1d", Cooldown: "2d"}) }, nil},
		{"bad glob", func() error { return Create(root, "moe", "x", Spec{Trigger: "[", Cooldown: "1d"}) }, nil},
		{"bad duration", func() error { return Create(root, "moe", "x", Spec{Cadence: "soon"}) }, nil},
		{"bad name", func() error { return Create(root, "moe", "Bad_Name", Spec{Cadence: "1d"}) }, nil},
		{"unknown project", func() error { return Create(root, "nope", "x", Spec{Cadence: "1d"}) }, nil},
		{"multi-line when", func() error { return Create(root, "moe", "x", Spec{When: "a\nb"}) }, nil},
		{"update invalid", func() error { return Update(root, "moe", "audit", Spec{Cadence: "1d", Cooldown: "2d"}) }, nil},
	}
	for _, c := range cases {
		err := c.do()
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		if c.is != nil && !errors.Is(err, c.is) {
			t.Errorf("%s: err %v, want %v", c.name, err, c.is)
		}
		if c.is == nil {
			if _, ok := errors.AsType[*InvalidError](err); !ok {
				t.Errorf("%s: err %v is not an *InvalidError", c.name, err)
			}
		}
	}
	if head(t, root) != before {
		t.Fatal("a refusal committed")
	}
	if st := status(t, root); st != "" {
		t.Fatalf("a refusal left the tree dirty: %q", st)
	}
}

func TestUpdateRoundTripIsStable(t *testing.T) {
	root := specRepo(t)
	before := head(t, root)
	jsonPath := filepath.Join(root, "projects/moe/chores/audit/chore.json")
	orig, _ := os.ReadFile(jsonPath)

	s, err := LoadSpec(root, "moe", "audit")
	if err != nil {
		t.Fatal(err)
	}
	if err := Update(root, "moe", "audit", s); err != nil {
		t.Fatalf("no-op Update: %v", err)
	}
	if head(t, root) != before {
		t.Fatal("no-op update committed")
	}
	// The default workflow spelled out is the same definition.
	s.Workflow = "sdlc"
	if err := Update(root, "moe", "audit", s); err != nil || head(t, root) != before {
		t.Fatalf("workflow sdlc spelled out: err %v, committed %v", err, head(t, root) != before)
	}
	if b, _ := os.ReadFile(jsonPath); string(b) != string(orig) {
		t.Fatalf("no-op rewrote chore.json: %q", b)
	}

	s.Workflow = ""
	s.Cooldown = "30d"
	if err := Update(root, "moe", "audit", s); err != nil {
		t.Fatalf("Update: %v", err)
	}
	b, _ := os.ReadFile(jsonPath)
	if string(b) != "{\n  \"cadence\": \"90d\",\n  \"cooldown\": \"30d\"\n}\n" {
		t.Fatalf("chore.json = %q; cadence must stay as typed", b)
	}
	if subject, _ := lastCommitFiles(t, root); subject != "Edit chore moe/audit" {
		t.Errorf("subject %q", subject)
	}
}

func TestUpdateEmptyPromptDropsPromptFile(t *testing.T) {
	root := specRepo(t)
	if err := Update(root, "moe", "audit", Spec{Cadence: "90d", Cooldown: "90d", Prompt: "  \n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "projects/moe/chores/audit/prompt.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prompt.md still there: %v", err)
	}
	if st := status(t, root); st != "" {
		t.Fatalf("dirty after update: %q", st)
	}
}

func TestUpdateCheckedRefusesAMovedDefinition(t *testing.T) {
	root := specRepo(t)
	opened, _ := LoadSpec(root, "moe", "audit")
	moved := opened
	moved.Prompt = "someone else's words\n"
	if err := Update(root, "moe", "audit", moved); err != nil {
		t.Fatal(err)
	}
	before := head(t, root)
	mine := opened
	mine.Cooldown = "1d"
	if err := UpdateChecked(root, "moe", "audit", opened, mine); !errors.Is(err, ErrChanged) {
		t.Fatalf("err %v, want ErrChanged", err)
	}
	if head(t, root) != before {
		t.Fatal("refused checked update committed")
	}
	if err := UpdateChecked(root, "moe", "audit", moved, mine); err != nil {
		t.Fatalf("checked update against current: %v", err)
	}
}

// failCommitsUnder installs a pre-commit hook that fails any commit
// staging a path under prefix (lore: induce-commit-failure-via-hookspath).
func failCommitsUnder(t *testing.T, root, prefix string) {
	t.Helper()
	hooks := t.TempDir()
	script := "#!/bin/sh\ngit diff --cached --name-only | grep -q '^" + prefix + "' && { echo induced >&2; exit 1; }\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
}

func TestFailedCommitRestoresTheTree(t *testing.T) {
	root := specRepo(t)
	failCommitsUnder(t, root, "projects/moe/chores/")
	before := head(t, root)
	orig, _ := os.ReadFile(filepath.Join(root, "projects/moe/chores/audit/chore.json"))

	for name, do := range map[string]func() error{
		"create": func() error { return Create(root, "moe", "new-one", Spec{Cadence: "1d", Prompt: "# x\n"}) },
		"update": func() error { return Update(root, "moe", "audit", Spec{Cadence: "90d", Cooldown: "1d"}) },
		"remove": func() error { return Remove(root, "moe", "audit") },
	} {
		err := do()
		if err == nil || !strings.Contains(err.Error(), "nothing recorded") {
			t.Errorf("%s: err %v, want a nothing-recorded failure", name, err)
		}
		if head(t, root) != before {
			t.Fatalf("%s: committed", name)
		}
		if st := status(t, root); st != "" {
			t.Fatalf("%s: tree or index not restored: %q", name, st)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "projects/moe/chores/audit/chore.json")); string(b) != string(orig) {
		t.Fatalf("chore.json not restored: %q", b)
	}
	if _, err := os.Stat(filepath.Join(root, "projects/moe/chores/new-one")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed create left its directory: %v", err)
	}
	if _, err := LoadAll(root); err != nil {
		t.Fatalf("LoadAll after failed writes: %v", err)
	}
}

func TestRemoveDeletesInOneCommit(t *testing.T) {
	root := specRepo(t)
	before := head(t, root)
	if err := Remove(root, "moe", "audit"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if n, _ := git.Output(root, "rev-list", "--count", before+"..HEAD"); strings.TrimSpace(n) != "1" {
		t.Fatalf("commits = %q", n)
	}
	subject, files := lastCommitFiles(t, root)
	if subject != "Remove chore moe/audit" || len(files) != 2 {
		t.Errorf("subject %q files %v", subject, files)
	}
	if _, err := os.Stat(filepath.Join(root, "projects/moe/chores/audit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory still there: %v", err)
	}
	if defs, err := LoadAll(root); err != nil || len(defs) != 0 {
		t.Fatalf("LoadAll = %v, %v", defs, err)
	}
	if st := status(t, root); st != "" {
		t.Fatalf("dirty after remove: %q", st)
	}
}

// An edit makes a cooldown-less mechanical chore due; a no-op save
// does not, because it commits nothing.
func TestEditMakesMechanicalChoreDue(t *testing.T) {
	root := specRepo(t)
	if err := Create(root, "moe", "lint", Spec{Trigger: "*.go"}); err != nil {
		t.Fatal(err)
	}
	// Completed after the create: not due.
	idx := &run.JournalIndex{ChoreByRun: map[string]string{"moe/lint-1": "moe/lint"}, LastActivity: map[string]time.Time{"moe/lint-1": time.Now().Add(time.Minute)}}
	mds := []*run.Metadata{{Project: "moe", ID: "lint-1", Status: run.StatusClosed}}
	load := func() Definition {
		t.Helper()
		defs, err := LoadAll(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			if d.Name == "lint" {
				return d
			}
		}
		t.Fatal("lint missing")
		return Definition{}
	}
	if st := Evaluate(load(), mds, idx, time.Now()); st.Due {
		t.Fatalf("due before any edit: %v", st.Reasons)
	}
	s, _ := LoadSpec(root, "moe", "lint")
	if err := Update(root, "moe", "lint", s); err != nil {
		t.Fatal(err)
	}
	if st := Evaluate(load(), mds, idx, time.Now()); st.Due {
		t.Fatalf("no-op update made it due: %v", st.Reasons)
	}
	// Completion in the past, then a real edit.
	idx.LastActivity["moe/lint-1"] = time.Now().Add(-time.Hour)
	s.Trigger = "*.md"
	if err := Update(root, "moe", "lint", s); err != nil {
		t.Fatal(err)
	}
	st := Evaluate(load(), mds, idx, time.Now())
	if !st.Due || st.ReasonString() != "definition changed" {
		t.Fatalf("after edit: due=%v reasons=%v", st.Due, st.Reasons)
	}
}

func TestSpecTextRoundTrip(t *testing.T) {
	for _, s := range []Spec{
		{},
		{Cadence: "90d", Cooldown: "90d", Prompt: "# audit\n\nbody\n"},
		{Trigger: "docs/*", Workflow: "sdlc", Prompt: "intro\n---\nnot a separator\n"},
		{When: "a change landed: commands, flags — or setup", Prompt: "no trailing newline"},
	} {
		got, err := ParseSpecText(s.Text())
		if err != nil {
			t.Errorf("ParseSpecText(%q): %v", s.Text(), err)
			continue
		}
		if got != s {
			t.Errorf("round trip\n got %+v\nwant %+v", got, s)
		}
	}
	if got := (Spec{Cadence: "90d", Prompt: "# x\n"}).Text(); got != "trigger:\ncadence: 90d\ncooldown:\nwhen:\nworkflow:\n---\n# x\n" {
		t.Errorf("Text() = %q", got)
	}
}

func TestParseSpecTextRefusals(t *testing.T) {
	for text, want := range map[string]string{
		"trigger:\nflavour: mint\n---\n":  "line 2",
		"cadence: 1d\ncadence: 2d\n---\n": "line 2: cadence is set twice",
		"cadence: 1d\n# prompt\n":         "line 2",
		"cadence: 1d":                     "no `---`",
		"":                                "no `---`",
		"just words\n---\n":               "line 1",
	} {
		_, err := ParseSpecText(text)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseSpecText(%q) = %v, want %q", text, err, want)
		}
	}
	// Blank header lines and CRLF are tolerated.
	got, err := ParseSpecText("\r\ncadence: 1d\r\n\n---\r\nbody\r\n")
	if err != nil || got.Cadence != "1d" || got.Prompt != "body\r\n" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// An unchanged save leaves a hand-formatted chore.json alone: rewriting
// it in the writer's layout would commit, and any commit under the
// directory reads as "definition changed".
func TestUnchangedUpdateKeepsHandFormatting(t *testing.T) {
	root := specRepo(t)
	rel := "projects/moe/chores/audit/chore.json"
	writeChore(t, root, rel, `{"cooldown":"90d","cadence":"90d","workflow":"sdlc"}`)
	gittest.Run(t, root, "commit", "-q", "-am", "hand edit")
	before := head(t, root)
	s, err := LoadSpec(root, "moe", "audit")
	if err != nil {
		t.Fatal(err)
	}
	if err := Update(root, "moe", "audit", s); err != nil {
		t.Fatal(err)
	}
	if head(t, root) != before {
		t.Fatal("an unchanged save committed a reformat")
	}
	if b, _ := os.ReadFile(filepath.Join(root, rel)); string(b) != `{"cooldown":"90d","cadence":"90d","workflow":"sdlc"}` {
		t.Fatalf("rewritten: %q", b)
	}
}
