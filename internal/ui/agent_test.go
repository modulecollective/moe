package ui

import (
	"strings"
	"testing"
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

func TestAgentStatusOverviewSurvivesResizeAndScroll(t *testing.T) {
	m, _ := testModel(t)
	b := dashboardBoard()
	b.AgentSummary = "Default: Codex until 2026-11-25 12:00 UTC"
	b.Overview = append(b.Overview, b.AgentSummary)
	m.resize(40, 120)
	m.setBoard(b)
	for range 50 {
		press(m, Key{Code: KeyDown})
	}
	assertPinned(t, m)
	m.resize(20, 40)
	keyboard(m, ":agent\r")
	if !strings.Contains(consoleText(m), "Default: Codex until") {
		t.Fatal("narrow terminal lost status command")
	}
	m.resize(40, 120)
	assertPinned(t, m)
}
