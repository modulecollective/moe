package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/ui"
)

func TestUIChoreActionsLandCommits(t *testing.T) {
	root := seedChoreRootWith(t, "7d")
	claudeRan := filepath.Join(t.TempDir(), "claude-ran")
	fakeClaudeOnPath(t, "#!/bin/sh\ntouch '"+claudeRan+"'\n")
	ctx := context.Background()
	subject := func() string { return strings.TrimSpace(gittest.Output(t, root, "log", "-1", "--format=%s")) }

	// Create from the text form.
	form := chore.Spec{Cadence: "30d", Prompt: "# lint\n"}.Text()
	if r := uiDo(ctx, root, ui.Action{Kind: ui.ActChoreCreate, Project: "moe", Run: "lint", Text: form}); !r.OK || subject() != "chore: add moe/lint" {
		t.Fatalf("create: %+v, subject %q", r, subject())
	}

	// Refusals are definite and write nothing.
	before := gittest.HeadSHA(t, root)
	for name, a := range map[string]ui.Action{
		"unparsable":   {Kind: ui.ActChoreCreate, Project: "moe", Run: "x", Text: "cadence: 1d\n"},
		"invalid":      {Kind: ui.ActChoreCreate, Project: "moe", Run: "x", Text: chore.Spec{Cadence: "1d", When: "w"}.Text()},
		"no workflow":  {Kind: ui.ActChoreCreate, Project: "moe", Run: "x", Text: chore.Spec{Cadence: "1d", Workflow: "no-such"}.Text()},
		"exists":       {Kind: ui.ActChoreCreate, Project: "moe", Run: "lint", Text: form},
		"delete ghost": {Kind: ui.ActChoreDelete, Project: "moe", Run: "ghost"},
	} {
		r := uiDo(ctx, root, a)
		if r.OK || r.Uncertain || r.Message == "" {
			t.Errorf("%s: %+v, want a definite refusal", name, r)
		}
	}
	if gittest.HeadSHA(t, root) != before {
		t.Fatal("a refusal committed")
	}

	// Edit against the form it opened from.
	edited := chore.Spec{Cadence: "30d", Cooldown: "7d", Prompt: "# lint\n"}.Text()
	if r := uiDo(ctx, root, ui.Action{Kind: ui.ActChoreEdit, Project: "moe", Run: "lint", Expected: form, Text: edited}); !r.OK || subject() != "chore: edit moe/lint" {
		t.Fatalf("edit: %+v", r)
	}
	// A stale Expected is a definite Changed refusal.
	before = gittest.HeadSHA(t, root)
	r := uiDo(ctx, root, ui.Action{Kind: ui.ActChoreEdit, Project: "moe", Run: "lint", Expected: form, Text: chore.Spec{Cadence: "60d"}.Text()})
	if r.OK || !r.Changed || r.Uncertain || gittest.HeadSHA(t, root) != before {
		t.Fatalf("stale edit: %+v", r)
	}

	// Open now on a cooling-down chore: the run opens, nothing starts.
	r = uiDo(ctx, root, ui.Action{Kind: ui.ActChoreOpen, Project: "moe", Run: "readme-refresh"})
	if !r.OK || r.Opened == "" || r.Opened != choreOpenRun(t, root, "readme-refresh") {
		t.Fatalf("open: %+v", r)
	}
	if msg := gittest.Output(t, root, "log", "-1", "--format=%B"); !strings.Contains(msg, "MoE-Chore: moe/readme-refresh") || strings.Contains(msg, "MoE-Consent") {
		t.Fatalf("open commit:\n%s", msg)
	}
	before = gittest.HeadSHA(t, root)
	if r := uiDo(ctx, root, ui.Action{Kind: ui.ActChoreOpen, Project: "moe", Run: "readme-refresh"}); r.OK || r.Uncertain || gittest.HeadSHA(t, root) != before {
		t.Fatalf("second open: %+v", r)
	}
	if _, err := os.Stat(claudeRan); !os.IsNotExist(err) {
		t.Fatal("open started an agent")
	}

	// Delete.
	if r := uiDo(ctx, root, ui.Action{Kind: ui.ActChoreDelete, Project: "moe", Run: "lint"}); !r.OK || subject() != "chore: remove moe/lint" {
		t.Fatalf("delete: %+v", r)
	}
}
