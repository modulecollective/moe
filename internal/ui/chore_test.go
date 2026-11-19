package ui

import (
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
)

func TestChoreInspectionScrollRefreshAndCapabilities(t *testing.T) {
	m, q := testModel(t)
	m.resize(20, 40)
	chore := Item{Project: "p", Run: "same", Bucket: int(dash.BucketChores), Kind: ItemChore,
		Detail: []string{"judged — pulse decides", "criterion " + strings.Repeat("full condition ", 12), "prompt: \x1b[2J" + strings.Repeat("full prompt ", 100), "last prompt line"}}
	r := run("p", "same")
	m.setBoard(board(1, r, chore))
	press(m, Key{Code: KeyDown})
	for _, key := range []rune{'e', 'n', 'a', 't'} {
		press(m, Key{Code: KeyRune, Rune: key})
	}
	for _, command := range []string{"pause", "finish", "edit", "close", "terminal", "tag-sdlc"} {
		typed(m, ":"+command)
		press(m, Key{Code: KeyEnter})
	}
	if len(q) != 0 {
		t.Fatal("chore queued an action")
	}
	// Escape control bytes and wrap long authored lines in the real details pane.
	lines, _ := m.detailLines(chore, true, 39, 200)
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "\x1b") || !strings.Contains(text, "^[[2J") {
		t.Fatalf("unsafe or lost control text: %q", text)
	}
	press(m, Key{Code: KeyTab}, Key{Code: KeyEnd})
	if m.detailTop == 0 {
		t.Fatal("chore detail did not scroll")
	}
	frame := strings.Join(m.view().rows, "\n")
	if !strings.Contains(frame, "last prompt line") {
		t.Fatal("full prompt tail unreachable")
	}
	// A time refresh can reorder due rows. Same-name run must not win selection.
	chore.Detail[0] = "due — cadence"
	m.setBoard(board(2, chore, r))
	it, ok := m.selected()
	if !ok || it.Kind != ItemChore || it.Bucket != int(dash.BucketChores) {
		t.Fatalf("refresh selected %+v", it)
	}
	press(m, Key{Code: KeyTab}, Key{Code: KeyTab}, Key{Code: KeyDown})
	if it, _ = m.selected(); it.Kind != ItemRun {
		t.Fatalf("run no longer selectable: %+v", it)
	}
}
