package ui

import (
	"fmt"
	"strings"
	"testing"
)

func TestIdeaTagKeys(t *testing.T) {
	for _, focus := range []pane{paneBoard, paneDetail, paneConsole} {
		for _, tc := range []struct {
			key, workflow string
			design        bool
		}{{"g", "sdlc", false}, {"d", "sdlc", true}, {"u", "", false}} {
			t.Run(fmt.Sprintf("%v/%s", focus, tc.key), func(t *testing.T) {
				m, q := testModel(t)
				m.setBoard(board(1, idea("p", "first", 1), idea("p", "second", 1)))
				m.focus = focus
				selected := m.sel
				keyboard(m, tc.key)
				if len(q) != 1 || m.sel != selected || m.mode != modeNormal || m.focus != focus {
					t.Fatalf("dispatch: queue=%d selection=%+v mode=%v focus=%v", len(q), m.sel, m.mode, m.focus)
				}
				j := <-q
				if len(j.steps) != 1 || j.target != selected {
					t.Fatalf("job = %+v", j)
				}
				a := j.steps[0]
				if a.Kind != ActTag || a.Project != "p" || a.Run != "first" || a.Workflow != tc.workflow || a.DesignOnly != tc.design {
					t.Fatalf("action = %+v", a)
				}
				m.focus = paneBoard
				keyboard(m, "j"+tc.key)
				m.setBoard(board(2, idea("p", "second", 1), idea("p", "first", 1)))
				if len(q) != 0 || !strings.Contains(consoleText(m), "busy:") || m.busy.steps[0].Run != "first" || m.sel.run != "second" {
					t.Fatalf("pending job retargeted/queued: %+v, %s", m.busy, consoleText(m))
				}
			})
		}
	}
}

func TestIdeaTagKeysRefuseIneligible(t *testing.T) {
	for _, name := range []string{"empty", "intent", "run", "chore", "closed", "promoted", "kept"} {
		t.Run(name, func(t *testing.T) {
			m, q := testModel(t)
			it := idea("p", name, 1)
			switch name {
			case "intent":
				it.Kind = ItemIntent
			case "run":
				it = run("p", name)
			case "chore":
				it.Kind, it.Editable = ItemChore, false
			case "closed", "promoted":
				it.Editable = false
			}
			if name != "empty" {
				m.setBoard(board(1, it))
			}
			if name == "kept" {
				keyboard(m, "eprivate text\x1b\x1b")
				m.setBoard(board(2))
				if m.sel.bucket != keptBucket {
					t.Fatal("did not select retained draft row")
				}
			}
			for _, key := range []string{"g", "d", "u"} {
				keyboard(m, key)
				if len(q) != 0 || !strings.Contains(m.console.events[len(m.console.events)-1].text, "select an open idea") {
					t.Fatalf("%s: queue=%d, %s", key, len(q), consoleText(m))
				}
			}
		})
	}
}

func TestIdeaTagKeysStayInTheirModes(t *testing.T) {
	for _, mode := range []string{"editor", "command", "help", "confirm", "choice", "paste"} {
		t.Run(mode, func(t *testing.T) {
			m, q := testModel(t)
			m.setBoard(board(1, idea("p", "i", 1)))
			switch mode {
			case "editor":
				keyboard(m, "e")
			case "command":
				keyboard(m, ":")
			case "help":
				keyboard(m, "?")
			case "confirm":
				keyboard(m, ":close\r")
			case "choice":
				m.mode = modeChoice
				m.choices = []Choice{{Label: "app"}}
			}
			if mode == "paste" {
				keyboard(m, "\x1b[200~gdu\x1b[201~")
			} else if mode == "confirm" {
				for _, key := range []string{"g", "d", "u"} {
					m.ask("Close idea?", func(*model) { t.Fatal("shortcut confirmed close") })
					keyboard(m, key)
				}
			} else {
				keyboard(m, "gdu")
				keyboard(m, "\x1b[200~gdu\x1b[201~")
				if mode == "editor" && !strings.HasSuffix(m.active.ed.String(), "gdugdu") {
					t.Fatal("editor consumed shortcut letters")
				}
				if mode == "command" && m.cmd.String() != "gdugdu" {
					t.Fatal("command consumed shortcut letters")
				}
			}
			if len(q) != 0 {
				t.Fatal("tag dispatched outside normal typed input")
			}
		})
	}
	// t retains terminal reveal, including on an idea with a terminal.
	m, q := testModel(t)
	it := idea("p", "i", 1)
	it.Terminal = true
	m.setBoard(board(1, it))
	keyboard(m, "t")
	if len(q) != 1 || (<-q).steps[0].Kind != ActReveal {
		t.Fatal("t no longer reveals the terminal")
	}
}

func TestIdeaTagKeyFailureAllowsRetryAndKeepsDraft(t *testing.T) {
	m, jobs := testModel(t)
	m.setBoard(board(1, idea("p", "i", 1)))
	keyboard(m, "eprivate text\x1b\x1b")
	draft := m.drafts[draftKey{project: "p", run: "i", purpose: "body"}]
	selected := m.sel
	results := make(chan jobResult, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker(func(Action) Result { return Result{Message: "commit refused"} }, jobs, results)
	}()
	keyboard(m, "g")
	m.finished(<-results)
	if m.busy != nil || m.sel != selected || !strings.Contains(consoleText(m), "failed: tag sdlc") || !strings.Contains(consoleText(m), "commit refused") || m.dirtyDrafts() != 1 || !strings.HasSuffix(draft.ed.String(), "private text") {
		t.Fatalf("failed shortcut lost recovery: %s", consoleText(m))
	}
	keyboard(m, "d")
	m.finished(<-results)
	close(jobs)
	<-done
	if m.busy != nil || m.sel != selected || m.drafts[draft.key] != draft {
		t.Fatal("retry changed selection or draft")
	}
}

func TestIdeaTagKeyHints(t *testing.T) {
	for _, size := range [][2]int{{20, 40}, {30, 100}} {
		for _, color := range []bool{false, true} {
			m, _ := testModel(t)
			m.resize(size[0], size[1])
			m.color = color
			for _, it := range []Item{idea("p", "i", 1), {Project: "p", Run: "closed", Kind: ItemIdea}, {Project: "p", Run: "intent", Kind: ItemIntent, Editable: true}, run("p", "r"), {Kind: ItemChore}} {
				m.setBoard(board(m.raw.Seq+1, it))
				row := m.view().rows[size[0]-1]
				for _, hint := range []string{"g sdlc", "d design", "u untag"} {
					if strings.Contains(row, hint) != (it.Kind == ItemIdea && it.Editable) {
						t.Fatalf("size=%v color=%v item=%+v: %q", size, color, it, row)
					}
				}
			}
			m.setBoard(board(m.raw.Seq + 1))
			if strings.Contains(m.view().rows[size[0]-1], "g sdlc") {
				t.Fatal("empty board advertises tags")
			}
		}
	}
	for _, want := range []string{"SDLC licence", "design-only licence", "clear either idea licence"} {
		if !strings.Contains(helpText, want) {
			t.Fatalf("help missing %q", want)
		}
	}
}
