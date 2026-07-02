package stylesheet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolve pins the selector/specificity/cascade semantics the
// design fixes: highest specificity wins per property, equal specificity
// breaks to last-rule-in-file, and the two properties cascade
// independently.
func TestResolve(t *testing.T) {
	const src = `
* { model: opus; }
sdlc { agent: claude; }
.review { model: gpt-5-codex; agent: codex; }
sdlc.design { model: claude-fable-5; }
`
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := []struct {
		workflow, stage    string
		wantAgent, wantMod string
	}{
		// sdlc.design (spec 2) beats * (spec 0) for model; agent comes
		// from sdlc (the only agent rule that matches design).
		{"sdlc", "design", "claude", "claude-fable-5"},
		// review: model from .review (spec 1 > * spec 0); agent is a tie
		// between sdlc and .review at spec 1 — last-in-file (.review)
		// wins.
		{"sdlc", "review", "codex", "gpt-5-codex"},
		// A workflow that matches only `*`: opus model, no agent rule.
		{"chores", "code", "", "opus"},
		// .review in a different workflow still matches the stage rule.
		{"pdlc", "review", "codex", "gpt-5-codex"},
	}
	for _, c := range cases {
		gotAgent, gotMod := s.Resolve(c.workflow, c.stage)
		if gotAgent != c.wantAgent || gotMod != c.wantMod {
			t.Errorf("Resolve(%q,%q) = (%q,%q), want (%q,%q)",
				c.workflow, c.stage, gotAgent, gotMod, c.wantAgent, c.wantMod)
		}
	}
}

// TestResolveEmptyAndNil covers the no-rules paths: an empty sheet and a
// nil *Sheet both resolve to ("", "") without panicking.
func TestResolveEmptyAndNil(t *testing.T) {
	empty, err := Parse(nil)
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if a, m := empty.Resolve("sdlc", "design"); a != "" || m != "" {
		t.Errorf("empty sheet: got (%q,%q), want empty", a, m)
	}
	var nilSheet *Sheet
	if a, m := nilSheet.Resolve("sdlc", "design"); a != "" || m != "" {
		t.Errorf("nil sheet: got (%q,%q), want empty", a, m)
	}
}

// TestResolveLastWinsSameSelector pins that a property repeated across
// two rules of identical specificity takes the later rule's value.
func TestResolveLastWinsSameSelector(t *testing.T) {
	s, err := Parse([]byte("sdlc.design { model: a; }\nsdlc.design { model: b; }\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, m := s.Resolve("sdlc", "design"); m != "b" {
		t.Errorf("last-wins: got %q, want b", m)
	}
}

// TestParseOperatorFile is the design's first acceptance case: the
// operator's own checked-in file must parse and resolve, comments and
// all.
func TestParseOperatorFile(t *testing.T) {
	const src = `/* Model stylesheet — see projects/moe/runs/model-stylesheets.
   Stages not matched here keep the vendor CLI's own default model.
   ` + "`fable`" + ` is claude's floating latest-in-family alias. */

sdlc.design { model: fable; }
sdlc.review { model: fable; }
`
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a, m := s.Resolve("sdlc", "design"); a != "" || m != "fable" {
		t.Errorf("design: got (%q,%q), want ('','fable')", a, m)
	}
	if a, m := s.Resolve("sdlc", "code"); a != "" || m != "" {
		t.Errorf("unmatched code stage should keep vendor default: got (%q,%q)", a, m)
	}
}

// TestParseUnknownPropertyIgnored pins that a property Resolve never
// reads is legal (valid CSS a browser ignores), not a parse error.
func TestParseUnknownPropertyIgnored(t *testing.T) {
	s, err := Parse([]byte("* { color: red; model: opus; }"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, m := s.Resolve("sdlc", "design"); m != "opus" {
		t.Errorf("got model %q, want opus", m)
	}
}

// TestParseErrors pins that structural malformation refuses loudly, with
// a line number in the message.
func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, src, wantSub string
	}{
		{"no brace", "sdlc.design model: opus;", "no '{'"},
		{"no close", "sdlc.design { model: opus;", "no '}'"},
		{"no colon", "sdlc.design { model opus; }", "missing ':'"},
		{"empty value", "sdlc.design { model: ; }", "empty value"},
		{"empty selector", "{ model: opus; }", "empty selector"},
		{"bad selector", "sd lc { model: opus; }", "invalid"},
		{"unterminated comment", "/* nope", "unterminated comment"},
		{"stray brace", "sdlc { model: opus; ", "no '}'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

// TestParseLineNumbers pins that the line number in a parse error tracks
// through a multi-line comment (stripComments preserves newline count).
func TestParseLineNumbers(t *testing.T) {
	src := "/* one\ntwo\nthree */\nsdlc.design { model opus; }\n"
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "line 4") {
		t.Errorf("want line 4 in %q", err.Error())
	}
}

// TestLoadMissingFile pins that a missing stylesheet is a no-op empty
// sheet, not an error — today's behaviour for a bureaucracy without one.
func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if a, m := s.Resolve("sdlc", "design"); a != "" || m != "" {
		t.Errorf("missing file should resolve empty: got (%q,%q)", a, m)
	}
}

// TestLoadParseFailure pins that a present-but-malformed file surfaces
// the parse error (the caller refuses the turn on it).
func TestLoadParseFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("sdlc { oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected parse error from Load")
	}
}

// TestLoadValidFile round-trips a real file through Load.
func TestLoadValidFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("sdlc.review { model: fable; agent: codex; }"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if a, m := s.Resolve("sdlc", "review"); a != "codex" || m != "fable" {
		t.Errorf("got (%q,%q), want (codex,fable)", a, m)
	}
}
