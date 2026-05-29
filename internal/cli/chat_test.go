package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestChatRegistered partners with TestAuditRegistered:
// a registration drift in init() ordering would silently drop the chat
// workflow. Walking the typed CLI to print the group's usage is the
// cheapest integration check that both the CommandGroup and the Workflow
// registry hold the wiring.
func TestChatRegistered(t *testing.T) {
	if _, err := LookupWorkflow(chatWorkflow); err != nil {
		t.Fatal(err)
	}
	g, err := LookupGroup(chatWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if g.Summary == "" {
		t.Fatal("chat group summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := Run([]string{chatWorkflow}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "chat", "close", "cat", "log"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("chat usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestChatWorkflowSingleStage confirms the one-stage, terminal shape:
// chat is the only stage, it has no prereqs, and it has no successor.
// Adding a stage should be a deliberate edit that updates this test.
func TestChatWorkflowSingleStage(t *testing.T) {
	wf, err := LookupWorkflow(chatWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	if len(got) != 1 || got[0] != chatDoc {
		t.Fatalf("stages=%v want [%s]", got, chatDoc)
	}
	if pre := wf.Prereqs(chatDoc); len(pre) != 0 {
		t.Fatalf("chat prereqs=%v want empty", pre)
	}
	if succ := wf.Successor(chatDoc); succ != "" {
		t.Fatalf("chat successor=%q want empty (terminal stage)", succ)
	}
}

// TestBuildSystemPromptInjectsChatFragment is the wiring check:
// workflows/chat/chat.md lands in the prompt when the run names the
// chat workflow at the chat stage. Sentinels on the load-bearing
// framing — the canvas-is-not-yours override and the no-coding rule —
// so a refactor that drops either becomes a failing test.
func TestBuildSystemPromptInjectsChatFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{
		ID:       "ponder-2026-05-28",
		Project:  "moe",
		Workflow: chatWorkflow,
	}
	got, err := buildSystemPrompt(root, md, chatDoc, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: chat") {
		t.Fatalf("prompt missing chat fragment heading:\n%s", got)
	}
	// The canvas-is-moe-owned override is load-bearing: without it the
	// generic operational core tells the agent to write the canvas,
	// which would muddy the session log.
	if !strings.Contains(got, "The canvas is not yours") {
		t.Fatalf("chat.md missing canvas-override framing:\n%s", got)
	}
	if !strings.Contains(got, "No coding") {
		t.Fatalf("chat.md missing no-coding framing:\n%s", got)
	}
}

// TestChatCascadeDispatcherRegistered confirms the cascade driver can
// reach the chat stage via the workflow-agnostic dispatcher registry.
// chat is interactive-only, but the registration keeps the cascade
// machinery uniform — without it, `!` at a chat run's chain prompt would
// print "workflow has no cascade dispatcher".
func TestChatCascadeDispatcherRegistered(t *testing.T) {
	if d := lookupCascadeDispatcher(chatWorkflow); d == nil {
		t.Fatal("chat workflow has no cascade dispatcher registered")
	}
}

// TestMoeHowtoSkillEmbedded pins the //go:embed wiring for the chat
// workflow's moe-howto skill. A silently broken embed directive (typo'd
// path, renamed file) would degrade to an empty skill body and the chat
// agent would lose its grooming guidance. Unlike the templated siblings,
// moe-howto carries no `{{...}}` placeholders — assert a couple of the
// verbs it teaches instead.
func TestMoeHowtoSkillEmbedded(t *testing.T) {
	body := moe.MoeHowtoSkill()
	if body == "" {
		t.Fatal("MoeHowtoSkill() is empty; //go:embed skills/... likely broken")
	}
	for _, want := range []string{
		"name: moe-howto",
		"moe idea new",
		"moe dash",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("embedded moe-howto skill missing %q in body", want)
		}
	}
}

// TestChatCanvasOnOpenSeedsAndAppends pins the moe-owned session-log
// behaviour: the first open writes the header plus Session 1, and a
// second open appends Session 2 without disturbing the first. This is
// what keeps the canvas moving off main every turn so session.Close's
// canvas-unchanged guard passes even though the agent never writes the
// canvas.
func TestChatCanvasOnOpenSeedsAndAppends(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "ponder-2026-05-28", Project: "moe", Workflow: chatWorkflow}
	if _, _, err := run.EnsureDocument(root, md, chatDoc); err != nil {
		t.Fatalf("ensure document: %v", err)
	}

	if err := chatCanvasOnOpen(root, md, "claude"); err != nil {
		t.Fatalf("first open: %v", err)
	}
	first := readChatCanvas(t, root, md)
	if !strings.Contains(first, "# Chat: moe") {
		t.Fatalf("first open missing header:\n%s", first)
	}
	if c := strings.Count(first, "\nSession "); c != 1 {
		t.Fatalf("first open marker count=%d want 1:\n%s", c, first)
	}
	if !strings.Contains(first, "Session 1 — opened ") || !strings.Contains(first, ", agent claude\n") {
		t.Fatalf("first open marker malformed:\n%s", first)
	}

	if err := chatCanvasOnOpen(root, md, "codex"); err != nil {
		t.Fatalf("second open: %v", err)
	}
	second := readChatCanvas(t, root, md)
	if c := strings.Count(second, "\nSession "); c != 2 {
		t.Fatalf("second open marker count=%d want 2:\n%s", c, second)
	}
	if !strings.Contains(second, "Session 2 — opened ") || !strings.Contains(second, ", agent codex\n") {
		t.Fatalf("second open marker malformed:\n%s", second)
	}
	// The first session's marker survives the append verbatim.
	if !strings.Contains(second, "Session 1 — opened ") {
		t.Fatalf("second open clobbered Session 1:\n%s", second)
	}
	// Exactly one header — the append branch must not re-seed.
	if c := strings.Count(second, "# Chat: moe"); c != 1 {
		t.Fatalf("header count=%d want 1 after append:\n%s", c, second)
	}
}

// TestChatGroomingHomeOverridesScratch is the core of the chat-sandbox
// fix. For the chat workflow, chatGroomingHome must repoint MOE_HOME at
// the canonical bureaucracy root even when a project dev-env hook has
// already redirected it to a scratch path (the moe-on-moe silent-scratch
// trap). The repointed root must then ride two channels: the agent's env
// (so in-session `moe idea new` resolves the live backlog) and the
// writable --add-dir set via devEnvWritableDirs (so the write isn't
// refused).
func TestChatGroomingHomeOverridesScratch(t *testing.T) {
	root := t.TempDir()
	devEnv := map[string]string{"MOE_HOME": "/tmp/scratch-bureaucracy"}

	got := chatGroomingHome(chatWorkflow, devEnv, root)

	if got["MOE_HOME"] != root {
		t.Fatalf("MOE_HOME=%q want %q (scratch must be overridden)", got["MOE_HOME"], root)
	}
	// ExtraEnv side: the agent subprocess must see MOE_HOME=root, since
	// ExtraEnv is appended last and last wins over the hook's scratch.
	if !containsStr(mapToEnv(got), "MOE_HOME="+root) {
		t.Fatalf("mapToEnv(devEnv)=%v missing MOE_HOME=%q", mapToEnv(got), root)
	}
	// AddDirs side: devEnvWritableDirs keys off MOE_HOME, so the real
	// bureaucracy must land in the writable scope.
	if dirs := devEnvWritableDirs(got); !containsStr(dirs, root) {
		t.Fatalf("writable dirs %v missing root %q", dirs, root)
	}
}

// TestChatGroomingHomeNilMap covers a chat run on a project with no
// dev-env hooks: devEnvSetupEnv can hand back an empty/absent map, and
// the helper must still mint MOE_HOME=root so every project's chat —
// not just moe-on-moe — grooms the real bureaucracy.
func TestChatGroomingHomeNilMap(t *testing.T) {
	root := t.TempDir()
	got := chatGroomingHome(chatWorkflow, nil, root)
	if got == nil {
		t.Fatal("nil devEnv should be initialised for chat")
	}
	if got["MOE_HOME"] != root {
		t.Fatalf("MOE_HOME=%q want %q", got["MOE_HOME"], root)
	}
}

// TestChatGroomingHomeNonChatUntouched pins the no-op for every other
// workflow. sdlc code/test must keep the project's own MOE_HOME (scratch
// for moe-on-moe) — that redirect is exactly what protects the real
// bureaucracy from the WIP `moe` binary during code/test.
func TestChatGroomingHomeNonChatUntouched(t *testing.T) {
	root := t.TempDir()
	const scratch = "/tmp/scratch-bureaucracy"
	devEnv := map[string]string{"MOE_HOME": scratch}
	got := chatGroomingHome("sdlc", devEnv, root)
	if got["MOE_HOME"] != scratch {
		t.Fatalf("non-chat MOE_HOME=%q want %q (must not be repointed)", got["MOE_HOME"], scratch)
	}
}

func containsStr(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

func readChatCanvas(t *testing.T, root string, md *run.Metadata) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath(md.Project, md.ID, chatDoc)))
	if err != nil {
		t.Fatalf("read chat canvas: %v", err)
	}
	return string(body)
}

