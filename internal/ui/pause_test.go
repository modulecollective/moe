package ui

import (
	"slices"
	"strings"
	"testing"
)

func TestServeCommandsOnEmptyAndFilteredBoards(t *testing.T) {
	for _, items := range [][]Item{nil, {run("p", "a")}} {
		m, q := testModel(t)
		b := board(1, items...)
		b.PauseSummary, b.PauseRevision, b.ProjectFilter = "new automatic sweeps paused until tomorrow", 17, "p"
		m.setBoard(b)
		keyboard(m, ":serve status\r")
		if len(q) != 0 || !strings.Contains(consoleText(m), b.PauseSummary) {
			t.Fatal("status did not read global state")
		}
		for _, command := range []string{"pause", "unpause"} {
			keyboard(m, ":serve "+command+"\r")
			j := <-q
			if len(j.steps) != 1 {
				t.Fatalf("job: %+v", j)
			}
			a := j.steps[0]
			if a.Kind != ActServePause || a.PauseRevision != 17 || a.Pause != (command == "pause") || a.Project != "" || a.Run != "" {
				t.Fatalf("action: %+v", a)
			}
			m.finished(jobResult{job: j, results: []Result{{OK: true, Message: "changed"}}})
		}
		for _, command := range []string{":serve\r", ":serve pause extra\r", ":serve invalid\r"} {
			keyboard(m, command)
		}
		if len(q) != 0 {
			t.Fatal("invalid serve command mutated")
		}
		b.Seq++
		b.PauseError = "malformed pause record"
		m.setBoard(b)
		keyboard(m, ":serve pause\r")
		if len(q) != 0 || !strings.Contains(consoleText(m), b.PauseError) {
			t.Fatal("error hidden")
		}
	}
	c := commandMatches(newEditor("serve ", true))
	if strings.Join(c.matches, ",") != "pause,unpause,status" {
		t.Fatalf("completion: %v", c.matches)
	}
}

func TestPauseOverviewAndNarrowStatusPreserveSelection(t *testing.T) {
	for _, width := range []int{120, 80, 40} {
		m, _ := testModel(t)
		m.resize(50, width)
		b := dashboardBoard()
		b.PauseSummary = "new automatic sweeps paused until 2026-11-26 12:00 PST"
		lines, art := b.Overview(width)
		b.Overview = fixedOverview(append(slices.Clone(lines), "new automatic sweeps paused (:serve status)"), art)
		m.setBoard(b)
		selected := m.sel
		frame := m.view()
		visible := strings.Contains(strings.Join(frame.rows, "\n"), "new automatic sweeps paused (:serve status)")
		if width >= 80 && !visible {
			t.Fatalf("pause missing at width %d", width)
		}
		for _, row := range frame.rows {
			if textWidth(row) > width-1 {
				t.Fatalf("overflow at %d: %q", width, row)
			}
		}
		keyboard(m, ":serve status\r")
		if !strings.Contains(consoleText(m), b.PauseSummary) || m.sel != selected {
			t.Fatalf("status lost readback/selection at %d", width)
		}
	}
}
