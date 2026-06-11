package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/workspace"
)

// seedRunJSON writes a minimal run.json directly so run.Scan picks it
// up, without paying for run.New's clean-tree + project-registration
// preconditions. completeWords only reads Project / ID / Workflow /
// Status, so a hand-written metadata file is the cheapest fixture.
func seedRunJSON(t *testing.T, root, project, id, workflow, status string) {
	t.Helper()
	dir := filepath.Join(root, "projects", project, "runs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(run.Metadata{
		ID:        id,
		Project:   project,
		Workflow:  workflow,
		Status:    status,
		Documents: map[string]*run.Document{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCompleteTopLevelVerbs: with the cursor on the first word,
// __complete emits registered top-level verb names, prefix-filtered,
// and never the hidden callback itself.
func TestCompleteTopLevelVerbs(t *testing.T) {
	got := completeWords("", []string{"sd"})
	if len(got) == 0 || got[0] != "sdlc" {
		t.Fatalf("expected sdlc among prefix=sd verbs, got %v", got)
	}
	for _, c := range got {
		if !strings.HasPrefix(c, "sd") {
			t.Fatalf("candidate %q does not match prefix sd: %v", c, got)
		}
	}

	// The hidden __complete callback must never be offered.
	all := completeWords("", []string{""})
	for _, c := range all {
		if c == "__complete" {
			t.Fatal("__complete (hidden) should not appear in verb completion")
		}
	}
	if !slices.Contains(all, "completion") {
		t.Fatalf("visible 'completion' verb missing from %v", all)
	}
}

// TestCompleteGroupSubcommands: word 0 is a group, so __complete walks
// the group's own dispatch table for the second word.
func TestCompleteGroupSubcommands(t *testing.T) {
	got := completeWords("", []string{"sdlc", ""})
	for _, want := range []string{"design", "code", "review", "test", "push"} {
		if !slices.Contains(got, want) {
			t.Fatalf("sdlc subcommands missing %q: %v", want, got)
		}
	}
	// Prefix filter applies to subcommands too.
	d := completeWords("", []string{"sdlc", "de"})
	if !slices.Contains(d, "design") || slices.Contains(d, "code") {
		t.Fatalf("prefix=de should match design only, got %v", d)
	}
}

// TestCompleteProjectRunValues: a leaf annotated argProjectRun draws
// `project/run` candidates from run.Scan, excluding idea-workflow runs,
// prefix-filtered on the partial.
func TestCompleteProjectRunValues(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedRunJSON(t, root, "tele", "fix-bug", "sdlc", run.StatusInProgress)
	seedRunJSON(t, root, "tele", "add-thing", "sdlc", run.StatusMerged)
	seedRunJSON(t, root, "other", "spike", "sdlc", run.StatusInProgress)
	seedRunJSON(t, root, "tele", "an-idea", dash.IdeaWorkflow, run.StatusInProgress)

	got := completeWords(root, []string{"sdlc", "code", ""})
	for _, want := range []string{"tele/fix-bug", "tele/add-thing", "other/spike"} {
		if !slices.Contains(got, want) {
			t.Fatalf("argProjectRun missing %q: %v", want, got)
		}
	}
	// Idea-workflow runs are a different token kind; never offered here.
	if slices.Contains(got, "tele/an-idea") {
		t.Fatalf("idea run leaked into project/run completion: %v", got)
	}

	// Prefix on the project half narrows the set.
	tele := completeWords(root, []string{"sdlc", "code", "tele/"})
	for _, c := range tele {
		if !strings.HasPrefix(c, "tele/") {
			t.Fatalf("prefix tele/ leaked %q: %v", c, tele)
		}
	}
	if slices.Contains(tele, "other/spike") {
		t.Fatalf("prefix tele/ should exclude other/spike: %v", tele)
	}
}

// TestCompleteSecondPositionalIsNotARun: `cat <project>/<run> <stage>`
// completes the run for the first positional only. Once a positional is
// present, the stage slot gets no run candidates (stage completion is
// out of scope for v1).
func TestCompleteSecondPositionalIsNotARun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedRunJSON(t, root, "tele", "fix-bug", "sdlc", run.StatusInProgress)

	got := completeWords(root, []string{"sdlc", "cat", "tele/fix-bug", ""})
	if len(got) != 0 {
		t.Fatalf("second positional should offer nothing, got %v", got)
	}
}

// TestCompleteIdeaValues: argIdea draws open ideas only.
func TestCompleteIdeaValues(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedRunJSON(t, root, "tele", "open-idea", dash.IdeaWorkflow, run.StatusInProgress)
	seedRunJSON(t, root, "tele", "dead-idea", dash.IdeaWorkflow, run.StatusClosed)
	seedRunJSON(t, root, "tele", "a-run", "sdlc", run.StatusInProgress)

	got := completeWords(root, []string{"idea", "edit", ""})
	if !slices.Contains(got, "tele/open-idea") {
		t.Fatalf("open idea missing: %v", got)
	}
	if slices.Contains(got, "tele/dead-idea") {
		t.Fatalf("closed idea should not be offered: %v", got)
	}
	if slices.Contains(got, "tele/a-run") {
		t.Fatalf("sdlc run leaked into idea completion: %v", got)
	}
}

// TestCompleteWorkspaceValues: argWorkspace draws `project/name` from
// the named-workspace set. Uses the real workspace fixture (a claimed
// workspace acquired against a seeded submodule) so the candidate path
// exercises workspace.List end-to-end.
func TestCompleteWorkspaceValues(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	if _, err := workspace.Acquire(root, "tele", "dev", "tele/run-a"); err != nil {
		t.Fatal(err)
	}

	got := completeWords(root, []string{"workspace", "shell", ""})
	if !slices.Contains(got, "tele/dev") {
		t.Fatalf("workspace completion missing tele/dev: %v", got)
	}
}

// TestCompleteFromIdeaFlagValue covers all three token shapes the
// --from-idea value arrives in across shells: space form, single-token
// `=` form, and bash's wordbreak-split `=` form.
func TestCompleteFromIdeaFlagValue(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedRunJSON(t, root, "tele", "open-idea", dash.IdeaWorkflow, run.StatusInProgress)

	// Space form: `... --from-idea <TAB>`.
	space := completeWords(root, []string{"sdlc", "new", "--from-idea", ""})
	if !slices.Contains(space, "tele/open-idea") {
		t.Fatalf("space form missing idea: %v", space)
	}

	// Single token (zsh/fish): `--from-idea=tele/<TAB>` — candidate
	// carries the flag prefix back so the whole token is replaced.
	single := completeWords(root, []string{"sdlc", "new", "--from-idea=tele/"})
	if !slices.Contains(single, "--from-idea=tele/open-idea") {
		t.Fatalf("single-token form missing prefixed idea: %v", single)
	}

	// bash wordbreak: `--from-idea` `=` `<partial>` — bare value.
	bash := completeWords(root, []string{"sdlc", "new", "--from-idea", "=", "tele/"})
	if !slices.Contains(bash, "tele/open-idea") {
		t.Fatalf("bash split form missing idea: %v", bash)
	}
}

// TestCompleteValueFlagSuppressesPositional: a value-taking flag with no
// completion source (--agent) must not let its value slot fall through
// to positional run candidates.
func TestCompleteValueFlagSuppressesPositional(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedRunJSON(t, root, "tele", "fix-bug", "sdlc", run.StatusInProgress)

	got := completeWords(root, []string{"sdlc", "code", "--agent", ""})
	if len(got) != 0 {
		t.Fatalf("--agent value slot should not offer runs, got %v", got)
	}

	// And the run after the flag's value still completes as the first
	// positional (the value was consumed, not counted as positional).
	after := completeWords(root, []string{"sdlc", "code", "--agent", "codex", ""})
	if !slices.Contains(after, "tele/fix-bug") {
		t.Fatalf("first positional after a consumed flag value should offer runs: %v", after)
	}
}

// TestCompleteUnannotatedLeafIsTreeOnly: a leaf with no argKind (idea
// new takes a fresh, user-invented slug) offers no value candidates —
// the safe degrade-to-tree-only behaviour.
func TestCompleteUnannotatedLeafIsTreeOnly(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedRunJSON(t, root, "tele", "open-idea", dash.IdeaWorkflow, run.StatusInProgress)

	got := completeWords(root, []string{"idea", "new", ""})
	if len(got) != 0 {
		t.Fatalf("unannotated leaf should offer no value candidates, got %v", got)
	}
}

// TestCompleteOutsideBureaucracyStillCompletesTree: with root == "" the
// static tree still resolves; value sources just yield nothing. This is
// the never-error-loudly contract: completion degrades, it doesn't fail.
func TestCompleteOutsideBureaucracyStillCompletesTree(t *testing.T) {
	verbs := completeWords("", []string{""})
	if !slices.Contains(verbs, "sdlc") {
		t.Fatalf("tree completion should work outside a bureaucracy: %v", verbs)
	}
	values := completeWords("", []string{"sdlc", "code", ""})
	if len(values) != 0 {
		t.Fatalf("value completion outside a bureaucracy should be empty, got %v", values)
	}
}

// TestRunCompleteAlwaysExitsZero: the callback never errors loudly, even
// on garbage input — a nonzero exit or stderr write corrupts the shell.
func TestRunCompleteAlwaysExitsZero(t *testing.T) {
	cases := [][]string{
		nil,
		{""},
		{"totally-unknown-verb", "x"},
		{"sdlc", "code", "tele/x", "y", "z"},
		{"--", "--"},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := Run(append([]string{"__complete"}, args...), &out, &errb); code != 0 {
			t.Fatalf("__complete %v exited %d", args, code)
		}
		if errb.Len() != 0 {
			t.Fatalf("__complete %v wrote to stderr: %q", args, errb.String())
		}
	}
}

// TestCompletionSnippetPrintsShims: each supported shell prints a
// non-empty script that calls back into `moe __complete`; an unknown
// shell exits nonzero with usage.
func TestCompletionSnippetPrintsShims(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var out, errb bytes.Buffer
		if code := Run([]string{"completion", shell}, &out, &errb); code != 0 {
			t.Fatalf("completion %s exit=%d stderr=%q", shell, code, errb.String())
		}
		if !strings.Contains(out.String(), "moe __complete") {
			t.Fatalf("completion %s missing callback wiring: %q", shell, out.String())
		}
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"completion", "tcsh"}, &out, &errb); code == 0 {
		t.Fatal("expected nonzero exit for unsupported shell")
	}
	if !strings.Contains(errb.String(), "usage: moe completion") {
		t.Fatalf("expected usage on bad shell, got stderr=%q", errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"completion"}, &out, &errb); code == 0 {
		t.Fatal("expected nonzero exit when no shell named")
	}
}
