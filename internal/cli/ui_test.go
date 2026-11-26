package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/ui"
)

func uiItem(t *testing.T, b ui.Board, project, slug string, kind ui.ItemKind) ui.Item {
	t.Helper()
	for _, it := range b.Items {
		if it.Project == project && it.Run == slug && it.Kind == kind {
			return it
		}
	}
	t.Fatalf("no %v item %s/%s in %+v", kind, project, slug, b.Items)
	return ui.Item{}
}

func takeBoard(t *testing.T, s *uiSource) ui.Board {
	t.Helper()
	select {
	case b := <-s.boards:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("no board")
	}
	return ui.Board{}
}

// The board is the dash frame with every completed row, and each item
// sits on the line that names it, carrying what the console may do.
func TestUIBoardIndexesDashRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root, _ := controlFixture(t)
	if _, err := createIdea(root, "moe", "an-idea", "# An idea\n\nBody \x1b[2J text.\n", "", trailers.Block{}); err != nil {
		t.Fatal(err)
	}
	if err := runopen.TagIdea(root, "moe", "an-idea", "sdlc", true); err != nil {
		t.Fatal(err)
	}
	askOn(t, root, "moe", "change-auth", "Which store?")
	s := newUISource(root, DashFilter{}, os.Stdout)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := takeBoard(t, s)
	for _, it := range b.Items {
		if !strings.Contains(b.Lines[it.Line], it.Project+"/"+it.Run) {
			t.Fatalf("%s on line %d: %q", it.Label(), it.Line, b.Lines[it.Line])
		}
	}
	if !slices.IsSortedFunc(b.Items, func(x, y ui.Item) int { return x.Line - y.Line }) {
		t.Fatal("items out of line order")
	}
	r := uiItem(t, b, "moe", "change-auth", ui.ItemRun)
	if !r.Notes || !r.Controls || r.Question == nil || r.Question.Text != "Which store?" || r.Terminal || r.Generation != "" {
		t.Fatalf("run item = %+v", r)
	}
	if d := strings.Join(r.Detail, "\n"); !strings.Contains(d, "owner: none") || !strings.Contains(d, "hold: none") || !strings.Contains(d, "open question #") {
		t.Fatalf("run detail:\n%s", d)
	}
	i := uiItem(t, b, "moe", "an-idea", ui.ItemIdea)
	if !i.Editable || !i.Notes || i.Controls || !strings.Contains(i.Body, "Body \x1b[2J text.") {
		t.Fatalf("idea item = %+v", i)
	}
	if d := strings.Join(i.Detail, "\n"); !strings.Contains(d, "tagged sdlc (design only)") {
		t.Fatalf("idea detail:\n%s", d)
	}
	for _, hint := range []string{"g set sdlc", "d set design only", "u clear either licence", ":tag-sdlc | :tag-design | :untag"} {
		if !strings.Contains(strings.Join(i.Detail, "\n"), hint) {
			t.Fatalf("idea detail missing %q: %v", hint, i.Detail)
		}
	}
}

func TestUIConversationGuidance(t *testing.T) {
	for _, workflow := range []string{"pulse", "sdlc", "chat"} {
		for _, settled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/settled=%v", workflow, settled), func(t *testing.T) {
				t.Setenv("NO_COLOR", "1")
				root, md := controlFixture(t)
				md.Workflow = workflow
				recordedStage, liveStage := workflow, workflow
				if workflow == "sdlc" {
					recordedStage, liveStage = "design", "review"
				}
				if settled {
					md.LastTurn = &run.Turn{Stage: recordedStage, Agent: "claude"}
				}
				if err := run.Save(root, md); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, root, "add", "-A")
				gittest.Commit(t, root, "record conversation")
				head := gittest.HeadSHA(t, root)
				s := newUISource(root, DashFilter{}, os.Stdout)
				ctx, cancel := context.WithCancel(context.Background())
				defer func() { cancel(); s.wg.Wait() }()
				for _, live := range []bool{false, true} {
					stage, backend := recordedStage, "claude"
					if live {
						ownForTest(t, root)
						publishRun(root, md.Project, md.ID, func(st *runsock.Status) {
							st.Phase, st.Stage, st.Agent = runsock.PhaseRunning, liveStage, "codex"
							st.Terminal = &runsock.Terminal{Session: "session", Block: "block"}
						})
						stage, backend = liveStage, "codex"
					}
					if err := s.refresh(ctx); err != nil {
						t.Fatal(err)
					}
					b := takeBoard(t, s)
					it := uiItem(t, b, md.Project, md.ID, ui.ItemRun)
					detail := strings.Join(it.Detail, "\n")
					want := ""
					if workflow != "pulse" && (live || settled) {
						want = fmt.Sprintf("conversation: moe %s %s moe/change-auth --agent %s", workflow, stage, backend)
					}
					if want == "" {
						if strings.Contains(detail, "conversation:") {
							t.Errorf("live=%v: unexpected guidance:\n%s", live, detail)
						}
					} else if !slices.Contains(it.Detail, want) {
						t.Errorf("live=%v: missing %q:\n%s", live, want, detail)
					}
					if !it.Notes || it.Controls != (workflow == "sdlc") || it.Terminal != live {
						t.Errorf("live=%v: actions = %+v", live, it)
					}
					if live {
						if !strings.Contains(detail, "owner: pid ") || !strings.Contains(detail, "phase: agent running (stage "+liveStage+")") {
							t.Errorf("live diagnostics missing:\n%s", detail)
						}
					} else if !strings.Contains(detail, "owner: none") {
						t.Errorf("recorded diagnostics missing:\n%s", detail)
					}
				}
				if gittest.HeadSHA(t, root) != head || strings.TrimSpace(gittest.Output(t, root, "status", "--porcelain")) != "" {
					t.Fatal("UI reads mutated the journal")
				}
			})
		}
	}
}

