package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// judgedChoreFixture is a bureaucracy holding one judged chore
// ("readme-update") and one mechanical one ("bump-deps", cadence-only
// and not yet due). The pair is the point: every assertion below is
// about the judged one behaving differently from its mechanical sibling.
func judgedChoreFixture(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")
	writeChoreDef(t, root, "readme-update",
		`{"when":"a landed change altered user-facing behavior that README.md describes","cooldown":"7d"}`,
		"# readme-update\n\nBring the README back in line.\n")
	writeChoreDef(t, root, "bump-deps", `{"cadence":"720h"}`, "# bump-deps\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "register chores")
	// The cadence chore would otherwise be due from its first evaluation
	// (a zero LastCompleted reads as "never done"), which would make the
	// mechanical-sibling assertions ambiguous.
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"chore: skip moe/bump-deps\n\nMoE-Chore-Skipped: moe/bump-deps\n")
	t.Setenv("MOE_HOME", root)
	return root
}

func writeChoreDef(t *testing.T, root, name, choreJSON, prompt string) {
	t.Helper()
	dir := filepath.Join(root, "projects", "moe", "chores", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{"chore.json": choreJSON, "prompt.md": prompt} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestJudgedChoreNominationOpensTheRun: the gate entry the whole family
// exists for. A `chore` spec opens the chore's own run — its workflow,
// its prompt.md seed, its MoE-Chore trailer — even though the chore is
// never mechanically due.
func TestJudgedChoreNominationOpensTheRun(t *testing.T) {
	root := judgedChoreFixture(t)

	var errb bytes.Buffer
	m := &pulseMinter{root: root, projectID: "moe", pulseSlug: "pulse-one"}
	id := m.mint(pulseRunSpec{Chore: "readme-update", Why: "the --dynamic flag landed and the README still lists three rungs"}, io.Discard, &errb)
	if id == "" {
		t.Fatalf("nomination opened nothing; stderr=%s", errb.String())
	}

	md, err := run.Load(root, "moe", id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	if md.Workflow != "sdlc" {
		t.Errorf("workflow=%q, want the chore's own (sdlc)", md.Workflow)
	}
	// The seed is the chore's prompt.md, not a survey-authored design.
	seed, err := os.ReadFile(filepath.Join(root, run.ContentPath("moe", id, "design")))
	if err != nil {
		t.Fatalf("read seeded canvas: %v", err)
	}
	if !strings.Contains(string(seed), "Bring the README back in line") {
		t.Errorf("seed=%q, want the chore's prompt.md", seed)
	}
	// The MoE-Chore trailer is what feeds cooldown and the open-run guard
	// on completion — a nominated run must be an ordinary chore run.
	if body := gittest.Output(t, root, "log", "--format=%B", "-20"); !strings.Contains(body, "MoE-Chore: moe/readme-update") {
		t.Errorf("journal missing the chore trailer:\n%s", body)
	}
	if !strings.Contains(errb.String(), "judged chore met") {
		t.Errorf("stderr=%q, want the sweep to say which chore it opened", errb.String())
	}
}

// TestJudgedChoreNominationMapsOntoOpenRun: nomination-not-create. With
// the chore's run already open the entry resolves to it, so a `chore`
// spec written at a thread position places the existing run instead of
// dropping out of the order.
func TestJudgedChoreNominationMapsOntoOpenRun(t *testing.T) {
	root := judgedChoreFixture(t)

	m := &pulseMinter{root: root, projectID: "moe", pulseSlug: "pulse-one"}
	first := m.mint(pulseRunSpec{Chore: "readme-update", Why: "first"}, io.Discard, os.Stderr)
	if first == "" {
		t.Fatal("first nomination opened nothing")
	}

	var errb bytes.Buffer
	second := m.mint(pulseRunSpec{Chore: "readme-update", Why: "second"}, io.Discard, &errb)
	if second != first {
		t.Errorf("second nomination = %q, want it mapped onto the open run %q", second, first)
	}
	if !strings.Contains(errb.String(), "already open") {
		t.Errorf("stderr=%q, want the mapping named", errb.String())
	}
}

// TestJudgedChoreNominationRefusesMechanicalChore: the principle guard.
// The survey may judge a condition the operator wrote; it may not decide
// that a glob or a clock has fired. A `chore` spec naming a mechanical
// chore is refused even though `--now` would open it.
func TestJudgedChoreNominationRefusesMechanicalChore(t *testing.T) {
	root := judgedChoreFixture(t)

	var errb bytes.Buffer
	m := &pulseMinter{root: root, projectID: "moe", pulseSlug: "pulse-one"}
	if id := m.mint(pulseRunSpec{Chore: "bump-deps", Why: "feels stale"}, io.Discard, &errb); id != "" {
		t.Errorf("nomination opened %q; a mechanical chore is not nominable", id)
	}
	if !strings.Contains(errb.String(), "only judged chores are nominable") {
		t.Errorf("stderr=%q, want the refusal named", errb.String())
	}
}

// TestJudgedChoreNominationRespectsCooldown: the cooldown is the
// anti-flap for an LLM judgment that over-fires, so it survives the
// waiver that the due check does not.
func TestJudgedChoreNominationRespectsCooldown(t *testing.T) {
	root := judgedChoreFixture(t)
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"chore: skip moe/readme-update\n\nMoE-Chore-Skipped: moe/readme-update\n")

	var errb bytes.Buffer
	m := &pulseMinter{root: root, projectID: "moe", pulseSlug: "pulse-one"}
	if id := m.mint(pulseRunSpec{Chore: "readme-update", Why: "landed again"}, io.Discard, &errb); id != "" {
		t.Errorf("nomination opened %q inside the cooldown", id)
	}
	if !strings.Contains(errb.String(), "cooling down") {
		t.Errorf("stderr=%q, want the cooldown refusal", errb.String())
	}
}

// TestJudgedChoreNominationWarnsOnFreshRunFields: a chore entry names a
// registration, so everything describing a fresh run is meaningless on
// it. Warned and ignored, the way a twin entry's title is.
func TestJudgedChoreNominationWarnsOnFreshRunFields(t *testing.T) {
	root := judgedChoreFixture(t)

	var errb bytes.Buffer
	m := &pulseMinter{root: root, projectID: "moe", pulseSlug: "pulse-one"}
	if id := m.mint(pulseRunSpec{
		Chore:    "readme-update",
		Slug:     "readme-refresh",
		Workflow: "twin",
		Title:    "Refresh the README",
		Design:   "# a design the chore never reads\n",
		Why:      "docs lie",
	}, io.Discard, &errb); id == "" {
		t.Fatalf("nomination dropped; stderr=%s", errb.String())
	}
	for _, want := range []string{"ignoring slug", "ignoring workflow", "ignoring title", "ignoring design"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr=%q, want %q warned", errb.String(), want)
		}
	}
}

// TestJudgedChoresBlockListsOnlyOpenableJudgedChores: the kickoff block
// is the read side of the seam. It carries the criterion the agent
// judges — and nothing it cannot act on, since a chore that is cooling
// down or already open is noise in a context window.
func TestJudgedChoresBlockListsOnlyOpenableJudgedChores(t *testing.T) {
	root := judgedChoreFixture(t)

	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatal("newPulseScan failed")
	}
	block := judgedChoresBlock(sc, "moe")
	if !strings.Contains(block, "readme-update") {
		t.Errorf("block=%q, want the judged chore listed", block)
	}
	if !strings.Contains(block, "a landed change altered user-facing behavior") {
		t.Errorf("block=%q, want the when criterion quoted", block)
	}
	if !strings.Contains(block, `"chore"`) {
		t.Errorf("block=%q, want the gate shape named", block)
	}
	if strings.Contains(block, "bump-deps") {
		t.Errorf("block=%q, want mechanical chores left out", block)
	}

	// Once the chore has an open run there is nothing to nominate, and
	// with no other judged chore the block drops entirely — same as its
	// sibling blocks.
	m := &pulseMinter{root: root, projectID: "moe", pulseSlug: "pulse-one"}
	if id := m.mint(pulseRunSpec{Chore: "readme-update", Why: "landed"}, io.Discard, os.Stderr); id == "" {
		t.Fatal("nomination opened nothing")
	}
	sc, ok = newPulseScan(root)
	if !ok {
		t.Fatal("newPulseScan failed after the open")
	}
	if block := judgedChoresBlock(sc, "moe"); block != "" {
		t.Errorf("block=%q, want nothing once the chore has an open run", block)
	}
}