// stubStageSession swaps runStageSession for a no-op that records
// whether it ran and which doc it was handed, restoring the original on
// cleanup. Lets the reopen tests assert the flip happened before the
// session opened without spinning a real session worktree.
func stubStageSession(t *testing.T, opened *bool) {
	t.Helper()
	prev := runStageSession
	runStageSession = func(_, _, docID string, _ stageSessionOpts, _, _ io.Writer) int {
		*opened = true
		if docID != chatDoc {
			t.Fatalf("docID=%q want %q", docID, chatDoc)
		}
		return 0
	}
	t.Cleanup(func() { runStageSession = prev })
}

// TestOpenChatReopensClosedRun is the auto-reopen path: re-entering a
// closed chat flips it back to in_progress, announces the revival, and
// falls through to open the session — close is a soft archive, not a
// one-way door, and there is no separate reopen verb.
func TestOpenChatReopensClosedRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedRun(t, root, "moe", "ponder", chatWorkflow, run.StatusClosed)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var opened bool
	stubStageSession(t, &opened)

	var out, errb bytes.Buffer
	if code := openChat("moe", "ponder", "", &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !opened {
		t.Fatal("session was not opened after reopen")
	}
	if !strings.Contains(out.String(), "reopened moe/ponder") {
		t.Fatalf("missing reopen notice: %q", out.String())
	}
	md, err := run.Load(root, "moe", "ponder")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusInProgress {
		t.Fatalf("status=%q want %q", md.Status, run.StatusInProgress)
	}
	// The shared runopen.Reopen lands a chat-flavoured commit so the
	// revival stays greppable per workflow.
	msg := gittest.Output(t, root, "log", "-1", "--format=%s%n%b")
	for _, want := range []string{"Reopen chat moe/ponder", "MoE-Run: ponder", "MoE-Project: moe", "MoE-Workflow: chat"} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message missing %q\n%s", want, msg)
		}
	}
}