func TestUICaptureConversationGuidance(t *testing.T) {
	for _, workflow := range []string{"idea", "intent"} {
		t.Run(workflow, func(t *testing.T) {
			root, md := controlFixture(t)
			md.Workflow = workflow
			md.LastTurn = &run.Turn{Stage: workflow, Agent: "codex"}
			if _, _, err := run.EnsureDocument(root, md, workflow); err != nil {
				t.Fatal(err)
			}
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "add", "-A")
			gittest.Commit(t, root, "record capture conversation")
			head := gittest.HeadSHA(t, root)
			s := newUISource(root, DashFilter{}, os.Stdout)
			if err := s.refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			kind := ui.ItemIdea
			if workflow == "intent" {
				kind = ui.ItemIntent
			}
			it := uiItem(t, takeBoard(t, s), md.Project, md.ID, kind)
			detail := strings.Join(it.Detail, "\n")
			if !it.Editable || it.Controls || !strings.Contains(detail, "e edit") || strings.Contains(detail, "conversation:") {
				t.Fatalf("capture actions or guidance changed: %+v", it)
			}
			if gittest.HeadSHA(t, root) != head || strings.TrimSpace(gittest.Output(t, root, "status", "--porcelain")) != "" {
				t.Fatal("capture reads mutated the journal")
			}
		})
	}
}

// A published owner update reaches the console as a pushed board, with
// no gather in between; a gather does not poll the watched endpoint.
func TestUIWatchPushesWithoutGather(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root, md := blockedBoardFixture(t)
	ownForTest(t, root)
	s := newUISource(root, DashFilter{}, os.Stdout)
	var gathers atomic.Int32
	inner := s.gather
	s.gather = func(now time.Time, cached liveLookup) (DashSnapshot, error) {
		gathers.Add(1)
		return inner(now, cached)
	}
	prev := uiDiscovery
	uiDiscovery = time.Hour // nothing but the watch may repaint
	t.Cleanup(func() { uiDiscovery = prev })
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	head := gittest.HeadSHA(t, root)
	gen := ownerGeneration(root, "moe", "change-auth")
	waitBoard := func(want func(ui.Item) bool) ui.Item {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case b := <-s.boards:
				if it := uiItem(t, b, "moe", "change-auth", ui.ItemRun); want(it) {
					return it
				}
			case <-deadline:
				t.Fatal("no matching board")
			}
		}
	}
	it := waitBoard(func(it ui.Item) bool { return it.Generation == gen })
	if !strings.Contains(strings.Join(it.Detail, "\n"), "live: watching") {
		t.Fatalf("detail = %v", it.Detail)
	}
	publishPhase(root, md, runsock.PhaseRunning, "code", "")
	it = waitBoard(func(it ui.Item) bool {
		return strings.Contains(strings.Join(it.Detail, "\n"), "phase: agent running (stage code)")
	})
	if n := gathers.Load(); n != 1 {
		t.Fatalf("%d gathers; the watch should have repainted alone", n)
	}

	publishPhase(root, md, runsock.PhaseDeciding, "design", "next stage")
	it = waitBoard(func(it ui.Item) bool {
		return strings.Contains(strings.Join(it.Detail, "\n"), "last authored stage result: design blocked")
	})
	s.mu.Lock()
	rows, _ := s.overlay(time.Now())
	s.mu.Unlock()
	if !rowFor(t, DashSnapshot{Rows: rows}, "moe/change-auth").StageBlocked {
		t.Fatal("watch decision did not restore badge")
	}
	publishPhase(root, md, runsock.PhaseRunning, "design", "")
	waitBoard(func(it ui.Item) bool {
		return strings.Contains(strings.Join(it.Detail, "\n"), "phase: agent running (stage design)") &&
			!strings.Contains(strings.Join(it.Detail, "\n"), "last authored stage result:")
	})
	s.mu.Lock()
	rows, _ = s.overlay(time.Now())
	s.mu.Unlock()
	if rowFor(t, DashSnapshot{Rows: rows}, "moe/change-auth").StageBlocked {
		t.Fatal("watch retry did not suppress badge")
	}
	if gathers.Load() != 1 || gittest.HeadSHA(t, root) != head {
		t.Fatal("watch projection required a gather or journal commit")
	}

	// A gather while the watch is healthy reads the cached snapshot
	// instead of polling.
	var probed atomic.Int32
	snap, err := gatherDashSnapshot(root, time.Now(), DashFilter{}, func(key string) (runsock.Status, bool) {
		probed.Add(1)
		return s.cachedLive(key)
	})
	if err != nil || probed.Load() == 0 || snap.Observations["moe/change-auth"].Live == nil {
		t.Fatalf("cached gather: probed=%d err=%v", probed.Load(), err)
	}
	writeReadyDesign(t, root, md)
	gittest.Run(t, root, "add", "-A")
	trailerstest.CommitWorkTurnAt(t, root, "moe", md.ID, "sdlc", "design", time.Now().Add(time.Second))
	if err := s.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	o := s.snap.Observations["moe/change-auth"]
	s.mu.Unlock()
	if len(o.Progress.GateProblems) != 0 {
		t.Fatal("durable refresh retained old gate")
	}

}

// An owner closing its endpoint — the end of every stage session — is a
// state, not a failure: no Console issue, the row reads as owned with
// no live status, and the detail keeps the last-known phase as stale. A
// silent owner is still an issue.
func TestUIOwnerEndpointClosedIsNotAnIssue(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root, md := blockedBoardFixture(t)
	ownForTest(t, root)
	s := newUISource(root, DashFilter{}, os.Stdout)
	prev := uiDiscovery
	uiDiscovery = time.Hour // no gather may settle ownership in between
	t.Cleanup(func() { uiDiscovery = prev })
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitBoard := func(want func(ui.Board, ui.Item) bool) (ui.Board, ui.Item) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case b := <-s.boards:
				if it := uiItem(t, b, "moe", "change-auth", ui.ItemRun); want(b, it) {
					return b, it
				}
			case <-deadline:
				t.Fatal("no matching board")
			}
		}
	}
	publishPhase(root, md, runsock.PhaseRunning, "code", "")
	waitBoard(func(_ ui.Board, it ui.Item) bool {
		return strings.Contains(strings.Join(it.Detail, "\n"), "phase: agent running (stage code)")
	})

	// Close the endpoint and keep the guard, as a stage does between its
	// session ending and the guard's release.
	invocationRunsMu.Lock()
	srv := invocationEndpoints[invocationRunKey(root, "moe", "change-auth")]
	invocationRunsMu.Unlock()
	srv.Close()
	b, it := waitBoard(func(_ ui.Board, it ui.Item) bool {
		return strings.Contains(strings.Join(it.Detail, "\n"), "owner endpoint closed")
	})
	for _, issue := range b.Issues {
		if strings.Contains(issue, "change-auth") {
			t.Fatalf("closed endpoint raised an issue: %q", issue)
		}
	}
	if line := b.Lines[it.Line]; !strings.Contains(line, "owned · no live status") || !strings.Contains(line, "[blocked]") {
		t.Fatalf("row = %q", line)
	}
	d := strings.Join(it.Detail, "\n")
	if !strings.Contains(d, "last authored stage result: design blocked") || !strings.Contains(d, "last known below is stale") || !strings.Contains(d, "phase: agent running (stage code)") || strings.Contains(d, "EOF") {
		t.Fatalf("detail:\n%s", d)
	}

	// Negative control: a silent owner is still an observation failure.
	s.mu.Lock()
	token := s.live["moe/change-auth"].token
	s.mu.Unlock()
	s.heard("moe/change-auth", token, nil, "watching", fmt.Errorf("%w for moe/change-auth in 30s", runsock.ErrContactLost))
	b, _ = waitBoard(func(b ui.Board, _ ui.Item) bool { return len(b.Issues) > 0 })
	if want := "moe/change-auth: live observation failed: contact lost: "; len(b.Issues) != 1 || !strings.HasPrefix(b.Issues[0], want) {
		t.Fatalf("issues = %q, want one starting %q", b.Issues, want)
	}
}