// TestAutoOpenSkipsJudgedChores: the deterministic half of the pulse
// stays judgment-free. Only the survey may conclude a judged chore is
// due, so the chore auto-open must walk straight past one.
func TestAutoOpenSkipsJudgedChores(t *testing.T) {
	root := judgedChoreFixture(t)

	var stdout, stderr bytes.Buffer
	autoOpenDueChores(root, "moe", nil /*pi*/, &stdout, &stderr)

	states, err := gatherChoreStates(root, "moe")
	if err != nil {
		t.Fatalf("gatherChoreStates: %v", err)
	}
	for _, s := range states {
		if s.OpenRun != "" {
			t.Errorf("chore %s opened run %s; auto-open acts on mechanical due-ness only", s.Definition.Key(), s.OpenRun)
		}
	}
}

// TestChoreCheckReportsJudged: a judged chore has no due/not-due answer
// to give. "not due" would read as a schedule that hasn't fired yet,
// which is exactly the misreading the family exists to end — and
// `chore check` is where the operator sees their registrations at all,
// since `chore list` is due-only and never shows them.
func TestChoreCheckReportsJudged(t *testing.T) {
	judgedChoreFixture(t)

	var stdout, stderr bytes.Buffer
	if code := runChoreCheck([]string{"--project", "moe"}, &stdout, &stderr); code != 0 {
		t.Fatalf("chore check = %d, stderr=%s", code, stderr.String())
	}
	var judgedLine string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "moe/readme-update\t") {
			judgedLine = line
		}
	}
	if judgedLine == "" {
		t.Fatalf("chore check did not report the judged chore:\n%s", stdout.String())
	}
	if !strings.Contains(judgedLine, "\tjudged\t") {
		t.Errorf("line=%q, want status judged", judgedLine)
	}

	var listOut, listErr bytes.Buffer
	if code := runChoreList([]string{"--project", "moe"}, &listOut, &listErr); code != 0 {
		t.Fatalf("chore list = %d, stderr=%s", code, listErr.String())
	}
	if strings.Contains(listOut.String(), "readme-update") {
		t.Errorf("chore list showed a judged chore:\n%s", listOut.String())
	}
}
