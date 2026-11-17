package serve

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/project"
)

var (
	openTagRE   = regexp.MustCompile(`<[a-zA-Z]+\s[^>]*>`)
	classAttrRE = regexp.MustCompile(`\sclass="([^"]*)"`)
)

// TestEveryActionChipHasATitle keeps "tooltips everywhere" true as chips
// are added: every opening tag in a template whose class list carries the
// action token must carry a title attribute. Matching the class token on
// the opening tag — not a substring of the file — keeps action-form,
// actions and the chip's own text out of the count.
func TestEveryActionChipHasATitle(t *testing.T) {
	// The chore page's two chips are out of scope: the waiting span
	// already carries its reason inline, and open-run is a navigation.
	exempt := map[string][]string{
		"templates/chore.html": {`aria-disabled="true">`, `href="{{.OpenRunURL}}"`},
	}
	used := map[string]int{}

	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	chips := 0
	for _, name := range files {
		b, err := fs.ReadFile(assets, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range openTagRE.FindAllString(string(b), -1) {
			m := classAttrRE.FindStringSubmatch(tag)
			if m == nil || !slices.Contains(strings.Fields(m[1]), "action") {
				continue
			}
			chips++
			if strings.Contains(tag, ` title="`) {
				continue
			}
			if i := slices.IndexFunc(exempt[name], func(s string) bool { return strings.Contains(tag, s) }); i >= 0 {
				used[name+exempt[name][i]]++
				continue
			}
			t.Errorf("%s: action chip without a title: %s", name, tag)
		}
	}
	if chips == 0 {
		t.Fatal("swept no action chips; the matcher is broken")
	}
	for name, subs := range exempt {
		for _, s := range subs {
			if used[name+s] != 1 {
				t.Errorf("exemption %s %q matched %d chips, want 1", name, s, used[name+s])
			}
		}
	}
}

// TestTipCoversEveryFixedChip: every word a {{tip .}} site can render
// from a known list has a sentence. The control words reach serve as
// opaque strings (serve does not import runcontrol), so they are listed
// here by hand.
func TestTipCoversEveryFixedChip(t *testing.T) {
	words := []string{"pause", "handoff", "park", "finish", "resume", "answer"}
	for _, m := range project.Modes {
		words = append(words, string(m))
	}
	for _, sh := range project.Ships {
		words = append(words, string(sh))
	}
	for _, w := range words {
		if tip(w) == "" {
			t.Errorf("tip(%q) is empty", w)
		}
	}
}

// assertTitled fails when body lacks any of the wanted title attributes
// or renders an empty one — an unknown control word or a runAction built
// without its Title.
func assertTitled(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, `title="`+w) {
			t.Errorf("body missing title starting %q", w)
		}
	}
	if strings.Contains(body, `title=""`) {
		t.Errorf("body renders an empty title:\n%s", body)
	}
}