// An owned run serving no endpoint at all — the gather's probe and the
// subscription's dial both find nothing — raises no live issue either.
func TestUIOwnedWithoutEndpointIsNotAnIssue(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root, _ := controlFixture(t)
	rel, err := ownRun(root, "moe", "change-auth", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rel() })
	s := newUISource(root, DashFilter{}, os.Stdout)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.wg.Wait() })
	if err := s.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(b ui.Board) {
		t.Helper()
		if len(b.Issues) != 0 {
			t.Fatalf("issues = %q", b.Issues)
		}
		it := uiItem(t, b, "moe", "change-auth", ui.ItemRun)
		if line := b.Lines[it.Line]; !strings.Contains(line, "owned · no live status") {
			t.Fatalf("row = %q", line)
		}
	}
	check(takeBoard(t, s))
	// The subscription's dial fails too; its stale error must read the same.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		l := s.live["moe/change-auth"]
		stale := l != nil && l.stale
		s.mu.Unlock()
		if stale {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subscription never failed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.publish()
	check(takeBoard(t, s))
}

// Notes and answers go through the input writers; a stale answer is a
// definite refusal, never an unknown outcome.
func TestUIDoInput(t *testing.T) {
	root, _ := controlFixture(t)
	askOn(t, root, "moe", "change-auth", "Which store?")
	f, _ := input.Load(root, "moe", "change-auth")
	ping, _ := f.OpenPing()

	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActNote, Project: "moe", Run: "change-auth", Text: "use sqlite"}); !r.OK {
		t.Fatalf("note = %+v", r)
	}
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActAnswer, Project: "moe", Run: "change-auth", QuestionID: ping.ID + 7, Text: "x"}); r.OK || r.Uncertain {
		t.Fatalf("stale answer = %+v", r)
	}
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActAnswer, Project: "moe", Run: "change-auth", QuestionID: ping.ID, Text: "postgres"}); !r.OK {
		t.Fatalf("answer = %+v", r)
	}
	f, _ = input.Load(root, "moe", "change-auth")
	if len(f.Pending()) != 2 {
		t.Fatalf("pending = %+v", f.Pending())
	}
}

// Saved input survives refresh, delivery and completion for both run
// and idea rows. Reading it must not consume the input or mint a commit.
func TestUIInputHistoryReadback(t *testing.T) {
	for _, workflow := range []string{"sdlc", "idea"} {
		t.Run(workflow, func(t *testing.T) {
			t.Setenv("NO_COLOR", "1")
			root, _ := controlFixture(t)
			slug, kind := "change-auth", ui.ItemRun
			if workflow == "idea" {
				slug, kind = "an-idea", ui.ItemIdea
				if _, err := createIdea(root, "moe", slug, "# Idea\n\nKeep this canvas.\n", "", trailers.Block{}); err != nil {
					t.Fatal(err)
				}
			}
			s := newUISource(root, DashFilter{}, os.Stdout)
			refresh := func(source *uiSource) ui.Item {
				t.Helper()
				if err := source.refresh(context.Background()); err != nil {
					t.Fatal(err)
				}
				return uiItem(t, takeBoard(t, source), "moe", slug, kind)
			}
			refresh(s) // start the source before any note is saved
			note := "Keep the names.\n\n世界 \x1b[2J stays visible.\nLast note line."
			if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActNote, Project: "moe", Run: slug, Text: note}); !r.OK {
				t.Fatalf("save note: %+v", r)
			}
			if err := input.MarkDelivered(root, "moe", slug, "design", []int{1}, ""); err != nil {
				t.Fatal(err)
			}
			askOn(t, root, "moe", slug, "Which store?\nKeep exports?")
			if _, err := input.Answer(root, "moe", slug, 2, "SQLite.\n\nKeep the export format."); err != nil {
				t.Fatal(err)
			}
			addOn(t, root, "moe", slug, "Pending note.\nFinal pending line.")
			askOn(t, root, "moe", slug, "Keep the import?\nOpen question last line.")
			want := []string{
				"2 pending notes", "",
				"note #1 · delivered to design", "Keep the names.", "", "世界 \x1b[2J stays visible.", "Last note line.", "",
				"question #2 · pending", "question:", "Which store?", "Keep exports?", "answer:", "SQLite.", "", "Keep the export format.", "",
				"note #3 · pending", "Pending note.", "Final pending line.", "",
				"open question #4", "Keep the import?", "Open question last line.",
			}
			check := func(it ui.Item, want []string) {
				t.Helper()
				start := slices.Index(it.Detail, want[0])
				if start < 0 || len(it.Detail) < start+len(want) || !slices.Equal(it.Detail[start:start+len(want)], want) {
					t.Fatalf("saved input detail = %q; want contiguous lines %q", it.Detail, want)
				}
				if strings.Count(strings.Join(it.Detail, "\n"), "Open question last line.") != 1 {
					t.Fatalf("open question repeated: %q", it.Detail)
				}
				if kind == ui.ItemIdea && it.Body != "# Idea\n\nKeep this canvas.\n" {
					t.Fatalf("capture body = %q", it.Body)
				}
			}
			before, err := os.ReadFile(filepath.Join(root, input.Path("moe", slug)))
			if err != nil {
				t.Fatal(err)
			}
			head := gittest.HeadSHA(t, root)
			for _, source := range []*uiSource{s, newUISource(root, DashFilter{}, os.Stdout)} {
				it := refresh(source)
				check(it, want)
				if kind == ui.ItemRun && (it.Question == nil || it.Question.ID != 4 || it.Question.Text != "Keep the import?\nOpen question last line.") {
					t.Fatalf("live question action = %+v", it.Question)
				}
			}
			after, err := os.ReadFile(filepath.Join(root, input.Path("moe", slug)))
			if err != nil || string(after) != string(before) || gittest.HeadSHA(t, root) != head {
				t.Fatalf("reading changed the input record or journal: %v", err)
			}
			if err := input.MarkDelivered(root, "moe", slug, "code", []int{2, 3}, ""); err != nil {
				t.Fatal(err)
			}
			want[0] = "0 pending notes"
			want[slices.Index(want, "question #2 · pending")] = "question #2 · delivered to code"
			want[slices.Index(want, "note #3 · pending")] = "note #3 · delivered to code"
			check(refresh(s), want)
			addOn(t, root, "moe", slug, "Still pending at completion.")
			want[0] = "1 pending note"
			want = append(want, "", "note #5 · pending", "Still pending at completion.")
			if kind == ui.ItemIdea {
				if err := runopen.CloseCapture(root, "moe", slug); err != nil {
					t.Fatal(err)
				}
			} else {
				markRunStatus(t, root, "moe", slug, run.StatusClosed)
			}
			for _, source := range []*uiSource{s, newUISource(root, DashFilter{}, os.Stdout)} {
				it := refresh(source)
				check(it, want)
				if it.Notes || it.Controls || it.Editable || it.Question != nil {
					t.Fatalf("completed item exposes live actions: %+v", it)
				}
			}
		})
	}
}

