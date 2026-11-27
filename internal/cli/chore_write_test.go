package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// fakeChoreEditor installs a $EDITOR that records the buffer it was
// handed and replaces it with replacement. It returns the path the
// handed-in buffer is copied to.
func fakeChoreEditor(t *testing.T, replacement string) string {
	t.Helper()
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	repl := filepath.Join(dir, "replacement")
	if err := os.WriteFile(repl, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "editor")
	body := "#!/bin/sh\ncp \"$1\" '" + seen + "'\ncp '" + repl + "' \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", script)
	return seen
}

// editorMustNotRun makes any editor launch fail the test loudly.
func editorMustNotRun(t *testing.T) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "editor")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", script)
	t.Cleanup(func() {
		if _, err := os.Stat(marker); err == nil {
			t.Error("the editor ran")
		}
	})
}

func readChoreJSON(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "projects/moe/chores", name, "chore.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChoreEditOneFlagChangesOneKey(t *testing.T) {
	root := seedChoreRootWith(t, "7d")
	editorMustNotRun(t)
	var out, errb bytes.Buffer
	if code := runChoreEdit([]string{"moe/readme-refresh", "--cooldown", "30d"}, &out, &errb); code != 0 {
		t.Fatalf("edit = %d: %s", code, errb.String())
	}
	if got := readChoreJSON(t, root, "readme-refresh"); got != "{\n  \"trigger\": \"README.md\",\n  \"cooldown\": \"30d\"\n}\n" {
		t.Fatalf("chore.json = %q", got)
	}
	if subj := gittest.Output(t, root, "log", "-1", "--format=%s"); strings.TrimSpace(subj) != "Edit chore moe/readme-refresh" {
		t.Errorf("subject %q", subj)
	}
	// An empty value clears the field.
	if code := runChoreEdit([]string{"--cooldown", "", "moe/readme-refresh"}, &out, &errb); code != 0 {
		t.Fatalf("clear = %d: %s", code, errb.String())
	}
	if got := readChoreJSON(t, root, "readme-refresh"); got != "{\n  \"trigger\": \"README.md\"\n}\n" {
		t.Fatalf("chore.json after clear = %q", got)
	}
	// Switching family in one call.
	if code := runChoreEdit([]string{"moe/readme-refresh", "--when", "docs drifted", "--trigger", ""}, &out, &errb); code != 0 {
		t.Fatalf("family switch = %d: %s", code, errb.String())
	}
	if got := readChoreJSON(t, root, "readme-refresh"); got != "{\n  \"when\": \"docs drifted\"\n}\n" {
		t.Fatalf("chore.json after switch = %q", got)
	}
}

