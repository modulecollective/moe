package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/cliout"
)

func TestAgentCommandOnEmptyAndFilteredBoards(t *testing.T) {
	for _, items := range [][]Item{nil, {run("p", "a")}} {
		m, q := testModel(t)
		b := board(1, items...)
		b.AgentRevision, b.AgentSummary, b.ProjectFilter = 17, "Default: Codex until tomorrow", "p"
		m.setBoard(b)
		keyboard(m, ":agent\r")
		if len(q) != 0 || !strings.Contains(consoleText(m), b.AgentSummary) {
			t.Fatal("status did not use global readback")
		}
		keyboard(m, ":agent claude\r")
		j := <-q
		if len(j.steps) != 1 || j.steps[0].Kind != ActAgent || j.steps[0].AgentRevision != 17 || j.steps[0].Agent != "claude" || j.steps[0].Project != "" {
			t.Fatalf("action: %+v", j)
		}
		m.finished(jobResult{job: j, results: []Result{{OK: true, Message: "Default: Claude until tomorrow"}}})
		keyboard(m, ":agent reset\r")
		if a := (<-q).steps[0]; a.Agent != "reset" || a.AgentRevision != 17 {
			t.Fatalf("reset: %+v", a)
		}
	}
}

func TestAgentCompletionAndInvalidCommands(t *testing.T) {
	e := newEditor("agent c", true)
	c := commandMatches(e)
	if len(c.matches) != 2 || c.matches[0] != "codex" || c.matches[1] != "claude" {
		t.Fatalf("completion: %+v", c)
	}
	m, q := testModel(t)
	for _, s := range []string{":agent invalid\r", ":agent codex extra\r"} {
		keyboard(m, s)
	}
	if len(q) != 0 {
		t.Fatal("invalid command dispatched")
	}
	b := board(1)
	b.AgentError = "malformed record"
	m.setBoard(b)
	keyboard(m, ":agent codex\r")
	if len(q) != 0 || !strings.Contains(consoleText(m), "malformed record") {
		t.Fatal("observation error was hidden")
	}
}

func TestAgentConsoleLabelFitsWithoutMovingContent(t *testing.T) {
	for _, width := range []int{120, 80, 40} {
		for _, unread := range []int{0, 12, 123456789} {
			for _, focused := range []bool{false, true} {
				t.Run(fmt.Sprintf("width=%d/unread=%d/focused=%t", width, unread, focused), func(t *testing.T) {
					m, _ := testModel(t)
					b := dashboardBoard()
					b.AgentSummary = "Default: Codex until 2026-11-25 12:00 UTC"
					m.resize(40, width)
					m.setBoard(b)
					for range 50 {
						press(m, Key{Code: KeyDown})
					}
					m.console.unread = unread
					if focused {
						m.focus = paneConsole
					}
					m.color = true
					before := m.view()
					b.AgentLabel = "default: codex"
					m.setBoard(b)
					m.console.unread = unread
					after := m.view()
					rule := m.layout().consoleRule
					title := "Console"
					if unread > 0 {
						title += fmt.Sprintf(" · %d new (End follows)", unread)
					}
					fits := textWidth(title+" · "+b.AgentLabel)+4 <= width-1
					row := after.rows[rule]
					if strings.Contains(row, b.AgentLabel) != fits {
						t.Fatalf("fit=%t: %q", fits, row)
					}
					if fits && !strings.Contains(row, " · "+cliout.Dim+b.AgentLabel+cliout.Reset) {
						t.Fatalf("default not muted: %q", row)
					}
					for i := range before.rows {
						if i != rule && before.rows[i] != after.rows[i] {
							t.Fatalf("default moved row %d", i)
						}
					}
					if textWidth(row) != width-1 {
						t.Fatalf("divider width: %q", row)
					}
					m.color = false
					if plain := m.view().rows[rule]; plain != stripSGR(row) {
						t.Fatalf("color changed layout: %q vs %q", plain, stripSGR(row))
					}
					// Reset/expiry refresh removes the label from the same divider.
					b.AgentLabel = ""
					m.setBoard(b)
					if strings.Contains(m.view().rows[rule], "default:") {
						t.Fatal("reset label retained")
					}
					m.resize(20, 40)
					keyboard(m, ":agent\r")
					if !strings.Contains(consoleText(m), "Default: Codex until") {
						t.Fatal("narrow readback lost")
					}
				})
			}
		}
	}
}

func TestAgentLabelAbsentFromFullscreenConfirmation(t *testing.T) {
	m, _ := testModel(t)
	m.resize(20, 40)
	b := board(1, idea("p", "capture", 0))
	b.AgentLabel = "default: claude"
	m.setBoard(b)
	keyboard(m, ":close\r")
	if m.mode != modeConfirm {
		t.Fatal("confirmation not reached")
	}
	screen := strings.Join(m.view().rows, "\n")
	if strings.Contains(screen, "default:") || strings.Contains(screen, "Console") {
		t.Fatal("label relocated in confirmation")
	}
}