func TestUIInputRecordDiagnostics(t *testing.T) {
	for _, status := range []string{run.StatusInProgress, run.StatusClosed} {
		t.Run(status, func(t *testing.T) {
			root, _ := controlFixture(t)
			markRunStatus(t, root, "moe", "change-auth", status)
			s := newUISource(root, DashFilter{}, os.Stdout)
			for _, body := range []string{"", `{"notes":[]}`, `{"notes":broken}`} {
				if body != "" {
					if err := os.WriteFile(filepath.Join(root, input.Path("moe", "change-auth")), []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.refresh(context.Background()); err != nil {
					t.Fatal(err)
				}
				b := takeBoard(t, s)
				it := uiItem(t, b, "moe", "change-auth", ui.ItemRun)
				detail := strings.Join(it.Detail, "\n")
				malformed := strings.Contains(body, "broken")
				if strings.Contains(detail, "input record unreadable:") != malformed || strings.Contains(strings.Join(b.Issues, "\n"), "input record unreadable:") != malformed {
					t.Fatalf("record %q: detail %q, issues %q", body, detail, b.Issues)
				}
				if !malformed && strings.Contains(detail, "pending note") {
					t.Fatalf("empty record has input section: %q", detail)
				}
			}
		})
	}
}

func TestUIDoInputCommitFailureIsDefinite(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind ui.ActionKind
	}{{"note", ui.ActNote}, {"answer", ui.ActAnswer}} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := controlFixture(t)
			askOn(t, root, "moe", "change-auth", "Which store?")
			rel := input.Path("moe", "change-auth")
			abs := filepath.Join(root, rel)
			before, err := os.ReadFile(abs)
			if err != nil {
				t.Fatal(err)
			}
			head := gittest.HeadSHA(t, root)
			index := gittest.Output(t, root, "ls-files", "-s", "--", rel)
			hooks := t.TempDir()
			if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte("#!/bin/sh\necho ui-input-test-rejected >&2\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "config", "core.hooksPath", hooks)
			t.Cleanup(func() { gittest.Run(t, root, "config", "--unset", "core.hooksPath") })
			r := uiDo(context.Background(), root, ui.Action{Kind: tc.kind, Project: "moe", Run: "change-auth", QuestionID: 1, Text: "sqlite"})
			if r.OK || r.Uncertain || !strings.Contains(r.Message, "input: commit failed, nothing recorded") || !strings.Contains(r.Message, "ui-input-test-rejected") {
				t.Fatalf("failed input = %+v", r)
			}
			if len(r.Lines) != 0 {
				t.Fatalf("definite failure included uncertain readback: %+v", r)
			}
			after, err := os.ReadFile(abs)
			if err != nil || string(after) != string(before) {
				t.Fatalf("record after failure = %q, %v; want %q", after, err, before)
			}
			if gittest.HeadSHA(t, root) != head || gittest.Output(t, root, "ls-files", "-s", "--", rel) != index {
				t.Fatal("failed input changed journal or index")
			}
		})
	}
}

// Controls use the CLI's writers: a pause holds, resume releases only
// the hold it was shown, and nothing launches.
func TestUIDoControls(t *testing.T) {
	root, _ := controlFixture(t)
	r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "pause", RequestID: "p1"})
	if !r.OK || r.RequestID != "p1" || controlRecord(t, root).Hold == nil {
		t.Fatalf("pause = %+v", r)
	}
	// The same id reads back rather than asking twice.
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "pause", RequestID: "p1"}); !r.OK {
		t.Fatalf("retry = %+v", r)
	}
	if n := len(controlRecord(t, root).Requests); n != 1 {
		t.Fatalf("retry recorded %d requests", n)
	}
	shown := controlRecord(t, root).Revision
	stale := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "resume", HoldRevision: shown + 1})
	if stale.OK || stale.Uncertain || controlRecord(t, root).Hold == nil {
		t.Fatalf("stale resume = %+v", stale)
	}
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "resume", HoldRevision: shown}); !r.OK || controlRecord(t, root).Hold != nil {
		t.Fatalf("resume = %+v", r)
	}
	// A park needs the owner it addresses; with none it is refused.
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "park"}); r.OK || r.Uncertain {
		t.Fatalf("ownerless park = %+v", r)
	}
}

