package cli

import (
	"slices"
	"testing"

	"github.com/modulecollective/moe/internal/stylesheet"
)

// TestStylesheetVocab pins that the vocabulary handed to Validate is
// built from the live registries: a rename or removal of a workflow,
// stage, or agent that this test names will fail here, flagging that the
// stylesheet's accepted vocabulary moved with it.
func TestStylesheetVocab(t *testing.T) {
	v := stylesheetVocab()

	sdlc, ok := v.Workflows["sdlc"]
	if !ok {
		t.Fatalf("vocab missing sdlc workflow; got %v", v.Workflows)
	}
	for _, stage := range []string{"design", "code", "review", "test", "push"} {
		if !slices.Contains(sdlc, stage) {
			t.Errorf("sdlc vocab missing stage %q; got %v", stage, sdlc)
		}
	}
	if pulse, ok := v.Workflows["pulse"]; !ok || !slices.Contains(pulse, "pulse") {
		t.Errorf("pulse workflow/stage missing; got %v", v.Workflows["pulse"])
	}
	for _, a := range []string{"claude", "codex"} {
		if !slices.Contains(v.Agents, a) {
			t.Errorf("agent vocab missing %q; got %v", a, v.Agents)
		}
	}
}

// TestStylesheetVocabRejectsTypos pins the integration seam: a typo'd
// sheet validated against the registry-built vocab actually refuses, so
// the live vocabulary really rejects unknown names (not just a hand-made
// test vocab). The per-message detail is covered in the stylesheet
// package; here we assert only that each class errors.
func TestStylesheetVocabRejectsTypos(t *testing.T) {
	v := stylesheetVocab()
	bad := []string{
		"sldc.design { model: fable; }", // unknown workflow
		"sdlc.pulse { model: fable; }",  // stage not in that workflow
		".reveiw { model: fable; }",     // unknown stage
		"sdlc.design { modle: fable; }", // unknown property
		"sdlc.design { agent: codxe; }", // unknown agent
	}
	for _, src := range bad {
		s, err := stylesheet.Parse([]byte(src))
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		if err := s.Validate(v); err == nil {
			t.Errorf("expected %q to fail validation against live vocab", src)
		}
	}

	// The operator's live bindings still pass against the real vocab.
	good, err := stylesheet.Parse([]byte("pulse.pulse { agent: claude; model: sonnet; }\nsdlc.design { agent: claude; model: fable; }\n"))
	if err != nil {
		t.Fatalf("parse operator bindings: %v", err)
	}
	if err := good.Validate(v); err != nil {
		t.Errorf("operator bindings rejected by live vocab: %v", err)
	}
}
