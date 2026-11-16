package cli

import (
	"bytes"
	"io"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/stylesheet"
)

// A headless turn that leaves a project-doc finding re-enters itself as a
// fix turn with a copy of the stage's opts. Today that copy carries the
// model the stylesheet resolved, because runStageSession rewrites
// opts.Model in place before the gate reads it. This pins that carry —
// it is not a ruling that the carry is intended; it keeps a refactor of
// runStageSession from changing it by accident. No other test reaches
// the gate's dispatch arm through the real session.
func TestHeadlessProjectDocFixTurnInheritsResolvedModel(t *testing.T) {
	root, _, _ := designFixture(t, "")
	t.Setenv("MOE_FORCE_AGENT", "")
	gittest.WriteAndCommit(t, root, stylesheet.FileName,
		"sdlc.design { model-claude: pinned-design-model; }\n", "seed stylesheet")
	// Writes the canvas (so the turn lands) and a knowledge topic that
	// index.md doesn't list (so the hygiene gate finds something).
	fakeClaudeOnPath(t, `#!/bin/sh
set -eu
prompt=
next=0
for a in "$@"; do
 if [ "$next" = 1 ]; then prompt=$a; next=0; fi
 case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
printf '\nUnresolved: still investigating.\n' >> "$canvas"
k="${canvas%/runs/*}/knowledge"
mkdir -p "$k/topics"
printf '# Knowledge\n' > "$k/index.md"
printf '# Orphan\n\nnot indexed.\n' > "$k/topics/orphan.md"
`)

	var got []stageSessionOpts
	stubProjectDocFixTurn(t, func(_, _, _ string, opts stageSessionOpts, _, _ io.Writer) int {
		got = append(got, opts)
		return 0
	})

	var stdout, stderr bytes.Buffer
	openSdlcDesign("tele", "design-gate", true, "", &stdout, &stderr)
	if len(got) != 1 {
		t.Fatalf("want one fix-turn dispatch, got %d\nstdout=%s\nstderr=%s", len(got), stdout.String(), stderr.String())
	}
	if !got[0].projectDocFixTurn {
		t.Errorf("fix turn dispatched without projectDocFixTurn set")
	}
	if got[0].Model != "pinned-design-model" {
		t.Errorf("fix turn model = %q, want the stylesheet-resolved %q", got[0].Model, "pinned-design-model")
	}
}