// Captures: created untagged, edited only over what was shown, tagged
// and untagged through the shared writer, closed with history kept.
func TestUIDoCaptures(t *testing.T) {
	root, _ := controlFixture(t)
	do := func(a ui.Action) ui.Result { return uiDo(context.Background(), root, a) }
	if r := do(ui.Action{Kind: ui.ActCreate, Project: "moe", Run: "new-thing", Workflow: "idea", Text: "# new-thing\n\nfirst\n"}); !r.OK {
		t.Fatalf("create = %+v", r)
	}
	md, err := run.Load(root, "moe", "new-thing")
	if err != nil || md.PromoteTo != "" || md.Workflow != dash.IdeaWorkflow {
		t.Fatalf("created idea = %+v, %v", md, err)
	}
	if r := do(ui.Action{Kind: ui.ActCreate, Project: "moe", Run: "new-thing", Workflow: "idea", Text: "# again\n"}); r.OK || r.Uncertain {
		t.Fatalf("duplicate create = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActEdit, Project: "moe", Run: "new-thing", Expected: "# new-thing\n\nfirst\n", Text: "# new-thing\n\nsecond\n"}); !r.OK {
		t.Fatalf("edit = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActEdit, Project: "moe", Run: "new-thing", Expected: "# new-thing\n\nfirst\n", Text: "# lost\n"}); !r.Changed || r.OK {
		t.Fatalf("stale edit = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActEdit, Project: "moe", Run: "new-thing", Expected: "# new-thing\n\nsecond\n", Text: "# new-thing\n\nsecond\n"}); !r.OK || !strings.Contains(r.Message, "unchanged") {
		t.Fatalf("same edit = %+v", r)
	}
	for _, tc := range []struct {
		wf     string
		design bool
	}{{"sdlc", true}, {"sdlc", false}, {"", false}} {
		if r := do(ui.Action{Kind: ui.ActTag, Project: "moe", Run: "new-thing", Workflow: tc.wf, DesignOnly: tc.design}); !r.OK {
			t.Fatalf("tag %+v = %+v", tc, r)
		}
		md, _ := run.Load(root, "moe", "new-thing")
		if md.PromoteTo != tc.wf || md.DesignOnly != tc.design || md.Status != run.StatusInProgress {
			t.Fatalf("after tag %+v: %+v", tc, md)
		}
	}
	if r := do(ui.Action{Kind: ui.ActTag, Project: "moe", Run: "new-thing", Workflow: "nope"}); r.OK {
		t.Fatalf("bad tag = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActCreate, Project: "moe", Run: "a-goal", Workflow: "intent", Text: "# a-goal\n\nMy words.\n"}); !r.OK {
		t.Fatalf("intent = %+v", r)
	}
	if b, _ := os.ReadFile(root + "/" + run.ContentPath("moe", "a-goal", dash.IntentDocID)); string(b) != "# a-goal\n\nMy words.\n" {
		t.Fatalf("intent text = %q", b)
	}
	if r := do(ui.Action{Kind: ui.ActClose, Project: "moe", Run: "new-thing"}); !r.OK {
		t.Fatalf("close = %+v", r)
	}
	if md, _ := run.Load(root, "moe", "new-thing"); md.Status != run.StatusClosed {
		t.Fatalf("closed = %s", md.Status)
	}
	if r := do(ui.Action{Kind: ui.ActTag, Project: "moe", Run: "new-thing", Workflow: "sdlc"}); r.OK || r.Uncertain {
		t.Fatalf("tag closed = %+v", r)
	}
}

// fakeRex answers rex api calls from a table and records them.
type fakeRex struct {
	calls   []string
	answers map[string]string
	errs    map[string]error
}

func (f *fakeRex) install(t *testing.T) {
	t.Helper()
	prev := rexCall
	rexCall = func(_ context.Context, method string, args any) ([]byte, error) {
		b, _ := json.Marshal(args)
		f.calls = append(f.calls, method+" "+string(b))
		if err := f.errs[method]; err != nil {
			return nil, err
		}
		return []byte(f.answers[method]), nil
	}
	t.Cleanup(func() { rexCall = prev })
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/rex", []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const (
	rexView    = `{"windows":[{"window_id":"window:w1","layers":[{"blocks":[{"block_id":"block:other"},{"block_id":"block:b1"}]}]}]}`
	rexInspect = `{"client":{},"actions":[{"name":"pane.split"},{"name":"session.select"}]}`
)

func rexClients(ids ...string) string {
	var cs []string
	for _, id := range ids {
		cs = append(cs, `{"client_id":"`+id+`","info":{"kind":"app","name":"Rex","label":"`+id+`"},"connection_state":"connected"}`)
	}
	cs = append(cs, `{"client_id":"client:cli","info":{"kind":"cli"},"connection_state":"connected"}`)
	return `{"clients":[` + strings.Join(cs, ",") + `]}`
}

func TestUIRevealTerminal(t *testing.T) {
	root, _ := controlFixture(t)
	t.Setenv("REX_SESSION", "session:s1")
	t.Setenv("REX_BLOCK", "block:b1")
	t.Setenv("REX_CLIENT", "")
	ownForTest(t, root)
	gen := ownerGeneration(root, "moe", "change-auth")
	a := ui.Action{Kind: ui.ActReveal, Project: "moe", Run: "change-auth", Generation: gen}

	f := &fakeRex{answers: map[string]string{"session.view": rexView, "client.list": rexClients("client:mac"), "client.inspect": rexInspect, "session.focus_block": `{"revision":2}`, "client.perform": `{"performed_by":"client:mac"}`}}
	f.install(t)
	r := revealTerminal(context.Background(), root, a)
	if !r.OK {
		t.Fatalf("reveal = %+v", r)
	}
	want := []string{
		`session.view {"session_id":"session:s1"}`,
		`client.list {}`,
		`client.inspect {"client_id":"client:mac"}`,
		`session.focus_block {"block_id":"block:b1","session_id":"session:s1"}`,
		`client.perform {"action":"session.select","args":{"session_id":"session:s1","window_id":"window:w1"},"client_id":"client:mac","origin":"api","timeout_ms":5000}`,
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
	}

	// Several apps: ask, don't guess; the choice is then honoured.
	f = &fakeRex{answers: map[string]string{"session.view": rexView, "client.list": rexClients("client:a", "client:b"), "client.inspect": rexInspect}}
	f.install(t)
	if r := revealTerminal(context.Background(), root, a); len(r.Choices) != 2 || r.OK {
		t.Fatalf("ambiguous = %+v", r)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "session.focus_block") }) {
		t.Fatal("focused before a client was chosen")
	}
	a.Client = "client:b"
	f.answers["client.perform"] = `{}`
	if r := revealTerminal(context.Background(), root, a); !r.OK || !strings.Contains(f.calls[len(f.calls)-1], `"client_id":"client:b"`) {
		t.Fatalf("chosen = %+v %v", r, f.calls)
	}
	a.Client = ""

	// A detached block has no window: reported, nothing focused.
	f = &fakeRex{answers: map[string]string{"session.view": `{"windows":[]}`}}
	f.install(t)
	if r := revealTerminal(context.Background(), root, a); r.OK || !strings.Contains(r.Message, "detached") || len(f.calls) != 1 {
		t.Fatalf("detached = %+v %v", r, f.calls)
	}

	// The app timing out is an unknown outcome, said as such.
	f = &fakeRex{answers: map[string]string{"session.view": rexView, "client.list": rexClients("client:mac"), "client.inspect": rexInspect}, errs: map[string]error{"client.perform": errors.New("rex client.perform: timeout")}}
	f.install(t)
	if r := revealTerminal(context.Background(), root, a); !r.Uncertain || !strings.Contains(r.Message, "may still") {
		t.Fatalf("timeout = %+v", r)
	}

	// No app: unavailable. An owner other than the one shown: refused
	// before any rex call.
	f = &fakeRex{answers: map[string]string{"session.view": rexView, "client.list": rexClients()}}
	f.install(t)
	if r := revealTerminal(context.Background(), root, a); r.OK || !strings.Contains(r.Message, "no connected Rex app") {
		t.Fatalf("no app = %+v", r)
	}
	f = &fakeRex{}
	f.install(t)
	a.Generation = "someone-else"
	if r := revealTerminal(context.Background(), root, a); r.OK || len(f.calls) != 0 || !strings.Contains(r.Message, "owner changed") {
		t.Fatalf("replaced owner = %+v %v", r, f.calls)
	}
}

// A resume whose acknowledgement was lost, resent whole after a new
// hold replaced the one it released, reads back what it did and leaves
// the new hold alone. Resent with the new revision, it would conflict.
func TestUIDoResumeRetryAfterNewHold(t *testing.T) {
	root, _ := controlFixture(t)
	do := func(a ui.Action) ui.Result { return uiDo(context.Background(), root, a) }
	if r := do(ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "pause", RequestID: "p1"}); !r.OK {
		t.Fatalf("pause = %+v", r)
	}
	original := ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "resume",
		RequestID: "r1", HoldRevision: controlRecord(t, root).Revision}
	if r := do(original); !r.OK { // acknowledgement "lost"
		t.Fatalf("resume = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "pause", RequestID: "p2"}); !r.OK {
		t.Fatalf("second pause = %+v", r)
	}
	if r := do(original); !r.OK || !strings.Contains(r.Message, "already recorded") || controlRecord(t, root).Hold == nil {
		t.Fatalf("retry = %+v, hold %+v", r, controlRecord(t, root).Hold)
	}
	changed := original
	changed.HoldRevision = controlRecord(t, root).Revision
	if r := do(changed); r.OK {
		t.Fatalf("retry rebuilt against the new hold succeeded: %+v", r)
	}
}

// Closed intents remain inspectable from the canonical scan, without a
// draft or a previously running source. Ordinary dash membership stays intact.
func TestUIClosedIntentsInspectable(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root, _ := controlFixture(t)
	do := func(a ui.Action) ui.Result { return uiDo(context.Background(), root, a) }
	body := "# A goal\n" + strings.Repeat("standing direction\n", 30) + strings.Repeat("x", 164) + "END_OF_INTENT\n"
	for _, a := range []ui.Action{
		{Kind: ui.ActCreate, Project: "moe", Run: "b-goal", Workflow: "intent", Text: "# Another goal\n"},
		{Kind: ui.ActCreate, Project: "moe", Run: "a-goal", Workflow: "intent", Text: body},
		{Kind: ui.ActCreate, Project: "moe", Run: "an-idea", Workflow: "idea", Text: "# an-idea\n"},
	} {
		if r := do(a); !r.OK {
			t.Fatalf("%+v: %+v", a, r)
		}
	}
	s := newUISource(root, DashFilter{}, os.Stdout)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := takeBoard(t, s)
	if it := uiItem(t, b, "moe", "a-goal", ui.ItemIntent); !it.Editable || it.Status != "" {
		t.Fatalf("open intent = %+v", it)
	}
	// The pulse's status.md rides beside the canvas, never inside the
	// Body an edit draft is seeded from.
	writeFile(t, filepath.Join(root, run.StatusPath("moe", "a-goal")), "2026-11-24, pulse\n")
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b = takeBoard(t, s)
	if it := uiItem(t, b, "moe", "a-goal", ui.ItemIntent); it.Status != "2026-11-24, pulse\n" || it.Body != body {
		t.Fatalf("intent with status.md = %+v", it)
	}
	for _, slug := range []string{"a-goal", "b-goal", "an-idea"} {
		if r := do(ui.Action{Kind: ui.ActClose, Project: "moe", Run: slug}); !r.OK {
			t.Fatalf("close %s: %+v", slug, r)
		}
	}
	for _, source := range []*uiSource{s, newUISource(root, DashFilter{}, os.Stdout)} {
		if err := source.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		b = takeBoard(t, source)
		it := uiItem(t, b, "moe", "a-goal", ui.ItemIntent)
		if it.Body != body || it.Editable || it.Notes || it.Controls || it.Terminal || it.Question != nil || !strings.Contains(strings.Join(it.Detail, "\n"), "closed") {
			t.Fatalf("closed intent = %+v", it)
		}
		// A closed intent is a finished goal, so it is a COMPLETED row
		// like any other history, not a console-only section.
		if !strings.Contains(b.Lines[it.Line], it.Label()) || !strings.Contains(strings.Join(b.Lines[:it.Line], "\n"), "COMPLETED") || !strings.Contains(b.Lines[it.Line], "intent:closed") {
			t.Fatalf("closed intent lines = %v", b.Lines)
		}
		if idea := uiItem(t, b, "moe", "an-idea", ui.ItemIdea); idea.Editable {
			t.Fatalf("closed idea = %+v", idea)
		}
	}
	// Closed intents follow the dashboard's own filters.
	for _, filter := range []DashFilter{{ProjectFilter: "elsewhere"}, {WorkflowFilter: "idea"}, {WorkflowFilter: "intent"}} {
		source := newUISource(root, filter, os.Stdout)
		if err := source.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		b := takeBoard(t, source)
		var intents []string
		for _, it := range b.Items {
			if it.Kind == ui.ItemIntent {
				intents = append(intents, it.Run)
			}
		}
		slices.Sort(intents)
		want := []string(nil)
		if filter.WorkflowFilter == "intent" {
			want = []string{"a-goal", "b-goal"}
		}
		if !slices.Equal(intents, want) {
			t.Fatalf("filter %+v: intents %v, want %v", filter, intents, want)
		}
	}
	// Inspection grants no write authority, even if a stale editor sends one.
	if r := do(ui.Action{Kind: ui.ActEdit, Project: "moe", Run: "a-goal", Expected: body, Text: "replacement"}); r.OK || r.Uncertain {
		t.Fatalf("closed edit = %+v", r)
	}
	got, err := os.ReadFile(root + "/" + run.ContentPath("moe", "a-goal", "intent"))
	if err != nil || string(got) != body {
		t.Fatalf("closed canvas changed: %q, %v", got, err)
	}
}

func TestUIFactoryFrameCache(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root, _ := controlFixture(t)
	s := newUISource(root, DashFilter{}, os.Stdout)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := takeBoard(t, s)
	if len(b.Art.Frames) != 8 || b.Art.Height != len(b.Art.Frames[0]) {
		t.Fatal("missing bounded animation frames")
	}
	for i, line := range b.Art.Frames[0] {
		if b.Overview[b.Art.Start+i] != line {
			t.Fatal("renderer span does not match board art")
		}
	}
	for _, frame := range b.Art.Frames {
		if len(frame) != b.Art.Height {
			t.Fatal("varying frame height")
		}
		if frame[len(frame)-1] != b.Art.Frames[0][b.Art.Height-1] {
			t.Fatal("rail animated")
		}
		for _, line := range frame {
			if strings.ContainsRune(line, '\x1b') {
				t.Fatal("NO_COLOR frame contains ANSI")
			}
		}
	}
	// Owner pushes and new gathers with unchanged factory state reuse the
	// frame set. There are no input/capture reads on a cached publish.
	s.mu.Lock()
	again, _ := s.buildBoard(time.Now().Add(time.Second))
	s.mu.Unlock()
	if &b.Art.Frames[0][0] != &again.Art.Frames[0][0] {
		t.Fatal("publish regenerated frames")
	}
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	again = takeBoard(t, s)
	if &b.Art.Frames[0][0] != &again.Art.Frames[0][0] {
		t.Fatal("unchanged gather regenerated frames")
	}
}

func TestUIBoardOmitsRoutineSweeps(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	now := time.Date(2026, 11, 13, 12, 30, 0, 0, time.Local)
	for _, tc := range []struct {
		name   string
		host   string
		live   serveLiveness
		filter DashFilter
		banner string
	}{
		{"rex", "rex", serveAlive, DashFilter{}, "serve armed"},
		{"local", "local", serveAlive, DashFilter{}, "serve armed"},
		{"unknown", "rex", serveUnknown, DashFilter{}, "serve unknown (sandbox)"},
		{"project filter", "rex", serveAlive, DashFilter{ProjectFilter: "moe"}, "serve armed"},
		{"workflow filter", "rex", serveAlive, DashFilter{WorkflowFilter: "sdlc"}, "serve armed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newUISource("", tc.filter, os.Stdout)
			s.serveNow = serveState{ok: true, live: tc.live, snap: serve.ActivitySnapshot{
				Armed: true, Host: tc.host, Started: now.Add(-time.Hour), WrittenAt: now,
				Projects: []serve.ActivityProject{
					{Project: "moe", SweepStarted: now.Add(-8 * time.Minute), Decision: "the journal moved"},
					// A sweep takes precedence over its historical failure.
					{Project: "other-sweeper", SweepStarted: now.Add(-time.Second), Decision: "retrying sweep",
						SweptAt: now.Add(-time.Hour), Fails: 2, CoolTicks: 2},
				},
			}}
			s.snap.Rows = []dash.Row{{Project: "moe", Run: "change-auth", Bucket: dash.BucketActiveRuns,
				Note: "sdlc/build", Stage: "build", Control: "working · agent", When: now}}
			b, reports := s.buildBoard(now)
			all := strings.Join(b.Lines, "\n")
			for _, unwanted := range []string{"sweeping", "the journal moved", "other-sweeper", "retrying sweep", "cooling"} {
				if strings.Contains(all, unwanted) {
					t.Fatalf("routine sweep leaked %q:\n%s", unwanted, all)
				}
			}
			// The board opens with the PULSE section — heading, status
			// line, blank — and ACTIVE follows it.
			if b.Lines[0] != "PULSE" || !strings.HasPrefix(b.Lines[3], "ACTIVE (1)") || len(b.Items) != 1 || len(b.Issues) != 0 {
				t.Fatalf("unexpected board prefix, items or issues: %+v", b)
			}
			line := b.Lines[b.Items[0].Line]
			for _, want := range []string{"moe/change-auth", "sdlc/build", "working · agent"} {
				if !strings.Contains(line, want) {
					t.Fatalf("run lost %q: %q", want, line)
				}
			}
			if !strings.Contains(b.Lines[1], tc.banner) || strings.Contains(b.Banner, "serve") {
				t.Fatalf("status line = %q, want %q; banner = %q, want logo and clock only", b.Lines[1], tc.banner, b.Banner)
			}
			if tc.live == serveAlive && tc.filter == (DashFilter{}) {
				if activity := reportSlot(reports, "activity"); activity.Message != "Live sweeps: moe, other-sweeper" {
					t.Fatalf("hidden rows changed activity: %+v", activity)
				}
			}
			s.snap.Rows = nil
			b, _ = s.buildBoard(now)
			if all := strings.Join(b.Lines, "\n"); strings.Contains(all, "sweeping") || strings.Contains(all, "the journal moved") || strings.Contains(all, "other-sweeper") {
				t.Fatalf("sweep returned without a run:\n%s", all)
			}
		})
	}
}

