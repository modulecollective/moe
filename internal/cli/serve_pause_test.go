package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/servepause"
	"github.com/modulecollective/moe/internal/ui"
)

func TestServePauseCLIAndTerminalWriter(t *testing.T) {
	root := spawnFixture(t)
	t.Setenv("MOE_HOME", root)
	var out bytes.Buffer
	command := func(args ...string) string {
		t.Helper()
		out.Reset()
		if code := Run(append([]string{"serve"}, args...), &out, &out); code != 0 {
			t.Fatalf("serve %v: %d %s", args, code, &out)
		}
		return out.String()
	}
	if got := command("status"); !strings.Contains(got, "unpaused") {
		t.Fatal(got)
	}
	if got := command("pause"); !strings.Contains(got, "paused until") {
		t.Fatal(got)
	}
	state, err := servepause.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	head := gittest.HeadSHA(t, root)
	command("pause")
	command("status")
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("duplicate/status committed")
	}
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActServePause, PauseRevision: 0}); r.OK || !strings.Contains(r.Message, "refresh") {
		t.Fatalf("stale UI: %+v", r)
	}
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActServePause, PauseRevision: state.Revision}); !r.OK || !strings.Contains(r.Message, "unpaused") {
		t.Fatalf("UI unpause: %+v", r)
	}
	src := newUISource(root, DashFilter{ProjectFilter: "missing"}, os.Stdout)
	if _, err := servepause.Set(root, true, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	if err := src.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := takeBoard(t, src)
	// The PULSE status row is the pause's one home on the board; the
	// overview above it carries only the chart.
	if b.PauseRevision != 3 || !strings.Contains(strings.Join(b.Lines, "\n"), "new automatic sweeps paused until") || b.PauseError != "" {
		t.Fatalf("PULSE row: %+v", b)
	}
	if strings.Contains(strings.Join(overviewAt(b, 120), "\n"), "sweeps paused") {
		t.Fatalf("pause repeated in the overview: %q", overviewAt(b, 120))
	}
	if got := readServeState(root).bannerCluster(time.Now()); !strings.Contains(got, "new automatic sweeps paused until") {
		t.Fatal("pause invisible with serve stopped: " + got)
	}
	command("unpause")
	if err := src.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b = takeBoard(t, src)
	if b.PauseRevision != 4 || strings.Contains(strings.Join(b.Lines, "\n"), "new automatic sweeps paused") || b.PauseSummary != "new automatic sweeps unpaused" {
		t.Fatalf("remote unpause: %+v", b)
	}
	if _, err := servepause.Set(root, true, nil, func() time.Time { return time.Now().Add(-25 * time.Hour) }); err != nil {
		t.Fatal(err)
	}
	if err := src.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b = takeBoard(t, src)
	if b.PauseRevision != 5 || b.PauseSummary != "new automatic sweeps unpaused" || strings.Contains(strings.Join(b.Lines, "\n"), "new automatic sweeps paused") {
		t.Fatalf("expiry: %+v", b)
	}
	for _, args := range [][]string{{"pause", "extra"}, {"unpause", "--dynamic"}, {"status", "extra"}} {
		out.Reset()
		if code := Run(append([]string{"serve"}, args...), &out, &out); code != 2 {
			t.Fatalf("extra operands %v: %d %s", args, code, &out)
		}
	}
	if got := strings.Join(completeWords(root, []string{"serve", ""}), ","); got != "pause,status,unpause" {
		t.Fatalf("completion: %s", got)
	}
	if err := os.WriteFile(filepath.Join(root, servepause.FileName), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run([]string{"serve", "status"}, &out, &out); code != 1 || !strings.Contains(out.String(), "serve-pause.json") {
		t.Fatalf("malformed status: %d %s", code, &out)
	}
	if err := src.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b = takeBoard(t, src)
	if b.PauseError == "" || !strings.Contains(strings.Join(b.Lines, "\n"), "pause unavailable") {
		t.Fatal("PULSE pause error missing")
	}
	if got := readServeState(root).bannerCluster(time.Now()); !strings.Contains(got, "pause unavailable") {
		t.Fatal("dash error missing: " + got)
	}
}
