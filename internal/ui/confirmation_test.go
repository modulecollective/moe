package ui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func confirmationScreen(t *testing.T, m *model, want string) {
	t.Helper()
	f := m.view()
	if len(f.rows) != m.high || f.cursor {
		t.Fatalf("invalid confirmation frame: %+v", f)
	}
	for _, row := range f.rows {
		if textWidth(row) > m.width-1 {
			t.Fatalf("confirmation exceeds screen width: %q", row)
		}
	}
	if !strings.Contains(strings.Join(f.rows, ""), want) {
		t.Fatalf("confirmation %q missing from screen:\n%s", want, strings.Join(f.rows, "\n"))
	}
	if !strings.Contains(f.rows[m.high-1], "y confirm") || !strings.Contains(f.rows[m.high-1], "cancel") {
		t.Fatalf("confirmation keys missing: %q", f.rows[m.high-1])
	}
}

func TestConfirmationCloseVisible(t *testing.T) {
	for _, size := range [][2]int{{30, 100}, {12, 40}} {
		for _, following := range []bool{false, true} {
			for _, accept := range []bool{false, true} {
				t.Run(fmt.Sprintf("%dx%d/follow=%t/accept=%t", size[1], size[0], following, accept), func(t *testing.T) {
					m, q := testModel(t)
					m.resize(size[0], size[1])
					m.setBoard(board(1, idea("p", "capture", 1)))
					for i := range 20 {
						m.say(fmt.Sprintf("old event %d", i))
					}
					keyboard(m, "\t\t\x1b[H")
					if following {
						keyboard(m, "\x1b[F")
					}
					// Complete an earlier action while the close question is pending.
					keyboard(m, ":untag\r")
					previous := <-q
					keyboard(m, ":close\r")
					const prompt = "Close idea p/capture? Its history stays"
					confirmationScreen(t, m, prompt)
					m.finished(jobResult{job: previous, results: []Result{{OK: true, Message: "untagged"}}})
					b := board(2, idea("p", "different", 1))
					b.Issues = []string{"observation failed"}
					m.setBoard(b)
					for range consoleLimit {
						m.say("later event displaces console history")
					}
					confirmationScreen(t, m, prompt)
					if m.console.follow != following {
						t.Fatal("confirmation changed console following")
					}
					select {
					case j := <-q:
						t.Fatalf("close submitted before confirmation: %+v", j)
					default:
					}
					key := "n"
					if accept {
						key = "y"
					}
					keyboard(m, key)
					if m.confirm != nil || m.mode != modeNormal {
						t.Fatal("confirmation not dismissed")
					}
					if accept {
						select {
						case j := <-q:
							if a := j.steps[0]; a.Kind != ActClose || a.Project != "p" || a.Run != "capture" {
								t.Fatalf("close lost captured target: %+v", a)
							}
						default:
							t.Fatal("confirmed close not submitted")
						}
					} else if len(q) != 0 {
						t.Fatal("cancel submitted a close")
					}
				})
			}
		}
	}
}

func TestConfirmationDraftProtectionVisible(t *testing.T) {
	for _, quit := range []bool{false, true} {
		for _, accept := range []bool{false, true} {
			t.Run(fmt.Sprintf("quit=%t/accept=%t", quit, accept), func(t *testing.T) {
				m, q := testModel(t)
				m.resize(12, 40)
				m.setBoard(board(1, run("p", "target")))
				keyboard(m, "nprivate unsaved text")
				if quit {
					keyboard(m, "\x1b\x1b")
					keyboard(m, "q")
					confirmationScreen(t, m, "Discard 1 unsaved draft and quit?")
				} else {
					keyboard(m, "\x18")
					confirmationScreen(t, m, "Discard this note for p/target?")
				}
				key := "n"
				if accept {
					key = "y"
				}
				eff := keyboard(m, key)
				if (eff == effQuit) != (quit && accept) {
					t.Fatalf("quit effect = %v", eff)
				}
				if len(q) != 0 {
					t.Fatal("discard or quit submitted a write")
				}
				if !quit && accept {
					if m.dirtyDrafts() != 0 {
						t.Fatal("confirmed discard retained draft")
					}
				} else if m.dirtyDrafts() != 1 {
					t.Fatal("draft lost before exit or on cancellation")
				}
				if !accept {
					keyboard(m, "n")
					if m.active == nil || m.active.ed.String() != "private unsaved text" {
						t.Fatal("cancelled draft cannot be resumed")
					}
				}
			})
		}
	}
}

func TestConfirmationNotOverwrittenByAnimation(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(animatedBoard())
	keyboard(m, "nunsaved\x03")
	f := m.view()
	before := slices.Clone(f.rows)
	if out := m.animate(&f); len(out) != 0 || !slices.Equal(before, f.rows) {
		t.Fatalf("animation overwrote confirmation: %q", out)
	}
	confirmationScreen(t, m, "Discard 1 unsaved draft and quit?")
}

func TestConfirmationLongTargetScrollAndResize(t *testing.T) {
	m, q := testModel(t)
	m.resize(12, 40)
	var parts []string
	for i := range 90 {
		parts = append(parts, fmt.Sprintf("capture-%02d", i))
	}
	target := strings.Join(parts, "-") + "-tail"
	m.setBoard(board(1, idea("p", target, 1)))
	keyboard(m, ":close\r")
	want := "Close idea p/" + target + "? Its history stays"
	visible := map[int]string{}
	for {
		f := m.view()
		if m.mode != modeConfirm || !strings.Contains(f.rows[m.high-1], "scroll") {
			t.Fatal("long confirmation lacks scrolling")
		}
		for i, row := range f.rows[2 : m.high-1] {
			visible[m.confirm.top+i] = row
		}
		top := m.confirm.top
		keyboard(m, "\x1b[6~") // Page Down
		if m.confirm.top == top {
			break
		}
	}
	var complete strings.Builder
	for i := range len(visible) {
		complete.WriteString(visible[i])
	}
	if complete.String() != want {
		t.Fatal("long target or consequence lost while paging")
	}
	keyboard(m, "\x1b[H\x1b[B") // Home, Down
	if m.confirm.top != 1 {
		t.Fatal("confirmation Down did not scroll")
	}
	keyboard(m, "\x1b[A") // Up
	if m.confirm.top != 0 {
		t.Fatal("confirmation Up did not scroll")
	}
	keyboard(m, "\x1b[F\x1b[5~") // End, Page Up
	if m.confirm.top == 0 {
		t.Fatal("confirmation End/Page Up did not retain a later page")
	}
	m.resize(10, 30)
	if !strings.Contains(strings.Join(m.view().rows, ""), "too small") || m.mode != modeConfirm {
		t.Fatal("resize lost pending question")
	}
	m.resize(30, 100)
	confirmationScreen(t, m, want)
	if m.confirm.top != 0 {
		t.Fatal("resize did not clamp confirmation scroll")
	}
	keyboard(m, "\x1b\x1b")
	if m.mode != modeNormal || m.confirm != nil || len(q) != 0 {
		t.Fatal("Escape did not cancel long confirmation")
	}
}