// Serve's exceptions are Console issues, not board rows, and their text
// is time-stable so the Console logs each one once rather than on every
// repaint. Routine sweeps and a dead serve's stale records say nothing.
func TestUIBoardLogsServiceExceptionsAsIssues(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	now := time.Date(2026, 11, 13, 12, 30, 0, 0, time.Local)
	for _, live := range []serveLiveness{serveAlive, serveDead, serveUnknown} {
		t.Run([]string{"alive", "dead", "unknown"}[live], func(t *testing.T) {
			s := newUISource("", DashFilter{}, os.Stdout)
			s.serveNow = serveState{ok: true, live: live, ahead: 2, snap: serve.ActivitySnapshot{
				Armed: true, Host: "rex", HostError: "host unavailable\x1b[2J", WrittenAt: now,
				PushFailingSince: now.Add(-time.Minute), PushError: "push rejected",
				PushRetryAt: now.Add(2 * time.Minute),
				Projects: []serve.ActivityProject{
					{Project: "sweeper", SweepStarted: now.Add(-8 * time.Minute), Decision: "the journal moved",
						SweptAt: now.Add(-time.Hour), Fails: 2, CoolTicks: 2},
					{Project: "cooler", CoolTicks: 2, Fails: 1, SweptAt: now.Add(-time.Hour), Decision: "cooling off after 1 failure(s)"},
					// The gate verdict alone is not a running sweep.
					{Project: "faller", Sweep: true, SweptAt: now.Add(-time.Hour), Fails: 3, Decision: "agent failed"},
					{Project: "deferred", SweepDeferred: true, Decision: "quota tomorrow"},
					{Project: "quiet", SweptAt: now.Add(-time.Hour), SweepClean: true},
				},
			}}
			s.snap.Rows = []dash.Row{{Project: "moe", Run: "change-auth", Bucket: dash.BucketActiveRuns, When: now}}
			b, _ := s.buildBoard(now)
			// The PULSE section's status line names the serve cluster; its
			// trouble rows stay off the board, so ACTIVE follows at once.
			if b.Lines[0] != "PULSE" || !strings.HasPrefix(b.Lines[3], "ACTIVE (1)") || len(b.Items) != 1 || b.Items[0].Line != 4 {
				t.Fatalf("service lines displaced the board: %q %+v", b.Lines, b.Items)
			}
			if all := strings.Join(b.Lines[2:], "\n"); strings.Contains(all, "rex") || strings.Contains(all, "push") || strings.Contains(all, "cooler") {
				t.Fatalf("service text left in the board:\n%s", all)
			}
			at := now.Add(-time.Hour).Local().Format("15:04")
			want := []string{
				"serve: cooler sweep failed at " + at + " (1 in a row)",
				"serve: deferred deferred — quota tomorrow",
				"serve: faller sweep failed at " + at + " (3 in a row)",
				// Raw here; the Console's wrapRows escapes it.
				"serve: host rex — host unavailable\x1b[2J",
				"serve: push failing since " + now.Add(-time.Minute).Local().Format("15:04") + " — push rejected",
			}
			if live == serveDead {
				want = nil
			}
			if !slices.Equal(b.Issues, want) {
				t.Fatalf("issues:\n got %q\nwant %q", b.Issues, want)
			}
			// An hour on, the same snapshot logs nothing new.
			later, _ := s.buildBoard(now.Add(time.Hour))
			if !slices.Equal(later.Issues, b.Issues) {
				t.Fatalf("issues moved with the clock:\n%q\n%q", b.Issues, later.Issues)
			}
			if live == serveUnknown && !strings.Contains(b.Lines[1], "serve unknown (sandbox)") {
				t.Fatalf("unknown serve state lost from the PULSE status line: %q", b.Lines[1])
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		s := newUISource("", DashFilter{}, os.Stdout)
		s.serveNow = serveState{err: errors.New("serve: bad serve.json")}
		if b, _ := s.buildBoard(now); !slices.Equal(b.Issues, []string{"serve: bad serve.json"}) || strings.Contains(strings.Join(b.Lines, "\n"), "serve.json") {
			t.Fatalf("issues %q, lines %q", b.Issues, b.Lines)
		}
	})
}

func TestUIBoardSeparatesPinnedRegionsAndEscapesLabels(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	now := time.Date(2026, 11, 12, 12, 30, 0, 0, time.Local)
	s := newUISource("", DashFilter{}, os.Stdout)
	s.gathered = now
	s.serveNow.err = errors.New("serve: hostile \x1b[31m red\nsecond line")
	s.gerr = errors.New("gather \x1b[2J failed")
	s.snap.Rows = []dash.Row{{Project: "p", Run: "r", Bucket: dash.BucketBacklog, Note: "title\x1b[31m red\nnext\tcolumn", When: now}}
	s.snap.Histogram = make([]int, dash.HistDays)
	s.snap.Histogram[59] = 5
	b, _ := s.buildBoard(now)
	var expected strings.Builder
	banner.Dash(&expected, dash.Clock(now))
	if b.Banner != strings.TrimSuffix(expected.String(), "\n") {
		t.Fatalf("not the shared dash banner: %q", b.Banner)
	}
	all := strings.Join(b.Lines, "\n")
	if strings.Contains(all, "MINISTRY OF EVERYTHING") || strings.Contains(all, "last 60 days") {
		t.Fatal("pinned content leaked into scrolling lines")
	}
	if strings.ContainsRune(all, '\x1b') || !strings.Contains(all, "^[[31m") || !strings.Contains(all, "^Jnext^Icolumn") {
		t.Fatalf("unsafe renderer input: %q", all)
	}
	if len(b.Items) != 1 || !strings.Contains(b.Lines[b.Items[0].Line], "p/r") {
		t.Fatalf("service lines displaced item: %+v", b.Items)
	}
	if len(b.Overview) != dash.HistRows+2+1+b.Art.Height {
		t.Fatal("chart or internal spacing lost")
	}
	for _, line := range b.Overview {
		if strings.Contains(line, "Default:") {
			t.Fatal("global default leaked into pinned overview")
		}
	}
	for i, line := range b.Art.Frames[0] {
		if b.Overview[b.Art.Start+i] != line {
			t.Fatal("factory boundary wrong")
		}
	}
}
