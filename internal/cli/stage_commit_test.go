package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// commitWikiTurn is the shared per-turn commit for the three wiki-
// attached out-of-band sessions (claim, reflect, lint). The behaviour
// it owns is: stage the wiki content dir; conditionally stage the
// per-run canvas if the agent wrote one; commit both in a single
// `work: <docID> pass <slug>` commit with the right trailers. Claim
// and reflect write a canvas; lint doesn't, and the helper's
// os.Stat-skip is what keeps that case wiki-only. One parameterised
// test pins all three.
func TestCommitWikiTurn(t *testing.T) {
	cases := []struct {
		docID       string
		runSlug     string
		writeCanvas bool
	}{
		// Claim and reflect instruct the agent (in their kickoffs) to
		// drop a per-pass record at canvasRel; the helper must stage it
		// alongside the wiki edits so the session-close gate sees a
		// non-empty canvas at the branch tip — without this, the gate
		// refuses to fast-forward main (the original bug).
		{docID: "claim", runSlug: "claim-2026-05-12-120000", writeCanvas: true},
		{docID: "reflect", runSlug: "reflect-2026-05-11-120000", writeCanvas: true},
		// Lint never writes a canvas; the os.Stat skip in commitWikiTurn
		// is what keeps the commit wiki-only. Pin that branch here —
		// it had no dedicated test before the helper collapse.
		{docID: "lint", runSlug: "lint-2026-05-10-120000", writeCanvas: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.docID, func(t *testing.T) {
			root := newTestBureaucracy(t)

			twinDir := wiki.TwinDir(root, "tele")
			if err := os.MkdirAll(twinDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# vision\n\nupdated.\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			canvasRel := run.ContentPath("tele", tc.runSlug, tc.docID)
			if tc.writeCanvas {
				canvasPath := filepath.Join(root, canvasRel)
				if err := os.MkdirAll(filepath.Dir(canvasPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(canvasPath, []byte("per-pass record.\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			wikiRel, err := filepath.Rel(root, twinDir)
			if err != nil {
				t.Fatal(err)
			}

			if err := commitWikiTurn(root, "twin", "tele", tc.runSlug, tc.docID, wikiRel); err != nil {
				t.Fatalf("commitWikiTurn: %v", err)
			}

			names := gittest.Output(t, root, "show", "--name-only", "--pretty=", "HEAD")
			wantPaths := []string{filepath.Join(wikiRel, "vision.md")}
			if tc.writeCanvas {
				wantPaths = append(wantPaths, canvasRel)
			}
			for _, want := range wantPaths {
				if !strings.Contains(names, want) {
					t.Errorf("commit missing %q in:\n%s", want, names)
				}
			}
			if !tc.writeCanvas && strings.Contains(names, canvasRel) {
				t.Errorf("commit unexpectedly staged absent canvas %q in:\n%s", canvasRel, names)
			}

			subject := gittest.Output(t, root, "log", "-1", "--pretty=%s")
			wantSubject := "work: " + tc.docID + " pass " + tc.runSlug
			if strings.TrimSpace(subject) != wantSubject {
				t.Errorf("commit subject = %q, want %q", strings.TrimSpace(subject), wantSubject)
			}
		})
	}
}
