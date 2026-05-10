package queue

import (
	"reflect"
	"testing"
)

func TestIndexOf(t *testing.T) {
	items := []Item{
		{Workflow: "sdlc", Project: "moe", Run: "alpha"},
		{Workflow: "sdlc", Project: "moe", Run: "beta"},
	}
	cases := []struct {
		target Item
		want   int
	}{
		{Item{"sdlc", "moe", "alpha"}, 1},
		{Item{"sdlc", "moe", "beta"}, 2},
		{Item{"sdlc", "moe", "gamma"}, 0},
	}
	for _, c := range cases {
		if got := IndexOf(items, c.target); got != c.want {
			t.Errorf("IndexOf(%+v) = %d, want %d", c.target, got, c.want)
		}
	}
}

func TestRemoveFirstIdentityMatch(t *testing.T) {
	items := []Item{
		{"sdlc", "moe", "a"},
		{"sdlc", "moe", "b"},
		{"sdlc", "moe", "a"}, // duplicate identity — only first should drop.
	}
	out, removed := RemoveFirst(items, Item{"sdlc", "moe", "a"})
	if !removed {
		t.Fatalf("expected removed=true")
	}
	want := []Item{{"sdlc", "moe", "b"}, {"sdlc", "moe", "a"}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("RemoveFirst dropped wrong slot:\n got %+v\nwant %+v", out, want)
	}
}

func TestRemoveFirstNoMatch(t *testing.T) {
	items := []Item{{"sdlc", "moe", "a"}}
	out, removed := RemoveFirst(items, Item{"sdlc", "moe", "missing"})
	if removed {
		t.Fatalf("expected removed=false")
	}
	if len(out) != 1 || out[0].Run != "a" {
		t.Fatalf("RemoveFirst should leave the slice intact, got %+v", out)
	}
}

func TestAddItemBackAndFront(t *testing.T) {
	items := []Item{{"sdlc", "moe", "a"}}
	back := AddItem(items, Item{"sdlc", "moe", "b"}, false)
	if back[len(back)-1].Run != "b" {
		t.Fatalf("AddItem(front=false) should append; got %+v", back)
	}
	front := AddItem(items, Item{"sdlc", "moe", "z"}, true)
	if front[0].Run != "z" {
		t.Fatalf("AddItem(front=true) should prepend; got %+v", front)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("missing queue file should not error: %v", err)
	}
	if got != nil {
		t.Fatalf("missing queue file should return nil, got %v", got)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	root := t.TempDir()
	in := []Item{
		{"sdlc", "moe", "alpha"},
		{"sdlc", "tele", "beta"},
	}
	if err := Save(root, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", out, in)
	}
}