func TestChoreEditRefusesUnregisteredWorkflow(t *testing.T) {
	root := seedChoreRoot(t)
	editorMustNotRun(t)
	before := gittest.Output(t, root, "rev-parse", "HEAD")
	var out, errb bytes.Buffer
	if code := runChoreEdit([]string{"moe/readme-refresh", "--workflow", "no-such"}, &out, &errb); code != 1 {
		t.Fatalf("edit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "no-such") {
		t.Errorf("stderr %q", errb.String())
	}
	if gittest.Output(t, root, "rev-parse", "HEAD") != before {
		t.Fatal("committed")
	}
}

func TestChoreNewWritesWhatTheEditorReturns(t *testing.T) {
	root := seedChoreRoot(t)
	seen := fakeChoreEditor(t, "trigger:\ncadence: 30d\ncooldown: 30d\nwhen:\nworkflow:\n---\n# prices\n\nrefresh them\n")
	var out, errb bytes.Buffer
	if code := runChoreNew([]string{"moe/prices", "--cadence", "30d"}, &out, &errb); code != 0 {
		t.Fatalf("new = %d: %s", code, errb.String())
	}
	handed, _ := os.ReadFile(seen)
	if string(handed) != "trigger:\ncadence: 30d\ncooldown:\nwhen:\nworkflow:\n---\n# prices\n" {
		t.Errorf("editor was handed %q", handed)
	}
	s, err := chore.LoadSpec(root, "moe", "prices")
	if err != nil {
		t.Fatal(err)
	}
	if s != (chore.Spec{Cadence: "30d", Cooldown: "30d", Prompt: "# prices\n\nrefresh them\n"}) {
		t.Fatalf("spec %+v", s)
	}
	if !strings.Contains(out.String(), "added chore moe/prices") {
		t.Errorf("stdout %q", out.String())
	}
}

func TestChoreNewRefusesBeforeTheEditor(t *testing.T) {
	seedChoreRoot(t)
	editorMustNotRun(t)
	for _, args := range [][]string{
		{"moe/Bad_Name"},
		{"moe/x", "--cadence", "soon"},
		{"moe/x", "--trigger", "["},
		{"moe/x", "--workflow", "no-such"},
		{"moe/readme-refresh"}, // exists
		{"ghost/x"},            // no project
	} {
		var out, errb bytes.Buffer
		if code := runChoreNew(args, &out, &errb); code == 0 {
			t.Errorf("%v: accepted", args)
		}
	}
}

func TestChoreNewKeepsTextOnARefusedSave(t *testing.T) {
	root := seedChoreRoot(t)
	// Both families: parses, then Validate refuses.
	typed := "trigger: *.go\ncadence:\ncooldown:\nwhen: it rains\nworkflow:\n---\nmy careful words\n"
	fakeChoreEditor(t, typed)
	before := gittest.Output(t, root, "rev-parse", "HEAD")
	var out, errb bytes.Buffer
	if code := runChoreNew([]string{"moe/x"}, &out, &errb); code != 1 {
		t.Fatalf("new = %d, want 1", code)
	}
	if gittest.Output(t, root, "rev-parse", "HEAD") != before {
		t.Fatal("committed")
	}
	_, path, ok := strings.Cut(errb.String(), "preserved at ")
	if !ok {
		t.Fatalf("no preserved path in %q", errb.String())
	}
	b, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil || string(b) != typed {
		t.Fatalf("preserved %q, %v", b, err)
	}
	if !strings.Contains(errb.String(), "exclusive") {
		t.Errorf("stderr should say why: %q", errb.String())
	}
}

func TestChoreEditInEditorAndRm(t *testing.T) {
	root := seedChoreRoot(t)
	fakeChoreEditor(t, "trigger: docs/*\ncadence:\ncooldown:\nwhen:\nworkflow:\n---\n# docs\n")
	var out, errb bytes.Buffer
	if code := runChoreEdit([]string{"moe/readme-refresh"}, &out, &errb); code != 0 {
		t.Fatalf("edit = %d: %s", code, errb.String())
	}
	if s, _ := chore.LoadSpec(root, "moe", "readme-refresh"); s != (chore.Spec{Trigger: "docs/*", Prompt: "# docs\n"}) {
		t.Fatalf("spec %+v", s)
	}
	if code := runChoreRm([]string{"moe/readme-refresh"}, &out, &errb); code != 0 {
		t.Fatalf("rm = %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, "projects/moe/chores/readme-refresh")); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
	if code := runChoreRm([]string{"moe/readme-refresh"}, &out, &errb); code != 1 {
		t.Fatalf("second rm = %d, want 1", code)
	}
}

// Removing a chore whose run is still open leaves the run working:
// the gathers that read chores and runs both carry on.
func TestChoreRmWithOpenRun(t *testing.T) {
	root := seedChoreRoot(t)
	if _, err := openChoreInProcess(root, "moe", "readme-refresh", choreOpenForced); err != nil {
		t.Fatal(err)
	}
	openRun := choreOpenRun(t, root, "readme-refresh")
	if openRun == "" {
		t.Fatal("no open run")
	}
	var out, errb bytes.Buffer
	if code := runChoreRm([]string{"moe/readme-refresh"}, &out, &errb); code != 0 {
		t.Fatalf("rm = %d: %s", code, errb.String())
	}
	states, err := gatherChoreStates(root, "")
	if err != nil || len(states) != 0 {
		t.Fatalf("states %v, %v", states, err)
	}
	if _, err := GatherDashSnapshot(root, time.Now(), DashFilter{}); err != nil {
		t.Fatalf("dash gather after rm: %v", err)
	}
}

// An editor save that changes nothing commits nothing and says so.
func TestChoreEditUnchangedSaysSo(t *testing.T) {
	root := seedChoreRoot(t)
	cur, err := chore.LoadSpec(root, "moe", "readme-refresh")
	if err != nil {
		t.Fatal(err)
	}
	fakeChoreEditor(t, cur.Text())
	before := gittest.Output(t, root, "rev-parse", "HEAD")
	var out, errb bytes.Buffer
	if code := runChoreEdit([]string{"moe/readme-refresh"}, &out, &errb); code != 0 {
		t.Fatalf("edit = %d: %s", code, errb.String())
	}
	if gittest.Output(t, root, "rev-parse", "HEAD") != before {
		t.Fatal("an unchanged save committed")
	}
	if !strings.Contains(out.String(), "moe/readme-refresh unchanged") {
		t.Fatalf("stdout %q", out.String())
	}
}

// Saving the prefilled form untouched still registers the chore, and
// says so: the seed is a proposal, not the definition on disk.
func TestChoreNewWithAnUntouchedFormSaysAdded(t *testing.T) {
	root := seedChoreRoot(t)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true")
	var out, errb bytes.Buffer
	if code := runChoreNew([]string{"moe/prices", "--cadence", "30d"}, &out, &errb); code != 0 {
		t.Fatalf("new = %d: %s", code, errb.String())
	}
	if _, err := chore.LoadSpec(root, "moe", "prices"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "added chore moe/prices") {
		t.Fatalf("stdout %q", out.String())
	}
}