// TestOpenChatInProgressResumesWithoutReopen: an already-open run is a
// plain resume — no flip, no reopen notice.
func TestOpenChatInProgressResumesWithoutReopen(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedRun(t, root, "moe", "ponder", chatWorkflow, run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var opened bool
	stubStageSession(t, &opened)

	var out, errb bytes.Buffer
	if code := openChat("moe", "ponder", "", &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !opened {
		t.Fatal("session was not opened")
	}
	if strings.Contains(out.String(), "reopened") {
		t.Fatalf("in-progress resume should not print a reopen notice: %q", out.String())
	}
}

// TestOpenChatRefusesUnexpectedStatus: chat never pushes, so a non-
// closed terminal status shouldn't occur — if it does, refuse loud
// rather than guessing, and do not open the session.
func TestOpenChatRefusesUnexpectedStatus(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedRun(t, root, "moe", "ponder", chatWorkflow, run.StatusPushed)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var opened bool
	stubStageSession(t, &opened)

	var out, errb bytes.Buffer
	if code := openChat("moe", "ponder", "", &out, &errb); code != 1 {
		t.Fatalf("exit=%d want 1; stderr=%q", code, errb.String())
	}
	if opened {
		t.Fatal("session must not open on an unexpected status")
	}
	if !strings.Contains(errb.String(), "unexpected status") {
		t.Fatalf("missing refusal message: %q", errb.String())
	}
}
